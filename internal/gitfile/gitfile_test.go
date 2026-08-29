package gitfile

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

// writeFile writes a fixture file, creating parent directories.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating fixture directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture file %s: %v", name, err)
	}
}

const (
	fileV1 = "one\ntwo\nthree\nfour\nfive\n"
	fileV2 = "one\nCHANGED\nthree\n"
)

// fixtureMirror creates a bare mirror with two commits touching file.txt
// (v1 then v2) plus a small tree, returning the mirror dir and both commits.
func fixtureMirror(t *testing.T) (mirror, commit1, commit2 string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	git(t, "", "init", "-b", "main", src)

	writeFile(t, src, "file.txt", fileV1)
	writeFile(t, src, "top.txt", "top\n")
	writeFile(t, src, "a/d.txt", "d\n")
	writeFile(t, src, "a/b/c.txt", "c\n")
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "v1")
	commit1 = git(t, src, "rev-parse", "HEAD")

	writeFile(t, src, "file.txt", fileV2)
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "v2")
	commit2 = git(t, src, "rev-parse", "HEAD")

	mirror = filepath.Join(t.TempDir(), "repo.git")
	git(t, "", "clone", "--bare", src, mirror)
	return mirror, commit1, commit2
}

func TestReadFilePinnedCommit(t *testing.T) {
	mirror, commit1, commit2 := fixtureMirror(t)
	ctx := context.Background()

	content, total, err := ReadFile(ctx, mirror, commit1, "file.txt", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile at commit1: %v", err)
	}
	if content != fileV1 {
		t.Errorf("content at commit1 = %q, want %q", content, fileV1)
	}
	if total != 5 {
		t.Errorf("totalLines at commit1 = %d, want 5", total)
	}

	content, total, err = ReadFile(ctx, mirror, commit2, "file.txt", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile at commit2: %v", err)
	}
	if content != fileV2 {
		t.Errorf("content at commit2 = %q, want %q", content, fileV2)
	}
	if total != 3 {
		t.Errorf("totalLines at commit2 = %d, want 3", total)
	}
}

func TestReadFileWindow(t *testing.T) {
	mirror, commit1, _ := fixtureMirror(t)
	ctx := context.Background()

	tests := []struct {
		name          string
		offset, limit int
		want          string
	}{
		{"middle window", 2, 2, "two\nthree\n"},
		{"from offset to end", 4, 0, "four\nfive\n"},
		{"limit past end", 4, 10, "four\nfive\n"},
		{"offset past end", 6, 2, ""},
		{"offset zero is line one", 0, 1, "one\n"},
		{"whole file", 1, 0, fileV1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, total, err := ReadFile(ctx, mirror, commit1, "file.txt", tt.offset, tt.limit)
			if err != nil {
				t.Fatalf("ReadFile(offset=%d, limit=%d): %v", tt.offset, tt.limit, err)
			}
			if content != tt.want {
				t.Errorf("content = %q, want %q", content, tt.want)
			}
			if total != 5 {
				t.Errorf("totalLines = %d, want 5", total)
			}
		})
	}

	if _, _, err := ReadFile(ctx, mirror, commit1, "file.txt", -1, 0); err == nil {
		t.Error("negative offset: err = nil, want error")
	}
	if _, _, err := ReadFile(ctx, mirror, commit1, "file.txt", 0, -1); err == nil {
		t.Error("negative limit: err = nil, want error")
	}
}

func TestReadFileMissingPath(t *testing.T) {
	mirror, commit1, _ := fixtureMirror(t)
	_, _, err := ReadFile(context.Background(), mirror, commit1, "nope.txt", 0, 0)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
	if err == nil || !strings.Contains(err.Error(), "nope.txt") {
		t.Errorf("err = %v, want message mentioning the path", err)
	}
}

func TestReadFileDirectory(t *testing.T) {
	mirror, commit1, _ := fixtureMirror(t)
	_, _, err := ReadFile(context.Background(), mirror, commit1, "a", 0, 0)
	if err == nil || !strings.Contains(err.Error(), "not a file") {
		t.Fatalf("err = %v, want 'not a file' error", err)
	}
}

func TestReadFileIndexMismatch(t *testing.T) {
	mirror, _, _ := fixtureMirror(t)
	bogus := strings.Repeat("d", 40)
	_, _, err := ReadFile(context.Background(), mirror, bogus, "file.txt", 0, 0)
	if !errors.Is(err, ErrIndexMismatch) {
		t.Fatalf("err = %v, want ErrIndexMismatch", err)
	}
	if !strings.Contains(err.Error(), "muninn sync") {
		t.Errorf("err = %v, want message telling the user to run muninn sync", err)
	}
}

func TestReadFileTooLarge(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	git(t, "", "init", "-b", "main", src)
	big := strings.Repeat("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", (11<<20)/32) // ~11 MiB
	writeFile(t, src, "big.txt", big)
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "big")
	commit := git(t, src, "rev-parse", "HEAD")

	_, _, err := ReadFile(context.Background(), src, commit, "big.txt", 0, 0)
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge", err)
	}
}

