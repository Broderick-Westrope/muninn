package gitcmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// scratchRepo creates a repo with one commit and returns its path.
func scratchRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "initial")
	return dir
}

// fakeGit installs a fake git shell script at the front of PATH via
// t.Setenv.
func fakeGit(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake git script: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestHermeticConfig(t *testing.T) {
	repo := scratchRepo(t)
	git(t, repo, "config", "user.name", "probe")

	poisoned := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(poisoned, []byte("[[[ this is not valid gitconfig\n"), 0o644); err != nil {
		t.Fatalf("writing poisoned gitconfig: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", poisoned)

	// Control assertion: raw exec with the poisoned global config hard-fails
	// any config-reading git command.
	cmd := exec.Command("git", "-C", repo, "config", "--get", "user.name")
	cmd.Env = os.Environ()
	if err := cmd.Run(); err == nil {
		t.Fatal("control: poisoned global gitconfig should fail raw git")
	}

	// The same probe through Runner succeeds: the poisoned file is never
	// read.
	out, err := Runner{}.Run(context.Background(), "-C", repo, "config", "--get", "user.name")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "probe" {
		t.Fatalf("user.name = %q, want %q", out, "probe")
	}
}

func TestEnvFiltering(t *testing.T) {
	repo := scratchRepo(t)
	t.Setenv("GIT_DIR", "/nonexistent")

	out, err := Runner{}.Run(context.Background(), "-C", repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != 40 {
		t.Fatalf("rev-parse output length = %d, want 40: %q", len(out), out)
	}
}

func TestTimeout(t *testing.T) {
	// exec replaces the shell so the kill on deadline reaches sleep
	// directly and Wait returns promptly.
	fakeGit(t, "#!/bin/sh\nexec sleep 10\n")

	start := time.Now()
	_, err := Runner{Timeout: 100 * time.Millisecond}.Run(context.Background(), "version")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("elapsed = %v; timeout must fire well before the fake git's 10s sleep", elapsed)
	}
}

func TestPartialOutputOnTimeout(t *testing.T) {
	// echo flushes before exec hands the process over to sleep, so the
	// line is captured before the kill. Shell startup can occasionally
	// exceed the timeout under full-suite load, missing the echo; a
	// bounded retry tolerates that scheduling race while a genuine
	// output-discarding regression still fails every attempt.
	fakeGit(t, "#!/bin/sh\necho partial-line\nexec sleep 10\n")

	var out string
	for attempt := range 3 {
		var err error
		out, err = Runner{Timeout: 2 * time.Second}.RunRaw(context.Background(), "log")
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("attempt %d: error = %v, want ErrTimeout", attempt+1, err)
		}
		if strings.Contains(out, "partial-line") {
			return
		}
	}
	t.Fatalf("output missing partial line after 3 attempts: %q", out)
}

// TestExtraConfigKeepsSafeDirectory is the regression test for the
// GIT_CONFIG_* collision: extra config values must extend the numbered
// block rather than shadow it, so safe.directory=* survives.
func TestExtraConfigKeepsSafeDirectory(t *testing.T) {
	repo := scratchRepo(t)
	r := Runner{ExtraConfig: []string{"muninn.probe=hello=world"}}

	out, err := r.Run(context.Background(), "-C", repo, "config", "--get", "safe.directory")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "*" {
		t.Fatalf("safe.directory = %q, want %q (must survive ExtraConfig)", out, "*")
	}

	out, err = r.Run(context.Background(), "-C", repo, "config", "--get", "muninn.probe")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "hello=world" {
		t.Fatalf("muninn.probe = %q, want %q (extra config value visible, '=' in value preserved)", out, "hello=world")
	}
}

// TestExtraEnvCannotClobberConfigBlock asserts that ExtraEnv entries naming
// the numbered GIT_CONFIG_* block or the hermetic keys are dropped.
func TestExtraEnvCannotClobberConfigBlock(t *testing.T) {
	repo := scratchRepo(t)
	r := Runner{ExtraEnv: []string{
		"GIT_CONFIG_COUNT=0",
		"GIT_CONFIG_KEY_0=evil.key",
		"GIT_CONFIG_VALUE_0=evil",
		"GIT_CONFIG_GLOBAL=/nonexistent",
		"GIT_CONFIG_NOSYSTEM=0",
		"GIT_TERMINAL_PROMPT=1",
	}}

	out, err := r.Run(context.Background(), "-C", repo, "config", "--get", "safe.directory")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "*" {
		t.Fatalf("safe.directory = %q, want %q (reserved ExtraEnv entries must be filtered)", out, "*")
	}

	if _, err := r.Run(context.Background(), "-C", repo, "config", "--get", "evil.key"); err == nil {
		t.Fatal("injected config key must not be visible")
	}
}

func TestExitCodeAndStderr(t *testing.T) {
	repo := scratchRepo(t)

	_, err := Runner{}.Run(context.Background(), "-C", repo, "cat-file", "-e", "doesnotexist")
	if err == nil {
		t.Fatal("expected error")
	}

	var gitErr *Error
	if !errors.As(err, &gitErr) {
		t.Fatalf("error = %v, want *Error", err)
	}
	if gitErr.ExitCode != 128 {
		t.Fatalf("ExitCode = %d, want 128", gitErr.ExitCode)
	}
	if !strings.Contains(gitErr.Stderr, "Not a valid object name") {
		t.Fatalf("Stderr = %q, want to contain %q", gitErr.Stderr, "Not a valid object name")
	}
	if !strings.Contains(gitErr.Error(), "Not a valid object name") {
		t.Fatalf("Error() = %q, want to contain %q", gitErr.Error(), "Not a valid object name")
	}
}

func TestRunStdin(t *testing.T) {
	repo := scratchRepo(t)

	sha, err := Runner{}.RunStdin(context.Background(), "hello\n", "-C", repo, "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatalf("RunStdin: %v", err)
	}
	if len(sha) != 40 {
		t.Fatalf("sha length = %d, want 40: %q", len(sha), sha)
	}

	out, err := Runner{}.RunRaw(context.Background(), "-C", repo, "cat-file", "blob", sha)
	if err != nil {
		t.Fatalf("RunRaw: %v", err)
	}
	if out != "hello\n" {
		t.Fatalf("blob content = %q, want %q", out, "hello\n")
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejectsOldGit(t *testing.T) {
	fakeGit(t, "#!/bin/sh\necho 'git version 2.20.0'\n")

	err := Validate()
	if err == nil {
		t.Fatal("expected error for old git version")
	}
	if !strings.Contains(err.Error(), "2.20") {
		t.Fatalf("error = %q, want to contain %q", err, "2.20")
	}
	if !strings.Contains(err.Error(), "2.32") {
		t.Fatalf("error = %q, want to contain %q", err, "2.32")
	}
}
