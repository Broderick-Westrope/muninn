package mcpsock

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
)

// ErrDaemonGone reports that the daemon closed the connection while the
// client was still sending: it crashed or was killed. The forwarder exits
// non-zero, as `muninn mcp` would have if it had crashed itself; it never
// falls back mid-session, because the session's state lived in the daemon.
var ErrDaemonGone = errors.New("the shared MCP server closed the session")

// Forward copies in to conn and conn to out until the session ends. When
// in ends it half-closes conn so the daemon sees the end of the session,
// then keeps copying until the daemon closes its side. It returns nil when
// the client ended the session, the read error if in failed, and
// ErrDaemonGone when the daemon ended it first.
func Forward(conn *net.UnixConn, in io.Reader, out io.Writer) error {
	// inErr is set, before CloseWrite, once in has ended: the daemon closes
	// only after it sees that, so it is visible by the time out's copy ends.
	var inEnded atomic.Bool
	var inErr atomic.Value
	go func() {
		if _, err := io.Copy(conn, in); err != nil {
			inErr.Store(err)
		}
		inEnded.Store(true)
		conn.CloseWrite()
	}()
	if _, err := io.Copy(out, conn); err != nil {
		return fmt.Errorf("forwarding shared MCP server output: %w", err)
	}
	if !inEnded.Load() {
		return ErrDaemonGone
	}
	if err, ok := inErr.Load().(error); ok {
		return fmt.Errorf("reading stdin: %w", err)
	}
	return nil
}
