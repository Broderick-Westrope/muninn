package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/broderick-westrope/muninn/internal/launchd"
	"github.com/broderick-westrope/muninn/internal/status"
	"github.com/broderick-westrope/muninn/internal/xdg"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print last sync result and per-repo failures",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			st, err := status.Read(xdg.StatusPath())
			switch {
			case errors.Is(err, status.ErrNotExist):
				fmt.Fprintln(out, "never synced — run `muninn sync`")
			case err != nil:
				return err
			default:
				if err := printStatus(out, st); err != nil {
					return err
				}
			}
			fmt.Fprintf(out, "launchd agent: %s\n", agentState())
			return nil
		},
	}
}

func printStatus(out io.Writer, st *status.SyncStatus) error {
	if st.FinishedAt.IsZero() {
		fmt.Fprintf(out, "last run: started %s, not finished (in progress or interrupted)\n", st.StartedAt.Format(time.RFC1123))
	} else {
		fmt.Fprintf(out, "last run: %s (%s ago)\n", st.FinishedAt.Format(time.RFC1123), status.Age(st).Truncate(time.Second))
	}
	// The success clock is reported separately from the attempt clock: a
	// job that fails every hour keeps "last run" recent while the index
	// silently rots, and only this line reveals it. This is the fleet-wide
	// clock, so it stays frozen while any single repo keeps failing — the
	// stale table below is what says whether data is actually old.
	if age, ok := status.SuccessAge(st); !ok {
		fmt.Fprintln(out, "last clean sweep: never")
	} else if age < 0 {
		fmt.Fprintf(out, "last clean sweep: %s (dated in the future — the system clock has moved backwards)\n", st.LastSuccessAt.Format(time.RFC1123))
	} else {
		fmt.Fprintf(out, "last clean sweep: %s (%s ago)\n", st.LastSuccessAt.Format(time.RFC1123), age.Truncate(time.Second))
	}

	failed := failedRepos(st)
	switch {
	case st.Success:
		fmt.Fprintf(out, "result: success (%d repos)\n", len(st.Repos))
	case len(failed) == 0:
		fmt.Fprintf(out, "result: failed (run aborted; %d repos retained from previous run)\n", len(st.Repos))
	default:
		fmt.Fprintf(out, "result: failed (%d of %d repos)\n", len(failed), len(st.Repos))
	}
	if st.Error != "" {
		fmt.Fprintf(out, "error: %s\n", st.Error)
	}
	if st.ConsecutiveFailures > 0 {
		fmt.Fprintf(out, "consecutive failures: %d\n", st.ConsecutiveFailures)
	}
	// Retries are reported even though the run succeeded: absorbing a
	// transient fetch failure is the point, but silently absorbing a
	// growing number of them is how a degrading network stays invisible.
	if repos, retries := fetchRetryTotals(st); retries > 0 {
		fmt.Fprintf(out, "fetch retries: %d across %d repos (transient network failures, recovered)\n", retries, repos)
	}

	if len(failed) > 0 {
		w := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
		fmt.Fprintln(w, "REPO\tERROR")
		for _, name := range failed {
			fmt.Fprintf(w, "%s\t%s\n", name, st.Repos[name].Error)
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("writing status table: %w", err)
		}
	}
	// Housekeeping failures are listed apart from real ones: these repos
	// are fully indexed, so they do not make the run a failure.
	if gcFailed := gcFailedRepos(st); len(gcFailed) > 0 {
		w := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
		fmt.Fprintln(w, "REPO\tGC WARNING (indexed anyway)")
		for _, name := range gcFailed {
			fmt.Fprintf(w, "%s\t%s\n", name, st.Repos[name].GCError)
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("writing gc table: %w", err)
		}
	}
	// Stale data is a separate question from a failed run: a repo can be
	// days old while every recent run "succeeded" at everything it could
	// still reach.
	if stale := status.StaleRepos(st, status.StaleAfter); len(stale) > 0 {
		w := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
		fmt.Fprintf(w, "REPO\tSTALE (no successful sync in over %s)\n", status.StaleAfter)
		for _, name := range stale {
			if age, ok := status.RepoSuccessAge(st, name); ok {
				fmt.Fprintf(w, "%s\t%s ago\n", name, age.Truncate(time.Second))
			} else {
				fmt.Fprintf(w, "%s\tnever synced\n", name)
			}
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("writing stale table: %w", err)
		}
		// Every run refetches and reindexes the whole fleet, so a repo
		// that will never succeed costs a full pass on every tick. Naming
		// that here keeps the cost visible instead of leaving it as
		// invisible background load once the warnings stopped shouting.
		fmt.Fprintln(out, "note: every sync refetches all repos, so persistent failures above cost a full pass each run")
	}
	return nil
}

