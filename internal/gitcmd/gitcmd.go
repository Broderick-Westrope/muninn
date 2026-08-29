// Package gitcmd runs git subprocesses with a hermetic environment, a
// deadline, and exit-code-aware errors. Every git-invoking component uses it
// so user and system gitconfig (grep.patternType, log.showSignature,
// credential helpers, url.insteadOf rewrites, ...) can never silently change
// semantics, and no invocation can hang forever.
package gitcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout is the deadline applied to git invocations when the Runner
// does not specify one.
const DefaultTimeout = 15 * time.Second

// waitDelay bounds how long Wait blocks after the context expires. A killed
// git can leave grandchildren (such as pack-objects) holding the stdout
// pipe; without WaitDelay, Wait would hang on the pipe — the exact failure
// the timeout exists to prevent.
const waitDelay = 5 * time.Second

// minMajor and minMinor form the git version floor. 2.32 is required for
// GIT_CONFIG_GLOBAL (silently ignored on older git, which would make
// hermeticity a lie) and also covers --end-of-options, %as, and
// update-ref --stdin needs.
const (
	minMajor = 2
	minMinor = 32
)

// ErrTimeout is wrapped by errors returned when a git invocation exceeds its
// deadline.
var ErrTimeout = errors.New("git command timed out")

// Error is the error type returned for failed git invocations. Callers need
// ExitCode because git uses exit 1 as a legitimate answer in places
// (merge-base with disjoint histories) — string-matching stderr is not an
// API.
type Error struct {
	// Args are the git arguments that were invoked.
	Args []string
	// ExitCode is the process exit code, or -1 if the process did not exit
	// normally (for example when killed on timeout).
	ExitCode int
	// Stderr is the captured standard error output.
	Stderr string
	// Err is the underlying error, wrapping ErrTimeout on deadline expiry.
	Err error
}

// Error implements the error interface. The message includes the trimmed
// stderr text so callers matching on git's diagnostics (such as "Not a valid
// object name") keep working.
func (e *Error) Error() string {
	msg := fmt.Sprintf("git %s: %v", strings.Join(e.Args, " "), e.Err)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += " (stderr: " + s + ")"
	}
	return msg
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// Runner invokes git with a hermetic environment. The zero value is usable:
// a zero Timeout means DefaultTimeout, and ExtraEnv is appended to the
// hermetic environment (last value wins for duplicated variables).
type Runner struct {
	// Timeout is the per-invocation deadline; 0 means DefaultTimeout.
	Timeout time.Duration
	// ExtraEnv is appended after the hermetic environment.
	ExtraEnv []string
}

// Run executes git with args and returns its trimmed stdout. Captured
// stdout is returned even on error so callers can surface partial results
// (for example labeled partial output after a timeout). On deadline expiry
// the returned error wraps ErrTimeout.
func (r Runner) Run(ctx context.Context, args ...string) (string, error) {
	out, err := r.run(ctx, "", args)
	return strings.TrimSpace(out), err
}

// RunRaw executes git with args and returns its stdout verbatim. Captured
// stdout is returned even on error so callers can surface partial results.
// On deadline expiry the returned error wraps ErrTimeout.
func (r Runner) RunRaw(ctx context.Context, args ...string) (string, error) {
	return r.run(ctx, "", args)
}

// RunStdin executes git with args, providing stdin to the process, and
// returns its trimmed stdout. Captured stdout is returned even on error so
// callers can surface partial results. On deadline expiry the returned
// error wraps ErrTimeout.
func (r Runner) RunStdin(ctx context.Context, stdin string, args ...string) (string, error) {
	out, err := r.run(ctx, stdin, args)
	return strings.TrimSpace(out), err
}

// run is the shared invocation path: hermetic env, deadline (respecting an
// earlier caller deadline), WaitDelay, and stdout capture even on error.
func (r Runner) run(ctx context.Context, stdin string, args []string) (string, error) {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	// WithTimeout keeps an earlier caller deadline if it is sooner.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = r.env()
	cmd.WaitDelay = waitDelay
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}

	exitCode := -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%w: %v", ErrTimeout, err)
	}
	return stdout.String(), &Error{
		Args:     args,
		ExitCode: exitCode,
		Stderr:   stderr.String(),
		Err:      err,
	}
}

// env builds the hermetic environment: the process environment with every
// GIT_* variable dropped (except GIT_TRACE* for debugging — an MCP server
// launched from an editor hook with GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE/
// GIT_OBJECT_DIRECTORY set must not operate on the wrong repo), global and
// system config disabled, terminal prompts off, and safe.directory=*
// re-injected (wiping global config also wipes any safe.directory entries;
// mirrors owned by a different uid — Docker, shared volumes — must keep
// working). ExtraEnv is appended last so callers can extend or override.
func (r Runner) env() []string {
	base := os.Environ()
	env := make([]string, 0, len(base)+6+len(r.ExtraEnv))
	for _, kv := range base {
		if dropVar(kv) {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0=*",
	)
	return append(env, r.ExtraEnv...)
}

// dropVar reports whether an environment entry is a GIT_* variable that must
// not leak into subprocesses. GIT_TRACE* is allowlisted for debugging.
func dropVar(kv string) bool {
	name, _, ok := strings.Cut(kv, "=")
	if !ok || !strings.HasPrefix(name, "GIT_") {
		return false
	}
	return !strings.HasPrefix(name, "GIT_TRACE")
}

// Validate checks that git is installed and at least version 2.32, the
// floor required for the hermetic environment (GIT_CONFIG_GLOBAL) to be
// honored. It returns an actionable error naming the found version and the
// floor.
func Validate() error {
	out, err := Runner{}.Run(context.Background(), "version")
	if err != nil {
		return fmt.Errorf("git not found or not runnable: %w; install git %d.%d or newer and ensure it is on PATH", err, minMajor, minMinor)
	}
	major, minor, err := parseVersion(out)
	if err != nil {
		return fmt.Errorf("%w; git %d.%d or newer is required", err, minMajor, minMinor)
	}
	if major < minMajor || (major == minMajor && minor < minMinor) {
		return fmt.Errorf("git %d.%d is too old: version %d.%d or newer is required (for GIT_CONFIG_GLOBAL support); please upgrade git", major, minor, minMajor, minMinor)
	}
	return nil
}

// parseVersion extracts the major and minor version from `git version`
// output such as "git version 2.39.5 (Apple Git-154)".
func parseVersion(out string) (major, minor int, err error) {
	fields := strings.Fields(out)
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return 0, 0, fmt.Errorf("unexpected `git version` output %q", out)
	}
	parts := strings.Split(fields[2], ".")
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("unexpected git version string %q", fields[2])
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parsing git major version from %q: %w", fields[2], err)
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parsing git minor version from %q: %w", fields[2], err)
	}
	return major, minor, nil
}
