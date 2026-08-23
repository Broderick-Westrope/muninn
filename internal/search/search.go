// Package search wraps Zoekt's searcher behind muninn-owned types. Zoekt
// types are an implementation detail and never appear in this package's
// public signatures (spec decision).
package search

import (
	"context"
	"fmt"
	"sort"
	"strings"

	// zoekt query atoms take regexps from the grafana fork, not stdlib.
	"github.com/grafana/regexp"
	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
	zoektsearch "github.com/sourcegraph/zoekt/search"
)

// defaultMaxResults caps returned line matches when Options.MaxResults is 0.
const defaultMaxResults = 1000

// maxLineBytes is the cap on returned line text.
const maxLineBytes = 500

// Searcher searches the shards in an index directory. It watches the
// directory, so a long-lived process picks up shards added or removed by a
// concurrent sync.
type Searcher struct {
	z zoekt.Streamer
}

// Open returns a Searcher over the shards in indexDir. The directory is
// watched for shard changes for the lifetime of the Searcher. Call Close
// to release it.
func Open(indexDir string) (*Searcher, error) {
	z, err := zoektsearch.NewDirectorySearcher(indexDir)
	if err != nil {
		return nil, fmt.Errorf("opening index directory %s: %w", indexDir, err)
	}
	return &Searcher{z: z}, nil
}

// Close releases the underlying searcher and stops watching the index
// directory.
func (s *Searcher) Close() {
	s.z.Close()
}

// Search runs a zoekt-syntax query and maps the results to muninn types.
// Query parse errors are surfaced verbatim so callers (agents) can fix
// their own syntax.
func (s *Searcher) Search(ctx context.Context, opts Options) (*Result, error) {
	q, err := query.Parse(opts.Query)
	if err != nil {
		return nil, fmt.Errorf("parsing query %q: %w", opts.Query, err)
	}
	if opts.RepoFilter != "" {
		re, err := regexp.Compile(opts.RepoFilter)
		if err != nil {
			return nil, fmt.Errorf("compiling repo filter %q: %w", opts.RepoFilter, err)
		}
		q = query.NewAnd(q, &query.Repo{Regexp: re})
	}
	q = query.Simplify(q)

	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = defaultMaxResults
	}
	zOpts := &zoekt.SearchOptions{
		// Stop searching one past the cap so truncation is detectable
		// (Stats.MatchCount exceeds what the display limits let through).
		TotalMaxMatchCount: maxResults + 1,
		// Display limits: at most maxResults matches, and since every
		// returned file has at least one match, maxResults files suffice.
		MaxDocDisplayCount:   maxResults,
		MaxMatchDisplayCount: maxResults,
	}

	res, err := s.z.Search(ctx, q, zOpts)
	if err != nil {
		return nil, fmt.Errorf("searching %q: %w", opts.Query, err)
	}

	result := &Result{
		Stats: Stats{
			FilesConsidered: res.Stats.FilesConsidered,
			MatchCount:      res.Stats.MatchCount,
			Duration:        res.Stats.Duration,
		},
	}
	returnedLines := 0
	for _, fm := range res.Files {
		file := FileMatches{Repo: fm.Repository, Path: fm.FileName}
		for _, lm := range fm.LineMatches {
			returnedLines++
			if lm.FileName {
				// Filename matches carry no meaningful line; the file
				// entry itself represents the match.
				continue
			}
			file.Lines = append(file.Lines, LineMatch{
				LineNumber:  lm.LineNumber,
				Line:        trimLine(lm.Line),
				IsSymbolDef: hasSymbolInfo(lm),
			})
		}
		result.Files = append(result.Files, file)
	}

	// Truncated when zoekt found more matches than the display limits let
	// through, or when it stopped searching early because a limit was hit.
	result.Truncated = returnedLines < res.Stats.MatchCount ||
		res.Stats.FilesSkipped > 0 || res.Stats.ShardsSkipped > 0

	if opts.GroupByRepo {
		sort.SliceStable(result.Files, func(i, j int) bool {
			return result.Files[i].Repo < result.Files[j].Repo
		})
	}
	return result, nil
}

// ListRepos returns every indexed repository with its branch and the commit
// its shards were built from, sorted by name.
func (s *Searcher) ListRepos(ctx context.Context) ([]RepoInfo, error) {
	res, err := s.z.List(ctx, &query.Const{Value: true}, nil)
	if err != nil {
		return nil, fmt.Errorf("listing repos: %w", err)
	}
	repos := make([]RepoInfo, 0, len(res.Repos))
	for _, entry := range res.Repos {
		info := RepoInfo{Name: entry.Repository.Name}
		if len(entry.Repository.Branches) > 0 {
			info.Branch = entry.Repository.Branches[0].Name
			info.IndexedCommit = entry.Repository.Branches[0].Version
		}
		repos = append(repos, info)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })
	return repos, nil
}

// trimLine strips the trailing newline and caps the line at maxLineBytes.
func trimLine(line []byte) string {
	s := strings.TrimRight(string(line), "\r\n")
	if len(s) > maxLineBytes {
		s = s[:maxLineBytes]
	}
	return s
}

// hasSymbolInfo reports whether any fragment of the line match carries
// ctags symbol info (set by zoekt for sym: atom matches).
func hasSymbolInfo(lm zoekt.LineMatch) bool {
	for _, frag := range lm.LineFragments {
		if frag.SymbolInfo != nil {
			return true
		}
	}
	return false
}
