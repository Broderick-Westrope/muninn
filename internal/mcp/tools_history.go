package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/broderick-westrope/muninn/internal/gitfile"
	"github.com/broderick-westrope/muninn/internal/githistory"
	"github.com/broderick-westrope/muninn/internal/status"
)

const (
	defaultCommitsLimit = 30
	maxCommitsLimit     = 100
	defaultBlameLines   = 200
	maxBlameLines       = 500
	// statLineCap bounds the rendered stat lines across stat-only file
	// sections and the omitted block, so stat output stays capped even
	// for diffs touching hundreds of files (patches have their own byte
	// budget).
	statLineCap = 200
)

const searchCommitsDescription = `Search the commit history of one indexed repo ('repo' is required; cross-repo history search is not supported). Filters compose: 'author' (regex), 'since'/'until' (filter on commit date, though output shows author dates), 'message' (commit message regex), 'path' (a literal path, never a glob; single-path queries follow renames), and the mutually exclusive pickaxe filters 'changed_literal' (commits where the occurrence count of the literal string changed) and 'changed_regex' (commits whose diff has a hunk matching the regex; substantially slower). 'rev' is the starting point and defaults to the indexed commit. 'first_parent' defaults true so merge-heavy repos return mainline history; merge rows are annotated '(merge)'. Output lines are 'sha  date  author  subject'. Default limit 30, max 100. Git parses since/until dates leniently and unparseable values are not rejected (they may silently match nothing), so use ISO dates like 2024-01-31.`

const getDiffDescription = `Show one commit or compare two revs in an indexed repo. With 'rev' alone (default: the indexed commit): commit metadata plus per-file stats, and 'patch: true' adds the patches; merge commits are diffed against their first parent. Adding 'base' diffs from base to rev with patches on by default; 'merge_base' defaults true (three-dot semantics: only rev-side changes since the common ancestor) — set 'merge_base: false' for a literal point-to-point comparison. 'path' is a literal path filter, never a glob. Patches are truncated at file boundaries under a 64 KiB budget; files past the budget appear as stat lines, as do binary files (always) and generated/lockfile paths — 'include_generated: true' restores patches for generated/lockfile paths only, never for binary files.`

const blameDescription = `Attribute each line of a file to the commit that introduced it. 'rev' defaults to the indexed commit so line numbers agree exactly with read_file and grep. Prefer a line range ('start_line'/'end_line', both 1-based and inclusive): full-file output is capped at 200 lines (an explicit range raises the cap to 500) with a truncation notice. 'path' is a literal path. Output lines are 'line: sha date author | content'.`

// SearchCommitsArgs are the parameters of the search_commits tool.
type SearchCommitsArgs struct {
	Repo           string `json:"repo" jsonschema:"exact repo name as owner/name"`
	Author         string `json:"author,omitempty" jsonschema:"filter by author regex"`
	Since          string `json:"since,omitempty" jsonschema:"only commits after this date (filters on commit date; e.g. 2024-01-01 or '2 weeks ago')"`
	Until          string `json:"until,omitempty" jsonschema:"only commits before this date (filters on commit date)"`
	Path           string `json:"path,omitempty" jsonschema:"literal file path to restrict history to (follows renames)"`
	Message        string `json:"message,omitempty" jsonschema:"filter by commit message regex"`
	ChangedLiteral string `json:"changed_literal,omitempty" jsonschema:"commits where the occurrence count of this literal string changed (mutually exclusive with changed_regex)"`
	ChangedRegex   string `json:"changed_regex,omitempty" jsonschema:"commits whose diff has a hunk matching this regex (slower; mutually exclusive with changed_literal)"`
	Rev            string `json:"rev,omitempty" jsonschema:"starting revision (default: the indexed commit)"`
	FirstParent    *bool  `json:"first_parent,omitempty" jsonschema:"follow only first parents of merges for mainline history (default true)"`
	Limit          int    `json:"limit,omitempty" jsonschema:"max commits to return (default 30, max 100)"`
}

