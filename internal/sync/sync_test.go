package sync

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/broderick-westrope/muninn/internal/config"
	"github.com/broderick-westrope/muninn/internal/discover"
	"github.com/broderick-westrope/muninn/internal/index"
	"github.com/broderick-westrope/muninn/internal/lock"
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
func (e *env) lockPath() string   { return filepath.Join(e.base, "sync.lock") }

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
		LockPath:     e.lockPath(),
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
	if _, err := os.Stat(filepath.Join(e.mirrorsDir(), "acme", "b.git")); !errors.Is(err, fs.ErrNotExist) {
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

// failingRemoveMirror delegates to the embedded Mirror except Remove,
// which always fails.
type failingRemoveMirror struct {
	Mirror
}

func (failingRemoveMirror) Remove(string) error { return errors.New("remove boom") }

func TestRunGCFailureDoesNotAbortRun(t *testing.T) {
	srcA, commitA := fixtureRepo(t)
	srcB, _ := fixtureRepo(t)
	e := newEnv(t)

	repoA, repoB := fileRepo("acme/a", srcA), fileRepo("acme/b", srcB)
	if _, err := Run(context.Background(), &config.Config{}, e.deps(&stubDiscoverer{repos: []discover.Repo{repoA, repoB}}, nil)); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	deps := e.deps(&stubDiscoverer{repos: []discover.Repo{repoA}}, nil)
	deps.Mirror = failingRemoveMirror{deps.Mirror}
	st, err := Run(context.Background(), &config.Config{}, deps)
	if err != nil {
		t.Fatalf("second Run: %v (a GC failure must not abort the run)", err)
	}
	if st.Success {
		t.Error("Success = true, want false")
	}
	rs, ok := st.Repos["acme/b"]
	if !ok {
		t.Fatal("GC-failed acme/b missing from status")
	}
	if !strings.Contains(rs.Error, "remove boom") {
		t.Errorf("acme/b Error = %q, want the removal failure", rs.Error)
	}
	if a := st.Repos["acme/a"]; !a.Indexed || a.IndexedCommit != commitA {
		t.Errorf("acme/a = %+v, want indexed at %s after GC failure", a, commitA)
	}
}

func TestRunReconcileFailureWritesStatus(t *testing.T) {
	src, commit := fixtureRepo(t)
	e := newEnv(t)
	repo := fileRepo("acme/a", src)

	if _, err := Run(context.Background(), &config.Config{}, e.deps(&stubDiscoverer{repos: []discover.Repo{repo}}, nil)); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Break shard listing so reconcile aborts before GC.
	deps := e.deps(&stubDiscoverer{repos: []discover.Repo{repo}}, func(string) Indexer {
		return failingListIndexer{e.indexer()}
	})
	if _, err := Run(context.Background(), &config.Config{}, deps); err == nil {
		t.Fatal("Run with failing reconcile: want error, got nil")
	}

	onDisk, err := status.Read(e.statusPath())
	if err != nil {
		t.Fatalf("reading status file: %v", err)
	}
	if onDisk.Success {
		t.Error("Success = true, want false after reconcile failure")
	}
	if onDisk.FinishedAt.IsZero() {
		t.Error("FinishedAt is zero, want set on abort")
	}
	if rs := onDisk.Repos["acme/a"]; rs.IndexedCommit != commit {
		t.Errorf("IndexedCommit = %q, want %q carried forward", rs.IndexedCommit, commit)
	}
}

// failingListIndexer delegates to the embedded Indexer except ListIndexed,
// which always fails.
type failingListIndexer struct {
	Indexer
}

func (failingListIndexer) ListIndexed() (map[string]string, error) {
	return nil, errors.New("list boom")
}

// countingIndexer tracks the peak number of concurrent IndexRepo calls.
type countingIndexer struct {
	Indexer
	mu        stdsync.Mutex
	cur, peak int
}

func (c *countingIndexer) IndexRepo(ctx context.Context, dir, name, branch, commit string) error {
	c.mu.Lock()
	c.cur++
	if c.cur > c.peak {
		c.peak = c.cur
	}
	c.mu.Unlock()
	// Hold the slot long enough that overlapping repos must queue.
	time.Sleep(30 * time.Millisecond)
	c.mu.Lock()
	c.cur--
	c.mu.Unlock()
	return c.Indexer.IndexRepo(ctx, dir, name, branch, commit)
}

func TestRunIndexConcurrencyBounded(t *testing.T) {
	e := newEnv(t)
	var repos []discover.Repo
	for _, name := range []string{"acme/a", "acme/b", "acme/c", "acme/d", "acme/e", "acme/f"} {
		src, _ := fixtureRepo(t)
		repos = append(repos, fileRepo(name, src))
	}

	ci := &countingIndexer{Indexer: e.indexer()}
	st, err := Run(context.Background(), &config.Config{}, e.deps(&stubDiscoverer{repos: repos}, func(string) Indexer { return ci }))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !st.Success {
		t.Errorf("Success = false, want true: %+v", st.Repos)
	}
	if ci.peak > 2 {
		t.Errorf("peak index concurrency = %d, want at most 2", ci.peak)
	}
	if ci.peak < 2 {
		t.Errorf("peak index concurrency = %d, want 2 (test did not exercise concurrency)", ci.peak)
	}
}

// TestRunInvalidCtagsFailsRun covers the total-failure recording contract:
// a ctags path that no longer resolves (it is baked into the config at
// install time, so this breaks every scheduled run at once) must fail the
// run *and* record it. Returning without writing status would leave the
// last good run's timestamps in place, making a permanently broken sync
// indistinguishable from a healthy one.
func TestRunInvalidCtagsFailsRun(t *testing.T) {
	e := newEnv(t)
	deps := e.deps(&stubDiscoverer{}, nil)
	deps.ResolveCtags = func(string) (string, error) { return "", errors.New("no ctags") }

	if _, err := Run(context.Background(), &config.Config{}, deps); err == nil {
		t.Fatal("Run with invalid ctags: want error, got nil")
	}
	onDisk, err := status.Read(e.statusPath())
	if err != nil {
		t.Fatalf("ctags hard failure was not recorded: %v", err)
	}
	if onDisk.Success {
		t.Error("Success = true, want false")
	}
	if !onDisk.LastSuccessAt.IsZero() {
		t.Errorf("LastSuccessAt = %v, want zero: a failed run must not advance the success clock", onDisk.LastSuccessAt)
	}
	if !strings.Contains(onDisk.Error, "ctags") {
		t.Errorf("Error = %q, want the ctags cause", onDisk.Error)
	}
	if onDisk.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", onDisk.ConsecutiveFailures)
	}
}

