package mcpsock

import (
	"bytes"
	"errors"
	"io"
	"net"
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
