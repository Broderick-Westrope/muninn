# Muninn Design Spec

**Problem:** Sourcebot provides excellent code-search MCP tools for agents, but runs as a heavy always-on Docker stack (Next.js app, zoekt-webserver, Postgres, Redis) that consumes significant resident memory on a laptop. The MCP use case — and occasional frontend search — doesn't need most of that stack.

**Goal:** A single Go binary (`muninn`) that replaces Sourcebot for the primary use cases:

1. An MCP server (stdio) exposing code search, symbol lookup, and file access tools over 200+ indexed repos, with near-zero idle memory cost.
2. A scheduled sync/index job with streamlined UX (self-installing launchd agent) that keeps the index fresh without a resident daemon.
3. An on-demand local web UI for interactive search.

**Scope:**

In (v1):

- **Core library**: repo sync (GitHub org auto-discovery + ad-hoc repos), Zoekt indexing (embedded as a library), search, and git file access — structured so multiple frontends share it.
- **MCP frontend** (stdio, official `modelcontextprotocol/go-sdk`) with tools:
  - `grep` — regex content search with repo/file filters and group-by-repo
  - `glob` — file path matching
  - `read_file` — read file contents with offset/limit
  - `list_tree` — directory listing from bare clones
  - `list_repos` — list indexed repos with filtering
  - `find_symbol_definitions` / `find_symbol_references` — ctags-backed via Zoekt symbol support
- **Sync command**: `muninn sync` fetches bare mirrors and (re)indexes; `muninn sync --install` writes a launchd agent for scheduled runs; `--uninstall` removes it.
- **Web UI**: `muninn web` starts a local server on demand (Ctrl-C to stop). Search box supporting Zoekt query syntax (`repo:`, `file:`, `sym:`, regex), results with highlighted snippets, click-through file viewer with syntax highlighting. Embedded assets, no Node.
- **CLI search**: `muninn search "pattern"` for terminal use and index debugging.
- **Config**: JSON file modeled on Sourcebot's v3 connection schema subset — GitHub `orgs`, `exclude` (archived + repo list), explicit `repos` for ad-hoc additions, token via env (or `gh auth token` fallback).

Out (v1, possible later):

- `list_commits` / `get_diff` MCP tools
- `ask_codebase` (the calling agent is already the reasoner)
- Multi-branch indexing (default branch only)
- Always-on web server, repo tree browsing in the UI, saved searches
- Editor/GitHub integration (open-in-Cursor links, GitHub permalinks) — explicit future direction
- Non-GitHub forges (GitLab, Gitea, etc.)

**Constraints:**

- Zero idle memory: stdio MCP process lives only during agent sessions; web server only while in use; sync is a short-lived scheduled process. No resident daemon, no Docker, no Postgres/Redis.
- Scale target: 200+ mid-sized repos plus one large repo (pilot-engine) in the `eucalyptusvc` org; queries should return in well under a second via mmap'd Zoekt shards.
- Freshness: scheduled sync (launchd) is sufficient; no realtime indexing.
- Storage: XDG paths — data (bare clones + index shards) under `~/.local/share/muninn/`, config at `~/.config/muninn/config.json`.
- Prerequisite: universal-ctags is a hard requirement for symbol extraction — fail loudly at sync if missing.
- macOS (darwin/arm64) is the only supported platform for v1 (launchd integration); core should remain portable.
- Single static Go binary; web assets embedded via `embed`.

**Success Criteria:**

- [ ] `muninn sync` clones/fetches all non-excluded `eucalyptusvc` repos as bare mirrors and builds Zoekt index shards, including symbols.
- [ ] `muninn sync --install` registers a launchd agent that keeps the index fresh without user intervention.
- [ ] MCP server exposes all 7 v1 tools and works end-to-end with an agent (e.g. answering "where is X defined?" across the org).
- [ ] Idle footprint is zero (no resident processes between agent sessions / web sessions / sync runs).
- [ ] Typical grep/symbol queries over the full index return in <1s.
- [ ] `muninn web` serves a usable search UI with Zoekt query syntax, snippets, and a file viewer.
- [ ] Config file migration from the existing Sourcebot `config.json` requires only trivial edits.
- [ ] Sourcebot Docker stack can be shut down with no loss for the primary workflows.

**Design Decisions:**

- **Embed Zoekt as a library** rather than live-grepping clones: at 200+ repos, on-demand regex search would be slow and memory-spiky — the exact problem being escaped. Zoekt shards are mmap'd, so cold-start cost of the stdio MCP process is negligible. (Index-free approach was considered and declined due to scale.)
- **stdio MCP, not HTTP**: process lifetime tied to the agent session gives zero idle cost for free; no port/daemon management.
- **Scheduled sync via launchd, not a daemon**: freshness needs are hours-to-daily; `--install` streamlines the UX so the external mechanism is invisible day-to-day.
- **Default branch only**: matches current Sourcebot usage; multi-branch inflates index size/sync time for little agent value. Zoekt supports it later if needed.
- **On-demand web server with custom minimal UI**: keeps zero-idle property; mmap'd index makes startup instant, so on-demand costs nothing in UX. Zoekt's stock UI was considered but declined (dated, less room to grow toward editor integration).
- **Sourcebot-compatible config subset**: familiar schema, trivial migration of the existing org/exclude list.
- **ctags as hard prerequisite**: user accepted one Homebrew dependency; failing loudly avoids silently degraded symbol search.
- **Core lib + frontends layering**: MCP, CLI, and web are thin frontends over one core (sync/index/search/git), guaranteeing future surfaces (editor integration, commits/diff tools) bolt on without rework.

**Context Files:**

- `/Users/broderick.westrope/dev/helse/sourcebot/config.json` — existing config to remain migration-compatible with
- `/Users/broderick.westrope/dev/helse/sourcebot/supervisord.conf` — shows zoekt-webserver as Sourcebot's search engine
- `/Users/broderick.westrope/dev/helse/sourcebot/docker-compose.yml` — current stack being replaced
- Upstream: `github.com/sourcegraph/zoekt` (index + search library), `github.com/modelcontextprotocol/go-sdk` (MCP server)