// agentState describes the launchd agent: not installed, healthy, or
// installed but no longer usable. The last case is the one worth detail —
// a plist pointing at a binary a package upgrade has moved, or a job that
// was never registered, is spawned by launchd never or not at all, and
// nothing else surfaces it.
func agentState() string {
	plistPath, err := launchd.PlistPath()
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	return describeAgent(launchd.ExecLaunchctl{}, plistPath)
}

// describeAgent is agentState with the plist path and launchctl injected so
// it can be tested without a real home directory or a real launchd.
func describeAgent(lc launchd.Launchctl, plistPath string) string {
	plist, err := launchd.Installed(plistPath)
	if errors.Is(err, launchd.ErrNotInstalled) {
		return "not installed — run `muninn sync --install`"
	}
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}

	// Ask launchd, not the filesystem. A bootstrap that failed after the
	// plist was written leaves a file identical to a healthy install, so
	// the plist alone cannot distinguish "scheduled" from "not running".
	loaded, lerr := lc.Loaded(launchd.Label)
	switch {
	case lerr != nil:
		return fmt.Sprintf("installed (%s) — could not confirm it is loaded: %v", plistPath, lerr)
	case !loaded:
		return fmt.Sprintf("installed (%s) — BROKEN: the plist exists but launchd has not loaded the job, so no sync is scheduled; run `muninn sync --install`", plistPath)
	}

	bin, err := launchd.ProgramPath(plist)
	if err != nil {
		return fmt.Sprintf("installed (%s) — unrecognized contents: %v; run `muninn sync --install`", plistPath, err)
	}
	if _, err := os.Stat(bin); err != nil {
		return fmt.Sprintf("installed (%s) — BROKEN: points at %s, which no longer exists (moved by an upgrade?); run `muninn sync --install`", plistPath, bin)
	}
	self, err := os.Executable()
	if err == nil && filepath.Clean(bin) != filepath.Clean(self) {
		return fmt.Sprintf("installed (%s) — points at %s, not this binary (%s); run `muninn sync --install` if that is wrong", plistPath, bin, self)
	}
	return "installed (" + plistPath + ")"
}

// failedRepos returns the names of repos with errors, sorted.
func failedRepos(st *status.SyncStatus) []string {
	var failed []string
	for name, rs := range st.Repos {
		if rs.Error != "" {
			failed = append(failed, name)
		}
	}
	sort.Strings(failed)
	return failed
}

// fetchRetryTotals returns how many repos needed a fetch retry in the most
// recent run and the total retries spent across them.
func fetchRetryTotals(st *status.SyncStatus) (repos, retries int) {
	for _, rs := range st.Repos {
		if rs.FetchRetries > 0 {
			repos++
			retries += rs.FetchRetries
		}
	}
	return repos, retries
}

// gcFailedRepos returns the names of repos whose housekeeping failed but
// which are otherwise fully synced, sorted.
func gcFailedRepos(st *status.SyncStatus) []string {
	var failed []string
	for name, rs := range st.Repos {
		if rs.GCError != "" && rs.Error == "" {
			failed = append(failed, name)
		}
	}
	sort.Strings(failed)
	return failed
}
