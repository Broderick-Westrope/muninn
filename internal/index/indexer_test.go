package index

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/broderick-westrope/muninn/internal/ctags"
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

// fixtureMirror creates a bare mirror of a repo with one commit on main
// containing a Go file with a function (so ctags emits symbols). Returns the
// mirror dir and the commit SHA of main.
func fixtureMirror(t *testing.T) (dir, commit string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	git(t, "", "init", "-b", "main", src)
	goFile := "package widget\n\n// Frobnicate does the thing.\nfunc Frobnicate() int { return 42 }\n"
	if err := os.WriteFile(filepath.Join(src, "widget.go"), []byte(goFile), 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "initial")

	mirror := filepath.Join(t.TempDir(), "widget.git")
	git(t, "", "clone", "--bare", src, mirror)
	return mirror, git(t, mirror, "rev-parse", "refs/heads/main")
}

// ctagsPathOrSkip resolves a usable universal-ctags binary, skipping the test
// when none is available.
func ctagsPathOrSkip(t *testing.T) string {
	t.Helper()
	path, err := ctags.Resolve("")
	if err != nil {
		t.Skipf("universal-ctags unavailable, skipping symbol indexing test: %v", err)
	}
	return path
}

// shardFiles returns all *.zoekt files in dir.
func shardFiles(t *testing.T, dir string) []string {
	t.Helper()
	shards, err := filepath.Glob(filepath.Join(dir, "*.zoekt"))
	if err != nil {
		t.Fatalf("globbing shards: %v", err)
	}
	return shards
}

// TestIndexerLifecycle exercises index → list → remove without ctags
// (CtagsPath empty disables symbol extraction, so shards can still be built
// when universal-ctags is not installed).
func TestIndexerLifecycle(t *testing.T) {
	mirror, commit := fixtureMirror(t)
	ix := &Indexer{IndexDir: t.TempDir()}

	if err := ix.IndexRepo(context.Background(), mirror, "acme/widget", "main", commit); err != nil {
		t.Fatalf("IndexRepo: %v", err)
	}
	if shards := shardFiles(t, ix.IndexDir); len(shards) == 0 {
		t.Fatal("no shard files created")
	}

	indexed, err := ix.ListIndexed()
	if err != nil {
		t.Fatalf("ListIndexed: %v", err)
	}
	if got, ok := indexed["acme/widget"]; !ok {
		t.Fatalf("ListIndexed = %v, missing acme/widget", indexed)
	} else if got != commit {
		t.Errorf("indexed commit = %q, want %q", got, commit)
	}

	// Incremental: re-indexing the same tip must be a cheap no-op, which
	// leaves the live shard as the very same file.
	shardBefore, err := os.Stat(shardFiles(t, ix.IndexDir)[0])
	if err != nil {
		t.Fatalf("stat shard: %v", err)
	}
	if err := ix.IndexRepo(context.Background(), mirror, "acme/widget", "main", commit); err != nil {
		t.Fatalf("IndexRepo (incremental): %v", err)
	}
	shardAfter, err := os.Stat(shardFiles(t, ix.IndexDir)[0])
	if err != nil {
		t.Fatalf("stat shard after incremental: %v", err)
	}
	if !os.SameFile(shardBefore, shardAfter) {
		t.Error("incremental re-index of an unchanged tip rebuilt the shard")
	}

	if err := ix.RemoveShards("acme/widget"); err != nil {
		t.Fatalf("RemoveShards: %v", err)
	}
	if shards := shardFiles(t, ix.IndexDir); len(shards) != 0 {
		t.Errorf("shards remain after RemoveShards: %v", shards)
	}
	if metas, _ := filepath.Glob(filepath.Join(ix.IndexDir, "*.meta")); len(metas) != 0 {
		t.Errorf(".meta files remain after RemoveShards: %v", metas)
	}
}

// TestIndexRepoWithCtags verifies the ctags symbol-extraction path with a
// real universal-ctags binary; self-skips when unavailable.
func TestIndexRepoWithCtags(t *testing.T) {
	ctagsPath := ctagsPathOrSkip(t)
	mirror, commit := fixtureMirror(t)
	ix := &Indexer{IndexDir: t.TempDir(), CtagsPath: ctagsPath}

	if err := ix.IndexRepo(context.Background(), mirror, "acme/widget", "main", commit); err != nil {
		t.Fatalf("IndexRepo with ctags: %v", err)
	}
	if shards := shardFiles(t, ix.IndexDir); len(shards) == 0 {
		t.Fatal("no shard files created")
	}

	indexed, err := ix.ListIndexed()
	if err != nil {
		t.Fatalf("ListIndexed: %v", err)
	}
	if got := indexed["acme/widget"]; got != commit {
		t.Errorf("indexed commit = %q, want %q", got, commit)
	}
}

func TestIndexRepoCancelledContext(t *testing.T) {
	mirror, commit := fixtureMirror(t)
	ix := &Indexer{IndexDir: t.TempDir()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ix.IndexRepo(ctx, mirror, "acme/widget", "main", commit); err == nil {
		t.Fatal("IndexRepo with cancelled context: want error, got nil")
	}
	if shards := shardFiles(t, ix.IndexDir); len(shards) != 0 {
		t.Errorf("shards created despite cancelled context: %v", shards)
	}
}

// corruptBlob overwrites the loose object holding blob with bytes that are
// not zlib, so go-git fails to read it while git still lists it in the tree.
func corruptBlob(t *testing.T, mirror, blob string) {
	t.Helper()
	path := filepath.Join(mirror, "objects", blob[:2], blob[2:])
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("making %s writable: %v", path, err)
	}
	if err := os.WriteFile(path, []byte("not a zlib stream"), 0o644); err != nil {
		t.Fatalf("corrupting %s: %v", path, err)
	}
}

