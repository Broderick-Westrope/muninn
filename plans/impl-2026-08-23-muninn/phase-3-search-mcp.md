# Phase 3: Search Core + MCP + CLI Search

> **Status:** COMPLETED
> **Spec:** `plans/design-2026-08-23-muninn.md`
> **Depends on:** Phase 2 merged (real shards + indexed-commit refs on disk)

Delivers the search core (Zoekt wrapped behind muninn types), the stdio MCP server with all 7 v1 tools, and `muninn search` for the terminal.

## Specification (excerpt)

**Goal:** An agent connected to `muninn mcp` can grep/glob/read/list/symbol-search across all indexed repos with capped, context-budget-friendly results; staleness is surfaced; `read_file`/`list_tree` are pinned to the indexed commit.

**Success Criteria:**

- [ ] All 7 tools work end-to-end against a real index
- [ ] No zoekt import outside `internal/search` (and `internal/index` from phase 2)
- [ ] Every search tool enforces result caps with explicit truncation notices
- [ ] `read_file` line numbers match `grep` results (indexed-commit pinning)
- [ ] `list_repos` includes a staleness warning when the index is old
- [ ] Long-lived MCP session picks up shards written by a concurrent sync

## Context Loading

_Run before starting:_

```bash
read /Users/broderick.westrope/dev/helse/muninn/plans/design-2026-08-23-muninn.md
read /Users/broderick.westrope/dev/helse/muninn/internal/index/indexer.go
read /Users/broderick.westrope/dev/helse/muninn/internal/status/status.go
read /Users/broderick.westrope/dev/helse/muninn/internal/mirror/mirror.go
```

At the pinned zoekt commit, locate: the directory-watching searcher constructor (historically `shards.NewDirectorySearcher`, recently moved toward `search` — confirm), `query.Parse`/query node types, `zoekt.SearchOptions` (`MaxDocDisplayCount`, `MaxMatchDisplayCount`, etc.), and per-line symbol info on matches. For the MCP server, add `github.com/modelcontextprotocol/go-sdk` (official SDK; stdio transport).

## Search Core Tasks

### Task 1: Search core wrapper

**Files:**
- Create: `internal/search/search.go`, `internal/search/types.go`, `internal/search/search_test.go`

**Steps:**

1. [ ] `Open(indexDir string) (*Searcher, error)` wrapping zoekt's directory-watching searcher so a long-lived process sees shard adds/removes mid-session (spec constraint). `Close()` releases it.
2. [ ] Muninn-owned result types (no zoekt types in signatures):

```go
type Options struct {
    Query      string   // zoekt query syntax (repo:, file:, sym:, lang:, regex)
    RepoFilter string   // optional repo name regex, ANDed in
    MaxResults int      // hard cap on returned line matches
    GroupByRepo bool
}
type Result struct {
    Files     []FileMatches
    Truncated bool
    Stats     Stats // files considered, match count, duration
}
type FileMatches struct {
    Repo, Path string
    Lines      []LineMatch
}
type LineMatch struct {
    LineNumber int
    Line       string // full line text, trimmed to 500 bytes
    IsSymbolDef bool  // true when zoekt reports symbol info for the match
}
```

3. [ ] `Search(ctx, opts) (*Result, error)`: parse query via zoekt's parser (surface parse errors verbatim — agents can fix their own syntax), AND in the repo filter, set zoekt display limits from `MaxResults`, mark `Truncated` when limits hit.
4. [ ] `ListRepos(ctx) ([]RepoInfo, error)` from the searcher's repo list: name, branch, indexed commit (from shard metadata).
5. [ ] Tests against a fixture index built with phase-2 `internal/index` (self-skip without ctags): literal search, regex, `file:` filter, `sym:` query, cap + truncation flag, repo listed.

**Verify:**
```bash
go test ./internal/search/
# Expected: PASS
grep -rn "sourcegraph/zoekt" --include="*.go" . | grep -v internal/search | grep -v internal/index
# Expected: no output (encapsulation holds)
```

### Task 2: Pinned-commit file access

**Files:**
- Create: `internal/gitfile/gitfile.go`, `internal/gitfile/gitfile_test.go`

**Steps:**

