package mcpsock

import (
	"errors"
	"fmt"
	"io"
	"net"
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
	// stdinEnded is closed once in has ended, before CloseWrite: the daemon
	// closes only after it sees that, so it is visible by the time out's
	// copy ends. A failed write to conn means the daemon went, which isn't
	// a stdin problem, so only in's own read error is kept.
	stdinEnded := make(chan struct{})
	src := &readErrRecorder{r: in}
	go func() {
		_, err := io.Copy(conn, src)
		if err == nil || src.err != nil {
			close(stdinEnded)
		}
		conn.CloseWrite()
	}()
	if _, err := io.Copy(out, conn); err != nil {
		return fmt.Errorf("forwarding shared MCP server output: %w", err)
	}
	select {
	case <-stdinEnded:
	default:
		return ErrDaemonGone
	}
	if src.err != nil {
		return fmt.Errorf("reading stdin: %w", src.err)
	}
	return nil
}

// readErrRecorder keeps r's first non-EOF read error.
type readErrRecorder struct {
	r   io.Reader
	err error
}

func (r *readErrRecorder) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
	}
	return n, err
}
