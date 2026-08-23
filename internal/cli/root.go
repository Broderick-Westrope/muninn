package cli

import "github.com/spf13/cobra"

var configPath string

func newRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:           "muninn",
		Short:         "Code search index and MCP server for your GitHub repos",
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
