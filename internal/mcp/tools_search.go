package mcp

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/broderick-westrope/muninn/internal/search"
	"github.com/broderick-westrope/muninn/internal/status"
)

const (
	defaultGrepLimit = 50
	maxGrepLimit     = 200
	defaultGlobLimit = 100
)

const grepDescription = `Search file contents across all indexed repos with a regular expression (RE2 syntax).

The pattern is a zoekt query, so these atoms can be combined with the regex (space-separated atoms are ANDed):
- repo:<regex>  filter by repo name
- file:<regex>  filter by file path
- sym:<regex>   match ctags symbol definitions
- lang:<name>   filter by language
- case:yes|no|auto  case sensitivity (default: smart case)
- -<atom>       negate an atom (e.g. -file:_test\.go)

Results are capped at 'limit' line matches (default 50, max 200); a truncation notice reports how many more matches were omitted. Output lines are 'repo/path:line: content'.`

const globDescription = `Find files by path glob across all indexed repos. Supports *, ?, ** and {a,b} alternation; matching is case-insensitive against the full path within each repo (e.g. '**/*.go', 'src/**/*.{ts,tsx}'). Results are capped at 'limit' files (default 100) and grouped by repo.`

const listReposDescription = `List indexed repositories: name, branch, and indexed commit, plus the age of the last sync. Optional 'query' regex filters by repo name. Output starts with a staleness warning when the index is older than 24h or has never been synced.`

// GrepArgs are the parameters of the grep tool.
type GrepArgs struct {
	Pattern     string `json:"pattern" jsonschema:"regular expression (RE2); may include zoekt atoms like repo:, file:, sym:, lang:, case:, -negation"`
	Repo        string `json:"repo,omitempty" jsonschema:"optional repo name regex to search in"`
	Include     string `json:"include,omitempty" jsonschema:"optional file glob to restrict matched paths (e.g. **/*.go)"`
	Literal     bool   `json:"literal,omitempty" jsonschema:"treat pattern as a literal string instead of a regex"`
	GroupByRepo bool   `json:"group_by_repo,omitempty" jsonschema:"group output under per-repo headers"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max line matches to return (default 50, max 200)"`
}

// Grep searches file contents and formats matches as repo/path:line lines.
func (s *Server) Grep(ctx context.Context, args GrepArgs) (string, error) {
	if args.Pattern == "" {
		return "", errors.New("pattern is required")
	}
	limit := clampLimit(args.Limit, defaultGrepLimit, maxGrepLimit)

	q := args.Pattern
	if args.Literal {
		// Escape the pattern and quote it so spaces and regex
		// metacharacters survive zoekt's query parser.
		q = quoteToken(regexp.QuoteMeta(args.Pattern))
	}
	if args.Include != "" {
		re, err := globToRegexp(args.Include)
		if err != nil {
			return "", fmt.Errorf("invalid include glob %q: %w", args.Include, err)
		}
		q += " file:" + quoteToken(re)
	}

	// Fetch one line past the cap so truncation is detected even when
	// zoekt's own display limits would round up.
	res, err := s.searcher.Search(ctx, search.Options{
		Query:       q,
		RepoFilter:  args.Repo,
		MaxResults:  limit + 1,
		GroupByRepo: args.GroupByRepo,
	})
	if err != nil {
		return "", err
	}
	return formatGrep(res, limit, args.GroupByRepo), nil
}

// GlobArgs are the parameters of the glob tool.
type GlobArgs struct {
	Pattern string `json:"pattern" jsonschema:"path glob: *, ?, ** and {a,b} (e.g. **/*.go)"`
	Repo    string `json:"repo,omitempty" jsonschema:"optional repo name regex to search in"`
	Limit   int    `json:"limit,omitempty" jsonschema:"max files to return (default 100)"`
}

// Glob lists files whose paths match a glob, grouped by repo.
func (s *Server) Glob(ctx context.Context, args GlobArgs) (string, error) {
	if args.Pattern == "" {
		return "", errors.New("pattern is required")
	}
	limit := args.Limit
	if limit <= 0 {
		limit = defaultGlobLimit
	}
	re, err := globToRegexp(args.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid glob %q: %w", args.Pattern, err)
	}

	// A filename-only query: zoekt returns one file match per path whose
	// name matches the file: atom, no content atom needed.
	res, err := s.searcher.Search(ctx, search.Options{
		Query:       "file:" + quoteToken(re),
		RepoFilter:  args.Repo,
		MaxResults:  limit,
		GroupByRepo: true,
	})
	if err != nil {
		return "", err
	}

	if len(res.Files) == 0 {
		return "no files match", nil
	}
	var b strings.Builder
	lastRepo := ""
	for _, f := range res.Files {
		if f.Repo != lastRepo {
			if lastRepo != "" {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "%s:\n", f.Repo)
			lastRepo = f.Repo
		}
		fmt.Fprintf(&b, "  %s\n", f.Path)
	}
	if res.Truncated {
		fmt.Fprintf(&b, "\n[truncated: more files match; narrow the glob or raise limit]\n")
	}
	fmt.Fprintf(&b, "\n%d files", len(res.Files))
	return b.String(), nil
}

// ListReposArgs are the parameters of the list_repos tool.
type ListReposArgs struct {
	Query string `json:"query,omitempty" jsonschema:"optional repo name regex filter"`
}