// SearchCommits searches one repo's commit history and renders one line
// per commit. Merge rows are always annotated; under a pickaxe filter the
// annotation adds the first_parent: false escape hatch, because that is
// the case where the mainline hit hides the underlying change.
func (s *Server) SearchCommits(ctx context.Context, args SearchCommitsArgs) (string, error) {
	limit := clampLimit(args.Limit, defaultCommitsLimit, maxCommitsLimit)
	mirrorDir, commit, err := s.resolveIndexedCommit(args.Repo)
	if err != nil {
		return "", err
	}
	rev := args.Rev
	if rev == "" {
		rev = commit
	}

	commits, truncated, timedOut, err := githistory.SearchCommits(ctx, mirrorDir, githistory.LogOptions{
		Rev:            rev,
		Author:         args.Author,
		Since:          args.Since,
		Until:          args.Until,
		Path:           args.Path,
		Message:        args.Message,
		ChangedLiteral: args.ChangedLiteral,
		ChangedRegex:   args.ChangedRegex,
		FirstParent:    args.FirstParent,
		Limit:          limit,
	})
	if err != nil {
		return "", defaultedRevErr(err, args.Rev == "")
	}
	if len(commits) == 0 && !timedOut {
		return s.stalenessWarning() + "no commits match", nil
	}

	pickaxe := args.ChangedLiteral != "" || args.ChangedRegex != ""
	var b strings.Builder
	b.WriteString(s.stalenessWarning())
	for _, c := range commits {
		fmt.Fprintf(&b, "%s  %s  %s  %s", shortSHA(c.SHA), c.AuthorDate, c.Author, c.Subject)
		if c.IsMerge {
			if pickaxe {
				b.WriteString(" (merge — rerun with first_parent: false for the underlying commit)")
			} else {
				b.WriteString(" (merge)")
			}
		}
		b.WriteString("\n")
	}
	if truncated {
		b.WriteString("\n[truncated: more commits match; narrow with since/until or a path filter, or raise limit]\n")
	}
	if timedOut {
		fmt.Fprintf(&b, "\n[partial results: timed out after %s; narrow with path or since/until]\n", githistory.LogTimeout)
	}
	b.WriteString("\n" + plural(len(commits), "commit"))
	return b.String() + clampNote(args.Limit, maxCommitsLimit), nil
}

// GetDiffArgs are the parameters of the get_diff tool.
type GetDiffArgs struct {
	Repo             string `json:"repo" jsonschema:"exact repo name as owner/name"`
	Rev              string `json:"rev,omitempty" jsonschema:"commit to show, or right-hand endpoint (default: the indexed commit)"`
	Base             string `json:"base,omitempty" jsonschema:"left-hand endpoint; the diff goes from base to rev"`
	Path             string `json:"path,omitempty" jsonschema:"literal file path to restrict the diff to"`
	Patch            *bool  `json:"patch,omitempty" jsonschema:"include per-file patches (default: false with rev alone, true with base)"`
	MergeBase        *bool  `json:"merge_base,omitempty" jsonschema:"diff from the merge base of base and rev instead of base itself (default true)"`
	StatOnly         *bool  `json:"stat_only,omitempty" jsonschema:"suppress patches entirely, showing only per-file stats"`
	IncludeGenerated *bool  `json:"include_generated,omitempty" jsonschema:"include patches for generated/lockfile paths (default false: stat lines only)"`
}

