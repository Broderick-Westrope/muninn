package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/broderick-westrope/muninn/internal/config"
	"github.com/broderick-westrope/muninn/internal/discover"
	"github.com/broderick-westrope/muninn/internal/index"
	"github.com/broderick-westrope/muninn/internal/mirror"
	muninnsync "github.com/broderick-westrope/muninn/internal/sync"
	"github.com/broderick-westrope/muninn/internal/xdg"
)

func newSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Reconcile repos: clone/fetch mirrors, build index, GC removed repos",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSync(cmd)
		},
	}
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
// does not mark the job crashed for one bad repo.
func runSync(cmd *cobra.Command) error {
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

	st, err := muninnsync.Run(cmd.Context(), cfg, muninnsync.Deps{
		Discoverer: &discover.Client{},
		Mirror:     &mirror.Manager{BaseDir: xdg.MirrorsDir()},
		NewIndexer: func(ctagsPath string) muninnsync.Indexer {
			return &index.Indexer{IndexDir: xdg.IndexDir(), CtagsPath: ctagsPath}
		},
		Token:      token,
		StatusPath: xdg.StatusPath(),
	})
	if err != nil {
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
	fmt.Fprintf(cmd.OutOrStdout(), "synced %d repos, %d failed — see muninn status\n", synced, failed)
	return nil
}