// entriesByPath maps entry path → entry for assertions.
func entriesByPath(entries []TreeEntry) map[string]TreeEntry {
	m := make(map[string]TreeEntry, len(entries))
	for _, e := range entries {
		m[e.Path] = e
	}
	return m
}

func TestListTreeDepth(t *testing.T) {
	mirror, commit1, _ := fixtureMirror(t)
	ctx := context.Background()

	// Depth 1 at root: only top-level entries.
	entries, total, err := ListTree(ctx, mirror, commit1, "", 1, 0)
	if err != nil {
		t.Fatalf("ListTree depth 1: %v", err)
	}
	if total != len(entries) {
		t.Errorf("total = %d, want %d (no cap)", total, len(entries))
	}
	got := entriesByPath(entries)
	if len(got) != 3 {
		t.Fatalf("depth 1 entries = %+v, want a, file.txt, top.txt", entries)
	}
	if e := got["a"]; e.Type != "dir" {
		t.Errorf("a = %+v, want dir", e)
	}
	if e := got["file.txt"]; e.Type != "file" || e.Size != int64(len(fileV1)) {
		t.Errorf("file.txt = %+v, want file of %d bytes", e, len(fileV1))
	}

	// Depth 2 at root adds a's children but not a/b's.
	entries, _, err = ListTree(ctx, mirror, commit1, "", 2, 0)
	if err != nil {
		t.Fatalf("ListTree depth 2: %v", err)
	}
	got = entriesByPath(entries)
	if len(got) != 5 {
		t.Fatalf("depth 2 entries = %+v, want 5 entries", entries)
	}
	if e := got["a/b"]; e.Type != "dir" {
		t.Errorf("a/b = %+v, want dir", e)
	}
	if e := got["a/d.txt"]; e.Type != "file" {
		t.Errorf("a/d.txt = %+v, want file", e)
	}
	if _, ok := got["a/b/c.txt"]; ok {
		t.Error("a/b/c.txt listed at depth 2, want excluded")
	}

	// Depth 1 under a subdirectory.
	entries, _, err = ListTree(ctx, mirror, commit1, "a", 1, 0)
	if err != nil {
		t.Fatalf("ListTree of a: %v", err)
	}
	got = entriesByPath(entries)
	if len(got) != 3 {
		t.Fatalf("entries under a = %+v, want a, a/b, a/d.txt", entries)
	}
	if _, ok := got["a/b/c.txt"]; ok {
		t.Error("a/b/c.txt listed at depth 1 under a, want excluded")
	}
}

