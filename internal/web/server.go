// Package web implements muninn's on-demand local web UI: an HTTP server
// exposing a JSON search/file/repos API over the search core, the status
// file, and the bare git mirrors. It binds loopback only unless explicitly
// overridden, because it serves an unauthenticated index of private code.
package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/broderick-westrope/muninn/internal/search"
)

const (
	// shutdownTimeout bounds how long graceful shutdown waits for
	// in-flight requests after the serve context is canceled.
	shutdownTimeout = 3 * time.Second
	// readHeaderTimeout bounds how long a client may dribble request
	// headers before the connection is dropped (slowloris guard).
	readHeaderTimeout = 5 * time.Second
	// defaultMaxTreeEntries caps one directory listing served by /api/tree.
	defaultMaxTreeEntries = 2000
)

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
	// checkouts maps lowercased "owner/name" to a local checkout path
	// (from ScanCheckouts); may be nil when no editor roots are
	// configured.
	checkouts map[string]string
	// editorScheme is the URL scheme for open-in-editor links ("cursor"
	// or "vscode").
	editorScheme string
	// maxTreeEntries caps one /api/tree listing, bounding the payload and
	// the DOM for a pathological generated directory. A field rather than
	// a const so tests can reach the truncation path without a fixture of
	// thousands of files.
	maxTreeEntries int
	// launch starts the editor. A field so tests can exercise the success
	// path of /api/open without executing a real editor.
	launch func(scheme, dir, file string, line int) error
}

// New returns a Server that searches with searcher, resolves indexed
// commits from the status file at statusPath, and reads pinned file
// content from the bare mirrors under mirrorsDir. checkouts (may be nil)
// maps lowercased "owner/name" repos to local checkout paths used for
// open-in-editor links with editorScheme.
func New(searcher *search.Searcher, statusPath, mirrorsDir string, checkouts map[string]string, editorScheme string) *Server {
	return &Server{
		searcher:       searcher,
		statusPath:     statusPath,
		mirrorsDir:     mirrorsDir,
		checkouts:      checkouts,
		editorScheme:   editorScheme,
		maxTreeEntries: defaultMaxTreeEntries,
		launch:         launchEditor,
	}
}

// Handler returns the HTTP handler serving the JSON API, the generated
// chroma stylesheet, and the embedded single-page UI at /.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/file", s.handleFile)
	mux.HandleFunc("GET /api/tree", s.handleTree)
	mux.HandleFunc("GET /api/repos", s.handleRepos)
	// Registered here rather than in Serve's middleware chain so httptest
	// requests against Handler() exercise the CSRF guard.
	mux.HandleFunc("POST /api/open", s.handleOpen)
	mux.HandleFunc("GET /chroma.css", s.handleChromaCSS)
	mux.Handle("GET /", staticHandler())
	return mux
}

// hostCheck wraps next with DNS-rebinding protection: a malicious page can
// point attacker-controlled DNS at 127.0.0.1 and drive a victim's browser
// to this server under the attacker's origin, so requests whose Host
// header is not localhost, a loopback IP literal, or the exact host the
// listener was bound with are rejected with 403. It also sets
// X-Content-Type-Options: nosniff on every response.
func hostCheck(boundHost string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !hostAllowed(r.Host, boundHost) {
			http.Error(w, "forbidden: unrecognized Host header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed reports whether a request Host header (possibly host:port)
// names this server: "localhost", any loopback IP literal, or the exact
// bound host.
func hostAllowed(hostport, boundHost string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	// A bracketed IPv6 literal without a port fails SplitHostPort.
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "localhost" {
		return true
	}
	if boundHost != "" && host == boundHost {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
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

	boundHost, _, _ := net.SplitHostPort(addr)
	httpSrv := &http.Server{
		Handler:           hostCheck(boundHost, s.Handler()),
		ReadHeaderTimeout: readHeaderTimeout,
		// The global stdlib logger is discarded to silence zoekt's shard
		// noise; keep the server's own error reporting visible.
		ErrorLog: log.New(os.Stderr, "muninn web: ", log.LstdFlags),
	}
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

// ResolveListenAddr validates that addr binds a loopback host and returns
// a concrete ip:port to listen on. The server exposes an unauthenticated
// index of private code, so non-loopback binds require an explicit
// override. Hostnames (like localhost) are resolved here, exactly once:
// the returned address is an IP literal, so the subsequent Listen cannot
// re-resolve the name to a different, unvetted address (TOCTOU).
func ResolveListenAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if host == "" {
		return "", fmt.Errorf("listen address %q binds all interfaces", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return addr, nil
		}
		return "", fmt.Errorf("listen address %q is not loopback", addr)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return "", fmt.Errorf("resolving listen host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return "", errors.New("listen host " + host + " resolved to no addresses")
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return "", fmt.Errorf("listen host %q resolves to non-loopback address %s", host, ip)
		}
	}
	return net.JoinHostPort(ips[0].String(), port), nil
}
