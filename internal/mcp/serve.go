package mcp

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
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

	srv.AddReceivingMiddleware(cancelWithSession)
	g := newGate(idle, func() { ln.Close() })
	stop := context.AfterFunc(ctx, func() {
		g.drain()
		ln.Close()
	})
	defer stop()

	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if !g.draining() {
				err = fmt.Errorf("accepting MCP connection: %w", err)
			} else {
				err = nil
			}
			// End every session before waiting for them; otherwise an
			// accept error with sessions open would wait forever.
			cancel()
			wg.Wait()
			return err
		}
		wg.Go(func() { serveConn(ctx, srv, conn, identity, g) })
	}
}

// sessionKey carries a session's context through the SDK to
// cancelWithSession. The SDK strips cancellation from the context given to
// Connect but keeps its values.
type sessionKey struct{}

// cancelWithSession cancels a call when its session ends, so a client
// that goes away, or a daemon shutting down, doesn't leave calls running.
// Responses can't be delivered then anyway: the SDK rejects every write
// once a session's input has ended.
func cancelWithSession(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		if session, ok := ctx.Value(sessionKey{}).(context.Context); ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			defer cancel()
			defer context.AfterFunc(session, cancel)()
		}
		return next(ctx, method, req)
	}
}

// sessionTransport ends its session as soon as the SDK fails to read from
// it, whether the client went away or sent something it can't decode.
// Either way the SDK accepts no more input and rejects every later write,
// so nothing the session's calls produce can be delivered.
type sessionTransport struct {
	sdk.Transport
	end context.CancelFunc
}

func (t sessionTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	c, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return sessionConnection{Connection: c, end: t.end}, nil
}

type sessionConnection struct {
	sdk.Connection
	end context.CancelFunc
}

func (c sessionConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	msg, err := c.Connection.Read(ctx)
	if err != nil {
		c.end()
	}
	return msg, err
}

// serveConn admits conn and runs its session until the client ends it or
// ctx is done.
func serveConn(ctx context.Context, srv *sdk.Server, conn net.Conn, identity string, g *gate) {
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

	transport := sessionTransport{Transport: &sdk.IOTransport{Reader: conn, Writer: conn}, end: end}
	ss, err := srv.Connect(context.WithValue(ctx, sessionKey{}, session), transport, nil)
	if err != nil {
		log.Printf("starting MCP session: %v", err)
		return
	}
	log.Printf("MCP session %s opened", ss.ID())
	err = ss.Wait()
	log.Printf("MCP session %s ended: %v", ss.ID(), err)
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
	timer        *time.Timer
	onIdle       func()
}

// newGate returns a gate whose idle clock is already running, so a daemon
// nobody connects to exits too.
func newGate(idle time.Duration, onIdle func()) *gate {
	g := &gate{idle: idle, onIdle: onIdle}
	g.timer = time.AfterFunc(idle, g.fire)
	return g
}

// reserve admits a session unless the daemon is draining.
func (g *gate) reserve() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.drainStarted {
		return false
	}
	g.active++
	g.timer.Stop()
	return true
}

// release ends a session and restarts the idle clock if it was the last.
func (g *gate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.active == 0 && !g.drainStarted {
		g.timer.Reset(g.idle)
	}
}

// fire starts draining if the daemon is still idle when the clock runs
// out; a session admitted in the meantime cancels it.
func (g *gate) fire() {
	g.mu.Lock()
	if g.active > 0 || g.drainStarted {
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
