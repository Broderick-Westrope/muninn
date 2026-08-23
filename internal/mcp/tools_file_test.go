package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newMirrorFixture builds acme/widget from the given files as a bare mirror
// with a fresh status file, but no search index: file tools resolve content
// from the mirror alone, so the Server needs no searcher.
func newMirrorFixture(t *testing.T, files map[string]string) *Server {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	git(t, "", "init", "-b", "main", src)
	for name, content := range files {
		path := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating fixture dir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing fixture file %s: %v", name, err)
		}
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "initial")

	root := t.TempDir()
	mirrorsDir := filepath.Join(root, "mirrors")
	mirror := filepath.Join(mirrorsDir, "acme", "widget.git")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("creating mirrors dir: %v", err)
	}
	git(t, "", "clone", "--bare", src, mirror)
	commit := git(t, mirror, "rev-parse", "refs/heads/main")

	statusPath := filepath.Join(root, "status.json")
	writeStatus(t, statusPath, commit, time.Now())
	return New(nil, statusPath, mirrorsDir)
}

func TestReadFileLimitCap(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= maxReadFileLimit+100; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	srv := newMirrorFixture(t, map[string]string{"big.txt": b.String()})

	out, err := srv.ReadFile(context.Background(), ReadFileArgs{Repo: "acme/widget", Path: "big.txt", Limit: 100_000})
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	wantHeader := fmt.Sprintf("(lines 1-%d of %d)", maxReadFileLimit, maxReadFileLimit+100)
	if !strings.Contains(out, wantHeader) {
		t.Errorf("output missing clamped header %q:\n%.200s", wantHeader, out)
	}
	if !strings.Contains(out, "use offset to page") {
		t.Errorf("output missing paging hint:\n%.200s", out)
	}
	if strings.Contains(out, fmt.Sprintf("line %d\n", maxReadFileLimit+1)) {
		t.Error("output includes lines past the cap")
	}
}

func TestListTreeTruncation(t *testing.T) {
	files := make(map[string]string, maxTreeEntries+20)
	for i := 0; i < maxTreeEntries+20; i++ {
		files[fmt.Sprintf("f%04d.txt", i)] = "x\n"
	}
	srv := newMirrorFixture(t, files)

	out, err := srv.ListTree(context.Background(), ListTreeArgs{Repo: "acme/widget"})
	if err != nil {
		t.Fatalf("ListTree: %v", err)
	}
	if !strings.Contains(out, "[truncated: 20 more entries") {
		t.Errorf("output missing truncation notice:\n%.200s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("%d entries", maxTreeEntries+20)) {
		t.Errorf("output missing total entry count:\n%.200s", out)
	}
	if got := strings.Count(out, ".txt ("); got != maxTreeEntries {
		t.Errorf("listed entries = %d, want %d", got, maxTreeEntries)
	}
}

func TestReadFileInvalidRepo(t *testing.T) {
	srv := New(nil, filepath.Join(t.TempDir(), "status.json"), t.TempDir())
	for _, repo := range []string{
		"../widget",
		"./widget",
		"acme/..",
		"acme/.",
		"acme/",
		"/widget",
		"acme/wid/get",
		`acme\evil/widget`,
		`acme/wid\get`,
	} {
		_, err := srv.ReadFile(context.Background(), ReadFileArgs{Repo: repo, Path: "a.txt"})
		if err == nil {
			t.Errorf("ReadFile(%q): err = nil, want invalid-repo error", repo)
			continue
		}
		if !strings.Contains(err.Error(), "invalid repo") {
			t.Errorf("ReadFile(%q): err = %q, want invalid-repo error", repo, err)
		}
	}
}

func TestToolLimitClamps(t *testing.T) {
	tests := []struct {
		name                 string
		limit, def, maxLimit int
		want                 int
	}{
		{"grep over max", 1000, defaultGrepLimit, maxGrepLimit, 200},
		{"glob over max", 10_000, defaultGlobLimit, maxGlobLimit, 500},
		{"definitions over max", 999, defaultDefinitionsLimit, maxDefinitionsLimit, 200},
		{"references over max", 999, defaultReferencesLimit, maxReferencesLimit, 300},
		{"read_file over max", 100_000, defaultReadFileLimit, maxReadFileLimit, 2000},
		{"zero uses default", 0, defaultGrepLimit, maxGrepLimit, defaultGrepLimit},
		{"negative uses default", -5, defaultGrepLimit, maxGrepLimit, defaultGrepLimit},
		{"in range passes through", 10, defaultGrepLimit, maxGrepLimit, 10},
	}
	for _, tt := range tests {
		if got := clampLimit(tt.limit, tt.def, tt.maxLimit); got != tt.want {
			t.Errorf("%s: clampLimit(%d, %d, %d) = %d, want %d", tt.name, tt.limit, tt.def, tt.maxLimit, got, tt.want)
		}
	}
}
