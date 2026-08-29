package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/broderick-westrope/muninn/internal/gitfile"
	"github.com/broderick-westrope/muninn/internal/githistory"
)

// historyFixture is a Server over a bare mirror of acme/widget with a
// scripted history: a root commit, a side-branch change to Foo behind a
// --no-ff merge, a rename, and a post-merge commit. No search index is
// built: history tools resolve everything from the mirror alone.
type historyFixture struct {
	srv        *Server
	commit     string // indexed commit (main tip)
	root       string // adds foo.go, bar.txt, many.txt
	side       string // C: changes Foo on branch feature
	merge      string // M: merges feature with --no-ff
	rename     string // renames bar.txt to baz.txt
	post       string // main tip after the merge
	statusPath string
}

// fooV1 deliberately mentions Foo once so the pickaxe fixture is exact:
// the side commit adds a second occurrence.
const historyFooV1 = "package widget\n\nfunc Foo() int { return 1 }\n"

const historyFooV2 = "package widget\n\n// Foo returns one.\nfunc Foo() int { return 1 }\n"

// manyLines exceeds the default blame line cap.
const manyLines = defaultBlameLines + 50

func newHistoryFixture(t *testing.T) *historyFixture {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	git(t, "", "init", "-b", "main", src)

	var many strings.Builder
	for i := 1; i <= manyLines; i++ {
		fmt.Fprintf(&many, "many line %d\n", i)
	}
	files := map[string]string{
		"foo.go":   historyFooV1,
		"bar.txt":  "bar v1\n",
		"many.txt": many.String(),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o644); err != nil {
			t.Fatalf("writing fixture file %s: %v", name, err)
		}
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "root: add foo and bar")
	f := &historyFixture{root: git(t, src, "rev-parse", "HEAD")}

	git(t, src, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(src, "foo.go"), []byte(historyFooV2), 0o644); err != nil {
		t.Fatalf("writing foo.go v2: %v", err)
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "document Foo")
	f.side = git(t, src, "rev-parse", "HEAD")

	git(t, src, "checkout", "main")
	git(t, src, "merge", "--no-ff", "feature", "-m", "merge feature into main")
	f.merge = git(t, src, "rev-parse", "HEAD")

	git(t, src, "mv", "bar.txt", "baz.txt")
	git(t, src, "commit", "-m", "rename bar to baz")
	f.rename = git(t, src, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(src, "baz.txt"), []byte("bar v1\nbaz addition\n"), 0o644); err != nil {
		t.Fatalf("writing baz.txt v2: %v", err)
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "extend baz")
	f.post = git(t, src, "rev-parse", "HEAD")
	f.commit = f.post

	root := t.TempDir()
	mirrorsDir := filepath.Join(root, "mirrors")
	mirror := filepath.Join(mirrorsDir, "acme", "widget.git")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("creating mirrors dir: %v", err)
	}
	git(t, "", "clone", "--bare", src, mirror)

	f.statusPath = filepath.Join(root, "status.json")
	writeStatus(t, f.statusPath, f.commit, time.Now())
	f.srv = New(nil, f.statusPath, mirrorsDir)
	return f
}

func TestSearchCommitsMainline(t *testing.T) {
	f := newHistoryFixture(t)
	out, err := f.srv.SearchCommits(context.Background(), SearchCommitsArgs{Repo: "acme/widget"})
	if err != nil {
		t.Fatalf("SearchCommits: %v", err)
	}
	// First-parent default: mainline only, side commit excluded.
	if strings.Contains(out, shortSHA(f.side)) {
		t.Errorf("mainline output includes side commit:\n%s", out)
	}
	for _, sha := range []string{f.root, f.merge, f.rename, f.post} {
		if !strings.Contains(out, shortSHA(sha)) {
			t.Errorf("output missing mainline commit %s:\n%s", shortSHA(sha), out)
		}
	}
	if !strings.Contains(out, "4 commits") {
		t.Errorf("output missing commit count:\n%s", out)
	}
	// Merge annotation without pickaxe stays short.
	if !strings.Contains(out, "merge feature into main (merge)\n") {
		t.Errorf("output missing plain merge annotation:\n%s", out)
	}
	if strings.Contains(out, "first_parent: false") {
		t.Errorf("plain merge annotation must not include the pickaxe advice:\n%s", out)
	}
}

