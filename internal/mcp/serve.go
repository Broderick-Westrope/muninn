package mcp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/broderick-westrope/muninn/internal/mcpsock"
)

// Serve runs muninn's shared MCP daemon on ln. Every connection that
// passes admission for identity becomes its own MCP session on one shared
// SDK server, so all sessions share s's searcher.
//
// Serve closes ln and returns nil once no session has been open for idle.
// When ctx is done it closes ln and every session, and returns nil.
func (s *Server) Serve(ctx context.Context, ln net.Listener, identity string, idle time.Duration) error {
	// Tools are registered before the first Accept: tool-list changes are
	// broadcast to every session, so the set must be fixed up front.
	return serve(ctx, ln, s.mcpServer(), identity, idle)
}

func serve(ctx context.Context, ln net.Listener, srv *sdk.Server, identity string, idle time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	srv.AddReceivingMiddleware(boundCalls)
	g := newGate(idle, func() { ln.Close() })
	stop := context.AfterFunc(ctx, func() {
		g.drain()
		ln.Close()
	})
	defer stop()

	var wg sync.WaitGroup
	var sessions atomic.Uint64 // numbers sessions in the log
	for {
		conn, err := ln.Accept()
		if err != nil {
			if !g.draining() {
				err = fmt.Errorf("accepting MCP connection: %w", err)
			} else {
				err = nil
			}
			// Close the listener here rather than leave it to the
			// AfterFunc: closing removes the socket file, and that must
			// happen before the caller releases the lock, or it could
			// remove a successor daemon's socket.
			ln.Close()
			// End every session before waiting for them; otherwise an
			// accept error with sessions open would wait forever.
			cancel()
			wg.Wait()
			return err
		}
		wg.Go(func() { serveConn(ctx, srv, conn, identity, g, &sessions) })
	}
}

// serveConn admits conn and runs its session until the client ends it or
// ctx is done.
func serveConn(ctx context.Context, srv *sdk.Server, conn net.Conn, identity string, g *gate, sessions *atomic.Uint64) {
	session, end := context.WithCancel(ctx)
	defer end()
	// Ending the session closes its connection, which also unblocks a
	// response write to a client that has stopped reading.
	defer context.AfterFunc(session, func() { conn.Close() })()
	defer conn.Close()

	if err := mcpsock.Admit(conn, identity, g.reserve, g.release); err != nil {
		log.Printf("refused MCP connection: %v", err)
		return
	}
	defer g.release()

	sc := sessionConn{Conn: conn, end: end}
	ss, err := srv.Connect(context.WithValue(ctx, sessionKey{}, session), &sdk.IOTransport{Reader: sc, Writer: sc}, nil)
	if err != nil {
		log.Printf("starting MCP session: %v", err)
		return
	}
	id := sessions.Add(1)
	log.Printf("session %d opened", id)
	// The daemon closes a session's socket to end it, so a closed
	// connection is how every session ends, not an error.
	if err := ss.Wait(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("session %d ended: %v", id, err)
		return
	}
	log.Printf("session %d ended", id)
}

// sessionKey carries a session's context through the SDK to boundCalls.
// The SDK strips cancellation from the context given to Connect but keeps
// its values.
type sessionKey struct{}

// callTimeout bounds every call the daemon serves. Git-backed tools have
// their own shorter deadlines, but searches have none, and a call whose
// session stopped reading undecodable input can't be cancelled any other
// way; in a shared daemon it would keep the daemon from ever idling out.
// No legitimate call comes near it.
var callTimeout = 5 * time.Minute

// boundCalls ends each call when its session ends, so a client that goes
// away, or a daemon shutting down, doesn't leave calls running, and after
// callTimeout regardless. Responses can't be delivered once a session has
// ended anyway: the SDK rejects every write after its input ends.
func boundCalls(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		ctx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		if session, ok := ctx.Value(sessionKey{}).(context.Context); ok {
			defer context.AfterFunc(session, cancel)()
		}
		return next(ctx, method, req)
	}
}

// responseWriteTimeout bounds each write to a session's client. A response
// is a few kilobytes of text, so a client that can't take one in this long
// has stopped reading; without the bound it would hold its session, and
// the daemon, for as long as its process lives.
var responseWriteTimeout = 30 * time.Second

// sessionConn ends its session when a read fails (the client went away or
// ended its input) or a write times out (the client stopped reading).
//
// It wraps the socket rather than the SDK's Connection, because the SDK
// tells its own Connection type the negotiated protocol version through an
// unexported method that a wrapper would hide. The cost: undecodable input
// doesn't end the session at once. The SDK stops reading, and the session
// ends when its calls finish, within callTimeout.
type sessionConn struct {
	net.Conn
	end context.CancelFunc
}

func (c sessionConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		c.end()
	}
	return n, err
}

func (c sessionConn) Write(p []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(responseWriteTimeout)); err != nil {
		c.end()
		return 0, err
	}
	n, err := c.Conn.Write(p)
	if err != nil {
		c.end()
	}
	return n, err
}

// gate counts open sessions and decides when the daemon has been idle long
// enough to exit. Admission and the idle-to-draining transition share its
// mutex, so a session is either admitted before draining starts or refused;
// it is never admitted by a daemon that is about to exit.
type gate struct {
	mu           sync.Mutex
	active       int
	drainStarted bool
	idle         time.Duration
	onIdle       func()

	// period numbers the current idle period. A timer callback carries the
	// period it was started for, so one that fires just as a session opens
	// and is still waiting for the lock after that session has closed
	// can't end the idle period that began after it.
	period uint64
	timer  *time.Timer
}

// newGate returns a gate whose idle clock is already running, so a daemon
// nobody connects to exits too.
func newGate(idle time.Duration, onIdle func()) *gate {
	g := &gate{idle: idle, onIdle: onIdle}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.startIdle()
	return g
}

// startIdle begins a new idle period. The caller holds mu.
func (g *gate) startIdle() {
	g.period++
	period := g.period
	g.timer = time.AfterFunc(g.idle, func() { g.expire(period) })
}

// reserve admits a session unless the daemon is draining.
func (g *gate) reserve() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.drainStarted {
		return false
	}
	g.active++
	g.period++ // no idle period while a session is open
	g.timer.Stop()
	return true
}

// release ends a session and starts an idle period if it was the last.
func (g *gate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.active == 0 && !g.drainStarted {
		g.startIdle()
	}
}

// expire starts draining if idle period is still the current one.
func (g *gate) expire(period uint64) {
	g.mu.Lock()
	if period != g.period || g.active > 0 || g.drainStarted {
		g.mu.Unlock()
		return
	}
	g.drainStarted = true
	g.mu.Unlock()
	g.onIdle()
}

// drain refuses all further sessions.
func (g *gate) drain() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.drainStarted = true
	g.timer.Stop()
}

func (g *gate) draining() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.drainStarted
}
