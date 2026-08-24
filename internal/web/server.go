// Package web implements muninn's on-demand local web UI: an HTTP server
// exposing a JSON search/file/repos API over the search core, the status
// file, and the bare git mirrors. It binds loopback only unless explicitly
// overridden, because it serves an unauthenticated index of private code.
package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/broderick-westrope/muninn/internal/search"
)

// shutdownTimeout bounds how long graceful shutdown waits for in-flight
// requests after the serve context is canceled.
const shutdownTimeout = 3 * time.Second

// Server holds the dependencies of the HTTP handlers. Handlers are plain
// methods on a mux from Handler, so tests can hit them via httptest without
// a real listener. The status file is re-read on every request that needs
// it (never cached at startup): a mid-session sync updates shards via the
// directory watcher, and a stale cached commit would break the search/file
// line-number guarantee.
type Server struct {
	searcher   *search.Searcher
	statusPath string
	mirrorsDir string
}

// New returns a Server that searches with searcher, resolves indexed
// commits from the status file at statusPath, and reads pinned file
// content from the bare mirrors under mirrorsDir.
func New(searcher *search.Searcher, statusPath, mirrorsDir string) *Server {
	return &Server{searcher: searcher, statusPath: statusPath, mirrorsDir: mirrorsDir}
}

// Handler returns the HTTP handler serving the JSON API, the generated
// chroma stylesheet, and the embedded single-page UI at /.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/file", s.handleFile)
	mux.HandleFunc("GET /api/repos", s.handleRepos)
	mux.HandleFunc("GET /chroma.css", s.handleChromaCSS)
	mux.Handle("GET /", staticHandler())
	return mux
}

// Serve listens on addr and serves the API until ctx is canceled, then
// shuts down gracefully. If ready is non-nil it is called once with the
// server's URL after the listener is bound (useful with port 0).
func (s *Server) Serve(ctx context.Context, addr string, ready func(url string)) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	if ready != nil {
		ready("http://" + ln.Addr().String())
	}

	httpSrv := &http.Server{Handler: s.Handler()}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()

	select {
	case err := <-errCh:
		return fmt.Errorf("serving on %s: %w", addr, err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutting down: %w", err)
		}
		// Serve returns ErrServerClosed once Shutdown starts; drain it so
		// the goroutine exits.
		<-errCh
		return nil
	}
}

// ValidateLoopback returns an error unless addr binds a loopback host
// (127.0.0.0/8, ::1, or a name like localhost resolving only to loopback
// addresses). The server exposes an unauthenticated index of private code,
// so non-loopback binds require an explicit override.
func ValidateLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("listen address %q binds all interfaces", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("listen address %q is not loopback", addr)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("resolving listen host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return errors.New("listen host " + host + " resolved to no addresses")
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("listen host %q resolves to non-loopback address %s", host, ip)
		}
	}
	return nil
}
