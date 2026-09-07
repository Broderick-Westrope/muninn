package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/broderick-westrope/muninn/internal/config"
	"github.com/broderick-westrope/muninn/internal/ctags"
	"github.com/broderick-westrope/muninn/internal/discover"
	"github.com/broderick-westrope/muninn/internal/gitcmd"
	"github.com/broderick-westrope/muninn/internal/index"
	"github.com/broderick-westrope/muninn/internal/launchd"
	"github.com/broderick-westrope/muninn/internal/mirror"
	"github.com/broderick-westrope/muninn/internal/status"
	muninnsync "github.com/broderick-westrope/muninn/internal/sync"
	"github.com/broderick-westrope/muninn/internal/xdg"
)

func newSyncCmd() *cobra.Command {
	var install, uninstall, ifDue, noRepair bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Reconcile repos: clone/fetch mirrors, build index, GC removed repos",
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case install && uninstall:
				return errors.New("--install and --uninstall are mutually exclusive")
			case install:
				return runInstall(cmd)
			case uninstall:
				return runUninstall(cmd)
			}
			return runSync(cmd, ifDue, noRepair)
		},
	}
	cmd.Flags().BoolVar(&install, "install", false, "install the scheduled launchd sync agent")
	cmd.Flags().BoolVar(&uninstall, "uninstall", false, "remove the scheduled launchd sync agent")
	cmd.Flags().BoolVar(&ifDue, "if-due", false, "only sync when the index is stale (used by the scheduled agent)")
	cmd.Flags().BoolVar(&noRepair, "no-repair", false, "never reinstall the launchd agent (used by the scheduled agent)")
	return cmd
}

// resolveConfigPath returns the --config flag value or the XDG default.
func resolveConfigPath() string {
	if configPath != "" {
		return configPath
	}
	return xdg.ConfigPath()
}

// runSync executes the sync pipeline. Per-repo failures are reported in the
// summary but exit zero; only total failure returns an error, so launchd
// does not mark the job crashed for one bad repo. A run skipped because
// another is in flight or because the index is fresh also exits zero: with
// --if-due most scheduled ticks end that way by design.
//
// noRepair suppresses the launchd agent repair. It is a separate flag from
// ifDue so the "never reinstall from inside the scheduled job" guarantee
// does not depend on the plist happening to pass --if-due.
func runSync(cmd *cobra.Command, ifDue, noRepair bool) error {
	// Git is a hard prerequisite, validated up front like ctags: fail the
	// run loudly rather than failing every repo individually.
	if err := gitcmd.Validate(); err != nil {
		return fmt.Errorf("validating git: %w", err)
	}
	cfg, err := config.Load(resolveConfigPath())
	if err != nil {
		return err
	}
	if err := xdg.EnsureDirs(); err != nil {
		return err
	}
	token, err := config.ResolveToken(cfg)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	st, err := muninnsync.Run(cmd.Context(), cfg, muninnsync.Deps{
		Discoverer: &discover.Client{},
		Mirror:     &mirror.Manager{BaseDir: xdg.MirrorsDir()},
		NewIndexer: func(ctagsPath string) muninnsync.Indexer {
			return &index.Indexer{IndexDir: xdg.IndexDir(), CtagsPath: ctagsPath}
		},
		Token:      token,
		StatusPath: xdg.StatusPath(),
		LockPath:   xdg.LockPath(),
		OnlyIfDue:  ifDue,
	})
	switch {
	case errors.Is(err, muninnsync.ErrLocked):
		fmt.Fprintln(out, "another sync is already running — skipped")
		return nil
	case errors.Is(err, muninnsync.ErrNotDue):
		fmt.Fprintf(out, "index is fresh (%s) — skipped\n", freshness(st))
		return nil
	case err != nil:
		return err
	}

	synced, failed := 0, 0
	for _, rs := range st.Repos {
		if rs.Error != "" {
			failed++
		} else {
			synced++
		}
	}
	fmt.Fprintf(out, "synced %d repos, %d failed — see muninn status\n", synced, failed)

	// Repair last, and never from the scheduled job. Reinstalling boots the
	// job out, which would kill a scheduled run mid-sync; doing it after
	// this run's work means the index is already updated if it happens.
	if !noRepair {
		repairAgent(cmd, cfg)
	}
	return nil
}

// freshness describes how long ago the last successful sync finished, for
// the skipped-run message.
func freshness(st *status.SyncStatus) string {
	age, ok := status.SuccessAge(st)
	if !ok {
		return "never synced"
	}
	return "last success " + age.Truncate(time.Second).String() + " ago"
}

