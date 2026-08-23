package search

import "time"

// Options controls a single search request.
type Options struct {
	// Query is a zoekt-syntax query (repo:, file:, sym:, lang:, regex,
	// case:, boolean operators).
	Query string
	// RepoFilter is an optional repo name regexp ANDed into the query.
	RepoFilter string
	// MaxResults is the hard cap on returned line matches; 0 means
	// defaultMaxResults.
	MaxResults int
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