func TestSearchCommitsPickaxeMergeAnnotation(t *testing.T) {
	f := newHistoryFixture(t)
	out, err := f.srv.SearchCommits(context.Background(), SearchCommitsArgs{
		Repo:           "acme/widget",
		ChangedLiteral: "Foo returns one",
	})
	if err != nil {
		t.Fatalf("SearchCommits pickaxe: %v", err)
	}
	want := shortSHA(f.merge) + "  "
	if !strings.Contains(out, want) {
		t.Fatalf("pickaxe output missing merge commit %s:\n%s", shortSHA(f.merge), out)
	}
	if !strings.Contains(out, "(merge — rerun with first_parent: false for the underlying commit)") {
		t.Errorf("pickaxe merge row missing extended annotation:\n%s", out)
	}

	// first_parent: false surfaces the underlying commit.
	fp := false
	out, err = f.srv.SearchCommits(context.Background(), SearchCommitsArgs{
		Repo:           "acme/widget",
		ChangedLiteral: "Foo returns one",
		FirstParent:    &fp,
	})
	if err != nil {
		t.Fatalf("SearchCommits pickaxe first_parent=false: %v", err)
	}
	if !strings.Contains(out, shortSHA(f.side)) {
		t.Errorf("first_parent: false output missing underlying commit %s:\n%s", shortSHA(f.side), out)
	}
}

func TestSearchCommitsTruncation(t *testing.T) {
	f := newHistoryFixture(t)
	out, err := f.srv.SearchCommits(context.Background(), SearchCommitsArgs{Repo: "acme/widget", Limit: 2})
	if err != nil {
		t.Fatalf("SearchCommits: %v", err)
	}
	if !strings.Contains(out, "[truncated: more commits match") {
		t.Errorf("output missing truncation notice:\n%s", out)
	}
	if !strings.Contains(out, "2 commits") {
		t.Errorf("output missing clamped commit count:\n%s", out)
	}
}

func TestSearchCommitsUnknownRepo(t *testing.T) {
	f := newHistoryFixture(t)
	_, err := f.srv.SearchCommits(context.Background(), SearchCommitsArgs{Repo: "acme/nope"})
	if err == nil {
		t.Fatal("SearchCommits: err = nil, want unknown-repo error")
	}
	if !strings.Contains(err.Error(), "no indexed commit") {
		t.Errorf("err = %q, want unknown-repo error", err)
	}
}

func TestSearchCommitsUnknownRev(t *testing.T) {
	f := newHistoryFixture(t)
	_, err := f.srv.SearchCommits(context.Background(), SearchCommitsArgs{Repo: "acme/widget", Rev: "no-such-branch"})
	if err == nil {
		t.Fatal("SearchCommits: err = nil, want unknown-rev error")
	}
	if !strings.Contains(err.Error(), "no-such-branch") {
		t.Errorf("err = %q, want mention of the unknown rev", err)
	}
}

func TestRenderDiffFilesStatLineCap(t *testing.T) {
	d := &githistory.Diff{}
	for i := range statLineCap + 50 {
		d.Files = append(d.Files, githistory.FileDiff{
			Path:     fmt.Sprintf("file%03d.txt", i),
			StatLine: fmt.Sprintf("file%03d.txt | +1 -0", i),
		})
	}
	var b strings.Builder
	renderDiffFiles(&b, d)
	out := b.String()
	if got := strings.Count(out, " | +1 -0"); got != statLineCap {
		t.Errorf("rendered %d stat lines, want %d", got, statLineCap)
	}
	notice := "[truncated: 50 more changed files; narrow with a path filter]"
	if !strings.Contains(out, notice) {
		t.Errorf("output missing truncation notice %q:\n%.300s", notice, out)
	}
}