1. [ ] `ReadFile(mirrorDir, commit, path string, offset, limit int) (content string, totalLines int, err error)` via `git -C <dir> cat-file blob <commit>:<path>` (shell out; stream, don't slurp files >10 MiB — return size error instead). Offset/limit are line-based, 1-indexed, default limit 500 lines; **`limit=0` means whole file** (needed by the phase-4 web viewer).
2. [ ] `ListTree(mirrorDir, commit, path string, depth int) ([]TreeEntry, error)` via `git ls-tree` (recursive up to depth), entries typed file/dir.
3. [ ] Both take the commit from the **status file's `IndexedCommit`** (callers resolve it) — never mirror HEAD (spec). If the commit is missing from the mirror (index/mirror mismatch), return a typed `ErrIndexMismatch` with a "run muninn sync" message, not a raw git error (spec implementation note).
4. [ ] Tests with local fixture repos: read at older commit after a new commit lands (content is the old version), offset/limit windows, missing path, ErrIndexMismatch on a bogus commit.

**Verify:**
```bash
go test ./internal/gitfile/
# Expected: PASS
```

## MCP Tasks

### Task 3: MCP server + search tools (`grep`, `glob`, `list_repos`)

**Files:**
- Create: `internal/mcp/server.go`, `internal/mcp/tools_search.go`, `internal/mcp/tools_search_test.go`
- Modify: `internal/cli/mcp.go`

**Steps:**

1. [ ] `muninn mcp` command: load config, open `search.Searcher`, serve MCP over **stdio** using the official go-sdk. Server name `muninn`. **Status-file read policy: re-read per call** — `read_file`/`list_tree` resolve the indexed commit and `list_repos` computes staleness from a fresh read of the status file on every invocation (cheap; atomic-rename-safe). Never cache it at startup: a mid-session sync updates shards via the directory watcher, and stale cached commits would break the read_file/grep line-number guarantee.
2. [ ] Staleness: if the status file is older than 24h (or missing), prepend a warning line to `list_repos` output; also note the possibility in the server instructions string (spec requirement: agents must not silently trust a stale index).
3. [ ] `grep` tool — params: `pattern` (regex), `repo` (name regex, optional), `include` (file glob, optional), `literal` (bool), `group_by_repo` (bool), `limit` (default 50 line matches, max 200). Output: compact text — `repo/path:line: content` blocks, truncation notice with the count of omitted matches, stats line. Glob→`file:` regex translation shared with the `glob` tool.
4. [ ] `glob` tool — params: `pattern` (glob: `*`, `**`, `{a,b}`), `repo` (optional), `limit` (default 100). Translate glob to an anchored case-insensitive `file:` regex (write the translator + table test: `**/*.go`, `src/**/*.{ts,tsx}`, `cmd/*/main.go`); query zoekt with a match-nothing content atom trick or file-only search (confirm the idiomatic file-list query at the pinned commit). Output: file paths grouped by repo.
5. [ ] `list_repos` tool — params: `query` (name filter, optional). Output: repo name, branch, indexed commit (short), index age; staleness warning per step 2.
6. [ ] Tool descriptions must state limits and syntax (agents read these): document zoekt query atoms in the grep description.
7. [ ] Tests: tool handlers called directly against the fixture index (no stdio needed): each tool's happy path + cap behavior + glob translation table. Fixture index is built with phase-2 `internal/index` — **self-skip without universal-ctags**, same as the search-core tests.

**Verify:**
```bash
go test ./internal/mcp/ && go build ./...
# Expected: PASS
```

### Task 4: MCP file + symbol tools (`read_file`, `list_tree`, `find_symbol_definitions`, `find_symbol_references`)

**Files:**
- Create: `internal/mcp/tools_file.go`, `internal/mcp/tools_symbol.go`, `internal/mcp/tools_symbol_test.go`
- Test: extend `internal/mcp/tools_search_test.go` fixtures

**Steps:**

1. [ ] `read_file` tool — params: `repo` (exact `owner/name`), `path`, `offset`, `limit`. Resolve indexed commit from status file → `gitfile.ReadFile`. Output includes the line range and total lines (mirrors Sourcebot UX). `ErrIndexMismatch` → clear message.
2. [ ] `list_tree` tool — params: `repo`, `path` (default root), `depth` (default 1, max 10). Via `gitfile.ListTree` at indexed commit.
3. [ ] `find_symbol_definitions` tool — params: `symbol` (exact identifier), `repo` (optional regex), `limit` (default 50). Query: `sym:` atom with word-boundary anchoring and **`case:yes`** (zoekt smart-case would otherwise match differently-cased identifiers). Output: `repo/path:line: <line>` grouped by file.
4. [ ] `find_symbol_references` tool — **approximate by design** (spec): word-boundary content search `\b<escaped>\b` with `case:yes`, then **filter out lines where `IsSymbolDef` is true before applying the cap** (spec implementation note: filter before cap so truncation counts stay honest; do NOT use `-sym:` negation — it excludes whole files, dropping references co-located with definitions). Params: `symbol`, `repo` (optional), `limit` (default 100, max 300). Tool description MUST say: "Approximate: text-based reference search excluding definition sites. Treat as leads, not ground truth."
5. [ ] Tests: fixture repo with a Go function defined once and called twice → definitions returns 1, references returns 2 (definition line excluded); cap + truncation notice.

**Verify:**
```bash
go test ./internal/mcp/
# Expected: PASS
# Manual acceptance: add muninn to an agent's MCP config ({"command":"muninn","args":["mcp"]}), ask it "where is <symbol> defined?" against a synced test index
```

## CLI Tasks

### Task 5: `muninn search` terminal command

**Files:**
- Modify: `internal/cli/search.go`

**Steps:**

1. [ ] `muninn search "<query>"` runs a zoekt-syntax query via the search core; flags: `--repo`, `--limit` (default 50), `--files-only`. Output: `repo/path:line: content`, ANSI color for the matched range when stdout is a TTY, plain otherwise (pipe-friendly). Print stats + truncation notice to stderr.
2. [ ] Exit codes: 0 with matches, 1 no matches, 2 error (grep convention).

**Verify:**
```bash
go build ./... && go test ./...
# Expected: PASS
# Manual: `go run . search "func main" --limit 5` against a synced index prints colored matches
```

## Verify Phase

```bash
cd /Users/broderick.westrope/dev/helse/muninn && go vet ./... && go test ./... -race && go build ./...
# Expected: all PASS
# Acceptance: full eucalyptusvc index — typical grep/symbol queries return <1s warm; also measure one cold query after dropping page cache/reboot (spec measures both); long-lived `muninn mcp` session sees results from a sync run mid-session
```

Create PR (or reviewed commit) for human review.
