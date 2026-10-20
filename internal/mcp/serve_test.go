package mcp

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/broderick-westrope/muninn/internal/mcpsock"
)

const testIdentity = "test-identity"

// daemon is a Server running Serve on a short Unix socket path.
type daemon struct {
	socket string
	done   chan error
}

func startDaemon(t *testing.T, srv *Server, idle time.Duration) *daemon {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mcpd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &daemon{socket: socket, done: make(chan error, 1)}
	go func() { d.done <- srv.Serve(ctx, ln, testIdentity, idle) }()
	t.Cleanup(func() {
		cancel()
		<-d.done
	})
	return d
}

func noStart(context.Context) error { return errors.New("no daemon start in this test") }

// connect opens an MCP client session to d through admission.
func (d *daemon) connect(t *testing.T) *sdk.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := mcpsock.Dial(ctx, d.socket, testIdentity, noStart)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test"}, nil)
	cs, err := client.Connect(ctx, &sdk.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func grepText(t *testing.T, cs *sdk.ClientSession, pattern string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "grep", Arguments: map[string]any{"pattern": pattern}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestServeSharesOneServerAcrossSessions(t *testing.T) {
	f := newFixture(t, "")
	d := startDaemon(t, f.srv, time.Minute)

	var wg sync.WaitGroup
	for range 3 {
		cs := d.connect(t)
		wg.Go(func() {
			if out := grepText(t, cs, "banana"); !strings.Contains(out, "acme/widget/other.go:3:") {
				t.Errorf("grep output missing match:\n%s", out)
			}
		})
	}
	wg.Wait()
}

func TestServeExitsAfterIdle(t *testing.T) {
	f := newFixture(t, "")
	d := startDaemon(t, f.srv, 100*time.Millisecond)

	cs := d.connect(t)
	grepText(t, cs, "banana")
	cs.Close()

	select {
	case err := <-d.done:
		if err != nil {
			t.Errorf("Serve = %v, want nil after idling out", err)
		}
		d.done <- err // for the cleanup
	case <-time.After(2 * time.Second):
		t.Fatal("Serve still running after its idle timeout with no sessions")
	}
	if _, err := net.Dial("unix", d.socket); err == nil {
		t.Error("socket still accepts connections after idle exit")
	}
}

func TestServeStaysUpWhileASessionIsOpen(t *testing.T) {
	f := newFixture(t, "")
	d := startDaemon(t, f.srv, 100*time.Millisecond)

	cs := d.connect(t)
	time.Sleep(300 * time.Millisecond)

	select {
	case err := <-d.done:
		d.done <- err
		t.Fatalf("Serve returned %v with a session still open", err)
	default:
	}
	if out := grepText(t, cs, "banana"); !strings.Contains(out, "other.go") {
		t.Errorf("grep after idle period failed:\n%s", out)
	}
}

func TestServeRefusesWrongIdentity(t *testing.T) {
	f := newFixture(t, "")
	d := startDaemon(t, f.srv, time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := mcpsock.Dial(ctx, d.socket, "someone-else", noStart); !errors.Is(err, mcpsock.ErrNotAdmitted) {
		t.Errorf("Dial err = %v, want ErrNotAdmitted", err)
	}
}

// blockingServer is an SDK server with one tool, "block", that waits until
// its context is cancelled and reports that on cancelled.
func blockingServer(started, cancelled chan<- struct{}) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: "test"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "block"}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
		started <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		return nil, nil, ctx.Err()
	})
	return srv
}

// startServe runs serve over srv on a short socket path.
func startServe(t *testing.T, ctx context.Context, ln net.Listener, srv *sdk.Server) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln, srv, testIdentity, time.Minute) }()
	return done
}

func listenShort(t *testing.T) (net.Listener, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mcpd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	return ln, socket
}

// callBlock starts a "block" call in a new session and returns the
// session's connection, which the test closes to make the client vanish.
func callBlock(t *testing.T, socket string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := mcpsock.Dial(ctx, socket, testIdentity, noStart)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	client := sdk.NewClient(&sdk.Implementation{Name: "test"}, nil)
	cs, err := client.Connect(ctx, &sdk.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	go cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "block", Arguments: map[string]any{}})
	return conn
}

func receive(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestServeCancelsCallsWhenTheClientGoes(t *testing.T) {
	started, cancelled := make(chan struct{}, 2), make(chan struct{}, 2)
	ln, socket := listenShort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := startServe(t, ctx, ln, blockingServer(started, cancelled))
	defer func() { cancel(); <-done }()

	gone := callBlock(t, socket)
	receive(t, started, "the first call to start")
	callBlock(t, socket)
	receive(t, started, "the second call to start")

	gone.Close()
	receive(t, cancelled, "the departed client's call to be cancelled")
	select {
	case <-cancelled:
		t.Error("the other session's call was cancelled too")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestServeShutdownCancelsCalls(t *testing.T) {
	started, cancelled := make(chan struct{}, 1), make(chan struct{}, 1)
	ln, socket := listenShort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := startServe(t, ctx, ln, blockingServer(started, cancelled))

	callBlock(t, socket)
	receive(t, started, "the call to start")
	cancel()

	receive(t, cancelled, "the call to be cancelled by shutdown")
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve = %v, want nil on shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return after shutdown")
	}
}

// failingListener returns err from Accept after its first connection.
type failingListener struct {
	net.Listener
	accepted bool
	err      error
}

func (l *failingListener) Accept() (net.Conn, error) {
	if l.accepted {
		return nil, l.err
	}
	l.accepted = true
	return l.Listener.Accept()
}

func TestServeAcceptErrorEndsOpenSessions(t *testing.T) {
	started, cancelled := make(chan struct{}, 1), make(chan struct{}, 1)
	ln, socket := listenShort(t)
	broken := errors.New("accept broke")
	gate := make(chan struct{})
	fl := &failingListener{Listener: ln, err: broken}
	done := startServe(t, context.Background(), &gatedListener{fl, gate}, blockingServer(started, cancelled))

	callBlock(t, socket)
	receive(t, started, "the call to start")
	close(gate) // let the next Accept fail

	select {
	case err := <-done:
		if !errors.Is(err, broken) {
			t.Errorf("serve = %v, want the accept error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve hung on an accept error with a session open")
	}
}

// gatedListener holds every Accept after the first until gate closes.
type gatedListener struct {
	*failingListener
	gate chan struct{}
}

func (l *gatedListener) Accept() (net.Conn, error) {
	if l.accepted {
		<-l.gate
	}
	return l.failingListener.Accept()
}
