package index

import (
	"context"
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

	// Incremental: re-indexing the same tip must be a cheap no-op.
	if err := ix.IndexRepo(context.Background(), mirror, "acme/widget", "main", commit); err != nil {
		t.Fatalf("IndexRepo (incremental): %v", err)
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
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("%s still exists after CleanTmp", tmp)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("non-tmp file removed by CleanTmp: %v", err)
	}
}
