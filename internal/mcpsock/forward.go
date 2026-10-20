package mcpsock

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
)

// InProcessEnv, set to any non-empty value, makes `muninn mcp` serve
// in-process. The proxy sets it when it hands a session back to muninn:
// an environment variable survives any argument list, where a flag
// appended after a "--" would be read as a positional argument.
const InProcessEnv = "MUNINN_MCP_IN_PROCESS"

// ErrDaemonGone reports that the daemon closed the connection while the
// client was still sending: it crashed or was killed. The forwarder exits
// non-zero, as `muninn mcp` would have if it had crashed itself; it never
// falls back mid-session, because the session's state lived in the daemon.
var ErrDaemonGone = errors.New("the shared MCP server closed the session")

// Forward copies in to conn and conn to out until the session ends. When
// in reaches EOF it half-closes conn so the daemon sees the end of the
// session, then keeps copying until the daemon closes its side. It returns
// nil when the client ended the session, and ErrDaemonGone when the
// daemon ended it first.
func Forward(conn *net.UnixConn, in io.Reader, out io.Writer) error {
	var inDone atomic.Bool
	go func() {
		if _, err := io.Copy(conn, in); err == nil {
			// Set before CloseWrite: the daemon closes only after it sees
			// this EOF, so the flag is visible by the time out's copy ends.
			inDone.Store(true)
		}
		conn.CloseWrite()
	}()
	_, err := io.Copy(out, conn)
	if err != nil {
		return fmt.Errorf("forwarding shared MCP server output: %w", err)
	}
	if !inDone.Load() {
		return ErrDaemonGone
	}
	return nil
}
