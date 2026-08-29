package githistory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/broderick-westrope/muninn/internal/gitfile"
)

// boolPtr returns a pointer to b for optional flag fields.
func boolPtr(b bool) *bool { return &b }

func TestSearchCommitsFirstParent(t *testing.T) {
	f := newFixtureRepo(t)
	ctx := context.Background()

	// Default (first-parent): the pickaxe hit for Foo's change is the
	// merge M, annotated as a merge; the underlying commit C is hidden.
	commits, truncated, timedOut, err := SearchCommits(ctx, f.Mirror, LogOptions{
		Rev:            "main",
		ChangedLiteral: "Foo",
	})
	if err != nil {
		t.Fatalf("SearchCommits: %v", err)
	}
	if truncated {
		t.Fatal("truncated = true, want false")
	}
	if timedOut {
		t.Fatal("timedOut = true, want false")
	}
	if want := []string{f.Merge, f.Root}; !slices.Equal(shas(commits), want) {
		t.Fatalf("shas = %v, want %v", shas(commits), want)
	}
	if !commits[0].IsMerge {
		t.Fatal("commits[0].IsMerge = false, want true")
	}
	if commits[1].IsMerge {
		t.Fatal("commits[1].IsMerge = true, want false")
	}

	// first_parent: false surfaces the underlying commit C instead of M.
	commits, _, _, err = SearchCommits(ctx, f.Mirror, LogOptions{
		Rev:            "main",
		ChangedLiteral: "Foo",
		FirstParent:    boolPtr(false),
	})
	if err != nil {
		t.Fatalf("SearchCommits: %v", err)
	}
	if want := []string{f.SideChange, f.Root}; !slices.Equal(shas(commits), want) {
		t.Fatalf("shas = %v, want %v", shas(commits), want)
	}
}

func TestSearchCommitsPickaxeIntroduceRemove(t *testing.T) {
	f := newFixtureRepo(t)

	commits, _, _, err := SearchCommits(context.Background(), f.Mirror, LogOptions{
		Rev:            "main",
		ChangedLiteral: "MAGIC",
	})
	if err != nil {
		t.Fatalf("SearchCommits: %v", err)
	}
	if want := []string{f.MagicRemove, f.MagicAdd}; !slices.Equal(shas(commits), want) {
		t.Fatalf("shas = %v, want %v", shas(commits), want)
	}
}

func TestSearchCommitsChangedRegex(t *testing.T) {
	f := newFixtureRepo(t)

	commits, _, _, err := SearchCommits(context.Background(), f.Mirror, LogOptions{
		Rev:          "main",
		ChangedRegex: `return "(one|two)"`,
	})
	if err != nil {
		t.Fatalf("SearchCommits: %v", err)
	}
	// The merge's first-parent diff only adds a comment, so -G on the
	// return line matches the root and lockfile commits only.
	if want := []string{f.Lockfile, f.Root}; !slices.Equal(shas(commits), want) {
		t.Fatalf("shas = %v, want %v", shas(commits), want)
	}
}

func TestSearchCommitsFollowRename(t *testing.T) {
	f := newFixtureRepo(t)

	// --follow walks past the bar.txt -> baz.txt rename to the original
	// creation in the root commit.
	commits, _, _, err := SearchCommits(context.Background(), f.Mirror, LogOptions{
		Rev:  "main",
		Path: "baz.txt",
	})
	if err != nil {
		t.Fatalf("SearchCommits: %v", err)
	}
	if want := []string{f.PostRename, f.Rename, f.BarV2, f.Root}; !slices.Equal(shas(commits), want) {
		t.Fatalf("shas = %v, want %v", shas(commits), want)
	}
}

func TestSearchCommitsParsesRootAndTabSubject(t *testing.T) {
	f := newFixtureRepo(t)

	commits, truncated, timedOut, err := SearchCommits(context.Background(), f.Mirror, LogOptions{
		Rev:   "main",
		Limit: 100,
	})
	if err != nil {
		t.Fatalf("SearchCommits: %v", err)
	}
	if truncated {
		t.Fatal("truncated = true, want false")
	}
	if timedOut {
		t.Fatal("timedOut = true, want false")
	}

	bySHA := make(map[string]Commit, len(commits))
	for _, c := range commits {
		bySHA[c.SHA] = c
	}

	root := bySHA[f.Root]
	if root.Subject != "root: add foo and bar" {
		t.Fatalf("root Subject = %q, want %q", root.Subject, "root: add foo and bar")
	}
	if root.IsMerge {
		t.Fatal("root commit (empty %P) must parse as non-merge")
	}
	if root.AuthorDate != "2024-01-01" {
		t.Fatalf("root AuthorDate = %q, want %q", root.AuthorDate, "2024-01-01")
	}
	if root.Author != "test" {
		t.Fatalf("root Author = %q, want %q", root.Author, "test")
	}

	if got := bySHA[f.TabSubject].Subject; got != tabSubject {
		t.Fatalf("Subject = %q, want %q (tab-containing subject must survive parsing)", got, tabSubject)
	}
	if !bySHA[f.Merge].IsMerge {
		t.Fatal("merge commit IsMerge = false, want true")
	}
}

