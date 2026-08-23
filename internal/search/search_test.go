package search

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/broderick-westrope/muninn/internal/ctags"
	"github.com/broderick-westrope/muninn/internal/index"
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

const widgetGo = `package widget

// Frobnicate does the thing.
func Frobnicate() int { return 42 }

func callerOne() int { return Frobnicate() }

func callerTwo() int { return Frobnicate() }
`

const otherGo = `package widget

const banana = "yellow"
`

// fixtureIndex builds a shard directory over a small fixture repo containing
// widget.go (a function defined once and called twice) and other.go. It
// returns the index directory and the indexed commit SHA.
func fixtureIndex(t *testing.T, ctagsPath string) (indexDir, commit string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	git(t, "", "init", "-b", "main", src)
	for name, content := range map[string]string{"widget.go": widgetGo, "other.go": otherGo} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o644); err != nil {
			t.Fatalf("writing fixture file %s: %v", name, err)
		}
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "initial")

	mirror := filepath.Join(t.TempDir(), "widget.git")
	git(t, "", "clone", "--bare", src, mirror)
	commit = git(t, mirror, "rev-parse", "refs/heads/main")

	indexDir = t.TempDir()
	ix := &index.Indexer{IndexDir: indexDir, CtagsPath: ctagsPath}
	if err := ix.IndexRepo(context.Background(), mirror, "acme/widget", "main", commit); err != nil {
		t.Fatalf("IndexRepo: %v", err)
	}
	return indexDir, commit
}

// openSearcher builds a fixture index (without ctags) and opens it.
func openSearcher(t *testing.T, ctagsPath string) (*Searcher, string) {
	t.Helper()
	indexDir, commit := fixtureIndex(t, ctagsPath)
	s, err := Open(indexDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s, commit
}

// allLines flattens a result to "path:line" strings for assertions.
func allLines(res *Result) []string {
	var lines []string
	for _, f := range res.Files {
		for _, l := range f.Lines {
			lines = append(lines, f.Path+":"+strings.TrimSpace(l.Line))
		}
	}
	return lines
}

func TestSearchLiteral(t *testing.T) {
	s, _ := openSearcher(t, "")
	res, err := s.Search(context.Background(), Options{Query: `"banana"`})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "other.go" {
		t.Fatalf("files = %+v, want single match in other.go", res.Files)
	}
	if got := res.Files[0].Lines[0].LineNumber; got != 3 {
		t.Errorf("line number = %d, want 3", got)
	}
	if res.Truncated {
		t.Error("Truncated = true, want false")
	}
	if res.Stats.MatchCount == 0 {
		t.Error("Stats.MatchCount = 0, want > 0")
	}
	if res.Files[0].Repo != "acme/widget" {
		t.Errorf("repo = %q, want acme/widget", res.Files[0].Repo)
	}
}

func TestSearchRegex(t *testing.T) {
	s, _ := openSearcher(t, "")
	res, err := s.Search(context.Background(), Options{Query: `func.Frob.icate`})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	lines := allLines(res)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "widget.go:") {
		t.Fatalf("lines = %v, want single definition line in widget.go", lines)
	}
}

func TestSearchFileFilter(t *testing.T) {
	s, _ := openSearcher(t, "")
	// "package" appears in both files; the file: atom must narrow to one.
	res, err := s.Search(context.Background(), Options{Query: `package file:other`})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "other.go" {
		t.Fatalf("files = %+v, want only other.go", res.Files)
	}
}

func TestSearchParseError(t *testing.T) {
	s, _ := openSearcher(t, "")
	if _, err := s.Search(context.Background(), Options{Query: `(unbalanced`}); err == nil {
		t.Fatal("Search with invalid query: err = nil, want parse error")
	}
}

func TestSearchRepoFilter(t *testing.T) {
	s, _ := openSearcher(t, "")
	res, err := s.Search(context.Background(), Options{Query: `banana`, RepoFilter: `^acme/`})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("files = %+v, want 1 match with matching repo filter", res.Files)
	}
	res, err = s.Search(context.Background(), Options{Query: `banana`, RepoFilter: `^nomatch/`})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Files) != 0 {
		t.Fatalf("files = %+v, want none with non-matching repo filter", res.Files)
	}
}

func TestSearchTruncation(t *testing.T) {
	s, _ := openSearcher(t, "")
	// Frobnicate appears on 4 lines (comment, definition, two calls).
	res, err := s.Search(context.Background(), Options{Query: `Frobnicate`, MaxResults: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := len(allLines(res)); got != 1 {
		t.Fatalf("returned lines = %d, want 1 (MaxResults cap)", got)
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true when cap is hit")
	}

	// Without the cap all lines come back and nothing is truncated.
	res, err = s.Search(context.Background(), Options{Query: `Frobnicate`})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := len(allLines(res)); got != 4 {
		t.Fatalf("returned lines = %d, want 4", got)
	}
	if res.Truncated {
		t.Error("Truncated = true, want false without cap")
	}
}

func TestSearchSymbols(t *testing.T) {
	ctagsPath, err := ctags.Resolve("")
	if err != nil {
		t.Skipf("universal-ctags unavailable, skipping symbol search test: %v", err)
	}
	s, _ := openSearcher(t, ctagsPath)

	// sym: finds only the definition and marks it as such.
	res, err := s.Search(context.Background(), Options{Query: `sym:Frobnicate`})
	if err != nil {
		t.Fatalf("Search sym:: %v", err)
	}
	lines := allLines(res)
	if len(lines) != 1 || !strings.Contains(lines[0], "func Frobnicate") {
		t.Fatalf("sym: lines = %v, want only the definition line", lines)
	}
	if !res.Files[0].Lines[0].IsSymbolDef {
		t.Error("IsSymbolDef = false on sym: match, want true")
	}

	// A plain content search returns the call sites (and the definition),
	// none marked as definitions: zoekt attaches symbol info only to
	// matches produced by sym: atoms, so callers distinguish definitions
	// by running a sym: query, not by inspecting content matches.
	res, err = s.Search(context.Background(), Options{Query: `Frobnicate\(\)`})
	if err != nil {
		t.Fatalf("Search content query: %v", err)
	}
	var callSites int
	for _, f := range res.Files {
		for _, l := range f.Lines {
			if l.IsSymbolDef {
				t.Errorf("IsSymbolDef = true on content match %q, want false", l.Line)
			}
			if strings.Contains(l.Line, "return Frobnicate()") {
				callSites++
			}
		}
	}
	if callSites != 2 {
		t.Errorf("call site lines = %d, want 2", callSites)
	}
}

func TestListRepos(t *testing.T) {
	s, commit := openSearcher(t, "")
	repos, err := s.ListRepos(context.Background())
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("repos = %+v, want exactly one", repos)
	}
	got := repos[0]
	if got.Name != "acme/widget" {
		t.Errorf("Name = %q, want acme/widget", got.Name)
	}
	if got.Branch != "main" {
		t.Errorf("Branch = %q, want main", got.Branch)
	}
	if got.IndexedCommit != commit {
		t.Errorf("IndexedCommit = %q, want %q", got.IndexedCommit, commit)
	}
}
