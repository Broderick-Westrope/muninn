package cli

import (
	"github.com/spf13/cobra"

	"github.com/broderick-westrope/muninn/internal/config"
	muninnmcp "github.com/broderick-westrope/muninn/internal/mcp"
	"github.com/broderick-westrope/muninn/internal/search"
	"github.com/broderick-westrope/muninn/internal/xdg"
)

func newMCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run the MCP server over stdio",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Load the config up front so misconfiguration fails fast,
			// even though the MCP server itself does not need it yet.
			if _, err := config.Load(resolveConfigPath()); err != nil {
				return err
			}
			searcher, err := search.Open(xdg.IndexDir())
			if err != nil {
				return err
			}
			defer searcher.Close()
			srv := muninnmcp.New(searcher, xdg.StatusPath(), xdg.MirrorsDir())
			return srv.Run(cmd.Context())
		},
	}
}
