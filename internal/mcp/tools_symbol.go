package mcp

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/broderick-westrope/muninn/internal/search"
)

const (
	defaultDefinitionsLimit = 50
	maxDefinitionsLimit     = 200
	defaultReferencesLimit  = 100
	maxReferencesLimit      = 300
)

const findDefinitionsDescription = `Find definition sites of an exact identifier using ctags-backed symbol search (case-sensitive, word-boundary anchored). Output lines are 'repo/path:line: content'. Default limit 50, max 200.`

const findReferencesDescription = `Find references to an exact identifier (case-sensitive, word-boundary text search) with definition sites excluded. Approximate: text-based reference search excluding definition sites. Treat as leads, not ground truth. It cannot distinguish shadowed names, comments, or strings, and only excludes definitions that ctags recognized. Entire definition lines are excluded, so a reference sharing a line with a definition is dropped. Default limit 100, max 300.`

// FindDefinitionsArgs are the parameters of the find_symbol_definitions
// tool.
type FindDefinitionsArgs struct {
	Symbol string `json:"symbol" jsonschema:"exact identifier to find definitions of"`
	Repo   string `json:"repo,omitempty" jsonschema:"optional repo name regex to search in"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max results (default 50, max 200)"`
}

// FindSymbolDefinitions finds definition sites of an identifier via a
// word-boundary, case-sensitive sym: query.
func (s *Server) FindSymbolDefinitions(ctx context.Context, args FindDefinitionsArgs) (string, error) {
	if args.Symbol == "" {
		return "", errors.New("symbol is required")
	}
	limit := clampLimit(args.Limit, defaultDefinitionsLimit, maxDefinitionsLimit)
	res, err := s.searcher.Search(ctx, search.Options{
		Query:      symbolQuery("sym:", args.Symbol),
		RepoFilter: args.Repo,
		MaxResults: limit + 1,
	})
	if err != nil {
		return "", err
	}
	if len(res.Files) == 0 {
		return fmt.Sprintf("no definitions found for %q (note: symbol search requires the index to be built with universal-ctags)", args.Symbol), nil
	}
	return formatGrep(res, limit, false) + clampNote(args.Limit, maxDefinitionsLimit), nil
}

// FindReferencesArgs are the parameters of the find_symbol_references tool.
type FindReferencesArgs struct {
	Symbol string `json:"symbol" jsonschema:"exact identifier to find references to"`
	Repo   string `json:"repo,omitempty" jsonschema:"optional repo name regex to search in"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max results (default 100, max 300)"`
}

// FindSymbolReferences finds likely reference sites of an identifier. Zoekt
// only attaches symbol info to matches from sym: atoms, so definitions
// cannot be recognized within a plain content search. Instead two queries
// run: a sym: query collects definition locations, then a word-boundary
// content search has those lines subtracted before the cap is applied (so
// truncation counts stay honest).
func (s *Server) FindSymbolReferences(ctx context.Context, args FindReferencesArgs) (string, error) {
	if args.Symbol == "" {
		return "", errors.New("symbol is required")
	}
	limit := clampLimit(args.Limit, defaultReferencesLimit, maxReferencesLimit)
	// Headroom: fetch enough content matches that subtracting definition
	// lines still leaves a full page plus a truncation signal.
	headroom := 2*limit + 50

	defRes, err := s.searcher.Search(ctx, search.Options{
		Query:      symbolQuery("sym:", args.Symbol),
		RepoFilter: args.Repo,
		MaxResults: headroom,
	})
	if err != nil {
		return "", fmt.Errorf("finding definition sites: %w", err)
	}
	defLines := make(map[string]bool)
	for _, f := range defRes.Files {
		for _, l := range f.Lines {
			defLines[lineKey(f.Repo, f.Path, l.LineNumber)] = true
		}
	}

	res, err := s.searcher.Search(ctx, search.Options{
		Query:      symbolQuery("", args.Symbol),
		RepoFilter: args.Repo,
		MaxResults: headroom,
	})
	if err != nil {
		return "", err
	}

	// Subtract definition lines BEFORE applying the cap.
	type refLine struct {
		repo, path string
		num        int
		text       string
	}
	var refs []refLine
	for _, f := range res.Files {
		for _, l := range f.Lines {
			if defLines[lineKey(f.Repo, f.Path, l.LineNumber)] {
				continue
			}
			refs = append(refs, refLine{repo: f.Repo, path: f.Path, num: l.LineNumber, text: l.Line})
		}
	}

	if len(refs) == 0 {
		return fmt.Sprintf("no references found for %q", args.Symbol), nil
	}
	truncated := res.Truncated
	shown := refs
	if len(shown) > limit {
		shown = shown[:limit]
		truncated = true
	}

	var b strings.Builder
	for _, r := range shown {
		fmt.Fprintf(&b, "%s/%s:%d: %s\n", r.repo, r.path, r.num, r.text)
	}
	if truncated {
		omitted := len(refs) - len(shown)
		if omitted < 1 {
			omitted = 1
		}
		fmt.Fprintf(&b, "\n[truncated: at least %d more references omitted; narrow with repo or raise limit (max %d)]\n", omitted, maxReferencesLimit)
	}
	if defRes.Truncated {
		b.WriteString("\n[caveat: the definition query was truncated, so definition exclusion may be incomplete; some listed references may be definition sites]\n")
	}
	fmt.Fprintf(&b, "\n%d references (approximate; definition sites excluded) (%d files considered, %s)",
		len(shown), res.Stats.FilesConsidered, res.Stats.Duration.Round(time.Millisecond))
	return b.String() + clampNote(args.Limit, maxReferencesLimit), nil
}

// symbolQuery builds a word-boundary, case-sensitive query for an exact
// identifier, optionally prefixed with an atom marker such as "sym:".
func symbolQuery(atom, symbol string) string {
	return atom + quoteToken(`\b`+regexp.QuoteMeta(symbol)+`\b`) + " case:yes"
}

// lineKey identifies a line across repos and files for set membership.
func lineKey(repo, path string, line int) string {
	return fmt.Sprintf("%s\x00%s\x00%d", repo, path, line)
}
