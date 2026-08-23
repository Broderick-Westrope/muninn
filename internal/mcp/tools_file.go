package mcp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/broderick-westrope/muninn/internal/gitfile"
	"github.com/broderick-westrope/muninn/internal/status"
)

const (
	defaultReadFileLimit = 500
	defaultTreeDepth     = 1
	maxTreeDepth         = 10
)

const readFileDescription = `Read a file from an indexed repo at its indexed commit: line numbers always agree with grep results, but content may lag the repo's latest commit until the next sync. 'repo' is the exact owner/name (see list_repos). 'offset' is the 1-based first line to return; 'limit' is the number of lines (default 500).`

const listTreeDescription = `List directory entries of an indexed repo at its indexed commit (content may lag the latest commit until the next sync). 'path' defaults to the repo root; 'depth' is how many levels to descend (default 1, max 10). Directories end with '/'.`

// ReadFileArgs are the parameters of the read_file tool.
type ReadFileArgs struct {
	Repo   string `json:"repo" jsonschema:"exact repo name as owner/name"`
	Path   string `json:"path" jsonschema:"file path within the repo"`
	Offset int    `json:"offset,omitempty" jsonschema:"1-based first line to return (default 1)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"number of lines to return (default 500)"`
}

// ReadFile returns file content at the repo's indexed commit with a
// "lines X-Y of Z" header.
func (s *Server) ReadFile(ctx context.Context, args ReadFileArgs) (string, error) {
	if args.Path == "" {
		return "", errors.New("path is required")
	}
	limit := args.Limit
	if limit <= 0 {
		limit = defaultReadFileLimit
	}
	mirrorDir, commit, err := s.resolveIndexedCommit(args.Repo)
	if err != nil {
		return "", err
	}

	content, totalLines, err := gitfile.ReadFile(ctx, mirrorDir, commit, args.Path, args.Offset, limit)
	if err != nil {
		return "", err
	}

	start := args.Offset
	if start <= 0 {
		start = 1
	}
	if start > totalLines {
		return fmt.Sprintf("%s/%s @ %s: offset %d is past the last line (%d lines total)",
			args.Repo, args.Path, shortSHA(commit), start, totalLines), nil
	}
	shown := strings.Count(content, "\n")
	if content != "" && !strings.HasSuffix(content, "\n") {
		shown++
	}
	end := start + shown - 1
	return fmt.Sprintf("%s/%s @ %s (lines %d-%d of %d)\n%s",
		args.Repo, args.Path, shortSHA(commit), start, end, totalLines, content), nil
}

// ListTreeArgs are the parameters of the list_tree tool.
type ListTreeArgs struct {
	Repo  string `json:"repo" jsonschema:"exact repo name as owner/name"`
	Path  string `json:"path,omitempty" jsonschema:"directory path within the repo (default: root)"`
	Depth int    `json:"depth,omitempty" jsonschema:"levels to descend (default 1, max 10)"`
}

// ListTree lists directory entries at the repo's indexed commit.
func (s *Server) ListTree(ctx context.Context, args ListTreeArgs) (string, error) {
	depth := args.Depth
	if depth <= 0 {
		depth = defaultTreeDepth
	}
	if depth > maxTreeDepth {
		depth = maxTreeDepth
	}
	mirrorDir, commit, err := s.resolveIndexedCommit(args.Repo)
	if err != nil {
		return "", err
	}

	entries, err := gitfile.ListTree(ctx, mirrorDir, commit, args.Path, depth)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	path := args.Path
	if path == "" {
		path = "."
	}
	fmt.Fprintf(&b, "%s/%s @ %s (depth %d)\n", args.Repo, path, shortSHA(commit), depth)
	for _, e := range entries {
		if e.Type == "dir" {
			fmt.Fprintf(&b, "%s/\n", e.Path)
		} else {
			fmt.Fprintf(&b, "%s (%d B)\n", e.Path, e.Size)
		}
	}
	fmt.Fprintf(&b, "\n%d entries", len(entries))
	return b.String(), nil
}

// resolveIndexedCommit re-reads the status file and returns the mirror
// directory and indexed commit for an exact owner/name repo. The status
// file is never cached: a mid-session sync must be picked up immediately
// so file reads stay pinned to the commit the shards were built from.
func (s *Server) resolveIndexedCommit(repo string) (mirrorDir, commit string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("repo %q must be an exact owner/name; use list_repos to see indexed repos", repo)
	}
	st, err := status.Read(s.statusPath)
	if err != nil {
		if errors.Is(err, status.ErrNotExist) {
			return "", "", fmt.Errorf("no sync status found (never synced?); run `muninn sync`: %w", err)
		}
		return "", "", err
	}
	rs, ok := st.Repos[repo]
	if !ok || rs.IndexedCommit == "" {
		return "", "", fmt.Errorf("repo %q has no indexed commit; use list_repos to see indexed repos, or run `muninn sync`", repo)
	}
	return filepath.Join(s.mirrorsDir, owner, name+".git"), rs.IndexedCommit, nil
}
