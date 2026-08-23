package sync

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/broderick-westrope/muninn/internal/config"
	"github.com/broderick-westrope/muninn/internal/discover"
	"github.com/broderick-westrope/muninn/internal/index"
	"github.com/broderick-westrope/muninn/internal/mirror"
	"github.com/broderick-westrope/muninn/internal/status"
)

// git runs a git command against dir (or without -C when dir is empty),
// failing the test on error.
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// fixtureRepo creates an upstream repo with one commit on main containing a
// Go file, returning the repo dir and its head commit.
func fixtureRepo(t *testing.T) (dir, commit string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "src")
	git(t, "", "init", "-b", "main", dir)
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "initial")
	return dir, git(t, dir, "rev-parse", "HEAD")
}

// addCommit adds a commit to an upstream repo and returns the new head.
func addCommit(t *testing.T, dir string) string {
	t.Helper()
	writeFile(t, filepath.Join(dir, "extra.go"), "package main\n\nfunc extra() {}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "extra")
	return git(t, dir, "rev-parse", "HEAD")
}

func fileRepo(fullName, dir string) discover.Repo {
	return discover.Repo{FullName: fullName, CloneURL: "file://" + dir, DefaultBranch: "main"}
}

// env holds the on-disk layout shared across multiple Run invocations
// within a test.
type env struct {
	base string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{base: t.TempDir()}
	if err := os.MkdirAll(e.indexDir(), 0o700); err != nil {
		t.Fatalf("creating index dir: %v", err)
	}
	return e
}

func (e *env) indexDir() string   { return filepath.Join(e.base, "index") }
func (e *env) mirrorsDir() string { return filepath.Join(e.base, "mirrors") }
func (e *env) statusPath() string { return filepath.Join(e.base, "status.json") }

func (e *env) indexer() *index.Indexer {
	return &index.Indexer{IndexDir: e.indexDir()}
}

// deps builds Deps with real mirror and index implementations. The ctags
// resolver is stubbed to return an empty path, which disables symbol
// extraction so tests do not require universal-ctags (shards are still
// built; see internal/index tests).
func (e *env) deps(d Discoverer, newIx func(string) Indexer) Deps {
	if newIx == nil {
		newIx = func(string) Indexer { return e.indexer() }
	}
	return Deps{
		Discoverer:   d,
		Mirror:       &mirror.Manager{BaseDir: e.mirrorsDir()},
		NewIndexer:   newIx,
		ResolveCtags: func(string) (string, error) { return "", nil },
		StatusPath:   e.statusPath(),
	}
}

type stubDiscoverer struct {
	repos []discover.Repo
	err   error
}

func (s *stubDiscoverer) Discover(context.Context, *config.Config, string) ([]discover.Repo, error) {
	return s.repos, s.err
}

// failingIndexer delegates everything to the embedded Indexer except
// IndexRepo, which always fails.
type failingIndexer struct {
	Indexer
}

func (failingIndexer) IndexRepo(context.Context, string, string, string, string) error {
	return errors.New("index boom")
}

func TestRunHappyPath(t *testing.T) {
	srcA, commitA := fixtureRepo(t)
	srcB, commitB := fixtureRepo(t)
	e := newEnv(t)
	deps := e.deps(&stubDiscoverer{repos: []discover.Repo{
		fileRepo("acme/a", srcA),
		fileRepo("acme/b", srcB),
	}}, nil)

	st, err := Run(context.Background(), &config.Config{}, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !st.Success {
		t.Errorf("Success = false, want true: %+v", st.Repos)
	}
	for name, want := range map[string]string{"acme/a": commitA, "acme/b": commitB} {
		rs := st.Repos[name]
		if !rs.Fetched || !rs.Indexed || rs.Error != "" {
			t.Errorf("%s = %+v, want fetched+indexed without error", name, rs)
		}
		if rs.IndexedCommit != want {
			t.Errorf("%s IndexedCommit = %q, want %q", name, rs.IndexedCommit, want)
		}
	}

	onDisk, err := status.Read(e.statusPath())
	if err != nil {
		t.Fatalf("reading status file: %v", err)
	}
	if !onDisk.Success || len(onDisk.Repos) != 2 {
		t.Errorf("on-disk status = %+v, want success with 2 repos", onDisk)
	}
	if _, err := os.Stat(filepath.Join(e.mirrorsDir(), "acme", "a.git")); err != nil {
		t.Errorf("mirror for acme/a missing: %v", err)
	}
	indexed, err := e.indexer().ListIndexed()
	if err != nil {
		t.Fatalf("ListIndexed: %v", err)
	}
	if indexed["acme/a"] != commitA || indexed["acme/b"] != commitB {
		t.Errorf("shard metadata = %v, want both repos at their heads", indexed)
	}
}

func TestRunPerRepoFailureIsolation(t *testing.T) {
	srcA, commitA := fixtureRepo(t)
	e := newEnv(t)
	deps := e.deps(&stubDiscoverer{repos: []discover.Repo{
		fileRepo("acme/a", srcA),
		fileRepo("acme/broken", filepath.Join(e.base, "nonexistent.git")),
	}}, nil)

	st, err := Run(context.Background(), &config.Config{}, deps)
	if err != nil {
		t.Fatalf("Run: %v (per-repo failures must not fail the run)", err)
	}
	if st.Success {
		t.Error("Success = true, want false")
	}
	if rs := st.Repos["acme/a"]; !rs.Indexed || rs.IndexedCommit != commitA {
		t.Errorf("acme/a = %+v, want indexed at %s", rs, commitA)
	}
	rs := st.Repos["acme/broken"]
	if rs.Error == "" {
		t.Error("acme/broken has no recorded error")
	}
	if rs.Fetched || rs.Indexed || rs.IndexedCommit != "" {
		t.Errorf("acme/broken = %+v, want nothing recorded beyond the error", rs)
	}
}

func TestRunRemovedRepoGC(t *testing.T) {
	srcA, _ := fixtureRepo(t)
	srcB, _ := fixtureRepo(t)
	e := newEnv(t)

	repoA, repoB := fileRepo("acme/a", srcA), fileRepo("acme/b", srcB)
	if _, err := Run(context.Background(), &config.Config{}, e.deps(&stubDiscoverer{repos: []discover.Repo{repoA, repoB}}, nil)); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	st, err := Run(context.Background(), &config.Config{}, e.deps(&stubDiscoverer{repos: []discover.Repo{repoA}}, nil))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.mirrorsDir(), "acme", "b.git")); !os.IsNotExist(err) {
		t.Errorf("mirror for removed acme/b still exists (err = %v)", err)
	}
	indexed, err := e.indexer().ListIndexed()
	if err != nil {
		t.Fatalf("ListIndexed: %v", err)
	}
	if _, ok := indexed["acme/b"]; ok {
		t.Error("shards for removed acme/b still exist")
	}
	if _, ok := st.Repos["acme/b"]; ok {
		t.Error("removed acme/b still present in status")
	}
	if _, ok := st.Repos["acme/a"]; !ok {
		t.Error("acme/a missing from status")
	}
}

