package cli

import (
	"fmt"
	"io"
	"log"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/broderick-westrope/muninn/internal/config"
	"github.com/broderick-westrope/muninn/internal/search"
	"github.com/broderick-westrope/muninn/internal/web"
	"github.com/broderick-westrope/muninn/internal/xdg"
)

func newWebCmd() *cobra.Command {
	var (
		addr         string
		openBrowser  bool
		unsafeListen bool
	)
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Start the local web UI for interactive search",
		Long: `Serve a local search UI over the index until interrupted (Ctrl-C).

The server binds loopback only: it exposes an unauthenticated index of
private code, so non-loopback addresses require --unsafe-listen.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !unsafeListen {
				if err := web.ValidateLoopback(addr); err != nil {
					return fmt.Errorf("%w (pass --unsafe-listen to serve an unauthenticated index of private code beyond this machine)", err)
				}
			}
			// Zoekt logs shard loading via the stdlib logger; keep the
			// server's stderr free of that noise.
			log.SetOutput(io.Discard)
			// Load the config up front so misconfiguration fails fast,
			// even though the web server itself does not need it yet.
			if _, err := config.Load(resolveConfigPath()); err != nil {
				return err
			}
			searcher, err := search.Open(xdg.IndexDir())
			if err != nil {
				return err
			}
			defer searcher.Close()

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			srv := web.New(searcher, xdg.StatusPath(), xdg.MirrorsDir())
			return srv.Serve(ctx, addr, func(url string) {
				fmt.Fprintf(cmd.ErrOrStderr(), "muninn web serving at %s (Ctrl-C to stop)\n", url)
				if openBrowser {
					// Best-effort: the URL is printed regardless, so a
					// failed launch is not worth aborting the server.
					_ = exec.Command("open", url).Start()
				}
			})
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7576", "listen address")
	cmd.Flags().BoolVar(&openBrowser, "open", true, "open the UI in the default browser")
	cmd.Flags().BoolVar(&unsafeListen, "unsafe-listen", false, "allow binding a non-loopback address (dangerous: serves your code unauthenticated)")
	return cmd
}
