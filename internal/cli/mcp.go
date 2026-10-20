package cli

import (
	"fmt"
	"io"
	"log"
	"os"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/broderick-westrope/muninn/internal/config"
	"github.com/broderick-westrope/muninn/internal/gitcmd"
	muninnmcp "github.com/broderick-westrope/muninn/internal/mcp"
	"github.com/broderick-westrope/muninn/internal/search"
	"github.com/broderick-westrope/muninn/internal/xdg"
)

// mcpMemoryLimit is the soft heap limit for the MCP server. Measured on a
// 316-repo index, the default GC let one process sit at about 88 MB after
// a burst of greps; 64 MiB brought that to about 67 MB with no extra GC
// CPU, while 48 and 32 MiB saved more but cost 2.5 to 3.5 times the CPU.
const mcpMemoryLimit = 64 << 20

// applyMemoryLimit sets mcpMemoryLimit unless the user chose their own
// limit through GOMEMLIMIT (passed as gomemlimit).
func applyMemoryLimit(gomemlimit string) {
	if gomemlimit != "" {
		return
	}
	debug.SetMemoryLimit(mcpMemoryLimit)
}

func newMCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run the MCP server over stdio",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Zoekt logs shard loading via the stdlib logger; keep the
			// server's stderr free of that noise.
			log.SetOutput(io.Discard)
			applyMemoryLimit(os.Getenv("GOMEMLIMIT"))
			// Git is a hard prerequisite for the mirror-backed tools: fail
			// fast and loudly instead of surfacing per-call errors later.
			if err := gitcmd.Validate(); err != nil {
				return fmt.Errorf("validating git: %w", err)
			}
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
