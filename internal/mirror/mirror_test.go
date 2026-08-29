package mirror

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/broderick-westrope/muninn/internal/discover"
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

// initUpstream creates a fixture repo with one commit on main.
func initUpstream(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "upstream")
	git(t, "", "init", "-b", "main", dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "initial")
	return dir
}

func fixtureRepo(t *testing.T, fullName string) (discover.Repo, string) {
	t.Helper()
	upstream := initUpstream(t)
	return discover.Repo{
		FullName:      fullName,
		CloneURL:      "file://" + upstream,
		DefaultBranch: "main",
	}, upstream
}

func TestEnsureClonesBareWithNarrowRefspec(t *testing.T) {
	m := &Manager{BaseDir: t.TempDir()}
	repo, upstream := fixtureRepo(t, "acme/widget")
	// A pull ref on the remote must NOT be fetched by the narrow refspec.
	git(t, upstream, "update-ref", "refs/pull/1/head", "HEAD")

	created, err := m.Ensure(context.Background(), repo, "")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !created {
		t.Error("created = false, want true")
	}

	dir := m.Dir(repo.FullName)
	if got := git(t, dir, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Errorf("is-bare-repository = %q, want true", got)
	}
	if got := git(t, dir, "config", "remote.origin.fetch"); got != "+refs/heads/*:refs/heads/*" {
		t.Errorf("remote.origin.fetch = %q, want +refs/heads/*:refs/heads/*", got)
	}
	if got := git(t, dir, "config", "gc.auto"); got != "0" {
		t.Errorf("gc.auto = %q, want 0", got)
	}

	head, err := m.HeadCommit(context.Background(), dir, "main")
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	if want := git(t, upstream, "rev-parse", "HEAD"); head != want {
		t.Errorf("HeadCommit = %q, want %q", head, want)
	}

	// Fetch to exercise the configured refspec, then confirm no pull refs.
	if _, err := m.Ensure(context.Background(), repo, ""); err != nil {
		t.Fatalf("Ensure (fetch): %v", err)
	}
	if refs := git(t, dir, "for-each-ref", "refs/pull"); refs != "" {
		t.Errorf("refs/pull fetched into mirror: %q", refs)
	}
}

