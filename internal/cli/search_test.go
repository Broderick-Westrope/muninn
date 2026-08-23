package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/broderick-westrope/muninn/internal/search"
)

func fixtureResult() *search.Result {
	return &search.Result{
		Files: []search.FileMatches{
			{
				Repo: "acme/api",
				Path: "main.go",
				Lines: []search.LineMatch{
					{LineNumber: 3, Line: "func main() {"},
					{LineNumber: 10, Line: "\tmain2()"},
				},
			},
			{
				Repo: "acme/web",
				Path: "cmd/main.go",
				Lines: []search.LineMatch{
					{LineNumber: 1, Line: "package main"},
				},
			},
		},
		Stats: search.Stats{FilesConsidered: 5, MatchCount: 3, Duration: 2 * time.Millisecond},
	}
}

func TestFormatMatchesPlain(t *testing.T) {
	out := formatMatches(fixtureResult(), 50, false)

	want := "acme/api/main.go:3: func main() {\n" +
		"acme/api/main.go:10: \tmain2()\n" +
		"acme/web/cmd/main.go:1: package main\n"
	if out.body != want {
		t.Errorf("body = %q, want %q", out.body, want)
	}
	if out.shown != 3 || out.files != 2 || out.truncated {
		t.Errorf("shown=%d files=%d truncated=%v, want 3, 2, false", out.shown, out.files, out.truncated)
	}
}

func TestFormatMatchesColored(t *testing.T) {
	out := formatMatches(fixtureResult(), 50, true)

	wantFirst := ansiMagenta + "acme/api/main.go" + ansiReset + ":" + ansiGreen + "3" + ansiReset + ": func main() {\n"
	if !strings.HasPrefix(out.body, wantFirst) {
		t.Errorf("body does not start with colored line:\ngot  %q\nwant %q", out.body, wantFirst)
	}
	// Content must stay uncolored: no escape code after the ': ' separator.
	firstLine, _, _ := strings.Cut(out.body, "\n")
	if strings.Contains(strings.SplitN(firstLine, ": ", 2)[1], "\x1b") {
		t.Errorf("content is colored: %q", firstLine)
	}
}

func TestFormatMatchesTruncation(t *testing.T) {
	out := formatMatches(fixtureResult(), 2, false)

	if out.shown != 2 {
		t.Errorf("shown = %d, want 2", out.shown)
	}
	if !out.truncated {
		t.Error("truncated = false, want true")
	}
	if strings.Contains(out.body, "package main") {
		t.Errorf("body includes match beyond limit:\n%s", out.body)
	}
}

func TestFormatMatchesFilenameOnly(t *testing.T) {
	res := &search.Result{
		Files: []search.FileMatches{{Repo: "acme/api", Path: "README.md"}},
	}
	out := formatMatches(res, 50, false)

	want := "acme/api/README.md (filename match)\n"
	if out.body != want {
		t.Errorf("body = %q, want %q", out.body, want)
	}
	if out.shown != 1 {
		t.Errorf("shown = %d, want 1", out.shown)
	}
}

func TestFormatFilesOnlyDedup(t *testing.T) {
	res := fixtureResult()
	// Duplicate file entry: must be deduped.
	res.Files = append(res.Files, search.FileMatches{
		Repo:  "acme/api",
		Path:  "main.go",
		Lines: []search.LineMatch{{LineNumber: 20, Line: "x"}},
	})
	out := formatFilesOnly(res, 50, false)

	want := "acme/api/main.go\nacme/web/cmd/main.go\n"
	if out.body != want {
		t.Errorf("body = %q, want %q", out.body, want)
	}
	if out.shown != 2 || out.files != 2 || out.truncated {
		t.Errorf("shown=%d files=%d truncated=%v, want 2, 2, false", out.shown, out.files, out.truncated)
	}
}

func TestFormatFilesOnlyColored(t *testing.T) {
	out := formatFilesOnly(fixtureResult(), 50, true)

	want := ansiMagenta + "acme/api/main.go" + ansiReset + "\n" +
		ansiMagenta + "acme/web/cmd/main.go" + ansiReset + "\n"
	if out.body != want {
		t.Errorf("body = %q, want %q", out.body, want)
	}
}

func TestFormatFilesOnlyTruncation(t *testing.T) {
	out := formatFilesOnly(fixtureResult(), 1, false)

	if out.shown != 1 || !out.truncated {
		t.Errorf("shown=%d truncated=%v, want 1, true", out.shown, out.truncated)
	}
}

func TestFormatMatchesEmpty(t *testing.T) {
	out := formatMatches(&search.Result{}, 50, false)
	if out.body != "" || out.shown != 0 {
		t.Errorf("body=%q shown=%d, want empty, 0", out.body, out.shown)
	}
}
