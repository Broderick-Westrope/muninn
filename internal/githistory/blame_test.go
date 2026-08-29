package githistory

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/broderick-westrope/muninn/internal/gitfile"
)

func TestBlameAgreesWithReadFile(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)
	ctx := context.Background()

	lines, err := Blame(ctx, f.Mirror, BlameOptions{Rev: "main", Path: "doc.txt"})
	require.NoError(t, err)

	content, totalLines, err := gitfile.ReadFile(ctx, f.Mirror, f.Head, "doc.txt", 0, 0)
	require.NoError(t, err)
	require.Len(t, lines, totalLines)

	want := strings.SplitAfter(content, "\n")
	for i, line := range lines {
		require.Equal(t, i+1, line.Line, "blame line numbers must be sequential from 1")
		require.Equal(t, strings.TrimSuffix(want[i], "\n"), line.Content)
	}

	// Line attribution: line 1 came with the root commit, line 2 with
	// the tab-subject commit.
	require.Equal(t, f.Root, lines[0].SHA)
	require.Equal(t, f.TabSubject, lines[1].SHA)
	require.Equal(t, "2024-01-01", lines[0].AuthorDate)
	require.Equal(t, "2024-01-02", lines[1].AuthorDate)
	require.Equal(t, "test", lines[0].Author)
}

func TestBlameLineRange(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	lines, err := Blame(context.Background(), f.Mirror, BlameOptions{
		Rev:       "main",
		Path:      "doc.txt",
		StartLine: 2,
		EndLine:   2,
	})
	require.NoError(t, err)
	require.Len(t, lines, 1)
	require.Equal(t, 2, lines[0].Line)
	require.Equal(t, "more", lines[0].Content)
	require.Equal(t, f.TabSubject, lines[0].SHA)
}

func TestBlameOlderRevDiffers(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)
	ctx := context.Background()

	// At the tip, foo.go's return line was rewritten by the lockfile
	// commit; at the merge it still carries the root commit's version.
	byContent := func(lines []BlameLine, substr string) BlameLine {
		for _, l := range lines {
			if strings.Contains(l.Content, substr) {
				return l
			}
		}
		t.Fatalf("no line containing %q", substr)
		return BlameLine{}
	}

	tip, err := Blame(ctx, f.Mirror, BlameOptions{Rev: "main", Path: "foo.go"})
	require.NoError(t, err)
	tipLine := byContent(tip, "return \"")
	require.Equal(t, f.Lockfile, tipLine.SHA)
	require.Equal(t, `	return "two"`, tipLine.Content)

	old, err := Blame(ctx, f.Mirror, BlameOptions{Rev: f.Merge, Path: "foo.go"})
	require.NoError(t, err)
	oldLine := byContent(old, "return \"")
	require.Equal(t, f.Root, oldLine.SHA)
	require.Equal(t, `	return "one"`, oldLine.Content)
}

func TestBlameMissingPath(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	_, err := Blame(context.Background(), f.Mirror, BlameOptions{Rev: "main", Path: "nope.txt"})
	require.ErrorIs(t, err, gitfile.ErrUnknownPath)
}

func TestBlameRangePastEOF(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)

	_, err := Blame(context.Background(), f.Mirror, BlameOptions{
		Rev:       "main",
		Path:      "doc.txt",
		StartLine: 100,
		EndLine:   200,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "has only 2 lines", "git's diagnostic must surface clearly")
}

func TestBlameValidation(t *testing.T) {
	t.Parallel()
	f := newFixtureRepo(t)
	ctx := context.Background()

	_, err := Blame(ctx, f.Mirror, BlameOptions{Rev: "main"})
	require.ErrorContains(t, err, "requires a path")

	_, err = Blame(ctx, f.Mirror, BlameOptions{Rev: "main", Path: "-L1,2"})
	require.ErrorContains(t, err, "must not start with")

	_, err = Blame(ctx, f.Mirror, BlameOptions{Rev: "main", Path: "doc.txt", StartLine: 3, EndLine: 2})
	require.ErrorContains(t, err, "after end_line")

	_, err = Blame(ctx, f.Mirror, BlameOptions{Rev: "no-such-branch", Path: "doc.txt"})
	require.ErrorIs(t, err, gitfile.ErrUnknownRev)
}