// gcTrackingMirror delegates to the embedded Mirror while recording the
// ordering of MarkIndexed and MaybeGC calls, optionally failing MaybeGC.
type gcTrackingMirror struct {
	Mirror
	gcErr error

	mu            stdsync.Mutex
	marked        int
	gcCalls       int
	gcAfterMarked bool
}

func (g *gcTrackingMirror) MarkIndexed(ctx context.Context, dir, sha string) error {
	g.mu.Lock()
	g.marked++
	g.mu.Unlock()
	return g.Mirror.MarkIndexed(ctx, dir, sha)
}

func (g *gcTrackingMirror) MaybeGC(ctx context.Context, dir string) (bool, error) {
	g.mu.Lock()
	g.gcCalls++
	g.gcAfterMarked = g.marked >= g.gcCalls
	g.mu.Unlock()
	if g.gcErr != nil {
		return false, g.gcErr
	}
	return g.Mirror.MaybeGC(ctx, dir)
}

func TestRunMaybeGCAfterMarkIndexed(t *testing.T) {
	src, _ := fixtureRepo(t)
	e := newEnv(t)
	deps := e.deps(&stubDiscoverer{repos: []discover.Repo{fileRepo("acme/a", src)}}, nil)
	gm := &gcTrackingMirror{Mirror: deps.Mirror}
	deps.Mirror = gm

	st, err := Run(context.Background(), &config.Config{}, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !st.Success {
		t.Errorf("Success = false, want true: %+v", st.Repos)
	}
	if gm.gcCalls != 1 {
		t.Errorf("MaybeGC calls = %d, want 1", gm.gcCalls)
	}
	if !gm.gcAfterMarked {
		t.Error("MaybeGC ran before MarkIndexed")
	}
}

func TestRunMaybeGCSkippedOnIndexFailure(t *testing.T) {
	src, _ := fixtureRepo(t)
	e := newEnv(t)
	failIx := func(string) Indexer { return failingIndexer{e.indexer()} }
	deps := e.deps(&stubDiscoverer{repos: []discover.Repo{fileRepo("acme/a", src)}}, failIx)
	gm := &gcTrackingMirror{Mirror: deps.Mirror}
	deps.Mirror = gm

	if _, err := Run(context.Background(), &config.Config{}, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gm.gcCalls != 0 {
		t.Errorf("MaybeGC calls = %d, want 0 after an index failure", gm.gcCalls)
	}
}

func TestRunMaybeGCFailureKeepsRepoSynced(t *testing.T) {
	src, commit := fixtureRepo(t)
	e := newEnv(t)
	deps := e.deps(&stubDiscoverer{repos: []discover.Repo{fileRepo("acme/a", src)}}, nil)
	gm := &gcTrackingMirror{Mirror: deps.Mirror, gcErr: errors.New("gc boom")}
	deps.Mirror = gm

	st, err := Run(context.Background(), &config.Config{}, deps)
	if err != nil {
		t.Fatalf("Run: %v (a gc failure must not abort the run)", err)
	}
	if st.Success {
		t.Error("Success = true, want false with a recorded gc error")
	}
	rs := st.Repos["acme/a"]
	if !rs.Fetched || !rs.Indexed {
		t.Errorf("repo = %+v, want Fetched and Indexed despite gc failure", rs)
	}
	if rs.IndexedCommit != commit {
		t.Errorf("IndexedCommit = %q, want %q", rs.IndexedCommit, commit)
	}
	if !strings.Contains(rs.Error, "gc boom") {
		t.Errorf("Error = %q, want the gc failure recorded", rs.Error)
	}
}

func TestRunSuccessAdvancesSuccessClock(t *testing.T) {
	src, _ := fixtureRepo(t)
	e := newEnv(t)
	deps := e.deps(&stubDiscoverer{repos: []discover.Repo{fileRepo("acme/a", src)}}, nil)

	st, err := Run(context.Background(), &config.Config{}, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !st.LastSuccessAt.Equal(st.FinishedAt) {
		t.Errorf("LastSuccessAt = %v, want FinishedAt %v", st.LastSuccessAt, st.FinishedAt)
	}
	if st.ConsecutiveFailures != 0 || st.Error != "" {
		t.Errorf("failure state = %d/%q, want cleared on success", st.ConsecutiveFailures, st.Error)
	}
	if status.Degraded(st) {
		t.Error("Degraded = true after a fully successful run")
	}
}

// TestRunFailureDoesNotAdvanceSuccessClock is the regression test for the
// bug that made repeated failures invisible: every run set FinishedAt, and
// staleness was measured from it, so an index that had not updated in days
// still reported as minutes old.
func TestRunFailureDoesNotAdvanceSuccessClock(t *testing.T) {
	src, _ := fixtureRepo(t)
	e := newEnv(t)
	repo := fileRepo("acme/a", src)

	good, err := Run(context.Background(), &config.Config{}, e.deps(&stubDiscoverer{repos: []discover.Repo{repo}}, nil))
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	firstSuccess := good.LastSuccessAt

	// Two failing runs in a row: per-repo failure, then total failure.
	failIx := func(string) Indexer { return failingIndexer{e.indexer()} }
	bad, err := Run(context.Background(), &config.Config{}, e.deps(&stubDiscoverer{repos: []discover.Repo{repo}}, failIx))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	aborted, err := Run(context.Background(), &config.Config{}, e.deps(&stubDiscoverer{err: errors.New("discovery boom")}, nil))
	if err == nil {
		t.Fatal("third Run: want discovery error, got nil")
	}

	for name, st := range map[string]*status.SyncStatus{"per-repo failure": bad, "total failure": aborted} {
		if !st.LastSuccessAt.Equal(firstSuccess) {
			t.Errorf("%s: LastSuccessAt = %v, want the first success %v", name, st.LastSuccessAt, firstSuccess)
		}
		if !st.FinishedAt.After(st.LastSuccessAt) {
			t.Errorf("%s: FinishedAt %v not after LastSuccessAt %v", name, st.FinishedAt, st.LastSuccessAt)
		}
		if !status.Degraded(st) {
			t.Errorf("%s: Degraded = false, want true", name)
		}
		if st.Error == "" {
			t.Errorf("%s: Error is empty, want a recorded cause", name)
		}
	}
	if bad.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures after one failure = %d, want 1", bad.ConsecutiveFailures)
	}
	if aborted.ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures after two failures = %d, want 2", aborted.ConsecutiveFailures)
	}

	// Recovery clears the counter and moves the clock forward.
	recovered, err := Run(context.Background(), &config.Config{}, e.deps(&stubDiscoverer{repos: []discover.Repo{repo}}, nil))
	if err != nil {
		t.Fatalf("recovery Run: %v", err)
	}
	if !recovered.LastSuccessAt.After(firstSuccess) {
		t.Errorf("LastSuccessAt = %v, want later than %v after recovery", recovered.LastSuccessAt, firstSuccess)
	}
	if recovered.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0 after recovery", recovered.ConsecutiveFailures)
	}
}

