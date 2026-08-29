// Package mcp implements muninn's MCP server: a stdio transport exposing
// ten tools (grep, glob, list_repos, read_file, list_tree,
// find_symbol_definitions, find_symbol_references, search_commits,
// get_diff, blame) over the search core, the status file, and the bare
// git mirrors.
package mcp

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/broderick-westrope/muninn/internal/search"
)

// staleAfter is how old the last successful sync may be before tools warn
// that the index is stale.
const staleAfter = 24 * time.Hour

// Staleness warning fragments shared by ListRepos and stalenessWarning so
// every tool renders the identical wording.
const (
	noStatusWarning    = "WARNING: no sync status found; the index may be empty or stale — run `muninn sync`\n"
	staleWarningFormat = "WARNING: index is stale: last sync finished %s ago — run `muninn sync`\n"
)

// instructions is sent to clients at initialization so agents know how to
// interpret results.
const instructions = `muninn serves code search over locally indexed GitHub repositories.
read_file and list_tree return content at the commit each repo was indexed
at, so line numbers always agree with grep results — but content may lag
the repos' latest commits until the next sync. The history tools
(search_commits, get_diff, blame) operate on each repo's full fetched
history, while file reads stay pinned to the indexed commit; blame
defaults to that commit so its line numbers agree with read_file. Use
list_repos to see which repos are indexed and how old the index is; it
warns when the index is stale.`

// Server holds the dependencies of the MCP tool handlers. Each tool is a
// plain method so tests can call handlers directly without a stdio
// session. The status file is re-read on every call that needs it (never
// cached at startup): a mid-session sync updates shards via the directory
// watcher, and a stale cached commit would break the read_file/grep
// line-number guarantee.
type Server struct {
	searcher   *search.Searcher
	statusPath string
	mirrorsDir string
}

// New returns a Server that searches with searcher, resolves indexed
// commits from the status file at statusPath, and reads pinned file
// content from the bare mirrors under mirrorsDir.
func New(searcher *search.Searcher, statusPath, mirrorsDir string) *Server {
	return &Server{searcher: searcher, statusPath: statusPath, mirrorsDir: mirrorsDir}
}

// Run serves MCP over stdio until ctx is canceled or the client
// disconnects.
func (s *Server) Run(ctx context.Context) error {
	return s.mcpServer().Run(ctx, &sdk.StdioTransport{})
}

// mcpServer builds the SDK server with all tools registered.
func (s *Server) mcpServer() *sdk.Server {
	srv := sdk.NewServer(
		&sdk.Implementation{Name: "muninn"},
		&sdk.ServerOptions{Instructions: instructions},
	)
	sdk.AddTool(srv, &sdk.Tool{Name: "grep", Description: grepDescription}, textHandler(s.Grep))
	sdk.AddTool(srv, &sdk.Tool{Name: "glob", Description: globDescription}, textHandler(s.Glob))
	sdk.AddTool(srv, &sdk.Tool{Name: "list_repos", Description: listReposDescription}, textHandler(s.ListRepos))
	sdk.AddTool(srv, &sdk.Tool{Name: "read_file", Description: readFileDescription}, textHandler(s.ReadFile))
	sdk.AddTool(srv, &sdk.Tool{Name: "list_tree", Description: listTreeDescription}, textHandler(s.ListTree))
	sdk.AddTool(srv, &sdk.Tool{Name: "find_symbol_definitions", Description: findDefinitionsDescription}, textHandler(s.FindSymbolDefinitions))
	sdk.AddTool(srv, &sdk.Tool{Name: "find_symbol_references", Description: findReferencesDescription}, textHandler(s.FindSymbolReferences))
	sdk.AddTool(srv, &sdk.Tool{Name: "search_commits", Description: searchCommitsDescription}, textHandler(s.SearchCommits))
	sdk.AddTool(srv, &sdk.Tool{Name: "get_diff", Description: getDiffDescription}, textHandler(s.GetDiff))
	sdk.AddTool(srv, &sdk.Tool{Name: "blame", Description: blameDescription}, textHandler(s.Blame))
	return srv
}

// textHandler adapts a text-returning tool method to the SDK's typed
// handler signature. Errors are surfaced by the SDK as tool errors.
func textHandler[In any](fn func(context.Context, In) (string, error)) sdk.ToolHandlerFor[In, any] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, any, error) {
		text, err := fn(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: text}},
		}, nil, nil
	}
}