func TestListTreeMaxEntries(t *testing.T) {
	mirror, commit1, _ := fixtureMirror(t)

	// Depth 2 at root has 5 entries; a cap of 2 returns the first 2 but
	// still reports the full total.
	entries, total, err := ListTree(context.Background(), mirror, commit1, "", 2, 2)
	if err != nil {
		t.Fatalf("ListTree capped: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("entries = %+v, want 2 entries", entries)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
}

func TestListTreeMissingPath(t *testing.T) {
	mirror, commit1, _ := fixtureMirror(t)
	_, _, err := ListTree(context.Background(), mirror, commit1, "nope", 1, 0)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestListTreeIndexMismatch(t *testing.T) {
	mirror, _, _ := fixtureMirror(t)
	bogus := strings.Repeat("d", 40)
	_, _, err := ListTree(context.Background(), mirror, bogus, "", 1, 0)
	if !errors.Is(err, ErrIndexMismatch) {
		t.Fatalf("err = %v, want ErrIndexMismatch", err)
	}
}

func TestListDirRoot(t *testing.T) {
	mirror, commit1, _ := fixtureMirror(t)

	entries, total, err := ListDir(context.Background(), mirror, commit1, "", 0)
	if err != nil {
		t.Fatalf("ListDir of root: %v", err)
	}
	got := entriesByPath(entries)
	if len(got) != 3 || total != 3 {
		t.Fatalf("root entries = %+v (total %d), want a, file.txt, top.txt", entries, total)
	}
	if e := got["a"]; e.Type != "dir" {
		t.Errorf("a = %+v, want dir", e)
	}
	if e := got["file.txt"]; e.Type != "file" {
		t.Errorf("file.txt = %+v, want file", e)
	}
	// Non-recursive: nothing below the listed directory.
	if _, ok := got["a/d.txt"]; ok {
		t.Error("a/d.txt listed from root, want excluded")
	}
}

func TestListDirSubdir(t *testing.T) {
	mirror, commit1, _ := fixtureMirror(t)

	entries, _, err := ListDir(context.Background(), mirror, commit1, "a", 0)
	if err != nil {
		t.Fatalf("ListDir of a: %v", err)
	}
	got := entriesByPath(entries)
	// Contrast with ListTree, which includes the anchor: 2 entries, not 3.
	if len(got) != 2 {
		t.Fatalf("entries under a = %+v, want a/b and a/d.txt only", entries)
	}
	if _, ok := got["a"]; ok {
		t.Error("anchor entry a listed, want excluded")
	}
	if e := got["a/b"]; e.Type != "dir" {
		t.Errorf("a/b = %+v, want dir", e)
	}
	if _, ok := got["a/b/c.txt"]; ok {
		t.Error("a/b/c.txt listed, want excluded (non-recursive)")
	}
}

func TestListDirNotFound(t *testing.T) {
	mirror, commit1, _ := fixtureMirror(t)

	_, _, err := ListDir(context.Background(), mirror, commit1, "nope", 0)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ListDir of missing path: err = %v, want fs.ErrNotExist", err)
	}
}

func TestListDirNotDir(t *testing.T) {
	mirror, commit1, _ := fixtureMirror(t)

	_, _, err := ListDir(context.Background(), mirror, commit1, "file.txt", 0)
	if !errors.Is(err, ErrNotDir) {
		t.Errorf("ListDir of a file: err = %v, want ErrNotDir", err)
	}
}

func TestListDirMaxEntries(t *testing.T) {
	mirror, commit1, _ := fixtureMirror(t)

	entries, total, err := ListDir(context.Background(), mirror, commit1, "", 2)
	if err != nil {
		t.Fatalf("ListDir with cap: %v", err)
	}
	if len(entries) != 2 || total != 3 {
		t.Errorf("entries = %d, total = %d; want 2 and 3", len(entries), total)
	}
}

func TestListDirIndexMismatch(t *testing.T) {
	mirror, _, _ := fixtureMirror(t)

	_, _, err := ListDir(context.Background(), mirror, "0000000000000000000000000000000000000000", "", 0)
	if !errors.Is(err, ErrIndexMismatch) {
		t.Errorf("ListDir at unreachable commit: err = %v, want ErrIndexMismatch", err)
	}
}

func TestResolveRev(t *testing.T) {
	mirror, commit1, commit2 := fixtureMirror(t)
	ctx := context.Background()

	sha, err := ResolveRev(ctx, mirror, commit1)
	require.NoError(t, err)
	require.Equal(t, commit1, sha, "full SHA resolves to itself")

	sha, err = ResolveRev(ctx, mirror, commit1[:8])
	require.NoError(t, err)
	require.Equal(t, commit1, sha, "short SHA resolves to the full SHA")

	sha, err = ResolveRev(ctx, mirror, "main")
	require.NoError(t, err)
	require.Equal(t, commit2, sha, "branch name resolves to its tip")
}

func TestResolveRevUnknown(t *testing.T) {
	mirror, _, _ := fixtureMirror(t)

	_, err := ResolveRev(context.Background(), mirror, strings.Repeat("a", 40))
	require.ErrorIs(t, err, ErrUnknownRev)
	require.NotErrorIs(t, err, ErrIndexMismatch)
}

// TestResolveRevRejectsOptionLookalikes asserts dash-prefixed and empty
// revs are rejected before any git process is spawned: a fake git at the
// front of PATH writes a marker file if it is ever executed.
func TestResolveRevRejectsOptionLookalikes(t *testing.T) {
	// Uses t.Setenv, so no t.Parallel.
	fakeDir := t.TempDir()
	marker := filepath.Join(fakeDir, "executed")
	script := "#!/bin/sh\ntouch " + marker + "\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(fakeDir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, rev := range []string{"--upload-pack=/bin/true", "-x", ""} {
		_, err := ResolveRev(context.Background(), t.TempDir(), rev)
		require.ErrorIs(t, err, ErrUnknownRev, "rev %q", rev)
	}
	_, err := os.Stat(marker)
	require.ErrorIs(t, err, fs.ErrNotExist, "git was executed for a rejected rev")
}

func TestClassifyPathErr(t *testing.T) {
	tests := []struct {
		name    string
		stderr  string
		unknown bool
	}{
		{"log/blame missing path", "fatal: no such path 'nope.txt' in HEAD", true},
		{"cat-file missing path", "fatal: path 'nope.txt' does not exist in 'deadbeef'", true},
		{"invalid object name", "fatal: Not a valid object name deadbeef:nope.txt", true},
		{"untracked path hint", "fatal: path 'nope.txt' exists on disk, but not in 'deadbeef'", true},
		{"unrelated failure", "fatal: bad revision walk", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := errors.New(tt.stderr)
			out := ClassifyPathErr(in)
			if tt.unknown {
				require.ErrorIs(t, out, ErrUnknownPath)
				require.Contains(t, out.Error(), tt.stderr, "original diagnostics preserved")
			} else {
				require.Equal(t, in, out, "non-path errors pass through unchanged")
			}
		})
	}
	require.NoError(t, ClassifyPathErr(nil))
}
