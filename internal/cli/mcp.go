package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/broderick-westrope/muninn/internal/config"
	"github.com/broderick-westrope/muninn/internal/gitcmd"
	muninnmcp "github.com/broderick-westrope/muninn/internal/mcp"
	"github.com/broderick-westrope/muninn/internal/mcpsock"
	"github.com/broderick-westrope/muninn/internal/search"
	"github.com/broderick-westrope/muninn/internal/xdg"
)

// mcpMemoryLimit is the soft heap limit for the MCP server. Measured on a
// 316-repo index, the default GC let one process sit at about 88 MB after
// a burst of greps; 64 MiB brought that to about 67 MB for 0.04 s more GC
// CPU, while 48 and 32 MiB saved more but cost 2.5 to 3.5 times the CPU.
const mcpMemoryLimit = 64 << 20

// applyMemoryLimit sets mcpMemoryLimit unless the user chose their own
// limit through GOMEMLIMIT.
func applyMemoryLimit() {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	debug.SetMemoryLimit(mcpMemoryLimit)
}

func newMCPCmd() *cobra.Command {
	var inProcess, daemon, daemonStart bool
	var spec string
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run the MCP server over stdio",
		Long: `Run the MCP server over stdio.

Sessions share one background server per index, started on demand and
stopped after a few idle minutes, so each extra session costs little
memory. If the shared server can't be reached, the session is served
in-process instead. --in-process always serves in-process.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case daemon:
				return runMCPDaemon(cmd.Context(), spec)
			case daemonStart:
				return startMCPDaemon(spec)
			}
			// Zoekt logs shard loading via the stdlib logger; keep the
			// server's stderr free of that noise, but not of errors such
			// as a recovered tool panic.
			log.SetOutput(dropInfo{os.Stderr})
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
			if !inProcess && os.Getenv(mcpsock.InProcessEnv) == "" {
				err := forwardToMCPDaemon(cmd.Context())
				var fallback serveInProcess
				if !errors.As(err, &fallback) {
					return err
				}
				fmt.Fprintf(os.Stderr, "muninn: serving in-process: %v\n", fallback.reason)
			}
			return serveMCPInProcess(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&inProcess, "in-process", false, "serve this session in-process instead of through the shared server")
	cmd.Flags().BoolVar(&daemon, daemonFlag, false, "run the shared MCP server")
	cmd.Flags().BoolVar(&daemonStart, mcpsock.DaemonStartFlag, false, "start the shared MCP server detached")
	cmd.Flags().StringVar(&spec, mcpsock.SpecFlag, "", "startup spec for --daemon and --daemon-start")
	for _, name := range []string{daemonFlag, mcpsock.DaemonStartFlag, mcpsock.SpecFlag} {
		cmd.Flags().MarkHidden(name)
	}
	return cmd
}

// serveInProcess reports why the session could not be handed to the
// shared MCP server, before any MCP bytes were read.
type serveInProcess struct{ reason error }

func (e serveInProcess) Error() string { return e.reason.Error() }
func (e serveInProcess) Unwrap() error { return e.reason }

// forwardToMCPDaemon hands this session's stdio to the shared daemon for
// its spec, starting one if needed. It returns a serveInProcess
// error if no daemon admitted the session; nothing has been
// read from stdin then, so the caller can still serve in-process. After
// admission it never falls back.
func forwardToMCPDaemon(ctx context.Context) error {
	exe, err := os.Executable()
	if err != nil {
		return serveInProcess{fmt.Errorf("locating muninn binary: %w", err)}
	}
	spec, err := mcpsock.NewSpec(exe, xdg.IndexDir(), xdg.StatusPath(), xdg.MirrorsDir(), xdg.StateDir())
	if err != nil {
		return serveInProcess{err}
	}
	// Check before dialing, not only before starting a daemon: whatever
	// already listens in a directory others can reach might not be ours.
	if err := os.MkdirAll(spec.StateDir, 0o700); err != nil {
		return serveInProcess{fmt.Errorf("creating state directory: %w", err)}
	}
	if err := checkPrivateDir(spec.StateDir); err != nil {
		return serveInProcess{err}
	}
	execProxy(spec)
	conn, err := mcpsock.Connect(ctx, spec)
	if err != nil {
		return serveInProcess{err}
	}
	defer conn.Close()
	return mcpsock.Forward(conn, os.Stdin, os.Stdout)
}

// proxyName is the small forwarder installed next to muninn. It links only
// the standard library, so it costs about 3 MB per session where muninn,
// which links Zoekt, costs about 30 MB before it does anything.
const proxyName = "muninn-mcp-proxy"

// execProxy replaces this process with the proxy installed next to the
// muninn binary, if there is one, handing it the spec and this process's
// arguments so it can exec back into muninn to serve in-process. Nothing
// has been read from stdin yet, and exec keeps the pid and stdio, so the
// client sees no change. It returns only if there's no proxy or the exec
// failed, and the caller then forwards from this process instead.
func execProxy(spec mcpsock.Spec) {
	proxy := filepath.Join(filepath.Dir(spec.Muninn), proxyName)
	if _, err := os.Stat(proxy); err != nil {
		return
	}
	argv := append([]string{proxy, "--spec", spec.Encode(), "--"}, os.Args[1:]...)
	err := syscall.Exec(proxy, argv, os.Environ())
	fmt.Fprintf(os.Stderr, "muninn: running %s: %v\n", proxy, err)
}

// serveMCPInProcess serves one session over stdio from this process.
func serveMCPInProcess(ctx context.Context) error {
	applyMemoryLimit()
	searcher, err := search.Open(xdg.IndexDir())
	if err != nil {
		return err
	}
	defer searcher.Close()
	srv := muninnmcp.New(searcher, xdg.StatusPath(), xdg.MirrorsDir())
	return srv.Run(ctx)
}
