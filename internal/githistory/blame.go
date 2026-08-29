package githistory

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/broderick-westrope/muninn/internal/gitcmd"
	"github.com/broderick-westrope/muninn/internal/gitfile"
)

// BlameOptions selects what Blame attributes.
type BlameOptions struct {
	// Rev is the revision to blame at; it must resolve in the mirror.
	Rev string
	// Path is the literal file path to blame.
	Path string
	// StartLine and EndLine map to -L; 0 means unbounded on that side.
	StartLine int
	// EndLine is the last line to blame (inclusive).
	EndLine int
}

// BlameLine is one attributed line of Blame output.
type BlameLine struct {
	// Line is the 1-based line number at the blamed revision.
	Line int
	// SHA is the full SHA of the commit that introduced the line.
	SHA string
	// AuthorDate is the author date in YYYY-MM-DD form (author-local).
	AuthorDate string
	// Author is the author name.
	Author string
	// Content is the line's content without its terminator.
	Content string
}

// Blame attributes each line of path at rev in the bare mirror at
// mirrorDir. Line numbers are those of the file at rev, so they agree
// exactly with ReadFile at the same rev. A missing path yields an error
// wrapping gitfile.ErrUnknownPath.
func Blame(ctx context.Context, mirrorDir string, opts BlameOptions) ([]BlameLine, error) {
	if opts.Path == "" {
		return nil, errors.New("blame requires a path")
	}
	if err := validatePath(opts.Path); err != nil {
		return nil, err
	}
	if opts.EndLine > 0 && opts.StartLine > opts.EndLine {
		return nil, fmt.Errorf("start_line %d is after end_line %d", opts.StartLine, opts.EndLine)
	}
	sha, err := gitfile.ResolveRev(ctx, mirrorDir, opts.Rev)
	if err != nil {
		return nil, err
	}

	args := []string{"-C", mirrorDir, "blame", "--line-porcelain"}
	if opts.StartLine > 0 || opts.EndLine > 0 {
		start := max(opts.StartLine, 1)
		end := ""
		if opts.EndLine > 0 {
			end = strconv.Itoa(opts.EndLine)
		}
		args = append(args, fmt.Sprintf("-L%d,%s", start, end))
	}
	// Unlike log/diff, blame treats everything after --end-of-options as
	// pathspec, so the marker must follow the (already resolved, hex-only)
	// rev rather than precede it.
	args = append(args, sha, "--end-of-options", "--", opts.Path)

	out, err := runner(blameTimeout).RunRaw(ctx, args...)
	if err != nil {
		if errors.Is(err, gitcmd.ErrTimeout) {
			return nil, fmt.Errorf("blame of %q at %s timed out; narrow with a line range (start_line/end_line) or blame a smaller file: %w", opts.Path, shortSHA(sha), err)
		}
		if errors.Is(gitfile.ClassifyPathErr(err), gitfile.ErrUnknownPath) {
			// The classification is unambiguous, so drop the raw gitcmd
			// error: its text carries the mirror path and full argv,
			// which must not leak to the caller.
			return nil, fmt.Errorf("path %q not found at rev %s: %w", opts.Path, shortSHA(sha), gitfile.ErrUnknownPath)
		}
		// Anything else (such as -L past EOF, git exit 128) surfaces
		// verbatim — the gitcmd error carries git's stderr diagnostic.
		return nil, fmt.Errorf("blaming %q at %s: %w", opts.Path, shortSHA(sha), err)
	}
	return parsePorcelain(out)
}

// parsePorcelain parses git blame --line-porcelain output. Each record is
// a header line "<sha> <origLine> <finalLine> [<numLines>]", a run of tag
// lines (author, author-time, author-tz, ...), and a tab-prefixed content
// line that closes the record.
func parsePorcelain(out string) ([]BlameLine, error) {
	var lines []BlameLine
	var cur *BlameLine
	var tzOffset time.Duration
	var authorTime int64
	for raw := range strings.Lines(out) {
		line := strings.TrimSuffix(raw, "\n")
		if cur == nil {
			if line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 3 {
				return nil, fmt.Errorf("malformed blame header %q", line)
			}
			final, err := strconv.Atoi(fields[2])
			if err != nil {
				return nil, fmt.Errorf("malformed blame header %q: %w", line, err)
			}
			cur = &BlameLine{SHA: fields[0], Line: final}
			tzOffset, authorTime = 0, 0
			continue
		}
		if content, ok := strings.CutPrefix(line, "\t"); ok {
			if authorTime != 0 {
				cur.AuthorDate = time.Unix(authorTime, 0).UTC().Add(tzOffset).Format("2006-01-02")
			}
			cur.Content = content
			lines = append(lines, *cur)
			cur = nil
			continue
		}
		switch key, value, _ := strings.Cut(line, " "); key {
		case "author":
			cur.Author = value
		case "author-time":
			t, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("malformed author-time %q: %w", line, err)
			}
			authorTime = t
		case "author-tz":
			tzOffset = parseTZOffset(value)
		}
	}
	if cur != nil {
		return nil, errors.New("truncated blame output: record without a content line")
	}
	return lines, nil
}

// parseTZOffset converts a git ±HHMM timezone token to a duration, so
// author dates render in the author's local day (matching git %as). An
// unparseable token means UTC.
func parseTZOffset(tz string) time.Duration {
	if len(tz) != 5 || (tz[0] != '+' && tz[0] != '-') {
		return 0
	}
	hours, err1 := strconv.Atoi(tz[1:3])
	mins, err2 := strconv.Atoi(tz[3:5])
	if err1 != nil || err2 != nil {
		return 0
	}
	offset := time.Duration(hours)*time.Hour + time.Duration(mins)*time.Minute
	if tz[0] == '-' {
		offset = -offset
	}
	return offset
}