// GetDiff shows one commit or compares two revs, with patches truncated at
// file boundaries so agents never see a partial hunk.
func (s *Server) GetDiff(ctx context.Context, args GetDiffArgs) (string, error) {
	mirrorDir, commit, err := s.resolveIndexedCommit(args.Repo)
	if err != nil {
		return "", err
	}
	rev := args.Rev
	if rev == "" {
		rev = commit
	}

	d, err := githistory.GetDiff(ctx, mirrorDir, githistory.DiffOptions{
		Rev:              rev,
		Base:             args.Base,
		Path:             args.Path,
		Patch:            args.Patch,
		MergeBase:        args.MergeBase,
		StatOnly:         args.StatOnly,
		IncludeGenerated: args.IncludeGenerated,
	})
	if err != nil {
		return "", defaultedRevErr(err, args.Rev == "")
	}

	var b strings.Builder
	b.WriteString(s.stalenessWarning())
	if args.Base == "" {
		fmt.Fprintf(&b, "%s: commit %s  %s  %s\n", args.Repo, shortSHA(d.Meta.SHA), d.Meta.Author, d.Meta.AuthorDate)
		if d.Meta.Message != "" {
			fmt.Fprintf(&b, "%s\n", d.Meta.Message)
		}
	} else {
		// Name the resolved base SHA; keep the caller's spelling too when
		// it is not already a prefix of the SHA (a branch or tag name).
		base := shortSHA(d.BaseSHA)
		if !strings.HasPrefix(d.BaseSHA, args.Base) {
			base = fmt.Sprintf("%s (%s)", args.Base, shortSHA(d.BaseSHA))
		}
		fmt.Fprintf(&b, "%s: diff from %s to %s\n", args.Repo, base, shortSHA(d.Meta.SHA))
		if d.MergeBaseSHA != "" {
			fmt.Fprintf(&b, "merge-base: %s; rev is %s ahead, base is %s ahead\n",
				shortSHA(d.MergeBaseSHA), plural(d.Ahead, "commit"), plural(d.Behind, "commit"))
		}
	}
	if d.Warning != "" {
		fmt.Fprintf(&b, "Warning: %s\n", d.Warning)
	}

	if len(d.Files) == 0 && len(d.OmittedStats) == 0 && d.Warning == "" {
		b.WriteString("\n(no changes)")
		return b.String(), nil
	}
	renderDiffFiles(&b, d)
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// renderDiffFiles renders the per-file sections and the omitted-stats
// block. Patch sections are already budgeted upstream and render in full;
// stat lines (stat-only files plus the omitted block) are capped at
// statLineCap in total, with a truncation notice naming the overflow.
func renderDiffFiles(b *strings.Builder, d *githistory.Diff) {
	statLines, skipped := 0, 0
	for _, f := range d.Files {
		if f.Patch != "" {
			fmt.Fprintf(b, "\n--- %s ---\n%s", f.Path, f.Patch)
			continue
		}
		if statLines >= statLineCap {
			skipped++
			continue
		}
		fmt.Fprintf(b, "%s\n", f.StatLine)
		statLines++
	}
	wroteOmittedHeader := false
	for _, stat := range d.OmittedStats {
		if statLines >= statLineCap {
			skipped++
			continue
		}
		if !wroteOmittedHeader {
			b.WriteString("\n[omitted: patches withheld for the following files]\n")
			wroteOmittedHeader = true
		}
		fmt.Fprintf(b, "%s\n", stat)
		statLines++
	}
	if skipped > 0 {
		fmt.Fprintf(b, "\n[truncated: %s; narrow with a path filter]\n", plural(skipped, "more changed file"))
	}
}

// BlameArgs are the parameters of the blame tool.
type BlameArgs struct {
	Repo      string `json:"repo" jsonschema:"exact repo name as owner/name"`
	Path      string `json:"path" jsonschema:"literal file path within the repo"`
	Rev       string `json:"rev,omitempty" jsonschema:"revision to blame at (default: the indexed commit, matching read_file line numbers)"`
	StartLine int    `json:"start_line,omitempty" jsonschema:"1-based first line to blame"`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"1-based last line to blame (inclusive)"`
}

// Blame attributes lines of a file to the commits that introduced them.
// Output is capped at 200 lines by default; an explicit start_line/end_line
// range raises the cap to 500.
func (s *Server) Blame(ctx context.Context, args BlameArgs) (string, error) {
	if args.Path == "" {
		return "", errors.New("path is required")
	}
	mirrorDir, commit, err := s.resolveIndexedCommit(args.Repo)
	if err != nil {
		return "", err
	}
	rev := args.Rev
	if rev == "" {
		rev = commit
	}

	lines, err := githistory.Blame(ctx, mirrorDir, githistory.BlameOptions{
		Rev:       rev,
		Path:      args.Path,
		StartLine: args.StartLine,
		EndLine:   args.EndLine,
	})
	if err != nil {
		return "", defaultedRevErr(err, args.Rev == "")
	}

	// An explicit range signals intent, so honor it up to the hard max
	// even when it is open-ended (start_line without end_line).
	lineCap := defaultBlameLines
	if args.StartLine > 0 {
		lineCap = maxBlameLines
	}
	shown := lines
	if len(shown) > lineCap {
		shown = shown[:lineCap]
	}

	revLabel := args.Rev
	if revLabel == "" {
		revLabel = shortSHA(commit)
	}
	var b strings.Builder
	b.WriteString(s.stalenessWarning())
	fmt.Fprintf(&b, "%s/%s @ %s\n", args.Repo, args.Path, revLabel)
	for _, l := range shown {
		fmt.Fprintf(&b, "%d: %s %s %s | %s\n", l.Line, shortSHA(l.SHA), l.AuthorDate, l.Author, l.Content)
	}
	if omitted := len(lines) - len(shown); omitted > 0 {
		advice := "use start_line/end_line to blame a range"
		if args.StartLine > 0 {
			advice = "narrow the line range"
		}
		fmt.Fprintf(&b, "\n[truncated: %s; %s]\n", plural(omitted, "more line"), advice)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// defaultedRevErr maps an unknown-rev failure onto ErrIndexMismatch when
// the rev was defaulted to muninn's own recorded indexed commit: the
// caller supplied nothing, so a resolution failure means the index and
// the mirror have diverged and `muninn sync` — not a different rev — is
// the remedy.
func defaultedRevErr(err error, defaulted bool) error {
	if defaulted && errors.Is(err, gitfile.ErrUnknownRev) {
		return fmt.Errorf("resolving the indexed commit: %w", gitfile.ErrIndexMismatch)
	}
	return err
}

// plural renders "n word" with an "s" appended when n != 1.
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// stalenessWarning returns the same staleness warning fragment list_repos
// emits, or "" when the index is fresh. History answers span the full
// fetched history, but rev endpoints still default to the indexed commit,
// so staleness matters here exactly as much as for file reads. The
// missing-status case never renders in the history tools —
// resolveIndexedCommit fails first — but it is kept for parity.
func (s *Server) stalenessWarning() string {
	st, err := status.Read(s.statusPath)
	if err != nil {
		return noStatusWarning
	}
	if age := status.Age(st); age > staleAfter {
		return fmt.Sprintf(staleWarningFormat, formatAge(age))
	}
	return ""
}
