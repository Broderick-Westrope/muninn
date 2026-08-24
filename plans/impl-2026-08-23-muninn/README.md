# Muninn Implementation Plan

> **Status:** COMPLETED

**Spec:** `plans/design-2026-08-23-muninn.md` — read it before executing any phase.

## Overview

Muninn is a single Go binary replacing a locally-hosted Sourcebot Docker stack (Next.js + zoekt-webserver + Postgres + Redis) for two workflows: code-search MCP tools for AI agents over 200+ GitHub org repos, and occasional interactive search via a web UI. Core constraint: **zero idle memory** — stdio MCP lives only during agent sessions, web server only while in use, sync is a short-lived launchd-scheduled job. Zoekt is embedded as a library (pinned commit, wrapped behind a core-lib interface so Zoekt types never leak into frontends).

Phased because the work spans independent domains: foundation (config/paths/status), sync engine (GitHub + git + indexing + launchd), search core + MCP frontend, and CLI/web surfaces.

## Phases

| # | File | Delivers | Depends on | Review focus |
|---|------|----------|------------|--------------|
| 1 | `phase-1-foundation.md` | Module scaffold, config loading, XDG paths, status file, ctags detection | — | Config schema compat, atomic-write discipline |
| 2 | `phase-2-sync.md` | GitHub discovery, mirror reconciliation, Zoekt indexing, `sync`/`status` commands, launchd install | Phase 1 | GC correctness, per-repo failure isolation, launchd env baking |
| 3 | `phase-3-search-mcp.md` | Search core (Zoekt wrapper), MCP server with all 7 tools, CLI search | Phase 2 | Zoekt type encapsulation, result caps, indexed-commit pinning |
| 4 | `phase-4-web.md` | On-demand web UI: search + file viewer | Phase 3 | Scope discipline (search-only), embedded assets |

## Phase Boundaries

- **1 → 2:** Foundation isolates config/status/ctags plumbing so sync logic builds on reviewed primitives.
- **2 → 3:** Search needs real shards on disk to verify against; MCP `read_file` needs the indexed-commit refs written by sync.
- **3 → 4:** Web UI is a thin frontend over the phase-3 search core; no new search logic allowed.

## Execution notes

- Each phase ends with "create PR for human review" (or a reviewed commit — single-user repo, user's call).
- Verification of phases 2–4 requires network access to GitHub and universal-ctags installed (`brew install universal-ctags`). Use a small test config (2–3 public repos) during development; full `eucalyptusvc` sync is a final acceptance step, not a per-task verify.

## Review notes

Devils-advocate review (1 iteration) caught two majors, both fixed in the plan: (1) `clone --mirror` + `fetch --prune` would have deleted `refs/muninn/indexed` on every sync and pulled GitHub `refs/pull/*` bloat → switched to bare clone with narrow `+refs/heads/*` refspec + regression test; (2) status-file semantics — `IndexedCommit` is now carried forward across failed index runs, written incrementally per repo, and re-read per MCP call instead of cached at startup. Minors folded in: token via `GIT_CONFIG_*` env (not argv), idempotent `--install`, `case:yes` on symbol queries, MCP test ctags self-skip, web file API full-read, cold-query measurement. Review confirmed the `IsSymbolDef` post-filter approach against zoekt internals (correctly avoids `-sym:` file-level exclusion).