func TestRenderDiffFilesStatLineCapSpansOmitted(t *testing.T) {
	// The cap counts stat-only files and omitted-block lines together;
	// the omitted header renders only when at least one omitted line fits.
	d := &githistory.Diff{}
	for i := range statLineCap - 10 {
		d.Files = append(d.Files, githistory.FileDiff{
			Path:     fmt.Sprintf("file%03d.txt", i),
			StatLine: fmt.Sprintf("file%03d.txt | +1 -0", i),
		})
	}
	for i := range 20 {
		d.OmittedStats = append(d.OmittedStats, fmt.Sprintf("omit%02d.dat | binary", i))
	}
	var b strings.Builder
	renderDiffFiles(&b, d)
	out := b.String()
	if got := strings.Count(out, " | +1 -0") + strings.Count(out, " | binary"); got != statLineCap {
		t.Errorf("rendered %d stat lines, want %d", got, statLineCap)
	}
	if !strings.Contains(out, "[omitted: patches withheld") {
		t.Errorf("output missing omitted header:\n%.300s", out)
	}
	notice := "[truncated: 10 more changed files; narrow with a path filter]"
	if !strings.Contains(out, notice) {
		t.Errorf("output missing truncation notice %q:\n%.300s", notice, out)
	}
}

func TestRenderDiffFilesSingularTruncationNotice(t *testing.T) {
	d := &githistory.Diff{}
	for i := range statLineCap + 1 {
		d.Files = append(d.Files, githistory.FileDiff{
			Path:     fmt.Sprintf("file%03d.txt", i),
			StatLine: fmt.Sprintf("file%03d.txt | +1 -0", i),
		})
	}
	var b strings.Builder
	renderDiffFiles(&b, d)
	notice := "[truncated: 1 more changed file; narrow with a path filter]"
	if !strings.Contains(b.String(), notice) {
		t.Errorf("output missing singular truncation notice %q", notice)
	}
}

