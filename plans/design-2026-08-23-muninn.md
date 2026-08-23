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
  - `find_symbol_definitions` — ctags-backed via Zoekt `sym:` search (ctags extracts definitions only)
  - `find_symbol_references` — **approximate**: word-boundary identifier text search with definition sites excluded, hard result cap, grouped per repo. The MCP tool description must state this is approximate so agents don't over-trust it.
- **Sync command**: `muninn sync` reconciles (fetch + index + GC, see below); `muninn sync --install` writes a launchd agent for scheduled runs; `--uninstall` removes it.
- **Sync as reconciliation**, not a fetch loop:
  - Diff discovered repos vs on-disk state: clone new, fetch existing, **delete mirrors and shards** for repos that are removed/renamed/archived/newly-excluded.
  - Per-repo isolation: one failing repo (403, force-push, huge pack) never aborts the run; continue on error.
  - Clean orphaned `.tmp` shard files at start; rely on Zoekt's temp-write-then-atomic-rename for shard integrity, and hold the same discipline in any muninn-written files.
  - Cap concurrent index jobs (1–2) so pilot-engine indexing doesn't recreate the laptop-memory pain during sync; set Zoekt `SizeMax`/shard limits deliberately.
  - Record the **indexed commit per repo** in the status file.
- **Sync observability**:
  - Status file (JSON) under `~/.local/state/muninn/`: last run timestamp, per-repo result (fetched/indexed/failed + error), indexed commit.
  - launchd stdout/stderr redirected to a log under `~/.local/state/muninn/`.
  - The MCP server checks index age at startup and includes a staleness warning in `list_repos` output when the last successful sync is older than a threshold.
  - `muninn status` prints last sync result and per-repo failures.
