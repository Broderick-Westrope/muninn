package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

var configPath string

// Version is the release version, overridden at build time via ldflags
// (-X github.com/broderick-westrope/muninn/internal/cli.Version=...).
var Version = "dev"

// ExitError carries a specific process exit code for main to use. Err may
// be nil for silent exits (e.g. `muninn search` with no matches exits 1
// without an error message, following grep).
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit code %d", e.Code)
}

func (e *ExitError) Unwrap() error { return e.Err }

func newRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:           "muninn",
		Short:         "Code search index and MCP server for your GitHub repos",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.PersistentFlags().StringVar(&configPath, "config", "", "path to config file (defaults to ~/.config/muninn/config.json)")

	rootCmd.AddCommand(
		newSyncCmd(),
		newStatusCmd(),
		newMCPCmd(),
		newSearchCmd(),
		newWebCmd(),
	)

	return rootCmd
}

func Execute() error {
	return newRootCmd().Execute()
}
