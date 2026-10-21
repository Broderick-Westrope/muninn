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

// daemonFlag runs the shared MCP server itself; only --daemon-start uses it.
const daemonFlag = "daemon"

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
	if err := checkPrivateDir(spec.StateDir); err != nil {
		return err
	}
	logFile, err := os.OpenFile(spec.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening shared MCP server log: %w", err)
	}
	defer logFile.Close()
	cmd := exec.Command(spec.Muninn, "mcp", "--"+daemonFlag, "--"+mcpsock.SpecFlag, encoded)
	cmd.Stderr = logFile
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting shared MCP server: %w", err)
	}
	return cmd.Process.Release()
}

// checkPrivateDir returns an error unless dir belongs to this user and no
// one else can reach into it. The daemon's socket lives there, and any
// process that can connect to the socket can search the index; the
// directory's permissions are the only access control.
func checkPrivateDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("checking state directory: %w", err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("state directory %s belongs to uid %d, not this user", dir, st.Uid)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("state directory %s is open to other users (mode %v); run `chmod 700 %s` to share the MCP server", dir, perm, dir)
	}
	return nil
}

// runMCPDaemon is the shared daemon: it owns the lock for its spec's
// identity, listens on the spec's socket, and serves sessions until it
// has been idle for defaultMCPIdle. Its stderr is the daemon log.
func runMCPDaemon(ctx context.Context, encoded string) error {
	log.SetOutput(dropInfo{os.Stderr})
	log.SetPrefix(fmt.Sprintf("mcp daemon %d: ", os.Getpid()))
	if err := serveMCPDaemon(ctx, encoded); err != nil {
		// Its stderr is the log, so report it once there, timestamped,
		// rather than again through main's unprefixed "Error:" line.
		log.Printf("exiting: %v", err)
		return &ExitError{Code: 1}
	}
	log.Printf("exiting")
	return nil
}

func serveMCPDaemon(ctx context.Context, encoded string) error {
	spec, err := mcpsock.DecodeSpec(encoded)
	if err != nil {
		return err
	}

	// Wait until reparented so no session is admitted while the daemon is
	// still inside the process tree of the client that started it.
	for deadline := time.Now().Add(daemonSetupTimeout); os.Getppid() != 1; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			return fmt.Errorf("not reparented to launchd after %v (parent %d)", daemonSetupTimeout, os.Getppid())
		}
	}

	// An exiting daemon may still hold the lock for a moment.
	var lk *lock.Lock
	for deadline := time.Now().Add(daemonSetupTimeout); ; time.Sleep(10 * time.Millisecond) {
		lk, err = lock.TryAcquire(spec.Lock())
		if !errors.Is(err, lock.ErrLocked) || time.Now().After(deadline) {
			break
		}
	}
	if errors.Is(err, lock.ErrLocked) {
		log.Printf("another shared MCP server owns this index")
		return nil
	}
	if err != nil {
		// TryAcquire returns a usable lock alongside ErrUnsupported.
		lk.Release()
		return err
	}
	defer lk.Release()

	// Open the searcher before listening, so a socket that answers is one
	// that can serve, however long a cold index takes to load.
	applyMemoryLimit()
	searcher, err := search.Open(spec.IndexDir)
	if err != nil {
		return err
	}
	defer searcher.Close()

	// Holding the lock means no other daemon is alive for this identity,
	// so a socket file here is stale.
	if err := os.Remove(spec.Socket()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale socket: %w", err)
	}
	ln, err := net.Listen("unix", spec.Socket())
	if err != nil {
		return fmt.Errorf("listening on %s: %w", spec.Socket(), err)
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("serving %s", spec.Socket())
	srv := muninnmcp.New(searcher, spec.StatusPath, spec.MirrorsDir)
	return srv.Serve(ctx, ln, spec.Identity, defaultMCPIdle)
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