// ListRepos lists indexed repos with branch, short commit, and index age,
// prefixed with a staleness warning when the index is old or never synced.
// Staleness is computed from a fresh read of the status file on every call.
func (s *Server) ListRepos(ctx context.Context, args ListReposArgs) (string, error) {
	var filter *regexp.Regexp
	if args.Query != "" {
		re, err := regexp.Compile(args.Query)
		if err != nil {
			return "", fmt.Errorf("invalid query regex %q: %w", args.Query, err)
		}
		filter = re
	}

	repos, err := s.searcher.ListRepos(ctx)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	if warning := s.stalenessWarning(); warning != "" {
		b.WriteString(warning + "\n")
	}
	if st, err := status.Read(s.statusPath); err == nil {
		fmt.Fprintf(&b, "last sync: %s (%s ago)\n", st.FinishedAt.Format(time.RFC3339), formatAge(status.Age(st)))
	}

	shown := 0
	for _, r := range repos {
		if filter != nil && !filter.MatchString(r.Name) {
			continue
		}
		fmt.Fprintf(&b, "%s  %s  %s\n", r.Name, r.Branch, shortSHA(r.IndexedCommit))
		shown++
	}
	fmt.Fprintf(&b, "\n%d repos", shown)
	return b.String(), nil
}

// stalenessWarning returns a warning line when the status file is missing
// or the last sync finished more than staleAfter ago, and "" otherwise.
func (s *Server) stalenessWarning() string {
	st, err := status.Read(s.statusPath)
	if err != nil {
		return "WARNING: no sync status found; the index may be empty or stale — run `muninn sync`"
	}
	if age := status.Age(st); age > staleAfter {
		return fmt.Sprintf("WARNING: index is stale: last sync finished %s ago — run `muninn sync`", formatAge(age))
	}
	return ""
}

// formatGrep renders search results as repo/path:line lines, enforcing the
// line-match cap, adding a truncation notice and a final stats line.
func formatGrep(res *search.Result, limit int, groupByRepo bool) string {
	var b strings.Builder
	shown, filesShown := 0, 0
	lastRepo := ""
	truncated := res.Truncated
outer:
	for _, f := range res.Files {
		if shown >= limit {
			truncated = true
			break
		}
		if groupByRepo && f.Repo != lastRepo {
			if lastRepo != "" {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "# %s\n", f.Repo)
			lastRepo = f.Repo
		}
		filesShown++
		if len(f.Lines) == 0 {
			// Filename-only match: the path itself is the result.
			fmt.Fprintf(&b, "%s/%s (filename match)\n", f.Repo, f.Path)
			shown++
			continue
		}
		for _, l := range f.Lines {
			if shown >= limit {
				truncated = true
				break outer
			}
			fmt.Fprintf(&b, "%s/%s:%d: %s\n", f.Repo, f.Path, l.LineNumber, l.Line)
			shown++
		}
	}

	if shown == 0 {
		return "no matches"
	}
	if truncated {
		omitted := res.Stats.MatchCount - shown
		if omitted < 1 {
			omitted = 1
		}
		fmt.Fprintf(&b, "\n[truncated: at least %d more matches omitted; narrow the pattern or use repo:/file: filters]\n", omitted)
	}
	fmt.Fprintf(&b, "\n%d matches in %d files (%d files considered, %s)",
		shown, filesShown, res.Stats.FilesConsidered, res.Stats.Duration.Round(time.Millisecond))
	return b.String()
}

// globToRegexp translates a path glob to an anchored, case-insensitive
// regexp string. Supported: * (any chars except /), ? (one char except /),
// ** (any chars including /), {a,b} alternation. A "**/" segment also
// matches zero directories.
func globToRegexp(glob string) (string, error) {
	var b strings.Builder
	b.WriteString("(?i)^")
	i := 0
	for i < len(glob) {
		c := glob[i]
		switch c {
		case '*':
			if strings.HasPrefix(glob[i:], "**/") {
				b.WriteString("(?:.*/)?")
				i += 3
			} else if strings.HasPrefix(glob[i:], "**") {
				b.WriteString(".*")
				i += 2
			} else {
				b.WriteString("[^/]*")
				i++
			}
		case '?':
			b.WriteString("[^/]")
			i++
		case '{':
			end := strings.IndexByte(glob[i:], '}')
			if end < 0 {
				return "", fmt.Errorf("unclosed { in glob")
			}
			alts := strings.Split(glob[i+1:i+end], ",")
			for j, alt := range alts {
				alts[j] = regexp.QuoteMeta(alt)
			}
			b.WriteString("(?:" + strings.Join(alts, "|") + ")")
			i += end + 1
		case '}':
			return "", fmt.Errorf("unmatched } in glob")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	b.WriteString("$")
	return b.String(), nil
}

// quoteToken wraps s in a zoekt query string literal so spaces do not split
// it into multiple atoms. The parser strips one level of backslash escaping
// inside quotes, so backslashes are doubled to preserve s exactly.
func quoteToken(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// clampLimit applies a default for non-positive limits and a hard maximum.
func clampLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}

// shortSHA abbreviates a commit SHA for display.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// formatAge renders a duration as a compact human-readable age.
func formatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Minute).String()
	default:
		return d.Round(time.Hour).String()
	}
}
