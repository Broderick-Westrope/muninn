package githistory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/broderick-westrope/muninn/internal/gitfile"
)

func TestBlameAgreesWithReadFile(t *testing.T) {
	f := newFixtureRepo(t)
	ctx := context.Background()

	lines, err := Blame(ctx, f.Mirror, BlameOptions{Rev: "main", Path: "doc.txt"})
	if err != nil {
		t.Fatalf("Blame: %v", err)
	}

	content, totalLines, err := gitfile.ReadFile(ctx, f.Mirror, f.Head, "doc.txt", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(lines) != totalLines {
		t.Fatalf("blame lines = %d, want %d", len(lines), totalLines)
	}

	want := strings.SplitAfter(content, "\n")
	for i, line := range lines {
		if line.Line != i+1 {
			t.Fatalf("line %d number = %d, want %d (blame line numbers must be sequential from 1)", i, line.Line, i+1)
		}
		if got := strings.TrimSuffix(want[i], "\n"); line.Content != got {
			t.Fatalf("line %d content = %q, want %q", i+1, line.Content, got)
		}
	}

	// Line attribution: line 1 came with the root commit, line 2 with
	// the tab-subject commit.
	if lines[0].SHA != f.Root {
		t.Fatalf("line 1 SHA = %q, want root %q", lines[0].SHA, f.Root)
	}
	if lines[1].SHA != f.TabSubject {
		t.Fatalf("line 2 SHA = %q, want tab-subject %q", lines[1].SHA, f.TabSubject)
	}
	if lines[0].AuthorDate != "2024-01-01" {
		t.Fatalf("line 1 AuthorDate = %q, want %q", lines[0].AuthorDate, "2024-01-01")
	}
	if lines[1].AuthorDate != "2024-01-02" {
		t.Fatalf("line 2 AuthorDate = %q, want %q", lines[1].AuthorDate, "2024-01-02")
	}
	if lines[0].Author != "test" {
		t.Fatalf("line 1 Author = %q, want %q", lines[0].Author, "test")
	}
}

func TestBlameLineRange(t *testing.T) {
	f := newFixtureRepo(t)

	lines, err := Blame(context.Background(), f.Mirror, BlameOptions{
		Rev:       "main",
		Path:      "doc.txt",
		StartLine: 2,
		EndLine:   2,
	})
	if err != nil {
		t.Fatalf("Blame: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("len(lines) = %d, want 1", len(lines))
	}
	if lines[0].Line != 2 {
		t.Fatalf("Line = %d, want 2", lines[0].Line)
	}
	if lines[0].Content != "more" {
		t.Fatalf("Content = %q, want %q", lines[0].Content, "more")
	}
	if lines[0].SHA != f.TabSubject {
		t.Fatalf("SHA = %q, want %q", lines[0].SHA, f.TabSubject)
	}
}

func TestBlameOpenEndedLineRange(t *testing.T) {
	f := newFixtureRepo(t)

	// StartLine without EndLine blames from that line to EOF.
	lines, err := Blame(context.Background(), f.Mirror, BlameOptions{
		Rev:       "main",
		Path:      "doc.txt",
		StartLine: 2,
	})
	if err != nil {
		t.Fatalf("Blame: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("len(lines) = %d, want 1", len(lines))
	}
	if lines[0].Line != 2 {
		t.Fatalf("Line = %d, want 2", lines[0].Line)
	}
	if lines[0].Content != "more" {
		t.Fatalf("Content = %q, want %q", lines[0].Content, "more")
	}
	if lines[0].SHA != f.TabSubject {
		t.Fatalf("SHA = %q, want %q", lines[0].SHA, f.TabSubject)
	}
}

func TestBlameOlderRevDiffers(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Blame at tip: %v", err)
	}
	tipLine := byContent(tip, "return \"")
	if tipLine.SHA != f.Lockfile {
		t.Fatalf("tip line SHA = %q, want lockfile %q", tipLine.SHA, f.Lockfile)
	}
	if tipLine.Content != "\treturn \"two\"" {
		t.Fatalf("tip line Content = %q, want %q", tipLine.Content, "\treturn \"two\"")
	}

	old, err := Blame(ctx, f.Mirror, BlameOptions{Rev: f.Merge, Path: "foo.go"})
	if err != nil {
		t.Fatalf("Blame at merge: %v", err)
	}
	oldLine := byContent(old, "return \"")
	if oldLine.SHA != f.Root {
		t.Fatalf("old line SHA = %q, want root %q", oldLine.SHA, f.Root)
	}
	if oldLine.Content != "\treturn \"one\"" {
		t.Fatalf("old line Content = %q, want %q", oldLine.Content, "\treturn \"one\"")
	}
}

func TestBlameMissingPath(t *testing.T) {
	f := newFixtureRepo(t)

	_, err := Blame(context.Background(), f.Mirror, BlameOptions{Rev: "main", Path: "nope.txt"})
	if !errors.Is(err, gitfile.ErrUnknownPath) {
		t.Fatalf("error = %v, want ErrUnknownPath", err)
	}
}

func TestBlameRangePastEOF(t *testing.T) {
	f := newFixtureRepo(t)

	_, err := Blame(context.Background(), f.Mirror, BlameOptions{
		Rev:       "main",
		Path:      "doc.txt",
		StartLine: 100,
		EndLine:   200,
	})
	if err == nil {
		t.Fatal("expected error for range past EOF")
	}
	if !strings.Contains(err.Error(), "has only 2 lines") {
		t.Fatalf("error = %q, want to contain %q (git's diagnostic must surface clearly)", err, "has only 2 lines")
	}
}

func TestBlameValidation(t *testing.T) {
	f := newFixtureRepo(t)
	ctx := context.Background()

	_, err := Blame(ctx, f.Mirror, BlameOptions{Rev: "main"})
	if err == nil || !strings.Contains(err.Error(), "requires a path") {
		t.Fatalf("error = %v, want to contain %q", err, "requires a path")
	}

	_, err = Blame(ctx, f.Mirror, BlameOptions{Rev: "main", Path: "-L1,2"})
	if err == nil || !strings.Contains(err.Error(), "must not start with") {
		t.Fatalf("error = %v, want to contain %q", err, "must not start with")
	}

	_, err = Blame(ctx, f.Mirror, BlameOptions{Rev: "main", Path: "doc.txt", StartLine: 3, EndLine: 2})
	if err == nil || !strings.Contains(err.Error(), "after end_line") {
		t.Fatalf("error = %v, want to contain %q", err, "after end_line")
	}

	_, err = Blame(ctx, f.Mirror, BlameOptions{Rev: "no-such-branch", Path: "doc.txt"})
	if !errors.Is(err, gitfile.ErrUnknownRev) {
		t.Fatalf("error = %v, want ErrUnknownRev", err)
	}
}
