// Package mcp implements muninn's MCP server, served either to one client
// over stdio (Run) or to many sessions over a Unix listener (Serve). It
// exposes ten tools (grep, glob, list_repos, read_file, list_tree,
// find_symbol_definitions, find_symbol_references, search_commits,
// get_diff, blame) over the search core, the status file, and the bare
// git mirrors.
package mcp

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/broderick-westrope/muninn/internal/search"
	"github.com/broderick-westrope/muninn/internal/status"
)

// staleAfter is how long a repo may go without a successful sync before
// tools warn that its data is old. It aliases the shared definition so the
// MCP server and the web API cannot disagree.
const staleAfter = status.StaleAfter

// Warning fragments, shared so every tool renders identical wording.
//
// Staleness is a property of a repo, not of the fleet: it is measured from
// that repo's last *successful* sync. A failing run still updates
// FinishedAt, so warning on that would mean a broken index never looks
// stale; and one unreachable repo must not make the other forty-nine
// report as old.
const (
	noStatusWarning       = "WARNING: no sync status found; the index may be empty or stale — run `muninn sync`\n"
	futureClockWarning    = "WARNING: the last sync is dated in the future; the system clock has moved backwards, so the index age is unknown — run `muninn sync`\n"
	interruptedWarning    = "WARNING: a sync is in progress or the last one was interrupted; the index may be mid-update — run `muninn status`\n"
	degradedWarningFormat = "WARNING: the last %d sync attempt(s) failed outright (%s); the index is no longer being updated — run `muninn status`\n"

	// Repo-scoped: rendered when the agent is working with one known repo,
	// so it describes the data actually being returned.
	repoStaleWarningFormat = "WARNING: %s was last synced %s ago; the content below may be out of date — run `muninn sync`\n"
	repoNeverSyncedFormat  = "WARNING: %s has never been synced successfully; the content below may be missing or stale — run `muninn status`\n"

	// Fleet-scoped: only list_repos, which is the one tool whose job is to
	// describe the whole index.
	fleetStaleWarningFormat = "WARNING: %d of %d repos have not synced in over %s: %s — run `muninn status`\n"
	neverSucceededWarning   = "WARNING: no sync has ever succeeded; the index may be empty — run `muninn sync` to see why\n"
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
list_repos to see which repos are indexed and how old each one is; tools
warn when the repo they touched is stale or when syncing itself is
failing.`

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

// formatWarnings renders the fleet-wide warnings for st: whether the sync
// mechanism is working, and which repos have fallen behind. It is used by
// list_repos, whose job is to describe the whole index. Tools that operate
// on a single repo use repoWarning instead, so their output is bounded and
// describes the data they are actually returning.
//
// Stale repo names are capped: fifty names repeated in every response is
// noise an agent cannot act on, and `muninn status` has the full list.
func formatWarnings(st *status.SyncStatus) string {
	var b strings.Builder
	b.WriteString(mechanismWarning(st))

	stale := status.StaleRepos(st, staleAfter)
	if len(stale) == 0 {
		return b.String()
	}
	if _, ok := status.SuccessAge(st); !ok && len(stale) == len(st.Repos) {
		// Nothing has ever synced; naming every repo adds nothing.
		b.WriteString(neverSucceededWarning)
		return b.String()
	}
	fmt.Fprintf(&b, fleetStaleWarningFormat,
		len(stale), len(st.Repos), formatAge(staleAfter), summarizeRepos(stale, 3))
	return b.String()
}

// repoWarning renders the warnings relevant to a single repo: whether the
// sync mechanism is broken, and whether this repo's own data is old.
func repoWarning(st *status.SyncStatus, repo string) string {
	var b strings.Builder
	b.WriteString(mechanismWarning(st))

	age, ok := status.RepoSuccessAge(st, repo)
	switch {
	case !ok:
		fmt.Fprintf(&b, repoNeverSyncedFormat, repo)
	case age < 0:
		b.WriteString(futureClockWarning)
	case age > staleAfter:
		fmt.Fprintf(&b, repoStaleWarningFormat, repo, formatAge(age))
	}
	return b.String()
}

// mechanismWarning reports that syncing itself is broken, which is
// independent of any repo's age: the fleet may be minutes fresh and still
// have a sync that can no longer run.
func mechanismWarning(st *status.SyncStatus) string {
	if status.Interrupted(st) {
		// An interrupted run records no cause, so the degraded wording
		// ("attempts failed") would be both unsupported and misleading.
		return interruptedWarning
	}
	if status.Degraded(st) {
		reason := st.Error
		if reason == "" {
			reason = "cause not recorded"
		}
		return fmt.Sprintf(degradedWarningFormat, max(st.ConsecutiveFailures, 1), reason)
	}
	return ""
}

// summarizeRepos joins up to limit names, noting how many were omitted.
func summarizeRepos(names []string, limit int) string {
	if len(names) <= limit {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:limit], ", "), len(names)-limit)
}

// textHandler adapts a text-returning tool method to the SDK's typed
// handler signature. Errors are surfaced by the SDK as tool errors.
//
// A panic becomes a tool error too. The SDK doesn't recover panics in
// handlers, and in a shared daemon one would end every client's session,
// not just the caller's.
func textHandler[In any](fn func(context.Context, In) (string, error)) sdk.ToolHandlerFor[In, any] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in In) (_ *sdk.CallToolResult, _ any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[ERROR] tool handler panicked: %v\n%s", r, debug.Stack())
				err = fmt.Errorf("internal error: %v", r)
			}
		}()
		text, err := fn(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: text}},
		}, nil, nil
	}
}