- **Web UI**: `muninn web` starts a local server on demand (Ctrl-C to stop). Search box supporting Zoekt query syntax (`repo:`, `file:`, `sym:`, regex), results with highlighted snippets, click-through file viewer with syntax highlighting. Embedded assets, no Node.
- **CLI search**: `muninn search "pattern"` for terminal use and index debugging.
- **Config**: JSON file modeled on Sourcebot's v3 connection schema subset — GitHub `orgs`, `exclude` (archived via API flag + repo list), explicit `repos` for ad-hoc additions.
- **Auth**: GitHub token resolved at `--install` time and stored in the config file with `0600` perms (launchd jobs don't inherit shell env; `gh auth token` may hit keychain prompts non-interactively). Fetches use HTTPS with the token. Interactive runs may still fall back to env/`gh`.
- **ctags handling**: resolve the ctags binary path at `--install`/sync start (Homebrew names it `ctags`, and launchd's default PATH excludes `/opt/homebrew/bin`), verify it is universal-ctags with the `+interactive` feature, write the absolute path into config, and pass it to Zoekt explicitly rather than relying on PATH lookup. Fail loudly with an actionable message if missing/invalid.

Tool semantics:

- `glob`: translated to Zoekt `file:` regex (supporting `*`, `**`, `{a,b}`), case-insensitive by default — no git tree walk.
- All search/symbol tools enforce result-size limits (count caps + truncation notices) to protect agent context budgets.
- `read_file`/`list_tree` read from the bare mirror **at the indexed commit** recorded in the status file — never mirror HEAD — so line numbers always agree with search results even if a fetch succeeded but indexing failed.
- Web file viewer is deliberately capped: chroma-highlighted plain view, file-size limit, no images, no tree browsing.

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
- Freshness: scheduled sync via launchd `StartInterval` (default hourly); missed runs while asleep coalesce into one run on wake — acceptable. Failures are surfaced via the status file, log, `muninn status`, and MCP staleness warnings, never silent.
- Long-lived MCP sessions must see index updates: use Zoekt's directory-watching searcher (`shards.NewDirectorySearcher`) so shards refresh mid-session and deleted shards are released (verify watcher behavior on macOS). Concurrent sync + query is safe only via atomic-rename discipline.
- Disk budget: bare mirrors + shards for 200+ repos estimated 10–30 GB; reconciliation GC keeps it bounded.
- Storage: XDG paths — data (bare clones + index shards) under `~/.local/share/muninn/`, config at `~/.config/muninn/config.json`.
- Prerequisite: universal-ctags (with `+interactive`) is a hard requirement for symbol extraction — fail loudly at sync if missing or invalid.
- macOS (darwin/arm64) is the only supported platform for v1 (launchd integration); core should remain portable.
- Single static Go binary; web assets embedded via `embed`.

**Success Criteria:**

- [ ] `muninn sync` reconciles all non-excluded `eucalyptusvc` repos: clones/fetches bare mirrors, builds Zoekt shards including symbols, and GCs removed/renamed/archived repos (mirrors + shards).
- [ ] `muninn sync --install` registers a launchd agent that keeps the index fresh without user intervention, with ctags path and token validated and baked in at install time.
- [ ] A failed scheduled sync is visible: status file + log written, `muninn status` reports it, and the MCP server warns about a stale index.
- [ ] One failing repo does not abort a sync run; failures are recorded per repo.
- [ ] Peak sync RSS stays bounded via capped index-job concurrency; full-org sync completes in a reasonable time (measure; target <15 min warm, indexing only changed repos).
- [ ] MCP server exposes all 7 v1 tools and works end-to-end with an agent (e.g. answering "where is X defined?" across the org).
- [ ] Idle footprint is zero (no resident processes between agent sessions / web sessions / sync runs).
- [ ] Typical grep/symbol queries over the full index return in <1s warm (cold first-query after reboot may page in shards and take longer; measure both).
- [ ] `read_file` line numbers always match `grep` results (reads pinned to indexed commit).
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
- **ctags as hard prerequisite**: user accepted one Homebrew dependency; failing loudly avoids silently degraded symbol search. Path resolved and validated at install time because launchd PATH excludes Homebrew.
- **`find_symbol_references` is approximate by design**: neither ctags nor Zoekt index references; a word-boundary search minus definition sites is the honest implementation. Capped and labeled so agents treat it as a lead-generator, not ground truth.
- **Token in 0600 config, not env**: launchd doesn't inherit shell env, and keychain-backed `gh` can prompt non-interactively; explicit at-rest storage with tight perms is the least-surprise option.
- **Zoekt pinned + wrapped**: Zoekt has no stable library API (recent `build`→`index` restructure, churn driven by Sourcegraph). Pin a commit, keep all Zoekt types behind the core-lib interface so frontends never import Zoekt, budget for upgrade breakage.
- **Core lib + frontends layering**: MCP, CLI, and web are thin frontends over one core (sync/index/search/git), guaranteeing future surfaces (editor integration, commits/diff tools) bolt on without rework.

**Implementation Notes (from design review, resolve during planning):**

- Protect indexed commits from force-push + git gc on mirrors: write a `refs/muninn/indexed` ref at index time (and/or `gc.auto=0`); on unreachable commit, return a clear "index/mirror mismatch" error, not a raw git object error.
- Confirm the pinned Zoekt commit's package layout (`NewDirectorySearcher` recently moved `shards` → `search`) when writing the core-lib wrapper.
- Pick the definition-exclusion mechanism for `find_symbol_references` (`-sym:` query negation vs post-filter via `SymbolInfo`); filter before applying result caps.
- Apply atomic write-then-rename to the status file too, since a live MCP session reads it while sync rewrites it.

**Context Files:**

- `/Users/broderick.westrope/dev/helse/sourcebot/config.json` — existing config to remain migration-compatible with
- `/Users/broderick.westrope/dev/helse/sourcebot/supervisord.conf` — shows zoekt-webserver as Sourcebot's search engine
- `/Users/broderick.westrope/dev/helse/sourcebot/docker-compose.yml` — current stack being replaced
- Upstream: `github.com/sourcegraph/zoekt` (index + search library), `github.com/modelcontextprotocol/go-sdk` (MCP server)