func TestEnsureFetchesNewCommit(t *testing.T) {
	m := &Manager{BaseDir: t.TempDir()}
	repo, upstream := fixtureRepo(t, "acme/widget")

	if _, err := m.Ensure(context.Background(), repo, ""); err != nil {
		t.Fatalf("Ensure (clone): %v", err)
	}

	if err := os.WriteFile(filepath.Join(upstream, "b.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}
	git(t, upstream, "add", ".")
	git(t, upstream, "commit", "-m", "second")
	want := git(t, upstream, "rev-parse", "HEAD")

	created, err := m.Ensure(context.Background(), repo, "")
	if err != nil {
		t.Fatalf("Ensure (fetch): %v", err)
	}
	if created {
		t.Error("created = true, want false")
	}
	head, err := m.HeadCommit(context.Background(), m.Dir(repo.FullName), "main")
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	if head != want {
		t.Errorf("HeadCommit = %q, want %q", head, want)
	}
}

// TestEnsureSelfHealsConfig simulates a mirror left without the fetch
// refspec (as an older two-step clone could after a crash) and asserts the
// fetch path re-asserts both config keys.
func TestEnsureSelfHealsConfig(t *testing.T) {
	m := &Manager{BaseDir: t.TempDir()}
	repo, _ := fixtureRepo(t, "acme/widget")

	if _, err := m.Ensure(context.Background(), repo, ""); err != nil {
		t.Fatalf("Ensure (clone): %v", err)
	}
	dir := m.Dir(repo.FullName)
	git(t, dir, "config", "--unset-all", "remote.origin.fetch")
	git(t, dir, "config", "--unset-all", "gc.auto")

	if _, err := m.Ensure(context.Background(), repo, ""); err != nil {
		t.Fatalf("Ensure (fetch): %v", err)
	}
	if got := git(t, dir, "config", "remote.origin.fetch"); got != "+refs/heads/*:refs/heads/*" {
		t.Errorf("remote.origin.fetch = %q, want +refs/heads/*:refs/heads/*", got)
	}
	if got := git(t, dir, "config", "gc.auto"); got != "0" {
		t.Errorf("gc.auto = %q, want 0", got)
	}
}

// TestForcePushKeepsIndexedCommit is the regression test the narrow
// refspec exists for: with --mirror's +refs/*:refs/* refspec,
// fetch --prune would delete refs/muninn/indexed and gc would then
// discard the indexed commit after an upstream force-push.
func TestForcePushKeepsIndexedCommit(t *testing.T) {
	m := &Manager{BaseDir: t.TempDir()}
	repo, upstream := fixtureRepo(t, "acme/widget")
	dir := m.Dir(repo.FullName)

	if _, err := m.Ensure(context.Background(), repo, ""); err != nil {
		t.Fatalf("Ensure (clone): %v", err)
	}
	oldSHA, err := m.HeadCommit(context.Background(), dir, "main")
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	if err := m.MarkIndexed(context.Background(), dir, oldSHA); err != nil {
		t.Fatalf("MarkIndexed: %v", err)
	}

	// Force-push: rewrite upstream history so oldSHA is unreachable there.
	git(t, upstream, "commit", "--amend", "--allow-empty", "-m", "rewritten")
	newSHA := git(t, upstream, "rev-parse", "HEAD")
	if newSHA == oldSHA {
		t.Fatal("fixture amend did not change the commit SHA")
	}

	if _, err := m.Ensure(context.Background(), repo, ""); err != nil {
		t.Fatalf("Ensure (fetch --prune): %v", err)
	}

	head, err := m.HeadCommit(context.Background(), dir, "main")
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	if head != newSHA {
		t.Errorf("HeadCommit = %q, want force-pushed %q", head, newSHA)
	}
	if got := git(t, dir, "rev-parse", "--verify", "refs/muninn/indexed"); got != oldSHA {
		t.Errorf("refs/muninn/indexed = %q, want %q", got, oldSHA)
	}

	git(t, dir, "gc", "--prune=now")
	git(t, dir, "cat-file", "-e", oldSHA+"^{commit}")
}

func TestListRemove(t *testing.T) {
	m := &Manager{BaseDir: t.TempDir()}
	repoA, _ := fixtureRepo(t, "acme/widget")
	repoB, _ := fixtureRepo(t, "beta/gadget")
	for _, repo := range []discover.Repo{repoA, repoB} {
		if _, err := m.Ensure(context.Background(), repo, ""); err != nil {
			t.Fatalf("Ensure %s: %v", repo.FullName, err)
		}
	}

	list, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := []string{"acme/widget", "beta/gadget"}; !equal(list, want) {
		t.Errorf("List = %v, want %v", list, want)
	}

	if err := m.Remove("acme/widget"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	list, err = m.List()
	if err != nil {
		t.Fatalf("List after Remove: %v", err)
	}
	if want := []string{"beta/gadget"}; !equal(list, want) {
		t.Errorf("List after Remove = %v, want %v", list, want)
	}
	if _, err := os.Stat(filepath.Join(m.BaseDir, "acme")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("empty owner dir acme not pruned: stat err = %v", err)
	}
}

func TestAuthEnv(t *testing.T) {
	const token = "gho_secret123"
	lowSpeed := []string{
		"GIT_CONFIG_KEY_0=http.lowSpeedLimit",
		"GIT_CONFIG_VALUE_0=1000",
		"GIT_CONFIG_KEY_1=http.lowSpeedTime",
		"GIT_CONFIG_VALUE_1=60",
		"GIT_CONFIG_KEY_2=http.version",
		"GIT_CONFIG_VALUE_2=HTTP/1.1",
	}
	want := append(append([]string{}, lowSpeed...),
		"GIT_CONFIG_KEY_3=http.extraHeader",
		"GIT_CONFIG_VALUE_3=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)),
		"GIT_CONFIG_COUNT=4",
	)
	env := authEnv(token)
	if !equal(env, want) {
		t.Errorf("authEnv = %v, want %v", env, want)
	}
	for _, entry := range env {
		if strings.Contains(entry, token) {
			t.Errorf("raw token leaked into env entry %q", entry)
		}
	}
	wantEmpty := append(append([]string{}, lowSpeed...), "GIT_CONFIG_COUNT=3")
	if env := authEnv(""); !equal(env, wantEmpty) {
		t.Errorf("authEnv(\"\") = %v, want %v", env, wantEmpty)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCleanTmp(t *testing.T) {
	m := &Manager{BaseDir: t.TempDir()}
	orphan := filepath.Join(m.BaseDir, "acme", "widget.git.tmp")
	keep := filepath.Join(m.BaseDir, "acme", "widget.git")
	for _, dir := range []string{orphan, keep} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.CleanTmp(); err != nil {
		t.Fatalf("CleanTmp: %v", err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("orphaned temp clone not removed: stat err = %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("real mirror removed by CleanTmp: %v", err)
	}
}

// refValue returns the value of ref in dir, or "" if the ref does not
// exist.
func refValue(t *testing.T, dir, ref string) string {
	t.Helper()
	return git(t, dir, "for-each-ref", "--format=%(objectname)", ref)
}

// addUpstreamCommit adds a commit to an upstream repo and returns the new
// head SHA.
func addUpstreamCommit(t *testing.T, upstream string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(upstream, "b.txt"), []byte("two\n"), 0o644))
	git(t, upstream, "add", ".")
	git(t, upstream, "commit", "-m", "second")
	return git(t, upstream, "rev-parse", "HEAD")
}

func TestMarkIndexedTwoGenerations(t *testing.T) {
	m := &Manager{BaseDir: t.TempDir()}
	repo, upstream := fixtureRepo(t, "acme/widget")
	ctx := context.Background()

	_, err := m.Ensure(ctx, repo, "")
	require.NoError(t, err)
	dir := m.Dir(repo.FullName)
	commitA, err := m.HeadCommit(ctx, dir, "main")
	require.NoError(t, err)

	// First generation: indexed at A, no prev.
	require.NoError(t, m.MarkIndexed(ctx, dir, commitA))
	require.Equal(t, commitA, refValue(t, dir, "refs/muninn/indexed"))
	require.Empty(t, refValue(t, dir, "refs/muninn/indexed-prev"))

	// Second generation: indexed at B, prev rotated to A.
	commitB := addUpstreamCommit(t, upstream)
	_, err = m.Ensure(ctx, repo, "")
	require.NoError(t, err)
	require.NoError(t, m.MarkIndexed(ctx, dir, commitB))
	require.Equal(t, commitB, refValue(t, dir, "refs/muninn/indexed"))
	require.Equal(t, commitA, refValue(t, dir, "refs/muninn/indexed-prev"))

	// Same SHA again: a no-op that leaves both refs untouched.
	require.NoError(t, m.MarkIndexed(ctx, dir, commitB))
	require.Equal(t, commitB, refValue(t, dir, "refs/muninn/indexed"))
	require.Equal(t, commitA, refValue(t, dir, "refs/muninn/indexed-prev"))
}

// TestMarkIndexedStaleCAS exercises the CAS failure mode directly: a batch
// whose old-value guard no longer matches the ref must fail and leave the
// ref untouched. (Interleaving a concurrent MarkIndexed between its read
// and write is not orchestrable from a test, so the batch is issued raw.)
func TestMarkIndexedStaleCAS(t *testing.T) {
	m := &Manager{BaseDir: t.TempDir()}
	repo, upstream := fixtureRepo(t, "acme/widget")
	ctx := context.Background()

	_, err := m.Ensure(ctx, repo, "")
	require.NoError(t, err)
	dir := m.Dir(repo.FullName)
	commitA, err := m.HeadCommit(ctx, dir, "main")
	require.NoError(t, err)
	require.NoError(t, m.MarkIndexed(ctx, dir, commitA))

	commitB := addUpstreamCommit(t, upstream)
	_, err = m.Ensure(ctx, repo, "")
	require.NoError(t, err)

	// The ref is at A, but the batch claims it is at B: git must reject it.
	batch := "update refs/muninn/indexed " + commitB + " " + commitB + "\n"
	cmd := exec.Command("git", "-C", dir, "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader(batch)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "stale old-value batch must fail: %s", out)
	require.Equal(t, commitA, refValue(t, dir, "refs/muninn/indexed"))
}

// TestMarkIndexedSurvivesHostileMaintenance is the two-generation
// guarantee end to end: after an upstream force-push, a prune fetch, a new
// pin, and gc --prune=now, both the current and the previous indexed
// commits must still be readable (the previous is held by indexed-prev).
func TestMarkIndexedSurvivesHostileMaintenance(t *testing.T) {
	m := &Manager{BaseDir: t.TempDir()}
	repo, upstream := fixtureRepo(t, "acme/widget")
	ctx := context.Background()

	_, err := m.Ensure(ctx, repo, "")
	require.NoError(t, err)
	dir := m.Dir(repo.FullName)
	commitA, err := m.HeadCommit(ctx, dir, "main")
	require.NoError(t, err)
	require.NoError(t, m.MarkIndexed(ctx, dir, commitA))

	// Force-rewrite upstream so A is unreachable from any branch.
	git(t, upstream, "commit", "--amend", "--allow-empty", "-m", "rewritten")
	commitB := git(t, upstream, "rev-parse", "HEAD")
	require.NotEqual(t, commitA, commitB, "fixture amend did not change the commit SHA")

	_, err = m.Ensure(ctx, repo, "")
	require.NoError(t, err)
	require.NoError(t, m.MarkIndexed(ctx, dir, commitB))

	git(t, dir, "gc", "--prune=now")
	git(t, dir, "cat-file", "-e", commitA+"^{commit}")
	git(t, dir, "cat-file", "-e", commitB+"^{commit}")
}

// gitStdin runs a git command against dir with the given stdin, failing
// the test on error.
func gitStdin(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// looseCount returns the loose-object count of the repo at dir.
func looseCount(t *testing.T, dir string) int {
	t.Helper()
	count, err := parseLooseCount(git(t, dir, "count-objects", "-v"))
	require.NoError(t, err)
	return count
}

// seedLooseObjects writes n reachable loose blobs into the bare repo at
// dir using a single hash-object invocation (thousands of process spawns
// would be slow; fast-import is unusable because it writes packs, not
// loose objects). Each blob is pinned by a ref so gc packs it rather than
// leaving it loose as a fresh unreachable object.
func seedLooseObjects(t *testing.T, dir string, n int) {
	t.Helper()
	blobDir := t.TempDir()
	var paths strings.Builder
	for i := range n {
		path := filepath.Join(blobDir, fmt.Sprintf("blob-%d.txt", i))
		require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, "content %d\n", i), 0o644))
		paths.WriteString(path + "\n")
	}
	shas := gitStdin(t, dir, paths.String(), "hash-object", "-w", "--stdin-paths")
	var batch strings.Builder
	for i, sha := range strings.Fields(shas) {
		fmt.Fprintf(&batch, "create refs/muninn/test-keep-%d %s\n", i, sha)
	}
	gitStdin(t, dir, batch.String(), "update-ref", "--stdin")
}

func TestMaybeGCOverThreshold(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo.git")
	git(t, "", "init", "--bare", dir)
	seedLooseObjects(t, dir, 10)
	require.Equal(t, 10, looseCount(t, dir))

	m := &Manager{BaseDir: t.TempDir(), GCLooseObjectThreshold: 5}
	ran, err := m.MaybeGC(context.Background(), dir)
	require.NoError(t, err)
	require.True(t, ran)
	require.Less(t, looseCount(t, dir), 10, "gc should have packed the loose objects")
}

func TestMaybeGCUnderThreshold(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo.git")
	git(t, "", "init", "--bare", dir)
	seedLooseObjects(t, dir, 3)

	m := &Manager{BaseDir: t.TempDir(), GCLooseObjectThreshold: 5}
	ran, err := m.MaybeGC(context.Background(), dir)
	require.NoError(t, err)
	require.False(t, ran)
	require.Equal(t, 3, looseCount(t, dir), "gc must not have run")
}
