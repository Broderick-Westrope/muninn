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
	"syscall"
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
	ErrNotAdmitted = errors.New("not admitted by the shared MCP server")

	// ErrBadRequest reports that a connection didn't open with a valid
	// admission request.
	ErrBadRequest = errors.New("bad admission request")

	// ErrIdentityMismatch reports a session whose identity differs from
	// the daemon's: it resolved a different binary or different paths.
	ErrIdentityMismatch = errors.New("identity mismatch")

	// ErrRefused reports a reply Dial can't act on: a refusal other than
	// draining or identity, or something that isn't an admission reply
	// at all (another program, or a muninn speaking another version).
	ErrRefused = errors.New("refused by whatever listens on the socket")

	// ErrDraining reports that the daemon is shutting down and admits no
	// new sessions.
	ErrDraining = errors.New("the shared MCP server is shutting down")
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
		return fmt.Errorf("%w: session %q, server %q", ErrIdentityMismatch, got, identity)
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

// restartAfter is how long Dial waits after starting a daemon before it
// starts another, so a daemon that lost the lock race and exited gets
// replaced. On a busy machine a daemon can take longer than this to start,
// so a spare is sometimes launched; it loses the lock race and exits.
const restartAfter = 1500 * time.Millisecond

// Dial connects to the daemon at socket and asks it to admit a session
// with identity, retrying until ctx expires. When nothing answers or a
// draining daemon refuses it, it calls start to launch a daemon, at most
// once per restartAfter. start gets Dial's context and must return when it
// ends. A start error ends Dial at once. On success the connection carries
// only MCP bytes from then on; on failure the error wraps ErrNotAdmitted.
func Dial(ctx context.Context, socket, identity string, start func(context.Context) error) (*net.UnixConn, error) {
	var lastStart time.Time
	var last error
	for {
		conn, err := admit(ctx, socket, identity)
		if err == nil {
			return conn, nil
		}
		last = err
		if errors.Is(err, ErrIdentityMismatch) || errors.Is(err, ErrRefused) || cannotReach(err) {
			// Another binary, index or program owns this socket, or it
			// can't be reached at all; waiting won't change that.
			return nil, fmt.Errorf("%w: %w", ErrNotAdmitted, err)
		}
		needDaemon := errors.Is(err, ErrDraining) || isNoListener(err)
		if needDaemon && ctx.Err() == nil && (lastStart.IsZero() || time.Since(lastStart) >= restartAfter) {
			if err := start(ctx); err != nil {
				return nil, fmt.Errorf("%w: starting it: %w", ErrNotAdmitted, err)
			}
			lastStart = time.Now()
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
		return nil, fmt.Errorf("%w: reply %q", ErrRefused, reply)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// cannotReach reports whether a dial failed for a reason a daemon starting
// up won't fix: the socket path isn't a socket, or permission is denied.
func cannotReach(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial" && !opErr.Timeout() && !isNoListener(err)
}

// isNoListener reports whether a dial failed because no daemon is
// listening: the socket file is missing or left behind by a dead daemon.
//
// macOS also refuses a connection when a live daemon's listen backlog is
// full; that starts a spare daemon, which loses the lock race and exits.
func isNoListener(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
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
// Starting muninn is slow when many sessions start at once on a busy
// machine: with 15 sessions starting together, a daemon took up to 3 s
// from launch to its first line of Go. A healthy daemon admits a session
// as soon as it listens, so the budget only delays sessions whose daemon
// can't start at all.
const StartupBudget = 5 * time.Second

// Connect asks the daemon for spec to admit a session, starting one with
// `muninn mcp --daemon-start` if none answers, within StartupBudget. Its
// errors wrap ErrNotAdmitted.
func Connect(ctx context.Context, spec Spec) (*net.UnixConn, error) {
	ctx, cancel := context.WithTimeout(ctx, StartupBudget)
	defer cancel()
	return Dial(ctx, spec.Socket(), spec.Identity, func(ctx context.Context) error {
		cmd := exec.CommandContext(ctx, spec.Muninn, "mcp", "--"+DaemonStartFlag, "--"+SpecFlag, spec.Encode())
		cmd.WaitDelay = 100 * time.Millisecond
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
		}
		return nil
	})
}
