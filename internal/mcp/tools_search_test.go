package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/broderick-westrope/muninn/internal/index"
	"github.com/broderick-westrope/muninn/internal/search"
	"github.com/broderick-westrope/muninn/internal/status"
)

// git runs a git command against dir (or without -C when dir is empty),
// failing the test on error. Global and system config are disabled so
// host configuration (commit templates, hooks paths, ...) cannot leak
// into the fixture history.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{"-c", "user.name=test", "-c", "user.email=test@example.com"}
	if dir != "" {
		base = append(base, "-C", dir)
	}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The comment on Frobnicate deliberately avoids the symbol name so
// reference counts are exact (definition line 4, call sites lines 6 and 8).
const widgetGo = `package widget

// It does the thing.
func Frobnicate() int { return 42 }

func callerOne() int { return Frobnicate() }

func callerTwo() int { return Frobnicate() }
`

const otherGo = `package widget

const banana = "yellow"
`

const deepGo = `package sub

const deepThought = 42
`

// fixture is a fully-wired Server over a small indexed repo plus the paths
// needed to manipulate the mirror and status file from tests.
type fixture struct {
	srv        *Server
	commit     string
	src        string // source worktree, for adding newer commits
	mirror     string // bare mirror dir
	statusPath string
}

// newFixture builds acme/widget (widget.go, other.go, sub/deep.go), mirrors
// it under <root>/mirrors/acme/widget.git, indexes it, writes a fresh
// status file, and returns a Server over the lot.
func newFixture(t *testing.T, ctagsPath string) *fixture {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	git(t, "", "init", "-b", "main", src)
	files := map[string]string{
		"widget.go":   widgetGo,
		"other.go":    otherGo,
		"sub/deep.go": deepGo,
	}
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

	indexDir := filepath.Join(root, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatalf("creating index dir: %v", err)
	}
	ix := &index.Indexer{IndexDir: indexDir, CtagsPath: ctagsPath}
	if err := ix.IndexRepo(context.Background(), mirror, "acme/widget", "main", commit); err != nil {
		t.Fatalf("IndexRepo: %v", err)
	}

	statusPath := filepath.Join(root, "status.json")
	writeStatus(t, statusPath, commit, time.Now())

	searcher, err := search.Open(indexDir)
	if err != nil {
		t.Fatalf("search.Open: %v", err)
	}
	t.Cleanup(searcher.Close)

	return &fixture{
		srv:        New(searcher, statusPath, mirrorsDir),
		commit:     commit,
		src:        src,
		mirror:     mirror,
		statusPath: statusPath,
	}
}

// writeStatus writes a successful sync status for acme/widget finished at
// the given time.
func writeStatus(t *testing.T, path, commit string, finishedAt time.Time) {
	t.Helper()
	err := status.Write(path, &status.SyncStatus{
		StartedAt:     finishedAt.Add(-time.Minute),
		FinishedAt:    finishedAt,
		LastSuccessAt: finishedAt,
		Success:       true,
		Repos: map[string]status.RepoStatus{
			"acme/widget": {Fetched: true, Indexed: true, IndexedCommit: commit, LastSuccessAt: finishedAt},
		},
	})
	if err != nil {
		t.Fatalf("status.Write: %v", err)
	}
}

// writeDegradedStatus writes a status whose most recent run failed as a
// whole while the repo's own data is lastSuccess old: a broken sync
// mechanism over an index that may still be fresh.
func writeDegradedStatus(t *testing.T, path, commit string, lastSuccess time.Time, failures int) {
	t.Helper()
	err := status.Write(path, &status.SyncStatus{
		StartedAt:           time.Now().Add(-time.Minute),
		FinishedAt:          time.Now(),
		LastSuccessAt:       lastSuccess,
		Success:             false,
		TotalFailure:        true,
		Error:               "discovering repos: bad credentials",
		ConsecutiveFailures: failures,
		Repos: map[string]status.RepoStatus{
			"acme/widget": {Fetched: true, Indexed: true, IndexedCommit: commit, LastSuccessAt: lastSuccess},
		},
	})
	if err != nil {
		t.Fatalf("status.Write: %v", err)
	}
}