// runInstall prepares scheduled runs. launchd inherits neither the shell
// PATH nor its environment, so the paths to the binaries muninn shells out
// to (ctags, gh) are resolved now and persisted. The *token* deliberately
// is not: freezing a value that expires means every scheduled run fails
// once it does, and writes a credential to disk the user never asked us to
// store. Only its resolvability is checked here.
func runInstall(cmd *cobra.Command) error {
	cfgPath, err := filepath.Abs(resolveConfigPath())
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err := xdg.EnsureDirs(); err != nil {
		return err
	}

	ctagsPath, err := ctags.Resolve(cfg.Ctags.Path)
	if err != nil {
		return err
	}
	tok, err := config.ResolveTokenSource(cfg)
	if err != nil {
		return err
	}
	cfg.Ctags.Path = ctagsPath
	if tok.GHPath != "" {
		cfg.Auth.GHPath = tok.GHPath
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if tok.Source == config.TokenSourceEnv {
		// The env var resolved for this shell, but a launchd job has no
		// shell environment, so scheduled runs would fail on discovery.
		// Say so rather than silently copying the secret into the config.
		fmt.Fprintf(out, "warning: the token came from %s, which a scheduled run cannot see.\n", tok.EnvName)
		fmt.Fprintln(out, "         install the GitHub CLI and run `gh auth login` (preferred, survives rotation),")
		fmt.Fprintln(out, "         or set auth.githubToken in the config yourself.")
	}

	plistPath, plist, err := renderAgent(cfg, cfgPath)
	if err != nil {
		return err
	}
	if err := launchd.Install(launchd.ExecLaunchctl{}, plistPath, plist); err != nil {
		return err
	}
	fmt.Fprintf(out, "installed launchd agent %s (%s); checks every %d minutes, syncs when the index is older than %d minutes, reattempts every %d minutes while failing\n",
		launchd.Label, plistPath, cfg.Sync.TickMinutes(), cfg.Sync.IntervalMinutes, cfg.Sync.RetryIntervalMinutes)
	return nil
}

// renderAgent returns the plist path and the plist contents implied by the
// running binary and cfg.
//
// The executable path is deliberately *not* passed through
// filepath.EvalSymlinks. Package managers install a stable symlink that
// they repoint on upgrade (/opt/homebrew/bin/muninn); resolving it would
// bake in the versioned target underneath (…/Cellar/muninn/1.2.3/bin/…),
// which the next upgrade deletes, leaving launchd spawning a path that no
// longer exists — silently, forever.
func renderAgent(cfg *config.Config, cfgPath string) (plistPath string, plist []byte, err error) {
	bin, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("resolving executable path: %w", err)
	}
	bin, err = filepath.Abs(bin)
	if err != nil {
		return "", nil, fmt.Errorf("resolving absolute executable path: %w", err)
	}
	plist, err = launchd.Render(bin, cfgPath, xdg.LogPath(), cfg.Sync.TickMinutes())
	if err != nil {
		return "", nil, err
	}
	plistPath, err = launchd.PlistPath()
	if err != nil {
		return "", nil, err
	}
	return plistPath, plist, nil
}

// repairAgent reinstalls the launchd agent when the binary its plist names
// has gone missing, which is what a package upgrade does when it replaces
// a versioned install directory. Such an agent cannot fix itself — launchd
// spawns a path that no longer exists — so the repair rides along on an
// interactive run.
//
// The trigger is deliberately narrow. Repointing the agent whenever the
// plist merely *differs* from what the running binary implies would let any
// `go run` or scratch build silently capture the schedule, and those paths
// are deleted on exit: exactly the failure this is meant to prevent, only
// self-inflicted. A plist naming a different but existing binary is
// reported by `muninn status` and left alone for the user to decide.
//
// Failures are reported and otherwise ignored: the sync matters more. A
// failed repair is stated as leaving no agent at all, because Install boots
// the old job out before loading the new one and the plist left on disk
// looks healthy either way.
func repairAgent(cmd *cobra.Command, cfg *config.Config) {
	plistPath, err := launchd.PlistPath()
	if err != nil {
		return
	}
	repairAgentAt(cmd, cfg, launchd.ExecLaunchctl{}, plistPath)
}

// repairAgentAt is repairAgent with the launchctl and plist path injected
// so the repair decision can be tested without a real launchd.
func repairAgentAt(cmd *cobra.Command, cfg *config.Config, lc launchd.Launchctl, plistPath string) {
	out := cmd.OutOrStdout()
	plist, err := launchd.Installed(plistPath)
	if err != nil {
		return // not installed, or unreadable: nothing to repair
	}
	bin, err := launchd.ProgramPath(plist)
	if err != nil {
		return // unrecognized plist; `muninn status` reports it
	}
	if _, err := os.Stat(bin); err == nil {
		return // the scheduled binary is still there
	}

	cfgPath, err := filepath.Abs(resolveConfigPath())
	if err != nil {
		return
	}
	_, want, err := renderAgent(cfg, cfgPath)
	if err != nil {
		return
	}
	if err := launchd.Install(lc, plistPath, want); err != nil {
		fmt.Fprintf(out, "warning: the launchd agent points at a missing binary (%s) and could not be repaired: %v\n", bin, err)
		fmt.Fprintln(out, "         no sync is scheduled until you run `muninn sync --install`.")
		return
	}
	fmt.Fprintf(out, "repaired the launchd agent: it pointed at %s, which no longer exists\n", bin)
}

func runUninstall(cmd *cobra.Command) error {
	plistPath, err := launchd.PlistPath()
	if err != nil {
		return err
	}
	if err := launchd.Uninstall(launchd.ExecLaunchctl{}, plistPath); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "removed launchd agent %s\n", launchd.Label)
	return nil
}
