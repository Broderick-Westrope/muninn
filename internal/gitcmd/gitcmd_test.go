package gitcmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// git runs a real git command for fixture setup, failing the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{"-c", "user.name=test", "-c", "user.email=test@example.com"}
	if dir != "" {
		base = append(base, "-C", dir)
	}
	cmd := exec.Command("git", append(base, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// scratchRepo creates a repo with one commit and returns its path.
func scratchRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644))
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "initial")
	return dir
}

// fakeGit installs a fake git shell script at the front of PATH via
// t.Setenv, so the calling test must not use t.Parallel.
func fakeGit(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestHermeticConfig(t *testing.T) {
	// Uses t.Setenv, so no t.Parallel.
	repo := scratchRepo(t)
	git(t, repo, "config", "user.name", "probe")

	poisoned := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(poisoned, []byte("[[[ this is not valid gitconfig\n"), 0o644))
	t.Setenv("GIT_CONFIG_GLOBAL", poisoned)

	// Control assertion: raw exec with the poisoned global config hard-fails
	// any config-reading git command.
	cmd := exec.Command("git", "-C", repo, "config", "--get", "user.name")
	cmd.Env = os.Environ()
	require.Error(t, cmd.Run(), "control: poisoned global gitconfig should fail raw git")

	// The same probe through Runner succeeds: the poisoned file is never
	// read.
	out, err := Runner{}.Run(context.Background(), "-C", repo, "config", "--get", "user.name")
	require.NoError(t, err)
	require.Equal(t, "probe", out)
}

func TestEnvFiltering(t *testing.T) {
	// Uses t.Setenv, so no t.Parallel.
	repo := scratchRepo(t)
	t.Setenv("GIT_DIR", "/nonexistent")

	out, err := Runner{}.Run(context.Background(), "-C", repo, "rev-parse", "HEAD")
	require.NoError(t, err)
	require.Len(t, out, 40)
}

func TestTimeout(t *testing.T) {
	// Uses t.Setenv, so no t.Parallel. exec replaces the shell so the kill
	// on deadline reaches sleep directly and Wait returns promptly.
	fakeGit(t, "#!/bin/sh\nexec sleep 10\n")

	start := time.Now()
	_, err := Runner{Timeout: 100 * time.Millisecond}.Run(context.Background(), "version")
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrTimeout)
	require.Less(t, elapsed, 2*time.Second, "timeout must fire well before the fake git's 10s sleep")
}

func TestPartialOutputOnTimeout(t *testing.T) {
	// Uses t.Setenv, so no t.Parallel. echo flushes before exec hands the
	// process over to sleep, so the line is captured before the kill. The
	// timeout is generous enough for the shell to start and print, yet far
	// below the 10s sleep.
	fakeGit(t, "#!/bin/sh\necho partial-line\nexec sleep 10\n")

	out, err := Runner{Timeout: 500 * time.Millisecond}.RunRaw(context.Background(), "log")
	require.ErrorIs(t, err, ErrTimeout)
	require.Contains(t, out, "partial-line")
}

func TestExitCodeAndStderr(t *testing.T) {
	t.Parallel()
	repo := scratchRepo(t)

	_, err := Runner{}.Run(context.Background(), "-C", repo, "cat-file", "-e", "doesnotexist")
	require.Error(t, err)

	var gitErr *Error
	require.ErrorAs(t, err, &gitErr)
	require.Equal(t, 128, gitErr.ExitCode)
	require.Contains(t, gitErr.Stderr, "Not a valid object name")
	require.Contains(t, gitErr.Error(), "Not a valid object name")
}

func TestRunStdin(t *testing.T) {
	t.Parallel()
	repo := scratchRepo(t)

	sha, err := Runner{}.RunStdin(context.Background(), "hello\n", "-C", repo, "hash-object", "-w", "--stdin")
	require.NoError(t, err)
	require.Len(t, sha, 40)

	out, err := Runner{}.RunRaw(context.Background(), "-C", repo, "cat-file", "blob", sha)
	require.NoError(t, err)
	require.Equal(t, "hello\n", out)
}

func TestValidate(t *testing.T) {
	t.Parallel()
	require.NoError(t, Validate())
}

func TestValidateRejectsOldGit(t *testing.T) {
	// Uses t.Setenv, so no t.Parallel.
	fakeGit(t, "#!/bin/sh\necho 'git version 2.20.0'\n")

	err := Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "2.20")
	require.Contains(t, err.Error(), "2.32")
}
