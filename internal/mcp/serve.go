package mcp

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Tools are registered before the first Accept: tool-list changes are
	// broadcast to every session, so the set must be fixed up front.
	srv := s.mcpServer()
	g := newGate(idle, func() { ln.Close() })
	stop := context.AfterFunc(ctx, func() {
		g.drain()
		ln.Close()
	})
	defer stop()

	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if g.draining() {
				return nil
			}
			return fmt.Errorf("accepting MCP connection: %w", err)
		}
		wg.Go(func() { serveConn(ctx, srv, conn, identity, g) })
	}
}

// serveConn admits conn and runs its session until the client ends it or
// ctx is done.
func serveConn(ctx context.Context, srv *sdk.Server, conn net.Conn, identity string, g *gate) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	if err := mcpsock.Admit(conn, identity, g.reserve, g.release); err != nil {
		log.Printf("refused MCP connection: %v", err)
		return
	}
	defer g.release()

	ss, err := srv.Connect(ctx, &sdk.IOTransport{Reader: conn, Writer: conn}, nil)
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
	mu      sync.Mutex
	active  int
	isDrain bool
	idle    time.Duration
	timer   *time.Timer
	onIdle  func()
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
	if g.isDrain {
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
	if g.active == 0 && !g.isDrain {
		g.timer.Reset(g.idle)
	}
}

// fire starts draining if the daemon is still idle when the clock runs
// out; a session admitted in the meantime cancels it.
func (g *gate) fire() {
	g.mu.Lock()
	if g.active > 0 || g.isDrain {
		g.mu.Unlock()
		return
	}
	g.isDrain = true
	g.mu.Unlock()
	g.onIdle()
}

// drain refuses all further sessions.
func (g *gate) drain() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.isDrain = true
	g.timer.Stop()
}

func (g *gate) draining() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.isDrain
}
