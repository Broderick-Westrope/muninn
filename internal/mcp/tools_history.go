package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/broderick-westrope/muninn/internal/githistory"
	"github.com/broderick-westrope/muninn/internal/status"
)

const (
	defaultCommitsLimit = 30
	maxCommitsLimit     = 100
	defaultBlameLines   = 200
	maxBlameLines       = 500
)

const searchCommitsDescription = `Search the commit history of one indexed repo ('repo' is required; cross-repo history search is not supported). Filters compose: 'author' (regex), 'since'/'until' (filter on commit date, though output shows author dates), 'message' (commit message regex), 'path' (a literal path, never a glob; single-path queries follow renames), and the mutually exclusive pickaxe filters 'changed_literal' (commits where the occurrence count of the literal string changed) and 'changed_regex' (commits whose diff has a hunk matching the regex; substantially slower). 'rev' is the starting point and defaults to the indexed commit. 'first_parent' defaults true so merge-heavy repos return mainline history; merge rows are annotated '(merge)'. Output lines are 'sha  date  author  subject'. Default limit 30, max 100.`

const getDiffDescription = `Show one commit or compare two revs in an indexed repo. With 'rev' alone (default: the indexed commit): commit metadata plus per-file stats, and 'patch: true' adds the patches; merge commits are diffed against their first parent. Adding 'base' diffs from base to rev with patches on by default; 'merge_base' defaults true (three-dot semantics: only rev-side changes since the common ancestor) — set 'merge_base: false' for a literal point-to-point comparison. 'path' is a literal path filter, never a glob. Patches are truncated at file boundaries under a 64 KiB budget; files past the budget — and always binary and generated/lockfile paths unless 'include_generated: true' — appear as stat lines instead.`

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
		return "", err
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
		b.WriteString("\n[partial results: timed out; narrow with path or since/until]\n")
	}
	fmt.Fprintf(&b, "\n%d commits", len(commits))
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
		return "", err
	}

	var b strings.Builder
	b.WriteString(s.stalenessWarning())
	if args.Base == "" {
		fmt.Fprintf(&b, "%s: commit %s  %s  %s\n", args.Repo, shortSHA(d.Meta.SHA), d.Meta.Author, d.Meta.AuthorDate)
		if d.Meta.Message != "" {
			fmt.Fprintf(&b, "%s\n", d.Meta.Message)
		}
	} else {
		fmt.Fprintf(&b, "%s: diff from %s to %s\n", args.Repo, args.Base, shortSHA(d.Meta.SHA))
		if d.MergeBaseSHA != "" {
			fmt.Fprintf(&b, "merge-base: %s; rev is %d commits ahead, base is %d commits ahead\n",
				shortSHA(d.MergeBaseSHA), d.Ahead, d.Behind)
		}
	}
	if d.Warning != "" {
		fmt.Fprintf(&b, "Warning: %s\n", d.Warning)
	}

	if len(d.Files) == 0 && len(d.OmittedStats) == 0 && d.Warning == "" {
		b.WriteString("\n(no changes)")
		return b.String(), nil
	}
	for _, f := range d.Files {
		if f.Patch != "" {
			fmt.Fprintf(&b, "\n--- %s ---\n%s", f.Path, f.Patch)
		} else {
			fmt.Fprintf(&b, "%s\n", f.StatLine)
		}
	}
	if len(d.OmittedStats) > 0 {
		b.WriteString("\n[omitted: patches withheld for the following files]\n")
		for _, stat := range d.OmittedStats {
			fmt.Fprintf(&b, "%s\n", stat)
		}
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
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
		return "", err
	}

	lineCap := defaultBlameLines
	if args.StartLine > 0 && args.EndLine >= args.StartLine {
		if want := args.EndLine - args.StartLine + 1; want > lineCap {
			lineCap = min(want, maxBlameLines)
		}
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
		fmt.Fprintf(&b, "\n[truncated: %d more lines; use start_line/end_line to blame a range]\n", omitted)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
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
		return "WARNING: no sync status found; the index may be empty or stale — run `muninn sync`\n"
	}
	if age := status.Age(st); age > staleAfter {
		return fmt.Sprintf("WARNING: index is stale: last sync finished %s ago — run `muninn sync`\n", formatAge(age))
	}
	return ""
}