// readShardBytes returns the content of every shard file in dir, keyed by
// base name.
func readShardBytes(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, shard := range shardFiles(t, dir) {
		b, err := os.ReadFile(shard)
		if err != nil {
			t.Fatalf("reading %s: %v", shard, err)
		}
		out[filepath.Base(shard)] = b
	}
	return out
}

// TestIndexRepoFailedBuildKeepsLiveShard reproduces a build that fails
// partway through reading the repository. Zoekt's deferred Finish would
// publish the files read so far over the complete shard; the staged build
// must instead leave the live shard untouched, and must not record the new
// tip, so the next run rebuilds rather than skipping.
func TestIndexRepoFailedBuildKeepsLiveShard(t *testing.T) {
	mirror, first := fixtureMirror(t)
	ix := &Indexer{IndexDir: t.TempDir()}
	if err := ix.IndexRepo(context.Background(), mirror, "acme/widget", "main", first); err != nil {
		t.Fatalf("IndexRepo (first): %v", err)
	}
	before := readShardBytes(t, ix.IndexDir)

	work := filepath.Join(t.TempDir(), "work")
	git(t, "", "clone", mirror, work)
	for _, name := range []string{"a.go", "b.go", "z.go"} {
		if err := os.WriteFile(filepath.Join(work, name), []byte("package widget // "+name+"\n"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "-m", "second")
	git(t, work, "push", "origin", "main")
	second := git(t, mirror, "rev-parse", "refs/heads/main")
	corruptBlob(t, mirror, git(t, mirror, "rev-parse", second+":z.go"))

	if err := ix.IndexRepo(context.Background(), mirror, "acme/widget", "main", second); err == nil {
		t.Fatal("IndexRepo over an unreadable blob: want error, got nil")
	}

	after := readShardBytes(t, ix.IndexDir)
	if len(after) != len(before) {
		t.Fatalf("shards after failed build = %d, want %d", len(after), len(before))
	}
	for name, b := range before {
		if string(after[name]) != string(b) {
			t.Errorf("shard %s changed after a failed build", name)
		}
	}
	indexed, err := ix.ListIndexed()
	if err != nil {
		t.Fatalf("ListIndexed: %v", err)
	}
	if got := indexed["acme/widget"]; got != first {
		t.Errorf("indexed commit after failed build = %q, want the previous %q", got, first)
	}
	if stagings, _ := filepath.Glob(filepath.Join(ix.IndexDir, stagingPrefix+"*")); len(stagings) != 0 {
		t.Errorf("staging dirs left behind: %v", stagings)
	}
}

// TestIndexRepoPublishesNewTip checks a successful rebuild replaces the live
// shard in place and leaves no staging directory.
func TestIndexRepoPublishesNewTip(t *testing.T) {
	mirror, first := fixtureMirror(t)
	ix := &Indexer{IndexDir: t.TempDir()}
	if err := ix.IndexRepo(context.Background(), mirror, "acme/widget", "main", first); err != nil {
		t.Fatalf("IndexRepo (first): %v", err)
	}

	work := filepath.Join(t.TempDir(), "work")
	git(t, "", "clone", mirror, work)
	if err := os.WriteFile(filepath.Join(work, "more.go"), []byte("package widget\n"), 0o644); err != nil {
		t.Fatalf("writing more.go: %v", err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "-m", "second")
	git(t, work, "push", "origin", "main")
	second := git(t, mirror, "rev-parse", "refs/heads/main")

	if err := ix.IndexRepo(context.Background(), mirror, "acme/widget", "main", second); err != nil {
		t.Fatalf("IndexRepo (second): %v", err)
	}
	indexed, err := ix.ListIndexed()
	if err != nil {
		t.Fatalf("ListIndexed: %v", err)
	}
	if got := indexed["acme/widget"]; got != second {
		t.Errorf("indexed commit = %q, want %q", got, second)
	}
	if shards := shardFiles(t, ix.IndexDir); len(shards) != 1 {
		t.Errorf("shards = %v, want exactly one", shards)
	}
	if stagings, _ := filepath.Glob(filepath.Join(ix.IndexDir, stagingPrefix+"*")); len(stagings) != 0 {
		t.Errorf("staging dirs left behind: %v", stagings)
	}
}

func TestCleanTmpRemovesStagingDirs(t *testing.T) {
	ix := &Indexer{IndexDir: t.TempDir()}
	staging := filepath.Join(ix.IndexDir, stagingPrefix+"123")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatalf("creating staging dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "x.zoekt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("writing staged shard: %v", err)
	}
	if err := ix.CleanTmp(); err != nil {
		t.Fatalf("CleanTmp: %v", err)
	}
	if _, err := os.Stat(staging); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("staging dir %s still exists after CleanTmp", staging)
	}
}

func TestRemoveShardsMissingRepo(t *testing.T) {
	ix := &Indexer{IndexDir: t.TempDir()}
	if err := ix.RemoveShards("acme/nonexistent"); err != nil {
		t.Fatalf("RemoveShards on missing repo: %v", err)
	}
}

func TestCleanTmp(t *testing.T) {
	ix := &Indexer{IndexDir: t.TempDir()}
	tmp := filepath.Join(ix.IndexDir, "foo.tmp")
	keep := filepath.Join(ix.IndexDir, "keep.zoekt")
	for _, f := range []string{tmp, keep} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", f, err)
		}
	}

	if err := ix.CleanTmp(); err != nil {
		t.Fatalf("CleanTmp: %v", err)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s still exists after CleanTmp", tmp)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("non-tmp file removed by CleanTmp: %v", err)
	}
}
