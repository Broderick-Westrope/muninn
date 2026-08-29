package githistory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/broderick-westrope/muninn/internal/gitfile"
)

// boolPtr returns a pointer to b for optional flag fields.
func boolPtr(b bool) *bool { return &b }

func TestSearchCommitsFirstParent(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)
	ctx := context.Background()

	// Default (first-parent): the pickaxe hit for Foo's change is the
	// merge M, annotated as a merge; the underlying commit C is hidden.
	commits, truncated, timedOut, err := SearchCommits(ctx, f.Mirror, LogOptions{
		Rev:            "main",
		ChangedLiteral: "Foo",
	})
	require.NoError(t, err)
	require.False(t, truncated)
	require.False(t, timedOut)
	require.Equal(t, []string{f.Merge, f.Root}, shas(commits))
	require.True(t, commits[0].IsMerge)
	require.False(t, commits[1].IsMerge)

	// first_parent: false surfaces the underlying commit C instead of M.
	commits, _, _, err = SearchCommits(ctx, f.Mirror, LogOptions{
		Rev:            "main",
		ChangedLiteral: "Foo",
		FirstParent:    boolPtr(false),
	})
	require.NoError(t, err)
	require.Equal(t, []string{f.SideChange, f.Root}, shas(commits))
}

func TestSearchCommitsPickaxeIntroduceRemove(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	commits, _, _, err := SearchCommits(context.Background(), f.Mirror, LogOptions{
		Rev:            "main",
		ChangedLiteral: "MAGIC",
	})
	require.NoError(t, err)
	require.Equal(t, []string{f.MagicRemove, f.MagicAdd}, shas(commits))
}

func TestSearchCommitsChangedRegex(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	commits, _, _, err := SearchCommits(context.Background(), f.Mirror, LogOptions{
		Rev:          "main",
		ChangedRegex: `return "(one|two)"`,
	})
	require.NoError(t, err)
	// The merge's first-parent diff only adds a comment, so -G on the
	// return line matches the root and lockfile commits only.
	require.Equal(t, []string{f.Lockfile, f.Root}, shas(commits))
}

func TestSearchCommitsFollowRename(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	// --follow walks past the bar.txt -> baz.txt rename to the original
	// creation in the root commit.
	commits, _, _, err := SearchCommits(context.Background(), f.Mirror, LogOptions{
		Rev:  "main",
		Path: "baz.txt",
	})
	require.NoError(t, err)
	require.Equal(t, []string{f.PostRename, f.Rename, f.BarV2, f.Root}, shas(commits))
}

func TestSearchCommitsParsesRootAndTabSubject(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	commits, truncated, timedOut, err := SearchCommits(context.Background(), f.Mirror, LogOptions{
		Rev:   "main",
		Limit: 100,
	})
	require.NoError(t, err)
	require.False(t, truncated)
	require.False(t, timedOut)

	bySHA := make(map[string]Commit, len(commits))
	for _, c := range commits {
		bySHA[c.SHA] = c
	}

	root := bySHA[f.Root]
	require.Equal(t, "root: add foo and bar", root.Subject)
	require.False(t, root.IsMerge, "root commit (empty %%P) must parse as non-merge")
	require.Equal(t, "2024-01-01", root.AuthorDate)
	require.Equal(t, "test", root.Author)

	require.Equal(t, tabSubject, bySHA[f.TabSubject].Subject, "tab-containing subject must survive parsing")
	require.True(t, bySHA[f.Merge].IsMerge)
}

func TestSearchCommitsAuthorAndMessage(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)
	ctx := context.Background()

	commits, _, _, err := SearchCommits(ctx, f.Mirror, LogOptions{Rev: "main", Author: "Alice"})
	require.NoError(t, err)
	require.Equal(t, []string{f.BarV2}, shas(commits))
	require.Equal(t, "Alice", commits[0].Author)

	commits, _, _, err = SearchCommits(ctx, f.Mirror, LogOptions{Rev: "main", Message: "extend baz"})
	require.NoError(t, err)
	require.Equal(t, []string{f.PostRename}, shas(commits))
}

func TestSearchCommitsSinceUntil(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	// Fixture commits carry deterministic committer dates; day 2 is the
	// tab-subject commit.
	commits, _, _, err := SearchCommits(context.Background(), f.Mirror, LogOptions{
		Rev:   "main",
		Since: "2024-01-02 00:00 +0000",
		Until: "2024-01-02 23:59 +0000",
	})
	require.NoError(t, err)
	require.Equal(t, []string{f.TabSubject}, shas(commits))
}

func TestSearchCommitsLimitTruncated(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	commits, truncated, _, err := SearchCommits(context.Background(), f.Mirror, LogOptions{
		Rev:   "main",
		Limit: 2,
	})
	require.NoError(t, err)
	require.True(t, truncated)
	require.Len(t, commits, 2)
	require.Equal(t, f.Head, commits[0].SHA)
}

func TestSearchCommitsValidation(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)
	ctx := context.Background()

	_, _, _, err := SearchCommits(ctx, f.Mirror, LogOptions{
		Rev:            "main",
		ChangedLiteral: "a",
		ChangedRegex:   "b",
	})
	require.ErrorContains(t, err, "mutually exclusive")

	_, _, _, err = SearchCommits(ctx, f.Mirror, LogOptions{Rev: "main", Path: "--all"})
	require.ErrorContains(t, err, "must not start with")

	_, _, _, err = SearchCommits(ctx, f.Mirror, LogOptions{Rev: "main", Path: ":(glob)*.go"})
	require.ErrorContains(t, err, "must not start with")

	_, _, _, err = SearchCommits(ctx, f.Mirror, LogOptions{Rev: "no-such-branch"})
	require.ErrorIs(t, err, gitfile.ErrUnknownRev)
}

// fakeGit installs a fake git shell script at the front of PATH via
// t.Setenv, so the calling test must not use t.Parallel.
func fakeGit(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestSearchCommitsTimeoutPartial(t *testing.T) {
	// Uses t.Setenv (via fakeGit), so no t.Parallel. The fake git answers
	// rev-parse (ResolveRev) instantly, emits two complete log lines plus
	// an unterminated fragment, then hangs; the caller deadline (sooner
	// than logTimeout) kills it and the complete lines survive as labeled
	// partial results.
	fakeGit(t, `#!/bin/sh
for a in "$@"; do
  if [ "$a" = "rev-parse" ]; then
    echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    exit 0
  fi
done
printf 'sha1\t2024-01-01\talice\t\tone\n'
printf 'sha2\t2024-01-02\tbob\tp1 p2\tsubject\twith tab\n'
printf 'sha3\t2024-01-03\tcarol'
exec sleep 10
`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	commits, truncated, timedOut, err := SearchCommits(ctx, t.TempDir(), LogOptions{Rev: "main"})
	require.NoError(t, err)
	require.True(t, timedOut)
	require.False(t, truncated)
	require.Equal(t, []string{"sha1", "sha2"}, shas(commits))
	require.True(t, commits[1].IsMerge)
	require.Equal(t, "subject\twith tab", commits[1].Subject)
}
