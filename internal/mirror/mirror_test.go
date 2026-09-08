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

	"github.com/broderick-westrope/muninn/internal/discover"
	"github.com/broderick-westrope/muninn/internal/gitcmd"
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

func TestAuthConfig(t *testing.T) {
	const token = "gho_secret123"
	lowSpeed := []string{
		"http.lowSpeedLimit=1000",
		"http.lowSpeedTime=60",
		"http.version=HTTP/1.1",
	}
	want := append(append([]string{}, lowSpeed...),
		"http.extraHeader=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)),
	)
	cfg := authConfig(token)
	if !equal(cfg, want) {
		t.Errorf("authConfig = %v, want %v", cfg, want)
	}
	for _, entry := range cfg {
		if strings.Contains(entry, token) {
			t.Errorf("raw token leaked into config entry %q", entry)
		}
	}
	if cfg := authConfig(""); !equal(cfg, lowSpeed) {
		t.Errorf("authConfig(\"\") = %v, want %v", cfg, lowSpeed)
	}
}

// TestAuthConfigKeepsSafeDirectory asserts the auth config extends
// gitcmd's numbered GIT_CONFIG_* block instead of shadowing it (the
// original collision dropped safe.directory=* on every authenticated
// fetch and clone).
func TestAuthConfigKeepsSafeDirectory(t *testing.T) {
	repo, _ := fixtureRepo(t, "acme/widget")
	upstream := strings.TrimPrefix(repo.CloneURL, "file://")
	r := gitcmd.Runner{ExtraConfig: authConfig("tok")}

	out, err := r.Run(context.Background(), "-C", upstream, "config", "--get", "safe.directory")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "*" {
		t.Fatalf("safe.directory = %q, want %q", out, "*")
	}

	out, err = r.Run(context.Background(), "-C", upstream, "config", "--get", "http.version")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "HTTP/1.1" {
		t.Fatalf("http.version = %q, want %q", out, "HTTP/1.1")
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
	if err := os.WriteFile(filepath.Join(upstream, "b.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}
	git(t, upstream, "add", ".")
	git(t, upstream, "commit", "-m", "second")
	return git(t, upstream, "rev-parse", "HEAD")
}

func TestMarkIndexedTwoGenerations(t *testing.T) {
	m := &Manager{BaseDir: t.TempDir()}
	repo, upstream := fixtureRepo(t, "acme/widget")
	ctx := context.Background()

	_, err := m.Ensure(ctx, repo, "")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	dir := m.Dir(repo.FullName)
	commitA, err := m.HeadCommit(ctx, dir, "main")
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}

	// First generation: indexed at A, no prev.
	if err := m.MarkIndexed(ctx, dir, commitA); err != nil {
		t.Fatalf("MarkIndexed: %v", err)
	}
	if got := refValue(t, dir, "refs/muninn/indexed"); got != commitA {
		t.Fatalf("indexed = %q, want %q", got, commitA)
	}
	if got := refValue(t, dir, "refs/muninn/indexed-prev"); got != "" {
		t.Fatalf("indexed-prev = %q, want empty", got)
	}

	// Second generation: indexed at B, prev rotated to A.
	commitB := addUpstreamCommit(t, upstream)
	if _, err = m.Ensure(ctx, repo, ""); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := m.MarkIndexed(ctx, dir, commitB); err != nil {
		t.Fatalf("MarkIndexed: %v", err)
	}
	if got := refValue(t, dir, "refs/muninn/indexed"); got != commitB {
		t.Fatalf("indexed = %q, want %q", got, commitB)
	}
	if got := refValue(t, dir, "refs/muninn/indexed-prev"); got != commitA {
		t.Fatalf("indexed-prev = %q, want %q", got, commitA)
	}

	// Same SHA again: a no-op that leaves both refs untouched.
	if err := m.MarkIndexed(ctx, dir, commitB); err != nil {
		t.Fatalf("MarkIndexed: %v", err)
	}
	if got := refValue(t, dir, "refs/muninn/indexed"); got != commitB {
		t.Fatalf("indexed = %q, want %q", got, commitB)
	}
	if got := refValue(t, dir, "refs/muninn/indexed-prev"); got != commitA {
		t.Fatalf("indexed-prev = %q, want %q", got, commitA)
	}
}

// TestMarkIndexedCreateRace exercises the create batch's must-not-exist
// guard: if a concurrent first sync pins the ref between MarkIndexed's
// read and its write, the create must fail loudly and leave the other
// pin untouched. (The interleaving is not orchestrable from a test, so
// the ref is pre-created behind the batch's back and the batch issued
// raw.)
func TestMarkIndexedCreateRace(t *testing.T) {
	m := &Manager{BaseDir: t.TempDir()}
	repo, upstream := fixtureRepo(t, "acme/widget")
	ctx := context.Background()

	_, err := m.Ensure(ctx, repo, "")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	dir := m.Dir(repo.FullName)
	commitA, err := m.HeadCommit(ctx, dir, "main")
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	commitB := addUpstreamCommit(t, upstream)
	if _, err = m.Ensure(ctx, repo, ""); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// A concurrent first sync already pinned the ref at A; the create
	// batch for B must be rejected rather than clobber it.
	git(t, dir, "update-ref", "refs/muninn/indexed", commitA)
	batch := "create refs/muninn/indexed " + commitB + "\n"
	cmd := exec.Command("git", "-C", dir, "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader(batch)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("create of an existing ref must fail: %s", out)
	}
	if got := refValue(t, dir, "refs/muninn/indexed"); got != commitA {
		t.Fatalf("indexed = %q, want %q", got, commitA)
	}
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
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	dir := m.Dir(repo.FullName)
	commitA, err := m.HeadCommit(ctx, dir, "main")
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	if err := m.MarkIndexed(ctx, dir, commitA); err != nil {
		t.Fatalf("MarkIndexed: %v", err)
	}

	commitB := addUpstreamCommit(t, upstream)
	if _, err = m.Ensure(ctx, repo, ""); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// The ref is at A, but the batch claims it is at B: git must reject it.
	batch := "update refs/muninn/indexed " + commitB + " " + commitB + "\n"
	cmd := exec.Command("git", "-C", dir, "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader(batch)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("stale old-value batch must fail: %s", out)
	}
	if got := refValue(t, dir, "refs/muninn/indexed"); got != commitA {
		t.Fatalf("indexed = %q, want %q", got, commitA)
	}
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
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	dir := m.Dir(repo.FullName)
	commitA, err := m.HeadCommit(ctx, dir, "main")
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	if err := m.MarkIndexed(ctx, dir, commitA); err != nil {
		t.Fatalf("MarkIndexed: %v", err)
	}

	// Force-rewrite upstream so A is unreachable from any branch.
	git(t, upstream, "commit", "--amend", "--allow-empty", "-m", "rewritten")
	commitB := git(t, upstream, "rev-parse", "HEAD")
	if commitA == commitB {
		t.Fatal("fixture amend did not change the commit SHA")
	}

	if _, err = m.Ensure(ctx, repo, ""); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := m.MarkIndexed(ctx, dir, commitB); err != nil {
		t.Fatalf("MarkIndexed: %v", err)
	}

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
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// looseCount returns the loose-object count of the repo at dir.
func looseCount(t *testing.T, dir string) int {
	t.Helper()
	count, err := parseLooseCount(git(t, dir, "count-objects", "-v"))
	if err != nil {
		t.Fatalf("parseLooseCount: %v", err)
	}
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
		if err := os.WriteFile(path, fmt.Appendf(nil, "content %d\n", i), 0o644); err != nil {
			t.Fatalf("writing blob file: %v", err)
		}
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
	if got := looseCount(t, dir); got != 10 {
		t.Fatalf("loose objects = %d, want 10", got)
	}

	m := &Manager{BaseDir: t.TempDir(), GCLooseObjectThreshold: 5}
	ran, err := m.MaybeGC(context.Background(), dir)
	if err != nil {
		t.Fatalf("MaybeGC: %v", err)
	}
	if !ran {
		t.Fatal("ran = false, want true")
	}
	if got := looseCount(t, dir); got >= 10 {
		t.Fatalf("loose objects = %d, want < 10 (gc should have packed the loose objects)", got)
	}
}

func TestMaybeGCUnderThreshold(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo.git")
	git(t, "", "init", "--bare", dir)
	seedLooseObjects(t, dir, 3)

	m := &Manager{BaseDir: t.TempDir(), GCLooseObjectThreshold: 5}
	ran, err := m.MaybeGC(context.Background(), dir)
	if err != nil {
		t.Fatalf("MaybeGC: %v", err)
	}
	if ran {
		t.Fatal("ran = true, want false")
	}
	if got := looseCount(t, dir); got != 3 {
		t.Fatalf("loose objects = %d, want 3 (gc must not have run)", got)
	}
}

// TestEnsureFetchErrorIsConciseAndUnwrappable pins both properties of the
// recorded fetch error. It used to be rendered as the repo name, the
// absolute mirror path, and the full argv followed by git's stderr, which
// exceeded the status file's per-error budget before the cause appeared —
// so every recorded fetch failure truncated away its own diagnosis.
func TestEnsureFetchErrorIsConciseAndUnwrappable(t *testing.T) {
	m := &Manager{BaseDir: t.TempDir()}
	repo, upstream := fixtureRepo(t, "acme/widget")
	if _, err := m.Ensure(context.Background(), repo, ""); err != nil {
		t.Fatalf("Ensure (clone): %v", err)
	}
	// Deleting the upstream makes the next fetch fail the way an
	// unreachable remote does.
	if err := os.RemoveAll(upstream); err != nil {
		t.Fatalf("removing upstream: %v", err)
	}

	_, err := m.Ensure(context.Background(), repo, "")
	if err == nil {
		t.Fatal("Ensure = nil error, want a fetch failure")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "fetch: ") {
		t.Errorf("message = %q, want it to lead with the operation", msg)
	}
	// The absolute mirror path is the bulk of the old prefix and is fully
	// derivable from the repo name the status file already keys by.
	if strings.Contains(msg, m.Dir(repo.FullName)) {
		t.Errorf("message = %q, want no absolute mirror path", msg)
	}
	if strings.Contains(msg, "--prune") {
		t.Errorf("message = %q, want no argv", msg)
	}
	// The budget that matters: status.TruncateError cuts at 200 runes, and
	// the cause has to survive it.
	if len([]rune(msg)) > 200 {
		t.Errorf("message is %d runes, want it within the 200-rune status budget: %q", len([]rune(msg)), msg)
	}
	// Retry classification in sync depends on errors.Is reaching through
	// this error to gitcmd's sentinel, so the chain must stay intact.
	var gitErr *gitcmd.Error
	if !errors.As(err, &gitErr) {
		t.Errorf("errors.As(*gitcmd.Error) = false, want the wrapped error reachable: %v", err)
	}
}

func TestDiagnosticLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"single", "fatal: boom", "fatal: boom"},
		{"prefers fatal over advisory tail", "fatal: boom\nhint: try harder\n", "fatal: boom"},
		// The regression this function exists for: git clone leads with an
		// unconditional banner naming the absolute destination, so taking
		// the first line both leaked the mirror path and dropped the cause.
		{
			"skips clone banner",
			"Cloning into bare repository '/abs/mirrors/acme/widget.git.tmp'...\nfatal: repository not found\n",
			"fatal: repository not found",
		},
		{
			"remote line counts as a diagnosis",
			"Cloning into bare repository '/abs/path.tmp'...\nremote: Repository not found.\nfatal: repository not found\n",
			"remote: Repository not found.",
		},
		{"error prefix", "Cloning into '/abs'...\nerror: bad thing\n", "error: bad thing"},
		// No recognised prefix: git narrates before it fails, so the tail
		// is a better guess than the banner.
		{"falls back to last line", "Cloning into '/abs'...\nsomething odd happened\n", "something odd happened"},
		{"leading blank", "\n\n  fatal: boom  \n", "fatal: boom"},
		{"empty", "", ""},
		{"blank only", "\n  \n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := diagnosticLine(tt.in); got != tt.want {
				t.Errorf("diagnosticLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestNetOpErrorfNonGitError covers the fallback path where the failure did
// not come from a git invocation at all.
func TestNetOpErrorfNonGitError(t *testing.T) {
	sentinel := errors.New("boom")
	err := netOpErrorf("fetch", sentinel)
	if got, want := err.Error(), "fetch: boom"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, sentinel) {
		t.Error("errors.Is(sentinel) = false, want the cause reachable")
	}
}

// TestNetOpErrorfEmptyStderr covers a killed git, which often writes
// nothing to stderr: recording just an exit code would lose the cause.
func TestNetOpErrorfEmptyStderr(t *testing.T) {
	gitErr := &gitcmd.Error{
		Args:     []string{"fetch", "--prune", "origin"},
		ExitCode: -1,
		Stderr:   "",
		Err:      fmt.Errorf("%w: signal: killed", gitcmd.ErrTimeout),
	}
	err := netOpErrorf("fetch", gitErr)
	if !strings.Contains(err.Error(), "signal: killed") {
		t.Errorf("Error() = %q, want the process-level cause", err.Error())
	}
	if !errors.Is(err, gitcmd.ErrTimeout) {
		t.Error("errors.Is(ErrTimeout) = false, want timeout classification to survive")
	}
}

// TestEnsureCloneErrorIsConcise covers the initial-clone path, which is
// where the recorded error was worst: clone announces "Cloning into bare
// repository '<abs tmp path>'..." before it fails, so an error built from
// the first stderr line leaked the mirror path and dropped git's cause
// entirely. This is the first-sync case for every new repo.
func TestEnsureCloneErrorIsConcise(t *testing.T) {
	m := &Manager{BaseDir: t.TempDir()}
	repo := discover.Repo{
		FullName:      "acme/missing",
		CloneURL:      "file://" + filepath.Join(t.TempDir(), "does-not-exist"),
		DefaultBranch: "main",
	}

	_, err := m.Ensure(context.Background(), repo, "")
	if err == nil {
		t.Fatal("Ensure = nil error, want a clone failure")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "clone: ") {
		t.Errorf("message = %q, want it to lead with the operation", msg)
	}
	if !strings.Contains(msg, "fatal:") {
		t.Errorf("message = %q, want git's own diagnosis", msg)
	}
	// The temp clone path is the absolute mirror path plus a suffix, and
	// was previously the entire recorded message.
	if strings.Contains(msg, m.Dir(repo.FullName)) {
		t.Errorf("message = %q, want no absolute mirror path", msg)
	}
	if strings.Contains(msg, "Cloning into") {
		t.Errorf("message = %q, want no clone banner", msg)
	}
	if len([]rune(msg)) > 200 {
		t.Errorf("message is %d runes, want it within the 200-rune status budget: %q", len([]rune(msg)), msg)
	}
}
