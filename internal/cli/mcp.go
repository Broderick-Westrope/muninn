package cli

import "github.com/spf13/cobra"

func newMCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run the MCP server over stdio",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errNotImplemented
		},
	}
}
