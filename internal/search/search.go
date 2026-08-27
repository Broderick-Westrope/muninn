// Package search wraps Zoekt's searcher behind muninn-owned types. Zoekt
// types are an implementation detail and never appear in this package's
// public signatures (spec decision).
package search

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

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
	if opts.FileFilter != "" {
		// Parsed as a file: atom and ANDed at the AST level, so the filter
		// covers the whole query however it is shaped. Concatenating it onto
		// the query text would bind tighter than a top-level `or` and leave
		// one side of the disjunction silently unfiltered.
		fq, err := query.Parse("file:" + opts.FileFilter)
		if err != nil {
			return nil, fmt.Errorf("parsing file filter %q: %w", opts.FileFilter, err)
		}
		q = query.NewAnd(q, fq)
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
	if opts.PerShardMatchLimit > 0 {
		// Per-shard cap instead of a cross-shard budget: deterministic, and
		// every shard is visited. See Options.PerShardMatchLimit.
		zOpts.ShardMaxMatchCount = opts.PerShardMatchLimit
		zOpts.TotalMaxMatchCount = math.MaxInt32
		zOpts.MaxDocDisplayCount = math.MaxInt32
		zOpts.MaxMatchDisplayCount = math.MaxInt32
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

// trimLine strips the trailing newline and caps the line at maxLineBytes,
// cutting at a rune boundary so multi-byte characters are never split.
func trimLine(line []byte) string {
	s := strings.TrimRight(string(line), "\r\n")
	if len(s) > maxLineBytes {
		cut := maxLineBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
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

// Aggregate runs q with no facet filters and buckets the matches by repo and
// by file extension. It exists so the facet sidebar stays stable: deriving
// facet values from a filtered search would make every unselected value
// disappear on the first click, leaving multi-select unbuildable and a
// zero-result selection with no chip to undo.
//
// perShardLimit bounds the work per index shard. It must not be turned into
// a total-match cap: that budget is shared across shards, so a truncated
// aggregation returns whichever repos won the race and the facet list
// reshuffles between identical searches. See Options.PerShardMatchLimit.
//
// Counts are line matches, matching what the UI renders as rows. They
// saturate for a repo with more than perShardLimit matches in one shard,
// which Facets.Truncated discloses.
func (s *Searcher) Aggregate(ctx context.Context, q string, perShardLimit int) (*Facets, error) {
	res, err := s.Search(ctx, Options{Query: q, PerShardMatchLimit: perShardLimit})
	if err != nil {
		return nil, err
	}
	repos, exts := map[string]int{}, map[string]int{}
	for _, f := range res.Files {
		// A filename-only match carries no lines but is still one result,
		// the same rule the web layer applies when counting against a limit.
		n := len(f.Lines)
		if n == 0 {
			n = 1
		}
		repos[f.Repo] += n
		exts[extOf(f.Path)] += n
	}
	return &Facets{
		Repos:     sortedFacets(repos),
		Exts:      sortedFacets(exts),
		Truncated: res.Truncated,
	}, nil
}

// extOf returns the lowercased extension of a path's basename, or "" when it
// has none. A leading dot does not start an extension, so "Makefile" and
// ".gitignore" both bucket as "".
func extOf(path string) string {
	base := path
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	i := strings.LastIndexByte(base, '.')
	if i <= 0 { // -1: no dot at all; 0: leading dot (a dotfile)
		return ""
	}
	return strings.ToLower(base[i+1:])
}

// sortedFacets orders buckets by count descending, then value ascending, so
// identical requests always produce an identical list.
func sortedFacets(counts map[string]int) []FacetValue {
	out := make([]FacetValue, 0, len(counts))
	for value, count := range counts {
		out = append(out, FacetValue{Value: value, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	return out
}
