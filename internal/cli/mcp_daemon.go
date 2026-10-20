package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/broderick-westrope/muninn/internal/lock"
	muninnmcp "github.com/broderick-westrope/muninn/internal/mcp"
	"github.com/broderick-westrope/muninn/internal/mcpsock"
	"github.com/broderick-westrope/muninn/internal/search"
)

// defaultMCPIdle is how long the shared daemon stays up with no sessions.
// Opening the searcher over 316 shards took 68 ms, so restarting after an
// idle exit is cheap; this mostly saves churn between agent sessions.
const defaultMCPIdle = 5 * time.Minute

// mcpIdle returns the daemon's idle timeout, overridable through
// MUNINN_MCP_IDLE (a Go duration) for tests and tuning.
func mcpIdle() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("MUNINN_MCP_IDLE")); err == nil && d > 0 {
		return d
	}
	return defaultMCPIdle
}

// daemonSetupTimeout bounds each of the daemon's startup waits: being
// reparented, and taking the lock from a daemon that is still exiting.
const daemonSetupTimeout = time.Second

// startMCPDaemon launches the daemon detached and returns at once. The
// daemon gets its own session, no stdio from the client, stderr on the
// log, and / as its working directory; once this process exits, the
// daemon is reparented to launchd and is no longer a descendant of any
// client, so a client killing its process group or tree can't take it
// down, and no client waits on a pipe the daemon holds.
func startMCPDaemon(encoded string) error {
	spec, err := mcpsock.DecodeSpec(encoded)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(spec.StateDir, 0o700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	logFile, err := os.OpenFile(spec.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening MCP daemon log: %w", err)
	}
	defer logFile.Close()
	cmd := exec.Command(spec.Muninn, "mcp", "--daemon", "--spec", encoded)
	cmd.Stderr = logFile
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting MCP daemon: %w", err)
	}
	return cmd.Process.Release()
}

// runMCPDaemon is the shared daemon: it owns the lock for its spec's
// identity, listens on the spec's socket, and serves sessions until it
// has been idle for mcpIdle. Its stderr is the daemon log.
func runMCPDaemon(ctx context.Context, encoded string) error {
	spec, err := mcpsock.DecodeSpec(encoded)
	if err != nil {
		return err
	}
	log.SetOutput(dropInfo{os.Stderr})
	log.SetPrefix(fmt.Sprintf("mcp daemon %d: ", os.Getpid()))

	// Wait until reparented so no session is admitted while the daemon is
	// still inside the process tree of the client that started it.
	if err := pollUntil(daemonSetupTimeout, func() bool { return os.Getppid() == 1 }); err != nil {
		log.Printf("not reparented to launchd (parent %d); exiting", os.Getppid())
		return fmt.Errorf("waiting to be reparented: %w", err)
	}

	lk, err := acquireLock(spec.Lock(), daemonSetupTimeout)
	if errors.Is(err, lock.ErrLocked) {
		// Another daemon owns this identity; the forwarder will reach it.
		return nil
	}
	if err != nil {
		lk.Release()
		log.Printf("can't own %s: %v", spec.Lock(), err)
		return err
	}
	defer lk.Release()

	// Holding the lock means no other daemon is alive for this identity,
	// so a socket file here is stale.
	if err := os.Remove(spec.Socket()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale socket: %w", err)
	}
	ln, err := net.Listen("unix", spec.Socket())
	if err != nil {
		log.Printf("listening: %v", err)
		return fmt.Errorf("listening on %s: %w", spec.Socket(), err)
	}

	applyMemoryLimit(os.Getenv("GOMEMLIMIT"))
	searcher, err := search.Open(spec.IndexDir)
	if err != nil {
		ln.Close()
		log.Printf("opening index: %v", err)
		return err
	}
	defer searcher.Close()

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("serving %s", spec.Socket())
	srv := muninnmcp.New(searcher, spec.StatusPath, spec.MirrorsDir)
	err = srv.Serve(ctx, ln, spec.Identity, mcpIdle())
	log.Printf("exiting: %v", err)
	return err
}

// acquireLock takes path, retrying for up to timeout while another process
// holds it. Its errors match lock.TryAcquire's.
func acquireLock(path string, timeout time.Duration) (*lock.Lock, error) {
	deadline := time.Now().Add(timeout)
	for {
		lk, err := lock.TryAcquire(path)
		if !errors.Is(err, lock.ErrLocked) || time.Now().After(deadline) {
			return lk, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pollUntil polls cond until it holds or timeout passes.
func pollUntil(timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			return fmt.Errorf("condition not met after %v", timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// dropInfo discards Zoekt's informational shard-loading lines and passes
// everything else, including its load errors, through to w. The log
// package makes one Write per line.
type dropInfo struct{ w io.Writer }

func (d dropInfo) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("[INFO]")) {
		return len(p), nil
	}
	return d.w.Write(p)
}
