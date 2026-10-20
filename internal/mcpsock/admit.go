// Package mcpsock is how MCP sessions reach muninn's shared MCP daemon
// over a Unix socket: the startup spec that decides which daemon a session
// may share, the admission exchange that runs before any MCP bytes, and
// the byte forwarding between a client's stdio and the daemon.
//
// It imports only the standard library, so a forwarder built on it stays
// small: anything that links Zoekt pays about 26 MB at package init.
package mcpsock

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// admissionTimeout bounds each side of the admission exchange.
const admissionTimeout = 500 * time.Millisecond

// maxLine caps an admission request or reply, so a stranger on the socket
// can't make either side buffer without bound.
const maxLine = 256

const (
	requestPrefix = "muninn-admit v1 "
	replyOK       = "ok "
	replyRefused  = "refused "
)

var (
	// ErrNotAdmitted reports that Dial found no daemon willing to serve
	// this session before its context expired. The caller should serve
	// in-process.
	ErrNotAdmitted = errors.New("not admitted by a shared MCP daemon")

	// ErrBadRequest reports that a connection didn't open with a valid
	// admission request.
	ErrBadRequest = errors.New("bad admission request")

	// ErrIdentityMismatch reports a session whose identity differs from
	// the daemon's: it resolved a different binary or different paths.
	ErrIdentityMismatch = errors.New("identity mismatch")

	// ErrDraining reports that the daemon is shutting down and admits no
	// new sessions.
	ErrDraining = errors.New("daemon is draining")
)

// Admit runs the daemon's side of the admission exchange on a fresh
// connection. reserve is called only for a well-formed request with a
// matching identity, and decides whether the daemon takes the session; it
// must do its own locking. If the reply can't be written after a
// successful reserve, Admit calls release. Admit returns nil only when the
// session was reserved and told so, and leaves every byte after the
// request unread.
func Admit(conn net.Conn, identity string, reserve func() bool, release func()) error {
	if err := conn.SetDeadline(time.Now().Add(admissionTimeout)); err != nil {
		return fmt.Errorf("setting admission deadline: %w", err)
	}
	line, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	got, ok := strings.CutPrefix(line, requestPrefix)
	if !ok {
		writeLine(conn, replyRefused+"request")
		return ErrBadRequest
	}
	if got != identity {
		writeLine(conn, replyRefused+"identity")
		return fmt.Errorf("%w: session %q, daemon %q", ErrIdentityMismatch, got, identity)
	}
	if !reserve() {
		writeLine(conn, replyRefused+"draining")
		return ErrDraining
	}
	if err := writeLine(conn, replyOK+identity); err != nil {
		release()
		return fmt.Errorf("writing admission reply: %w", err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		release()
		return fmt.Errorf("clearing admission deadline: %w", err)
	}
	return nil
}

// retryInterval is how long Dial waits between attempts while a daemon is
// starting or draining.
const retryInterval = 25 * time.Millisecond

// Dial connects to the daemon at socket and asks it to admit a session
// with identity, retrying until ctx expires. When nothing answers, it calls
// start once to launch a daemon, and again after a draining daemon refuses
// it. start gets Dial's context and must return when it ends. A start
// error ends Dial at once. On success the connection carries
// only MCP bytes from then on; on failure the error wraps ErrNotAdmitted.
func Dial(ctx context.Context, socket, identity string, start func(context.Context) error) (*net.UnixConn, error) {
	started := false
	var last error
	for {
		conn, err := admit(ctx, socket, identity)
		if err == nil {
			return conn, nil
		}
		last = err
		if errors.Is(err, ErrIdentityMismatch) {
			// Another binary or another index owns this socket; waiting
			// won't change that.
			return nil, fmt.Errorf("%w: %w", ErrNotAdmitted, err)
		}
		if errors.Is(err, ErrDraining) {
			started = false
		}
		if !started && (errors.Is(err, ErrDraining) || isNoListener(err)) {
			if err := start(ctx); err != nil {
				return nil, fmt.Errorf("%w: starting daemon: %w", ErrNotAdmitted, err)
			}
			started = true
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %w", ErrNotAdmitted, last)
		case <-time.After(retryInterval):
		}
	}
}

// admit makes one dial and admission attempt.
func admit(ctx context.Context, socket, identity string) (*net.UnixConn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	conn := c.(*net.UnixConn)
	deadline := time.Now().Add(admissionTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, err
	}
	if err := writeLine(conn, requestPrefix+identity); err != nil {
		conn.Close()
		return nil, fmt.Errorf("writing admission request: %w", err)
	}
	reply, err := readLine(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading admission reply: %w", err)
	}
	switch reply {
	case replyOK + identity:
	case replyRefused + "draining":
		conn.Close()
		return nil, ErrDraining
	case replyRefused + "identity":
		conn.Close()
		return nil, ErrIdentityMismatch
	default:
		conn.Close()
		return nil, fmt.Errorf("unexpected admission reply %q", reply)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// isNoListener reports whether a dial failed because no daemon is
// listening: the socket file is missing or left behind by a dead daemon.
func isNoListener(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// readLine reads one newline-terminated line a byte at a time, so nothing
// after the newline is consumed, and returns it without the newline.
func readLine(conn net.Conn) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for b.Len() < maxLine {
		if _, err := conn.Read(buf); err != nil {
			return "", err
		}
		if buf[0] == '\n' {
			return b.String(), nil
		}
		b.WriteByte(buf[0])
	}
	return "", fmt.Errorf("line longer than %d bytes", maxLine)
}

func writeLine(conn net.Conn, line string) error {
	_, err := conn.Write([]byte(line + "\n"))
	return err
}

// StartupBudget bounds how long Connect waits to be admitted, including
// starting a daemon, before the session should serve in-process instead.
const StartupBudget = 2 * time.Second

// Connect asks the daemon for spec to admit a session, starting one with
// `muninn mcp --daemon-start` if none answers, within StartupBudget. Its
// errors wrap ErrNotAdmitted.
func Connect(ctx context.Context, spec Spec) (*net.UnixConn, error) {
	ctx, cancel := context.WithTimeout(ctx, StartupBudget)
	defer cancel()
	return Dial(ctx, spec.Socket(), spec.Identity, func(ctx context.Context) error {
		cmd := exec.CommandContext(ctx, spec.Muninn, "mcp", "--daemon-start", "--spec", spec.Encode())
		cmd.WaitDelay = 100 * time.Millisecond
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
		}
		return nil
	})
}
