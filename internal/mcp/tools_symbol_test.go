package mcp

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/broderick-westrope/muninn/internal/ctags"
	"github.com/broderick-westrope/muninn/internal/gitfile"
)

// requireCtags resolves universal-ctags or skips the test: symbol queries
// only return results when the index carries ctags symbol data.
func requireCtags(t *testing.T) string {
	t.Helper()
	path, err := ctags.Resolve("")
	if err != nil {
		t.Skipf("universal-ctags unavailable, skipping symbol test: %v", err)
	}
	return path
}

func TestFindSymbolDefinitions(t *testing.T) {
	f := newFixture(t, requireCtags(t))
	out, err := f.srv.FindSymbolDefinitions(context.Background(), FindDefinitionsArgs{Symbol: "Frobnicate"})
	if err != nil {
		t.Fatalf("FindSymbolDefinitions: %v", err)
	}
	lines := matchLines(out)
	if len(lines) != 1 {
		t.Fatalf("definition lines = %v, want exactly one", lines)
	}
	if !strings.HasPrefix(lines[0], "acme/widget/widget.go:4: ") {
		t.Errorf("definition = %q, want widget.go:4", lines[0])
	}
}

func TestFindSymbolReferences(t *testing.T) {
	f := newFixture(t, requireCtags(t))
	out, err := f.srv.FindSymbolReferences(context.Background(), FindReferencesArgs{Symbol: "Frobnicate"})
	if err != nil {
		t.Fatalf("FindSymbolReferences: %v", err)
	}
	lines := matchLines(out)
	if len(lines) != 2 {
		t.Fatalf("reference lines = %v, want exactly the two call sites", lines)
	}
	for _, want := range []string{"widget.go:6:", "widget.go:8:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing call site %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "widget.go:4:") {
		t.Errorf("output includes the definition line:\n%s", out)
	}
	if !strings.Contains(out, "approximate") {
		t.Errorf("output missing approximate disclaimer:\n%s", out)
	}
}

func TestFindSymbolReferencesCap(t *testing.T) {
	f := newFixture(t, requireCtags(t))
	out, err := f.srv.FindSymbolReferences(context.Background(), FindReferencesArgs{Symbol: "Frobnicate", Limit: 1})
	if err != nil {
		t.Fatalf("FindSymbolReferences: %v", err)
	}
	if got := len(matchLines(out)); got != 1 {
		t.Errorf("reference lines = %d, want 1 (cap):\n%s", got, out)
	}
	if !strings.Contains(out, "[truncated") {
		t.Errorf("output missing truncation notice:\n%s", out)
	}
}

func TestReadFile(t *testing.T) {
	f := newFixture(t, "")
	out, err := f.srv.ReadFile(context.Background(), ReadFileArgs{Repo: "acme/widget", Path: "other.go"})
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(out, "(lines 1-3 of 3)") {
		t.Errorf("output missing line range header:\n%s", out)
	}
	if !strings.Contains(out, `const banana = "yellow"`) {
		t.Errorf("output missing content:\n%s", out)
	}

	out, err = f.srv.ReadFile(context.Background(), ReadFileArgs{Repo: "acme/widget", Path: "other.go", Offset: 3, Limit: 1})
	if err != nil {
		t.Fatalf("ReadFile window: %v", err)
	}
	if !strings.Contains(out, "(lines 3-3 of 3)") {
		t.Errorf("windowed output missing line range header:\n%s", out)
	}
	if strings.Contains(out, "package widget") {
		t.Errorf("windowed output includes lines outside the window:\n%s", out)
	}
}

// TestReadFileMatchesGrep asserts the spec criterion that read_file line
// numbers agree with grep output for the same content.
func TestReadFileMatchesGrep(t *testing.T) {
	f := newFixture(t, "")
	ctx := context.Background()
	grepOut, err := f.srv.Grep(ctx, GrepArgs{Pattern: "banana"})
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	lines := matchLines(grepOut)
	if len(lines) != 1 {
		t.Fatalf("grep lines = %v, want exactly one", lines)
	}
	// Parse "acme/widget/other.go:3: const banana = ..." into parts.
	loc, content, ok := strings.Cut(lines[0], ": ")
	if !ok {
		t.Fatalf("cannot split grep line %q", lines[0])
	}
	idx := strings.LastIndexByte(loc, ':')
	lineNum, err := strconv.Atoi(loc[idx+1:])
	if err != nil {
		t.Fatalf("parsing line number from %q: %v", loc, err)
	}
	path := strings.TrimPrefix(loc[:idx], "acme/widget/")

	readOut, err := f.srv.ReadFile(ctx, ReadFileArgs{Repo: "acme/widget", Path: path, Offset: lineNum, Limit: 1})
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	_, got, ok := strings.Cut(readOut, ")\n")
	if !ok {
		t.Fatalf("cannot split read_file output %q", readOut)
	}
	if strings.TrimRight(got, "\n") != content {
		t.Errorf("read_file line %d = %q, want grep content %q", lineNum, got, content)
	}
}

