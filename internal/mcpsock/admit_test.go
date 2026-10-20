package mcpsock

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shortSocketPath returns a socket path short enough for macOS's 104-byte
// sun_path limit; t.TempDir under /var/folders is often too long.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mcpsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// gate is a server-side admission policy for tests.
type gate struct {
	open     bool
	admitted int
}

func (g *gate) reserve() bool {
	if !g.open {
		return false
	}
	g.admitted++
	return true
}

func (g *gate) release() { g.admitted-- }

// serveOnce listens on socket and runs Admit on the first connection,
// sending its result and the connection on the returned channels.
func serveOnce(t *testing.T, socket, identity string, g *gate) (<-chan error, <-chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	errs := make(chan error, 1)
	conns := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errs <- err
			return
		}
		t.Cleanup(func() { conn.Close() })
		errs <- Admit(conn, identity, g.reserve, g.release)
		conns <- conn
	}()
	return errs, conns
}

func noStart() error { return errors.New("no daemon in this test") }

func TestDialAdmitted(t *testing.T) {
	socket := shortSocketPath(t)
	g := &gate{open: true}
	errs, _ := serveOnce(t, socket, "abc", g)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := Dial(ctx, socket, "abc", noStart)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if err := <-errs; err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if g.admitted != 1 {
		t.Errorf("admitted = %d, want 1", g.admitted)
	}
}

func TestDialRefused(t *testing.T) {
	tests := map[string]struct {
		serverIdentity string
		open           bool
		wantAdmitErr   error
	}{
		"identity mismatch": {serverIdentity: "other", open: true, wantAdmitErr: ErrIdentityMismatch},
		"draining":          {serverIdentity: "abc", open: false, wantAdmitErr: ErrDraining},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			socket := shortSocketPath(t)
			g := &gate{open: tc.open}
			errs, _ := serveOnce(t, socket, tc.serverIdentity, g)

			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if _, err := Dial(ctx, socket, "abc", noStart); !errors.Is(err, ErrNotAdmitted) {
				t.Errorf("Dial err = %v, want ErrNotAdmitted", err)
			}
			if err := <-errs; !errors.Is(err, tc.wantAdmitErr) {
				t.Errorf("Admit err = %v, want %v", err, tc.wantAdmitErr)
			}
			if g.admitted != 0 {
				t.Errorf("admitted = %d, want 0", g.admitted)
			}
		})
	}
}

func TestDialSilentServerTimesOut(t *testing.T) {
	socket := shortSocketPath(t)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		// Accept and never answer.
		conn, err := ln.Accept()
		if err == nil {
			t.Cleanup(func() { conn.Close() })
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := Dial(ctx, socket, "abc", noStart); !errors.Is(err, ErrNotAdmitted) {
		t.Errorf("Dial err = %v, want ErrNotAdmitted", err)
	}
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Errorf("Dial took %v, want it bounded by the context", elapsed)
	}
}

func TestAdmitRejectsStranger(t *testing.T) {
	socket := shortSocketPath(t)
	g := &gate{open: true}
	errs, _ := serveOnce(t, socket, "abc", g)

	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := <-errs; !errors.Is(err, ErrBadRequest) {
		t.Errorf("Admit err = %v, want ErrBadRequest", err)
	}
	if g.admitted != 0 {
		t.Errorf("admitted = %d, want 0", g.admitted)
	}
}

func TestAdmitLeavesFollowingBytesUnread(t *testing.T) {
	socket := shortSocketPath(t)
	g := &gate{open: true}
	errs, conns := serveOnce(t, socket, "abc", g)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := Dial(ctx, socket, "abc", noStart)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if err := <-errs; err != nil {
		t.Fatalf("Admit: %v", err)
	}
	server := <-conns

	if _, err := conn.Write([]byte("first MCP line\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(server).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "first MCP line\n" {
		t.Errorf("server read %q after admission, want the client's first line intact", line)
	}
}

func TestDialStartsDaemonWhenNoneListens(t *testing.T) {
	socket := shortSocketPath(t)
	g := &gate{open: true}
	starts := 0
	var errs <-chan error
	start := func() error {
		starts++
		errs, _ = serveOnce(t, socket, "abc", g)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := Dial(ctx, socket, "abc", start)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if err := <-errs; err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if starts != 1 {
		t.Errorf("start called %d times, want 1", starts)
	}
}

func TestDialStartError(t *testing.T) {
	socket := shortSocketPath(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := Dial(ctx, socket, "abc", noStart); !errors.Is(err, ErrNotAdmitted) {
		t.Errorf("Dial err = %v, want ErrNotAdmitted", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Dial took %v after start failed, want an immediate return", elapsed)
	}
}