func TestRenderDiffFilesUnderCap(t *testing.T) {
	d := &githistory.Diff{
		Files:        []githistory.FileDiff{{Path: "a.txt", StatLine: "a.txt | +1 -0"}},
		OmittedStats: []string{"b.dat | binary"},
	}
	var b strings.Builder
	renderDiffFiles(&b, d)
	out := b.String()
	if strings.Contains(out, "[truncated") {
		t.Errorf("under-cap output has unexpected truncation notice:\n%s", out)
	}
	for _, want := range []string{"a.txt | +1 -0", "[omitted: patches withheld", "b.dat | binary"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestGetDiffMergeCommit(t *testing.T) {
	f := newHistoryFixture(t)
	patch := true
	out, err := f.srv.GetDiff(context.Background(), GetDiffArgs{Repo: "acme/widget", Rev: f.merge, Patch: &patch})
	if err != nil {
		t.Fatalf("GetDiff merge: %v", err)
	}
	// The merge is diffed against its first parent, so the side-branch
	// change to foo.go is visible, never an empty combined diff.
	if !strings.Contains(out, "--- foo.go ---") {
		t.Errorf("merge diff missing foo.go section:\n%s", out)
	}
	if !strings.Contains(out, "+// Foo returns one.") {
		t.Errorf("merge diff missing the side-branch change:\n%s", out)
	}
	if !strings.Contains(out, "merge feature into main") {
		t.Errorf("merge diff missing commit message:\n%s", out)
	}
}

func TestGetDiffDefaultsToIndexedCommit(t *testing.T) {
	f := newHistoryFixture(t)
	out, err := f.srv.GetDiff(context.Background(), GetDiffArgs{Repo: "acme/widget"})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if !strings.Contains(out, "commit "+shortSHA(f.commit)) {
		t.Errorf("output missing indexed-commit header:\n%s", out)
	}
	// Single-rev default is stat-only.
	if !strings.Contains(out, "baz.txt | +1 -0") {
		t.Errorf("output missing stat line:\n%s", out)
	}
}

func TestGetDiffDescendantWarning(t *testing.T) {
	f := newHistoryFixture(t)
	// rev is an ancestor of base, so the three-dot diff is empty; the
	// header must warn instead of silently reporting no changes.
	out, err := f.srv.GetDiff(context.Background(), GetDiffArgs{Repo: "acme/widget", Base: f.post, Rev: f.root})
	if err != nil {
		t.Fatalf("GetDiff descendant: %v", err)
	}
	if !strings.Contains(out, "Warning:") {
		t.Errorf("output missing warning header:\n%s", out)
	}
	if !strings.Contains(out, "swapped") {
		t.Errorf("warning missing the swapped-arguments hint:\n%s", out)
	}
}

func TestGetDiffTwoRevHeader(t *testing.T) {
	f := newHistoryFixture(t)
	// A branch-name base must render its resolved short SHA in the
	// header, not just the caller's spelling.
	out, err := f.srv.GetDiff(context.Background(), GetDiffArgs{Repo: "acme/widget", Base: "feature", Rev: f.post})
	if err != nil {
		t.Fatalf("GetDiff two-rev: %v", err)
	}
	want := fmt.Sprintf("diff from feature (%s) to %s", shortSHA(f.side), shortSHA(f.post))
	if !strings.Contains(out, want) {
		t.Errorf("output missing endpoints header %q:\n%s", want, out)
	}
	if !strings.Contains(out, "merge-base: "+shortSHA(f.side)) {
		t.Errorf("output missing merge-base header:\n%s", out)
	}
	if !strings.Contains(out, "--- baz.txt ---") {
		t.Errorf("two-rev diff missing baz.txt patch:\n%s", out)
	}
}

func TestHistoryDefaultRevIndexMismatch(t *testing.T) {
	// A status file naming a commit absent from the mirror means the
	// index and mirror have diverged: every history tool that defaults
	// rev to the indexed commit must answer with ErrIndexMismatch (which
	// names `muninn sync`), never unknown-rev.
	f := newHistoryFixture(t)
	writeStatus(t, f.statusPath, strings.Repeat("0", 40), time.Now())
	ctx := context.Background()

	for name, call := range map[string]func() error{
		"search_commits": func() error {
			_, err := f.srv.SearchCommits(ctx, SearchCommitsArgs{Repo: "acme/widget"})
			return err
		},
		"get_diff": func() error {
			_, err := f.srv.GetDiff(ctx, GetDiffArgs{Repo: "acme/widget"})
			return err
		},
		"blame": func() error {
			_, err := f.srv.Blame(ctx, BlameArgs{Repo: "acme/widget", Path: "foo.go"})
			return err
		},
	} {
		err := call()
		if err == nil {
			t.Errorf("%s: err = nil, want index-mismatch error", name)
			continue
		}
		if !errors.Is(err, gitfile.ErrIndexMismatch) {
			t.Errorf("%s err = %v, want ErrIndexMismatch", name, err)
		}
		if !strings.Contains(err.Error(), "muninn sync") {
			t.Errorf("%s err = %q, want mention of `muninn sync`", name, err)
		}
	}
}

func TestBlameAgreesWithReadFile(t *testing.T) {
	f := newHistoryFixture(t)
	blameOut, err := f.srv.Blame(context.Background(), BlameArgs{Repo: "acme/widget", Path: "foo.go"})
	if err != nil {
		t.Fatalf("Blame: %v", err)
	}
	readOut, err := f.srv.ReadFile(context.Background(), ReadFileArgs{Repo: "acme/widget", Path: "foo.go"})
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Both outputs carry one header line, then the file's lines in order;
	// blame's "N: sha date author | content" must agree line-for-line
	// (number and content) with read_file at the indexed commit.
	blameLines := strings.Split(strings.TrimSuffix(blameOut, "\n"), "\n")[1:]
	readLines := strings.Split(strings.TrimSuffix(readOut, "\n"), "\n")[1:]
	if len(blameLines) != len(readLines) {
		t.Fatalf("blame lines = %d, read_file lines = %d", len(blameLines), len(readLines))
	}
	for i, bl := range blameLines {
		num, rest, ok := strings.Cut(bl, ": ")
		if !ok {
			t.Fatalf("malformed blame line %q", bl)
		}
		if want := fmt.Sprintf("%d", i+1); num != want {
			t.Errorf("blame line %d numbered %s, want %s", i, num, want)
		}
		_, content, ok := strings.Cut(rest, " | ")
		if !ok {
			t.Fatalf("malformed blame line %q", bl)
		}
		if content != readLines[i] {
			t.Errorf("blame line %d content = %q, read_file = %q", i+1, content, readLines[i])
		}
	}
}

func TestBlameLineCap(t *testing.T) {
	f := newHistoryFixture(t)
	out, err := f.srv.Blame(context.Background(), BlameArgs{Repo: "acme/widget", Path: "many.txt"})
	if err != nil {
		t.Fatalf("Blame: %v", err)
	}
	notice := fmt.Sprintf("[truncated: %d more lines; use start_line/end_line to blame a range]", manyLines-defaultBlameLines)
	if !strings.Contains(out, notice) {
		t.Errorf("output missing truncation notice %q:\n%.300s", notice, out)
	}
	if strings.Contains(out, fmt.Sprintf("| many line %d", defaultBlameLines+1)) {
		t.Error("output includes lines past the cap")
	}

	// An explicit range past the default cap is honored up to the max.
	out, err = f.srv.Blame(context.Background(), BlameArgs{
		Repo: "acme/widget", Path: "many.txt", StartLine: 1, EndLine: defaultBlameLines + 10,
	})
	if err != nil {
		t.Fatalf("Blame ranged: %v", err)
	}
	if !strings.Contains(out, fmt.Sprintf("| many line %d", defaultBlameLines+10)) {
		t.Errorf("ranged output missing requested line %d:\n%.300s", defaultBlameLines+10, out)
	}
	if strings.Contains(out, "[truncated") {
		t.Errorf("ranged output has unexpected truncation notice:\n%.300s", out)
	}
}

func TestBlameRange(t *testing.T) {
	f := newHistoryFixture(t)
	out, err := f.srv.Blame(context.Background(), BlameArgs{
		Repo: "acme/widget", Path: "foo.go", StartLine: 3, EndLine: 3,
	})
	if err != nil {
		t.Fatalf("Blame range: %v", err)
	}
	if !strings.Contains(out, "3: ") || !strings.Contains(out, "| // Foo returns one.") {
		t.Errorf("range output missing line 3:\n%s", out)
	}
	if strings.Contains(out, "1: ") {
		t.Errorf("range output includes lines outside the range:\n%s", out)
	}
}

func TestBlameUnknownPath(t *testing.T) {
	f := newHistoryFixture(t)
	_, err := f.srv.Blame(context.Background(), BlameArgs{Repo: "acme/widget", Path: "nope.go"})
	if err == nil {
		t.Fatal("Blame: err = nil, want unknown-path error")
	}
	if !strings.Contains(err.Error(), "path not found at revision") {
		t.Errorf("err = %q, want unknown-path error", err)
	}
	// The clean classified error must not leak the mirror path or argv.
	for _, leak := range []string{"mirrors", "git -C", "exit status", "stderr"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("err = %q, must not leak %q", err, leak)
		}
	}
}

// warningLine extracts the staleness warning line from tool output, or ""
// when there is none.
func warningLine(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "WARNING:") {
			return l
		}
	}
	return ""
}