// TestReadFilePinned asserts read_file serves the indexed commit even after
// the mirror advances without a reindex.
func TestReadFilePinned(t *testing.T) {
	f := newFixture(t, "")
	// Land a newer commit in the mirror without reindexing.
	newOther := otherGo + "\nconst grape = \"purple\"\n"
	if err := os.WriteFile(filepath.Join(f.src, "other.go"), []byte(newOther), 0o644); err != nil {
		t.Fatalf("writing updated file: %v", err)
	}
	git(t, f.src, "add", ".")
	git(t, f.src, "commit", "-m", "add grape")
	git(t, f.mirror, "fetch", "origin", "+refs/heads/main:refs/heads/main")

	out, err := f.srv.ReadFile(context.Background(), ReadFileArgs{Repo: "acme/widget", Path: "other.go"})
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(out, "grape") {
		t.Errorf("output includes content newer than the indexed commit:\n%s", out)
	}
	if !strings.Contains(out, "(lines 1-3 of 3)") {
		t.Errorf("output not pinned to indexed commit:\n%s", out)
	}
}

func TestReadFileUnknownRepo(t *testing.T) {
	f := newFixture(t, "")
	_, err := f.srv.ReadFile(context.Background(), ReadFileArgs{Repo: "acme/nope", Path: "other.go"})
	if err == nil {
		t.Fatal("ReadFile unknown repo: err = nil, want error")
	}
	if !strings.Contains(err.Error(), "acme/nope") || !strings.Contains(err.Error(), "list_repos") {
		t.Errorf("error = %q, want repo name and list_repos hint", err)
	}

	_, err = f.srv.ReadFile(context.Background(), ReadFileArgs{Repo: "not-owner-name", Path: "other.go"})
	if err == nil || !strings.Contains(err.Error(), "owner/name") {
		t.Errorf("error = %v, want owner/name format hint", err)
	}
}

func TestReadFileMissingPath(t *testing.T) {
	f := newFixture(t, "")
	_, err := f.srv.ReadFile(context.Background(), ReadFileArgs{Repo: "acme/widget", Path: "nope.go"})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error = %v, want fs.ErrNotExist", err)
	}
}

func TestReadFileIndexMismatch(t *testing.T) {
	f := newFixture(t, "")
	// Point the status file at a commit the mirror does not have.
	writeStatus(t, f.statusPath, strings.Repeat("d", 40), time.Now())
	_, err := f.srv.ReadFile(context.Background(), ReadFileArgs{Repo: "acme/widget", Path: "other.go"})
	if !errors.Is(err, gitfile.ErrIndexMismatch) {
		t.Errorf("error = %v, want gitfile.ErrIndexMismatch", err)
	}
}

func TestListTree(t *testing.T) {
	f := newFixture(t, "")
	ctx := context.Background()
	out, err := f.srv.ListTree(ctx, ListTreeArgs{Repo: "acme/widget"})
	if err != nil {
		t.Fatalf("ListTree: %v", err)
	}
	for _, want := range []string{"other.go (", "widget.go (", "sub/\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("depth-1 output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sub/deep.go") {
		t.Errorf("depth-1 output descends too far:\n%s", out)
	}

	out, err = f.srv.ListTree(ctx, ListTreeArgs{Repo: "acme/widget", Depth: 2})
	if err != nil {
		t.Fatalf("ListTree depth 2: %v", err)
	}
	if !strings.Contains(out, "sub/deep.go") {
		t.Errorf("depth-2 output missing sub/deep.go:\n%s", out)
	}

	out, err = f.srv.ListTree(ctx, ListTreeArgs{Repo: "acme/widget", Path: "sub"})
	if err != nil {
		t.Fatalf("ListTree sub: %v", err)
	}
	if !strings.Contains(out, "sub/deep.go") {
		t.Errorf("sub listing missing deep.go:\n%s", out)
	}
}
