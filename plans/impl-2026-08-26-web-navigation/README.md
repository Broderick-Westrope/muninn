# Web UI Navigation Implementation Plan

> **Status:** DRAFT

## Overview

The muninn web UI is search-only. Opening a file is a dead end, narrowing a broad query means hand-typing zoekt atoms, repo names render quieter than their own children and scroll off screen, and open-in-editor drops the file into whichever window was last focused with no repo loaded. This plan adds a sidebar with a lazy file tree, toggleable result facets, prominent sticky repo headers, and an editor launch that loads the repo.

Full problem statement, design decisions, and the alternatives considered live in the spec: [`../design-2026-08-26-web-navigation.md`](../design-2026-08-26-web-navigation.md). Read it before starting any phase.

Phased because the work spans git plumbing (`gitfile`), zoekt query composition (`search`), HTTP handlers with a new process-exec surface (`web`), and frontend layout — a single PR would force a reviewer across four unrelated expertise areas, including a security-sensitive endpoint that deserves focused attention.

## Phases

| # | File | Delivers | Depends on | Review focus |
|---|------|----------|------------|--------------|
| 1 | `phase-1-modules.md` | `app.js` split into ES modules, zero behaviour change | — | Did the code move faithfully? Any behaviour drift? |
| 2 | `phase-2-layout.md` | Width cap removed, `--sticky-top`, sticky repo headers with counts, empty sidebar shell | Phase 1 | CSS correctness, sticky offsets, the `#hints` trap |
| 3 | `phase-3-tree.md` | `gitfile.ListDir`, `GET /api/tree`, sidebar file tree | Phase 2 | git pathspec semantics, error mapping, abort/race handling |
| 4 | `phase-4-facets.md` | `Options.FileFilter`, server-side facet aggregation, facet UI | Phase 3 | Query AST composition, regex escaping, facet feedback loop |
| 5 | `phase-5-editor.md` *(parallel with 3 and 4)* | Scheme→binary map, `POST /api/open`, CSRF guard, client wiring | Phase 1 | **Security**: CSRF, path containment, argv construction |

> Phase 5 is parallel with 3 and 4: it touches the file-view header actions, not the sidebar. It needs Phase 1's modules but nothing from 2, 3, or 4. Merge it in whatever order suits review capacity — it is the phase most worth reviewing while fresh.

## Phase Boundaries

- **1 → 2:** The module split is a mechanical move. Isolating it means Phase 2's CSS diff is readable as actual change rather than buried in relocated code.
- **2 → 3:** Phase 2 delivers the sidebar shell and the `--sticky-top` fix that the tree sits inside. The tree arrives as the shell's first occupant.
- **3 → 4:** Both occupy the sidebar and touch `sidebar.js`. Sequential to avoid conflicting edits to the same module.
- **1 → 5:** Independent of the sidebar entirely.

## Testing Reality

The repo has no JavaScript test infrastructure (no `package.json`, no runner) and the spec's no-npm constraint rules out adding a headless-browser harness. Go tests cover `gitfile`, `search`, and every HTTP handler; client behaviour is verified by hand against the checklist in each phase. Every phase ends with a manual verification section — work through it before opening the PR.

## Execution

Each phase is run independently and ends with a PR for human review. Do not merge across phases automatically; the merge gate between phases is manual.

Before starting any phase:

```bash
cat plans/design-2026-08-26-web-navigation.md
go build ./... && go test ./...
```

## Review notes

The spec went through three adversarial review passes before planning; the plan through one. What review caught, and where the fix landed:

**Spec passes (already reflected in the design):**
- Facet filters concatenated onto the query string break on a top-level `or` — zoekt's `or` binds looser than implicit AND, so half a disjunction goes unfiltered, silently. Now composed via `query.NewAnd` at the AST level.
- Deriving facet values from the filtered results makes multi-select unbuildable and zero-result selections unrecoverable. Aggregation moved server-side, ignoring the facet params.
- Dropping `-r` from `ls-tree` would break `ListTree`'s anchor self-entry contract and `TestListTreeDepth`. Split into a separate `ListDir`.
- `editor.scheme` is not an executable name — VS Code's CLI is `code`. Without the map every `vscode` user silently falls back forever.
- `--header-h` excludes the `#hints` row, which is hidden only in the file view, so sticky group headers would slide under the chips in the one view that has them.

**Plan pass:**
- The no-extension regex `(^|/)[^/.][^/]*$` was wrong in both directions — it matched `a/b.go` and excluded `.gitignore`, disagreeing with `extOf`. Corrected to `(^|/)\.?[^/.]+$` and verified against nine paths (phase 4).
- "Skip the aggregation pass when no facets are selected" reintroduced the exact count-instability bug the design exists to prevent, because the two passes run at different caps — and the named test would have passed anyway on a 4-file fixture. Removed (phase 4).
- Facet counts specified as files while displayed rows are line matches, breaking the "count never lower than rows" criterion. Now line matches (phase 4).
- `facets.js` ↔ `search.js` import cycle, contradicting phase 1's own stated graph. Now an injected callback (phase 4).
- Phase 5's `handleOpen` had a `filepath.Dir(checkoutRoot(...))` placeholder that, followed literally, would open the repo's **parent** as the workspace folder. Replaced with a concrete two-return `resolveInCheckout`.
- Phase 5's status table, code, and tests disagreed three ways on unknown-repo and zero-line. Reconciled, and the spec's table updated to match.
- Phase 1 omitted a `resetFile()` export the router needs to abort an in-flight file fetch — a silent behaviour change in the one phase whose goal is no behaviour change. Also added `routes.js` to break a `file.js → search.js` edge.
- Both existing test fixtures are single-repo and all-`.go`, so phase 4's most important test could not fail against the bug it guards. Added an explicit fixture task (phase 4, Task 0).
- One shared `#sidebar` node let a slow tree response paint over the facet panel after Esc. Split into two containers toggled by `data-view` (phase 2).

Verified empirically during the plan pass: `query.Parse` accepts a grouped, anchored, alternated `file:` regex and `NewAnd` composes it correctly; `git ls-tree -- dir` returns only the anchor while `-- dir/` returns children; `node --check` handles ES modules on Node 22.21; `cursor` and `code` resolve on PATH, `vscode` does not.