func TestSearchCommitsAuthorAndMessage(t *testing.T) {
	f := newFixtureRepo(t)
	ctx := context.Background()

	commits, _, _, err := SearchCommits(ctx, f.Mirror, LogOptions{Rev: "main", Author: "Alice"})
	if err != nil {
		t.Fatalf("SearchCommits: %v", err)
	}
	if want := []string{f.BarV2}; !slices.Equal(shas(commits), want) {
		t.Fatalf("shas = %v, want %v", shas(commits), want)
	}
	if commits[0].Author != "Alice" {
		t.Fatalf("Author = %q, want %q", commits[0].Author, "Alice")
	}

	commits, _, _, err = SearchCommits(ctx, f.Mirror, LogOptions{Rev: "main", Message: "extend baz"})
	if err != nil {
		t.Fatalf("SearchCommits: %v", err)
	}
	if want := []string{f.PostRename}; !slices.Equal(shas(commits), want) {
		t.Fatalf("shas = %v, want %v", shas(commits), want)
	}
}

func TestSearchCommitsSinceUntil(t *testing.T) {
	f := newFixtureRepo(t)

	// Fixture commits carry deterministic committer dates; day 2 is the
	// tab-subject commit.
	commits, _, _, err := SearchCommits(context.Background(), f.Mirror, LogOptions{
		Rev:   "main",
		Since: "2024-01-02 00:00 +0000",
		Until: "2024-01-02 23:59 +0000",
	})
	if err != nil {
		t.Fatalf("SearchCommits: %v", err)
	}
	if want := []string{f.TabSubject}; !slices.Equal(shas(commits), want) {
		t.Fatalf("shas = %v, want %v", shas(commits), want)
	}
}

func TestSearchCommitsLimitTruncated(t *testing.T) {
	f := newFixtureRepo(t)

	commits, truncated, _, err := SearchCommits(context.Background(), f.Mirror, LogOptions{
		Rev:   "main",
		Limit: 2,
	})
	if err != nil {
		t.Fatalf("SearchCommits: %v", err)
	}
	if !truncated {
		t.Fatal("truncated = false, want true")
	}
	if len(commits) != 2 {
		t.Fatalf("len(commits) = %d, want 2", len(commits))
	}
	if commits[0].SHA != f.Head {
		t.Fatalf("commits[0].SHA = %q, want head %q", commits[0].SHA, f.Head)
	}
}

func TestSearchCommitsValidation(t *testing.T) {
	f := newFixtureRepo(t)
	ctx := context.Background()

	_, _, _, err := SearchCommits(ctx, f.Mirror, LogOptions{
		Rev:            "main",
		ChangedLiteral: "a",
		ChangedRegex:   "b",
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v, want to contain %q", err, "mutually exclusive")
	}

	_, _, _, err = SearchCommits(ctx, f.Mirror, LogOptions{Rev: "main", Path: "--all"})
	if err == nil || !strings.Contains(err.Error(), "must not start with") {
		t.Fatalf("err = %v, want to contain %q", err, "must not start with")
	}

	_, _, _, err = SearchCommits(ctx, f.Mirror, LogOptions{Rev: "main", Path: ":(glob)*.go"})
	if err == nil || !strings.Contains(err.Error(), "must not start with") {
		t.Fatalf("err = %v, want to contain %q", err, "must not start with")
	}

	_, _, _, err = SearchCommits(ctx, f.Mirror, LogOptions{Rev: "no-such-branch"})
	if !errors.Is(err, gitfile.ErrUnknownRev) {
		t.Fatalf("err = %v, want ErrUnknownRev", err)
	}
}

// fakeGit installs a fake git shell script at the front of PATH via
// t.Setenv.
func fakeGit(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake git script: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestSearchCommitsTimeoutPartial(t *testing.T) {
	// The fake git answers rev-parse (ResolveRev) instantly, emits two
	// complete log lines plus an unterminated fragment, then hangs; the
	// caller deadline (sooner than logTimeout) kills it and the complete
	// lines survive as labeled partial results.
	fakeGit(t, `#!/bin/sh
for a in "$@"; do
  if [ "$a" = "rev-parse" ]; then
    echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    exit 0
  fi
done
printf 'sha1\t\t2024-01-01\talice\tone\n'
printf 'sha2\tp1 p2\t2024-01-02\tbob\tsubject\twith tab\n'
printf 'sha3\tp3\t2024-01-03'
exec sleep 10
`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	commits, truncated, timedOut, err := SearchCommits(ctx, t.TempDir(), LogOptions{Rev: "main"})
	if err != nil {
		t.Fatalf("SearchCommits: %v", err)
	}
	if !timedOut {
		t.Fatal("timedOut = false, want true")
	}
	if truncated {
		t.Fatal("truncated = true, want false")
	}
	if want := []string{"sha1", "sha2"}; !slices.Equal(shas(commits), want) {
		t.Fatalf("shas = %v, want %v", shas(commits), want)
	}
	if !commits[1].IsMerge {
		t.Fatal("commits[1].IsMerge = false, want true")
	}
	if commits[1].Subject != "subject\twith tab" {
		t.Fatalf("Subject = %q, want %q", commits[1].Subject, "subject\twith tab")
	}
}