func TestHistoryStalenessParity(t *testing.T) {
	// The full fixture (with a searcher) so list_repos works too; then age
	// the status file past staleAfter.
	f := newFixture(t, "")
	writeStatus(t, f.statusPath, f.commit, time.Now().Add(-2*staleAfter))

	ctx := context.Background()
	reposOut, err := f.srv.ListRepos(ctx, ListReposArgs{})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	want := warningLine(reposOut)
	if want == "" {
		t.Fatalf("list_repos output missing staleness warning:\n%s", reposOut)
	}

	commitsOut, err := f.srv.SearchCommits(ctx, SearchCommitsArgs{Repo: "acme/widget"})
	if err != nil {
		t.Fatalf("SearchCommits: %v", err)
	}
	diffOut, err := f.srv.GetDiff(ctx, GetDiffArgs{Repo: "acme/widget"})
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	blameOut, err := f.srv.Blame(ctx, BlameArgs{Repo: "acme/widget", Path: "widget.go"})
	if err != nil {
		t.Fatalf("Blame: %v", err)
	}
	for name, out := range map[string]string{
		"search_commits": commitsOut,
		"get_diff":       diffOut,
		"blame":          blameOut,
	} {
		if got := warningLine(out); got != want {
			t.Errorf("%s staleness warning = %q, want the list_repos wording %q", name, got, want)
		}
	}
}

func TestHistoryNoStalenessWarningWhenFresh(t *testing.T) {
	f := newHistoryFixture(t)
	out, err := f.srv.SearchCommits(context.Background(), SearchCommitsArgs{Repo: "acme/widget"})
	if err != nil {
		t.Fatalf("SearchCommits: %v", err)
	}
	if got := warningLine(out); got != "" {
		t.Errorf("fresh index produced staleness warning %q", got)
	}
}
