package mcpsock

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// socketPair returns the two ends of a connected Unix socket.
func socketPair(t *testing.T) (client *net.UnixConn, server net.Conn) {
	t.Helper()
	socket := shortSocketPath(t)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	c, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server = <-accepted
	t.Cleanup(func() { c.Close(); server.Close() })
	return c.(*net.UnixConn), server
}

// forward runs Forward in the background and returns its result channel.
func forward(conn *net.UnixConn, in io.Reader, out io.Writer) <-chan error {
	done := make(chan error, 1)
	go func() { done <- Forward(conn, in, out) }()
	return done
}

func wait(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("Forward did not return")
		return nil
	}
}

func TestForwardCopiesBothWaysAndEndsCleanly(t *testing.T) {
	client, server := socketPair(t)
	var out bytes.Buffer
	done := forward(client, bytes.NewBufferString("request\n"), &out)

	// stdin's EOF must reach the daemon as EOF, so it knows the session
	// is over.
	got, err := io.ReadAll(server)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "request\n" {
		t.Errorf("daemon read %q, want %q", got, "request\n")
	}
	if _, err := server.Write([]byte("response\n")); err != nil {
		t.Fatal(err)
	}
	server.Close()

	if err := wait(t, done); err != nil {
		t.Errorf("Forward = %v, want nil after stdin ended first", err)
	}
	if out.String() != "response\n" {
		t.Errorf("stdout = %q, want %q", out.String(), "response\n")
	}
}

func TestForwardReportsDaemonGone(t *testing.T) {
	client, server := socketPair(t)
	stdin, _ := io.Pipe() // never ends
	done := forward(client, stdin, io.Discard)

	server.Close()

	if err := wait(t, done); !errors.Is(err, ErrDaemonGone) {
		t.Errorf("Forward = %v, want ErrDaemonGone", err)
	}
}

// failingReader fails its first read, as stdin does when the client's pipe
// breaks.
type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestForwardReportsAStdinFailureAsSuch(t *testing.T) {
	client, server := socketPair(t)
	broken := errors.New("stdin broke")
	done := forward(client, failingReader{broken}, io.Discard)

	// The daemon sees the half-close and ends the session.
	io.ReadAll(server)
	server.Close()

	if err := wait(t, done); !errors.Is(err, broken) {
		t.Errorf("Forward = %v, want the stdin error", err)
	}
}

// sendAfter returns one line once gate closes, then blocks; it records
// when that line was handed out.
type sendAfter struct {
	gate, sent chan struct{}
	once       sync.Once
}

func (r *sendAfter) Read(p []byte) (int, error) {
	<-r.gate
	n := 0
	r.once.Do(func() {
		n = copy(p, "request\n")
		close(r.sent)
	})
	if n == 0 {
		select {} // the client is still there, just quiet
	}
	return n, nil
}

// holdFirstWrite blocks its first Write until release closes.
type holdFirstWrite struct {
	release chan struct{}
	once    sync.Once
}

func (w *holdFirstWrite) Write(p []byte) (int, error) {
	w.once.Do(func() { <-w.release })
	return len(p), nil
}

// The daemon dies while the client is still sending, and the forwarder's
// write to it fails before the forwarder has seen the daemon's EOF.
func TestForwardReportsDaemonGoneMidWrite(t *testing.T) {
	client, server := socketPair(t)
	in := &sendAfter{gate: make(chan struct{}), sent: make(chan struct{})}
	out := &holdFirstWrite{release: make(chan struct{})}
	done := forward(client, in, out)

	// The daemon sends one byte, so the forwarder's output copy blocks in
	// out, then dies.
	if _, err := server.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	server.Close()
	close(in.gate)
	<-in.sent
	time.Sleep(100 * time.Millisecond) // let the socket write fail
	close(out.release)

	if err := wait(t, done); !errors.Is(err, ErrDaemonGone) {
		t.Errorf("Forward = %v, want ErrDaemonGone", err)
	}
}
