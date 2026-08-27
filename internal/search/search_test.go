package search

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

const kiwi = "green"
`

// Facet and file-filter tests need one token spanning several extensions,
// so an extension filter has something to exclude and a top-level `or` has
// two sides that differ by file type. "banana" stays unique to other.go for
// the single-match tests, so these use "kiwi" instead.
const otherTS = `export const kiwi = "green";
`

// makefile has no extension, covering the "no extension" facet bucket.
const makefile = `build:
	@echo kiwi
`

// fixtureIndex builds a shard directory over a small fixture repo containing
// widget.go (a function defined once and called twice) and other.go. It
// returns the index directory and the indexed commit SHA.
func fixtureIndex(t *testing.T, ctagsPath string) (indexDir, commit string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	git(t, "", "init", "-b", "main", src)
	for name, content := range map[string]string{
		"widget.go": widgetGo,
		"other.go":  otherGo,
		"other.ts":  otherTS,
		"Makefile":  makefile,
	} {
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

// fixtureIndexMultiRepo indexes acme/widget alongside a second repo whose
// name contains a regex metacharacter, so facet alternations can be tested
// for both multi-repo OR and QuoteMeta escaping. The second repo shares the
// "kiwi" token so one query spans both.
func fixtureIndexMultiRepo(t *testing.T) string {
	t.Helper()
	indexDir, _ := fixtureIndex(t, "")

	src := filepath.Join(t.TempDir(), "lib")
	git(t, "", "init", "-b", "main", src)
	if err := os.WriteFile(filepath.Join(src, "lib.go"), []byte("package lib\n\nconst kiwi = \"green\"\n"), 0o644); err != nil {
		t.Fatalf("writing lib fixture: %v", err)
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "initial")

	mirror := filepath.Join(t.TempDir(), "my.lib.git")
	git(t, "", "clone", "--bare", src, mirror)
	commit := git(t, mirror, "rev-parse", "refs/heads/main")

	ix := &index.Indexer{IndexDir: indexDir}
	if err := ix.IndexRepo(context.Background(), mirror, "acme/my.lib", "main", commit); err != nil {
		t.Fatalf("IndexRepo acme/my.lib: %v", err)
	}
	return indexDir
}

// openMultiRepoSearcher opens a searcher over the two-repo fixture.
func openMultiRepoSearcher(t *testing.T) *Searcher {
	t.Helper()
	s, err := Open(fixtureIndexMultiRepo(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestExtOf(t *testing.T) {
	tests := []struct{ path, want string }{
		{"a/b/c.go", "go"},
		{"main.go", "go"},
		{"Makefile", ""},
		{".gitignore", ""},
		{"a/.gitignore", ""},
		{"x.TAR.GZ", "gz"},
		{"dir.d/file", ""},
		{"a/b/LICENSE", ""},
	}
	for _, tt := range tests {
		if got := extOf(tt.path); got != tt.want {
			t.Errorf("extOf(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// TestSearchFileFilterComposition is the regression test for zoekt's operator
// precedence: `or` binds looser than implicit AND, so a file filter appended
// to the query text would apply to only one side of a disjunction. The
// fixture spreads the "kiwi" token across .go, .ts, and an extensionless
// file, so a wrongly-composed filter shows up as leaked non-.go matches.
func TestSearchFileFilterComposition(t *testing.T) {
	s, _ := openSearcher(t, "")

	res, err := s.Search(context.Background(), Options{
		Query:      "kiwi or Frobnicate",
		FileFilter: `\.go$`,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Files) == 0 {
		t.Fatal("no files matched; the filter should narrow, not empty, the result")
	}
	for _, f := range res.Files {
		if !strings.HasSuffix(f.Path, ".go") {
			t.Errorf("file %q leaked past the .go filter — the filter did not cover both sides of the `or`", f.Path)
		}
	}
	// Both sides of the disjunction must survive: kiwi lives in other.go and
	// Frobnicate in widget.go.
	paths := map[string]bool{}
	for _, f := range res.Files {
		paths[f.Path] = true
	}
	if !paths["other.go"] || !paths["widget.go"] {
		t.Errorf("matched %v, want both other.go (kiwi) and widget.go (Frobnicate)", paths)
	}
}

// TestFileFilterParses pins that zoekt accepts the grouped, anchored,
// alternated regexes the facet layer builds. If its lexer ever terminated a
// file: value at "(" or "|", extension facets would fail at parse time.
func TestFileFilterParses(t *testing.T) {
	s, _ := openSearcher(t, "")
	for _, filter := range []string{
		`(\.go$)`,
		`(\.go$|\.ts$)`,
		`(\.go$|(^|/)\.?[^/.]+$)`,
	} {
		if _, err := s.Search(context.Background(), Options{Query: "kiwi", FileFilter: filter}); err != nil {
			t.Errorf("FileFilter %q: %v", filter, err)
		}
	}
}

func TestSearchFileFilterNoExtension(t *testing.T) {
	s, _ := openSearcher(t, "")

	res, err := s.Search(context.Background(), Options{
		Query:      "kiwi",
		FileFilter: `(^|/)\.?[^/.]+$`,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "Makefile" {
		t.Errorf("files = %+v, want only Makefile", res.Files)
	}
}

func TestAggregateIgnoresFilters(t *testing.T) {
	s := openMultiRepoSearcher(t)

	facets, err := s.Aggregate(context.Background(), "kiwi", 0)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	byValue := func(vs []FacetValue) map[string]int {
		m := map[string]int{}
		for _, v := range vs {
			m[v.Value] = v.Count
		}
		return m
	}
	repos := byValue(facets.Repos)
	if repos["acme/widget"] == 0 || repos["acme/my.lib"] == 0 {
		t.Errorf("repos = %+v, want both fixture repos", facets.Repos)
	}
	exts := byValue(facets.Exts)
	for _, want := range []string{"go", "ts", ""} {
		if exts[want] == 0 {
			t.Errorf("exts = %+v, want a bucket for %q", facets.Exts, want)
		}
	}
}

func TestAggregateCountsLines(t *testing.T) {
	s, _ := openSearcher(t, "")

	// Frobnicate is on four lines of widget.go: the doc comment, the
	// definition, and two calls. Counting files would report 1.
	facets, err := s.Aggregate(context.Background(), "Frobnicate", 0)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if len(facets.Repos) != 1 || facets.Repos[0].Count != 4 {
		t.Errorf("repos = %+v, want acme/widget with 4 line matches", facets.Repos)
	}
}

// TestAggregateIsDeterministic guards the facet list against reshuffling
// between identical searches. zoekt's TotalMaxMatchCount is a budget shared
// across shards, so a truncated aggregation returns whichever shards
// finished first; the same query then reports a different set of repos run
// to run and facet chips appear and vanish as the user clicks. Aggregate
// must use a per-shard cap, which never halts the search early.
//
// The fixture is small enough not to truncate, so this pins the contract
// rather than the race. TestAggregateVisitsEverySaturatedShard covers the
// truncating case.
func TestAggregateIsDeterministic(t *testing.T) {
	s := openMultiRepoSearcher(t)

	var first string
	for i := 0; i < 8; i++ {
		facets, err := s.Aggregate(context.Background(), "kiwi", 0)
		if err != nil {
			t.Fatalf("Aggregate: %v", err)
		}
		var b strings.Builder
		for _, v := range facets.Repos {
			fmt.Fprintf(&b, "%s:%d|", v.Value, v.Count)
		}
		for _, v := range facets.Exts {
			fmt.Fprintf(&b, "%s:%d|", v.Value, v.Count)
		}
		if i == 0 {
			first = b.String()
			continue
		}
		if b.String() != first {
			t.Fatalf("run %d differs:\n first: %s\n  this: %s", i, first, b.String())
		}
	}
}

// TestAggregateIsExhaustive pins that aggregation counts every match rather
// than stopping at a cap. A capped pass distorts counts by shard spread
// instead of match volume, which inverts the ranking the facet sidebar
// sorts by.
func TestAggregateIsExhaustive(t *testing.T) {
	s, _ := openSearcher(t, "")

	facets, err := s.Aggregate(context.Background(), "kiwi", 0)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if facets.Partial {
		t.Error("Partial = true for a fixture that fits well inside any deadline")
	}
	// kiwi appears once in each of other.go, other.ts, and Makefile.
	total := 0
	for _, v := range facets.Exts {
		total += v.Count
	}
	if total != 3 {
		t.Errorf("total ext counts = %d, want 3 (one per fixture file)", total)
	}
}

// TestAggregateDeadlineReportsPartial pins that an expired deadline is
// disclosed rather than silently returning a subset. zoekt returns partial
// results with err == nil when MaxWallTime expires, so Partial has to be
// derived from skipped-shard stats or the caller cannot tell.
func TestAggregateDeadlineReportsPartial(t *testing.T) {
	s := openMultiRepoSearcher(t)

	// A deadline this small cannot complete, but must not error.
	facets, err := s.Aggregate(context.Background(), "kiwi", time.Nanosecond)
	if err != nil {
		t.Fatalf("Aggregate with an expired deadline: %v", err)
	}
	if !facets.Partial {
		t.Skip("fixture completed inside a 1ns deadline; nothing to assert")
	}
	// Even when counting is cut short, the repo value list stays complete:
	// List backfills the repos whose shards went unvisited.
	got := map[string]bool{}
	for _, v := range facets.Repos {
		got[v.Value] = true
	}
	if !got["acme/widget"] || !got["acme/my.lib"] {
		t.Errorf("repos = %+v, want both repos even when partial", facets.Repos)
	}
}

// TestSearchExhaustiveLiftsCaps pins that Exhaustive returns every match
// rather than stopping at the default display limit.
func TestSearchExhaustiveLiftsCaps(t *testing.T) {
	s := openMultiRepoSearcher(t)

	res, err := s.Search(context.Background(), Options{Query: "kiwi", Exhaustive: true})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Files) < 2 {
		t.Errorf("files = %d, want matches from more than one shard", len(res.Files))
	}
	if res.Truncated || res.Partial {
		t.Errorf("truncated = %v, partial = %v; want a complete result", res.Truncated, res.Partial)
	}
}
