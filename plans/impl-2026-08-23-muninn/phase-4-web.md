# Phase 4: Web UI

> **Status:** COMPLETED
> **Spec:** `plans/design-2026-08-23-muninn.md`
> **Depends on:** Phase 3 merged (search core + gitfile)

Delivers `muninn web`: an on-demand local server with a search page (zoekt query syntax, highlighted snippets) and a capped file viewer. **Scope discipline:** search-only — no tree browsing, no saved searches, no branch pickers (spec exclusions).

## Specification (excerpt)

**Goal:** `muninn web` starts instantly (mmap'd index), serves a fast minimal search UI at localhost, and exits on Ctrl-C leaving nothing resident.

**Success Criteria:**

- [ ] `muninn web` serves search with `repo:`/`file:`/`sym:` syntax and highlighted snippets
- [ ] Clicking a result opens a syntax-highlighted file view scrolled to the line
- [ ] All assets embedded — binary works offline, no Node/CDN
- [ ] File viewer enforces size cap and rejects binary files gracefully

## Context Loading

_Run before starting:_

```bash
read /Users/broderick.westrope/dev/helse/muninn/plans/design-2026-08-23-muninn.md
read /Users/broderick.westrope/dev/helse/muninn/internal/search/search.go
read /Users/broderick.westrope/dev/helse/muninn/internal/gitfile/gitfile.go
read /Users/broderick.westrope/dev/helse/muninn/internal/mcp/tools_search.go   # reuse result shaping where sensible
```

## Web Tasks

### Task 1: HTTP server + JSON API

**Files:**
- Create: `internal/web/server.go`, `internal/web/api.go`, `internal/web/api_test.go`
- Modify: `internal/cli/web.go`

**Steps:**

1. [ ] `muninn web` command: flags `--addr` (default `127.0.0.1:7576`), `--open` (default true: open browser via `open <url>`). Loads config, opens searcher, serves until SIGINT/SIGTERM; graceful shutdown. Bind loopback only — refuse non-local addrs without an explicit `--unsafe-listen` flag (it's an unauthenticated index of private code).
2. [ ] `GET /api/search?q=<query>&limit=<n>` → JSON of `search.Result` (limit default 50, max 200). Query parse errors → 400 with the parser message (shown inline in UI).
3. [ ] `GET /api/file?repo=<owner/name>&path=<p>` → JSON `{content, language, indexedCommit, totalLines}`; resolve indexed commit via status file (fresh read per request) + `gitfile.ReadFile` with **limit=0 (full file)** — the viewer needs complete content for scroll-to-line, unlike the MCP tool's 500-line default; 10 MiB / binary-content guard → 413/415 with friendly message.
4. [ ] `GET /api/repos` → repo list with index age (reuses `search.ListRepos` + staleness).
5. [ ] Tests: `httptest` against the fixture index — search happy path, parse error 400, file fetch, size guard, loopback enforcement.

**Verify:**
```bash
go test ./internal/web/
# Expected: PASS
```

### Task 2: Embedded UI (search page + file viewer)

**Files:**
- Create: `internal/web/static/index.html`, `internal/web/static/app.js`, `internal/web/static/style.css`, `internal/web/static.go` (`//go:embed static`)

**Steps:**

1. [ ] Single-page vanilla JS (no framework, no build step): search input (debounced 200ms, `Enter` forces), syntax hint line under the box (`repo: file: sym: lang: -negation "exact"`), results as file cards grouped by repo with highlighted match ranges and line numbers, stats footer (matches, files, duration, truncation notice).
2. [ ] File viewer route (`#/file/<repo>/<path>:<line>`): fetch via API, render with line numbers, scroll to + highlight the target line. Server-side syntax highlighting with `github.com/alecthomas/chroma/v2` (add `highlighted` HTML to the file API response; sanitize by construction — chroma emits escaped HTML).
3. [ ] Keep it small and fast: dark/light via `prefers-color-scheme`, no icons/fonts fetched remotely, total embedded assets < 50 KB.
4. [ ] URL state: query kept in `?q=` so searches are shareable/bookmarkable.

**Verify:**
```bash
go build ./... && go test ./...
# Expected: PASS
# Manual acceptance: `go run . web` against a synced index — search "repo:muninn func main", click through to file view, confirm highlight + scroll; Ctrl-C exits cleanly, `ps` shows nothing resident
```

## Verify Phase

```bash
cd /Users/broderick.westrope/dev/helse/muninn && go vet ./... && go test ./... -race && go build ./...
# Expected: all PASS
```

Final acceptance against the spec's success criteria (full eucalyptusvc sync, MCP end-to-end with an agent, launchd freshness, idle footprint zero, Sourcebot stack shut down).

Create PR (or reviewed commit) for human review.