func TestRunSkipsWhenLockHeld(t *testing.T) {
	src, _ := fixtureRepo(t)
	e := newEnv(t)
	deps := e.deps(&stubDiscoverer{repos: []discover.Repo{fileRepo("acme/a", src)}}, nil)

	held, err := lock.TryAcquire(e.lockPath())
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	defer held.Release()

	if _, err := Run(context.Background(), &config.Config{}, deps); !errors.Is(err, ErrLocked) {
		t.Errorf("Run with the lock held: err = %v, want ErrLocked", err)
	}
	// A skipped run must not touch anything, including the status file.
	if _, err := os.Stat(e.statusPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("skipped run wrote status (err = %v)", err)
	}

	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := Run(context.Background(), &config.Config{}, deps); err != nil {
		t.Errorf("Run after the lock was released: %v", err)
	}
}

func TestRunRequiresLockPath(t *testing.T) {
	e := newEnv(t)
	deps := e.deps(&stubDiscoverer{}, nil)
	deps.LockPath = ""

	_, err := Run(context.Background(), &config.Config{}, deps)
	if err == nil || !strings.Contains(err.Error(), "LockPath") {
		t.Errorf("err = %v, want a Deps validation error naming LockPath", err)
	}
}

func TestRunOnlyIfDue(t *testing.T) {
	src, _ := fixtureRepo(t)
	cfg := &config.Config{Sync: config.SyncConfig{IntervalMinutes: 60, RetryIntervalMinutes: 10}}

	// A fresh success: the scheduled tick must do nothing.
	t.Run("fresh", func(t *testing.T) {
		e := newEnv(t)
		deps := e.deps(&stubDiscoverer{repos: []discover.Repo{fileRepo("acme/a", src)}}, nil)
		if _, err := Run(context.Background(), cfg, deps); err != nil {
			t.Fatalf("seed Run: %v", err)
		}
		before, err := status.Read(e.statusPath())
		if err != nil {
			t.Fatal(err)
		}

		deps.OnlyIfDue = true
		if _, err := Run(context.Background(), cfg, deps); !errors.Is(err, ErrNotDue) {
			t.Errorf("err = %v, want ErrNotDue", err)
		}
		after, err := status.Read(e.statusPath())
		if err != nil {
			t.Fatal(err)
		}
		if !after.FinishedAt.Equal(before.FinishedAt) {
			t.Error("a not-due run rewrote the status file")
		}
	})

	// Stale beyond the interval: the tick syncs.
	t.Run("stale", func(t *testing.T) {
		e := newEnv(t)
		deps := e.deps(&stubDiscoverer{repos: []discover.Repo{fileRepo("acme/a", src)}}, nil)
		if _, err := Run(context.Background(), cfg, deps); err != nil {
			t.Fatalf("seed Run: %v", err)
		}

		// Two hours after the recorded success, the index is past the
		// hourly interval and the tick must run.
		deps.OnlyIfDue = true
		deps.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
		st, err := Run(context.Background(), cfg, deps)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !st.Success {
			t.Errorf("Success = false, want true: %+v", st.Repos)
		}
	})

	// Degraded and stale: reattempts follow the retry cadence, which is
	// why a tick minutes after a failure runs instead of waiting out the
	// full interval.
	t.Run("degraded", func(t *testing.T) {
		e := newEnv(t)
		repo := fileRepo("acme/a", src)
		failIx := func(string) Indexer { return failingIndexer{e.indexer()} }
		failed := e.deps(&stubDiscoverer{repos: []discover.Repo{repo}}, failIx)
		if _, err := Run(context.Background(), cfg, failed); err != nil {
			t.Fatalf("seed Run: %v", err)
		}
		prev, err := status.Read(e.statusPath())
		if err != nil {
			t.Fatal(err)
		}
		if !status.Degraded(prev) {
			t.Fatal("seed run is not degraded")
		}

		deps := e.deps(&stubDiscoverer{repos: []discover.Repo{repo}}, nil)
		deps.OnlyIfDue = true

		// Five minutes on: stale but inside the retry cooldown.
		deps.Now = func() time.Time { return prev.FinishedAt.Add(5 * time.Minute) }
		if _, err := Run(context.Background(), cfg, deps); !errors.Is(err, ErrNotDue) {
			t.Errorf("5m after a failure: err = %v, want ErrNotDue", err)
		}

		// Eleven minutes on: the retry interval has elapsed, so the
		// reattempt happens well before the hourly interval would.
		deps.Now = func() time.Time { return prev.FinishedAt.Add(11 * time.Minute) }
		st, err := Run(context.Background(), cfg, deps)
		if err != nil {
			t.Fatalf("11m after a failure: %v", err)
		}
		if !st.Success {
			t.Errorf("Success = false, want true: %+v", st.Repos)
		}
	})
}