func TestRunIndexFailureCarriesForwardIndexedCommit(t *testing.T) {
	src, commit1 := fixtureRepo(t)
	e := newEnv(t)
	repo := fileRepo("acme/a", src)

	if _, err := Run(context.Background(), &config.Config{}, e.deps(&stubDiscoverer{repos: []discover.Repo{repo}}, nil)); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	commit2 := addCommit(t, src)

	failIx := func(string) Indexer { return failingIndexer{e.indexer()} }
	st, err := Run(context.Background(), &config.Config{}, e.deps(&stubDiscoverer{repos: []discover.Repo{repo}}, failIx))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if st.Success {
		t.Error("Success = true, want false")
	}
	rs := st.Repos["acme/a"]
	if !rs.Fetched {
		t.Error("Fetched = false, want true (fetch succeeded)")
	}
	if rs.Indexed {
		t.Error("Indexed = true, want false")
	}
	if !strings.Contains(rs.Error, "index boom") {
		t.Errorf("Error = %q, want the indexer failure", rs.Error)
	}
	if rs.IndexedCommit != commit1 {
		t.Errorf("IndexedCommit = %q, want previous commit %q (not new head %q)", rs.IndexedCommit, commit1, commit2)
	}
}

func TestRunDiscoveryFailureRetainsPreviousEntries(t *testing.T) {
	src, commit := fixtureRepo(t)
	e := newEnv(t)
	repo := fileRepo("acme/a", src)

	if _, err := Run(context.Background(), &config.Config{}, e.deps(&stubDiscoverer{repos: []discover.Repo{repo}}, nil)); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	_, err := Run(context.Background(), &config.Config{}, e.deps(&stubDiscoverer{err: errors.New("github down")}, nil))
	if err == nil {
		t.Fatal("Run with failing discovery: want error, got nil")
	}

	onDisk, err := status.Read(e.statusPath())
	if err != nil {
		t.Fatalf("reading status file: %v", err)
	}
	if onDisk.Success {
		t.Error("Success = true, want false after discovery failure")
	}
	rs, ok := onDisk.Repos["acme/a"]
	if !ok {
		t.Fatalf("previous entry for acme/a lost: %+v", onDisk.Repos)
	}
	if rs.IndexedCommit != commit {
		t.Errorf("IndexedCommit = %q, want %q carried forward", rs.IndexedCommit, commit)
	}
	if _, err := os.Stat(filepath.Join(e.mirrorsDir(), "acme", "a.git")); err != nil {
		t.Errorf("mirror GC'd despite discovery failure: %v", err)
	}
}

func TestRunInvalidCtagsFailsRun(t *testing.T) {
	e := newEnv(t)
	deps := e.deps(&stubDiscoverer{}, nil)
	deps.ResolveCtags = func(string) (string, error) { return "", errors.New("no ctags") }

	if _, err := Run(context.Background(), &config.Config{}, deps); err == nil {
		t.Fatal("Run with invalid ctags: want error, got nil")
	}
	if _, err := os.Stat(e.statusPath()); !os.IsNotExist(err) {
		t.Errorf("status file written despite ctags hard failure (err = %v)", err)
	}
}
