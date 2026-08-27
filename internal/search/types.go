package search

import "time"

// Options controls a single search request.
type Options struct {
	// Query is a zoekt-syntax query (repo:, file:, sym:, lang:, regex,
	// case:, boolean operators).
	Query string
	// RepoFilter is an optional repo name regexp ANDed into the query.
	RepoFilter string
	// FileFilter is an optional file path regexp ANDed into the query.
	// Composed at the query AST level, never concatenated onto Query:
	// zoekt's `or` binds looser than implicit AND, so an appended atom
	// would silently fail to filter one side of a disjunction.
	FileFilter string
	// MaxResults is the hard cap on returned line matches; 0 means
	// defaultMaxResults.
	MaxResults int
	// PerShardMatchLimit, when non-zero, bounds matches per index shard and
	// lifts the cross-shard budget instead of capping total matches. Use it
	// for aggregation, where the returned *set* has to be stable.
	//
	// zoekt's TotalMaxMatchCount is a budget shared across shards: the
	// search stops once it is exhausted, so which shards contributed
	// depends on which happened to finish first. Two identical searches can
	// therefore report different repos. A per-shard cap is applied by each
	// shard independently and never halts the search, so every shard is
	// always visited and the result is deterministic — and coverage is
	// broader, since one heavily-matching repo can no longer consume the
	// whole budget.
	//
	// It must stay >= maxSearchLimit so an aggregated count can never fall
	// below the number of rows a capped search displays for that value.
	PerShardMatchLimit int
	// GroupByRepo sorts the returned files by repo name (stable, keeping
	// zoekt's relevance order within each repo).
	GroupByRepo bool
}

// Result is the outcome of a search.
type Result struct {
	// Files holds the matching files in relevance order (or grouped by
	// repo when Options.GroupByRepo is set).
	Files []FileMatches
	// Truncated reports that more matches exist than were returned
	// (a display or search limit was hit).
	Truncated bool
	// Stats summarizes the work zoekt did for this search.
	Stats Stats
}

// FileMatches holds all line matches within a single file.
type FileMatches struct {
	// Repo is the repository full name ("owner/name").
	Repo string
	// Path is the file path within the repo.
	Path string
	// Lines are the matching lines. It can be empty when the match was
	// on the file name only.
	Lines []LineMatch
}

// LineMatch is a single matching line.
type LineMatch struct {
	// LineNumber is 1-based.
	LineNumber int
	// Line is the full line text, trimmed to 500 bytes, without the
	// trailing newline.
	Line string
	// IsSymbolDef is true when zoekt reports ctags symbol info for the
	// match, i.e. the match is a symbol definition site. Zoekt only
	// attaches symbol info to matches produced by sym: query atoms.
	IsSymbolDef bool
}

// Stats summarizes the work done for a search.
type Stats struct {
	// FilesConsidered is the number of candidate files evaluated.
	FilesConsidered int
	// MatchCount is the number of non-overlapping matches found before
	// display truncation.
	MatchCount int
	// Duration is the wall clock time of the search.
	Duration time.Duration
}

// RepoInfo describes one indexed repository.
type RepoInfo struct {
	// Name is the repository full name ("owner/name").
	Name string
	// Branch is the indexed branch.
	Branch string
	// IndexedCommit is the commit SHA the shards were built from.
	IndexedCommit string
}

// Facets counts line matches per repo and per file extension for a query,
// ignoring any facet filters. Aggregation runs at its own cap, independent
// of the display limits, so a count is never lower than the rows a filtered
// search displays. Truncated reports that the cap was hit, meaning the value
// list is not exhaustive.
type Facets struct {
	Repos     []FacetValue
	Exts      []FacetValue
	Truncated bool
}

// FacetValue is one facet bucket: a value and how many matching lines carry
// it. Value is "" in Exts for files with no extension.
type FacetValue struct {
	Value string
	Count int
}