// matchLines returns the "repo/path:line:" output lines from tool output.
func matchLines(out string) []string {
	var lines []string
	re := regexp.MustCompile(`^\S+:\d+: `)
	for _, l := range strings.Split(out, "\n") {
		if re.MatchString(l) {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestGrep(t *testing.T) {
	f := newFixture(t, "")
	out, err := f.srv.Grep(context.Background(), GrepArgs{Pattern: "banana"})
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if !strings.Contains(out, `acme/widget/other.go:3: const banana = "yellow"`) {
		t.Errorf("output missing match line:\n%s", out)
	}
	if !strings.Contains(out, "1 matches in 1 files") {
		t.Errorf("output missing stats line:\n%s", out)
	}
	if strings.Contains(out, "[truncated") {
		t.Errorf("output has unexpected truncation notice:\n%s", out)
	}
}

func TestGrepNoMatches(t *testing.T) {
	f := newFixture(t, "")
	out, err := f.srv.Grep(context.Background(), GrepArgs{Pattern: "zzznotfound"})
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if out != "no matches" {
		t.Errorf("output = %q, want \"no matches\"", out)
	}
}

func TestGrepLiteral(t *testing.T) {
	f := newFixture(t, "")
	// As a regex banana.*yellow matches; as a literal string it does not.
	out, err := f.srv.Grep(context.Background(), GrepArgs{Pattern: "banana.*yellow", Literal: true})
	if err != nil {
		t.Fatalf("Grep literal: %v", err)
	}
	if out != "no matches" {
		t.Errorf("literal output = %q, want no matches", out)
	}

	out, err = f.srv.Grep(context.Background(), GrepArgs{Pattern: "banana.*yellow"})
	if err != nil {
		t.Fatalf("Grep regex: %v", err)
	}
	if got := len(matchLines(out)); got != 1 {
		t.Errorf("regex match lines = %d, want 1:\n%s", got, out)
	}

	// Literal patterns with spaces and quotes survive query quoting.
	out, err = f.srv.Grep(context.Background(), GrepArgs{Pattern: `banana = "yellow"`, Literal: true})
	if err != nil {
		t.Fatalf("Grep literal with quotes: %v", err)
	}
	lines := matchLines(out)
	if len(lines) != 1 || !strings.Contains(lines[0], "other.go:3:") {
		t.Errorf("quoted literal match lines = %v, want only other.go:3", lines)
	}
}

func TestGrepInclude(t *testing.T) {
	f := newFixture(t, "")
	// "package" appears in all three files; the include glob narrows to one.
	out, err := f.srv.Grep(context.Background(), GrepArgs{Pattern: "package", Include: "sub/*.go"})
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	lines := matchLines(out)
	if len(lines) != 1 || !strings.Contains(lines[0], "sub/deep.go:1:") {
		t.Errorf("match lines = %v, want only sub/deep.go", lines)
	}
}

func TestGrepCap(t *testing.T) {
	f := newFixture(t, "")
	// Frobnicate appears on 3 lines; cap at 1.
	out, err := f.srv.Grep(context.Background(), GrepArgs{Pattern: "Frobnicate", Limit: 1})
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if got := len(matchLines(out)); got != 1 {
		t.Errorf("match lines = %d, want 1 (cap):\n%s", got, out)
	}
	if !strings.Contains(out, "[truncated") {
		t.Errorf("output missing truncation notice:\n%s", out)
	}
}

func TestGrepGroupByRepo(t *testing.T) {
	f := newFixture(t, "")
	out, err := f.srv.Grep(context.Background(), GrepArgs{Pattern: "banana", GroupByRepo: true})
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if !strings.Contains(out, "# acme/widget\n") {
		t.Errorf("output missing repo header:\n%s", out)
	}
}

func TestGrepRepoFilter(t *testing.T) {
	f := newFixture(t, "")
	out, err := f.srv.Grep(context.Background(), GrepArgs{Pattern: "banana", Repo: "^nomatch/"})
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if out != "no matches" {
		t.Errorf("output = %q, want no matches with non-matching repo filter", out)
	}
}

func TestGlobToRegexp(t *testing.T) {
	tests := []struct {
		glob    string
		want    string
		matches []string
		rejects []string
	}{
		{
			glob:    `**/*.go`,
			want:    `(?i)^(?:.*/)?[^/]*\.go$`,
			matches: []string{"main.go", "a/b/c.go"},
			rejects: []string{"main.md"},
		},
		{
			glob:    `src/**/*.{ts,tsx}`,
			want:    `(?i)^src/(?:.*/)?[^/]*\.(?:ts|tsx)$`,
			matches: []string{"src/a.ts", "src/a/b/c.tsx"},
			rejects: []string{"lib/a.ts", "src/a.js"},
		},
		{
			glob:    `cmd/*/main.go`,
			want:    `(?i)^cmd/[^/]*/main\.go$`,
			matches: []string{"cmd/foo/main.go"},
			rejects: []string{"cmd/foo/bar/main.go", "cmd/main.go"},
		},
		{
			glob:    `*.md`,
			want:    `(?i)^[^/]*\.md$`,
			matches: []string{"README.md", "readme.MD"},
			rejects: []string{"docs/README.md"},
		},
	}
	for _, tt := range tests {
		got, err := globToRegexp(tt.glob)
		if err != nil {
			t.Errorf("globToRegexp(%q): %v", tt.glob, err)
			continue
		}
		if got != tt.want {
			t.Errorf("globToRegexp(%q) = %q, want %q", tt.glob, got, tt.want)
		}
		re := regexp.MustCompile(got)
		for _, m := range tt.matches {
			if !re.MatchString(m) {
				t.Errorf("globToRegexp(%q): %q should match %q", tt.glob, got, m)
			}
		}
		for _, m := range tt.rejects {
			if re.MatchString(m) {
				t.Errorf("globToRegexp(%q): %q should not match %q", tt.glob, got, m)
			}
		}
	}
}

func TestGlobToRegexpErrors(t *testing.T) {
	for _, glob := range []string{`{a,b`, `a}b`} {
		if _, err := globToRegexp(glob); err == nil {
			t.Errorf("globToRegexp(%q): err = nil, want error", glob)
		}
	}
}

func TestGlob(t *testing.T) {
	f := newFixture(t, "")
	out, err := f.srv.Glob(context.Background(), GlobArgs{Pattern: "**/*.go"})
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	for _, want := range []string{"acme/widget:", "widget.go", "other.go", "sub/deep.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "3 files") {
		t.Errorf("output missing file count:\n%s", out)
	}

	// A single-star glob does not cross directory boundaries.
	out, err = f.srv.Glob(context.Background(), GlobArgs{Pattern: "*.go"})
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if strings.Contains(out, "sub/deep.go") {
		t.Errorf("*.go should not match sub/deep.go:\n%s", out)
	}
	if !strings.Contains(out, "2 files") {
		t.Errorf("output missing file count:\n%s", out)
	}
}

func TestGlobCap(t *testing.T) {
	f := newFixture(t, "")
	out, err := f.srv.Glob(context.Background(), GlobArgs{Pattern: "**/*.go", Limit: 1})
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if !strings.Contains(out, "1 files") {
		t.Errorf("output missing capped file count:\n%s", out)
	}
	if !strings.Contains(out, "[truncated") {
		t.Errorf("output missing truncation notice:\n%s", out)
	}
}

func TestListRepos(t *testing.T) {
	f := newFixture(t, "")
	out, err := f.srv.ListRepos(context.Background(), ListReposArgs{})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	want := "acme/widget  main  " + f.commit[:7]
	if !strings.Contains(out, want) {
		t.Errorf("output missing %q:\n%s", want, out)
	}
	if !strings.Contains(out, "last sync run:") {
		t.Errorf("output missing sync run line:\n%s", out)
	}
	if strings.Contains(out, "WARNING") {
		t.Errorf("fresh index should not warn:\n%s", out)
	}

	out, err = f.srv.ListRepos(context.Background(), ListReposArgs{Query: "^nomatch/"})
	if err != nil {
		t.Fatalf("ListRepos filtered: %v", err)
	}
	if !strings.Contains(out, "0 repos") {
		t.Errorf("filtered output should list 0 repos:\n%s", out)
	}
}

func TestListReposStale(t *testing.T) {
	f := newFixture(t, "")
	writeStatus(t, f.statusPath, f.commit, time.Now().Add(-48*time.Hour))
	out, err := f.srv.ListRepos(context.Background(), ListReposArgs{})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if !strings.HasPrefix(out, "WARNING: 1 of 1 repos have not synced") {
		t.Errorf("stale repo output should start with the fleet warning:\n%s", out)
	}
	if !strings.Contains(out, "acme/widget") {
		t.Errorf("output should name the stale repo:\n%s", out)
	}
	if !strings.Contains(out, "(STALE)") {
		t.Errorf("output should mark the repo's own age column stale:\n%s", out)
	}

	if err := os.Remove(f.statusPath); err != nil {
		t.Fatalf("removing status file: %v", err)
	}
	out, err = f.srv.ListRepos(context.Background(), ListReposArgs{})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if !strings.HasPrefix(out, "WARNING: no sync status") {
		t.Errorf("missing status output should start with warning:\n%s", out)
	}
}

// TestListReposDegraded covers a broken sync mechanism: a total failure
// keeps FinishedAt fresh, so the failure is only visible through the
// mechanism warning, not through any repo's age.
func TestListReposDegraded(t *testing.T) {
	f := newFixture(t, "")
	writeDegradedStatus(t, f.statusPath, f.commit, time.Now().Add(-72*time.Hour), 5)

	out, err := f.srv.ListRepos(context.Background(), ListReposArgs{})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if !strings.Contains(out, "the last 5 sync attempt(s) failed outright") {
		t.Errorf("output missing the degradation warning:\n%s", out)
	}
	if !strings.Contains(out, "bad credentials") {
		t.Errorf("output missing the recorded cause:\n%s", out)
	}
	// The repo's own data is 72h old, so it is also reported stale.
	if !strings.Contains(out, "1 of 1 repos have not synced") {
		t.Errorf("output missing the stale-repo warning:\n%s", out)
	}
}

// A still-fresh index whose sync has started failing must warn about the
// failures even though nothing is stale yet — that is the window in which
// the problem is cheap to fix.
func TestListReposDegradedButFresh(t *testing.T) {
	f := newFixture(t, "")
	writeDegradedStatus(t, f.statusPath, f.commit, time.Now().Add(-90*time.Minute), 1)

	out, err := f.srv.ListRepos(context.Background(), ListReposArgs{})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if strings.Contains(out, "have not synced") {
		t.Errorf("repo is within staleAfter, should not warn stale:\n%s", out)
	}
	if !strings.Contains(out, "the last 1 sync attempt(s) failed outright") {
		t.Errorf("output missing the degradation warning:\n%s", out)
	}
}

// TestListReposPartialFailureDoesNotAgeTheFleet is the regression test for
// the behaviour this model exists to provide: one permanently failing repo
// must not make the other repos report as stale.
func TestListReposPartialFailureDoesNotAgeTheFleet(t *testing.T) {
	f := newFixture(t, "")
	now := time.Now()
	err := status.Write(f.statusPath, &status.SyncStatus{
		StartedAt:  now.Add(-time.Minute),
		FinishedAt: now,
		// No clean sweep for days because one repo keeps failing.
		LastSuccessAt:       now.Add(-96 * time.Hour),
		Success:             false,
		Error:               "1 of 2 repos failed",
		ConsecutiveFailures: 40,
		Repos: map[string]status.RepoStatus{
			// Freshly synced despite the run being marked failed.
			"acme/widget": {Fetched: true, Indexed: true, IndexedCommit: f.commit, LastSuccessAt: now},
			"acme/broken": {Error: "authentication failed", LastSuccessAt: now.Add(-96 * time.Hour)},
		},
	})
	if err != nil {
		t.Fatalf("status.Write: %v", err)
	}

	out, err := f.srv.ListRepos(context.Background(), ListReposArgs{})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if strings.Contains(out, "acme/widget") && strings.Contains(out, "STALE") {
		// acme/broken has no shard, so only acme/widget is listed; it
		// must not be marked stale.
		t.Errorf("a fresh repo was marked stale because another repo failed:\n%s", out)
	}
	if strings.Contains(out, "failed outright") {
		t.Errorf("a partial failure is not a broken mechanism:\n%s", out)
	}
}

func TestListReposNeverSucceeded(t *testing.T) {
	f := newFixture(t, "")
	// A repo that has never synced has no clock and is not indexed: the
	// two always move together in a real run.
	err := status.Write(f.statusPath, &status.SyncStatus{
		StartedAt:           time.Now().Add(-time.Minute),
		FinishedAt:          time.Now(),
		Success:             false,
		TotalFailure:        true,
		Error:               "discovering repos: bad credentials",
		ConsecutiveFailures: 3,
		Repos: map[string]status.RepoStatus{
			"acme/widget": {Error: "authentication failed"},
		},
	})
	if err != nil {
		t.Fatalf("status.Write: %v", err)
	}

	out, err := f.srv.ListRepos(context.Background(), ListReposArgs{})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if !strings.Contains(out, "failed outright") {
		t.Errorf("output should report the broken sync mechanism:\n%s", out)
	}
	if !strings.Contains(out, "never synced") {
		t.Errorf("output should mark the repo as never synced:\n%s", out)
	}
}
