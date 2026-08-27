# Phase 1: ES Module Split

> **Status:** DRAFT
> **Depends on:** —
> **Delivers:** `app.js` split into focused ES modules with zero behaviour change.

## Specification

**Problem:** `internal/web/static/app.js` is 593 lines in one file. Phases 2-5 add a sidebar shell, tree rendering, facet rendering, and an open-in-editor call, which would push it past ~900 lines. Large files are harder to edit reliably and harder to review.

**Goal:** The same UI, byte-for-byte identical in behaviour, served from focused ES modules split along the section comments already present in `app.js`.

**Scope:**

In: mechanical extraction into modules, `<script type="module">`, verification that nothing changed.

Out: any behaviour change, any styling change, any new feature. If you find a bug while moving code, leave it and note it in the PR — fixing it here makes the diff unreviewable.

**Success Criteria:**

- [ ] `app.js` is replaced by modules, each under ~250 lines
- [ ] `index.html` loads `main.js` with `type="module"`
- [ ] Every existing behaviour works identically (see Manual Verification)
- [ ] `go test ./internal/web/...` passes, including `TestUIStaticAssets`
- [ ] No file in `static/` contains a `TODO` or commented-out relocated code

## Context Loading

```bash
cat internal/web/static/app.js
cat internal/web/static/index.html
cat internal/web/static.go
sed -n '375,405p' internal/web/api_test.go   # TestUIStaticAssets
```

## Module Layout

Split at the existing `// ---` section comments. Target layout:

| Module | Source lines in `app.js` | Exports |
|---|---|---|
| `api.js` | 26-40 | `api` |
| `dom.js` | 71-150, 545-557 | `markMatches`, `copyBtn`, `icon`, `note`, `errorBox`, `plural`, `$` |
| `routes.js` | 155-158, 262-273 | `fileHash`, `parseFileHash` |
| `query.js` | 50-67, 489-519 | `bareTokens`, `initHints` |
| `search.js` | 160-256 | `runSearch`, `initSearch` |
| `file.js` | 275-450 | `showFile`, `targetLine`, `refreshFileActions`, `setCurrentFile`, `getCurrentFile`, `resetFile`, `initFile` |
| `repos.js` | 521-540 | `loadRepos` |
| `main.js` | 3-21, 452-484, 559-593 | — (entry point: element refs, routing, input wiring, init) |

`plural` moves to `dom.js` rather than staying with the stats code, because `file.js`, `search.js`, and `repos.js` all use it.

**`routes.js` exists to break a cycle.** `fileHash` is used by `search.js` (result links), `file.js` (the gutter click handler at `app.js:446`), `main.js` (routing), and `tree.js` in Phase 3. Leaving it in `search.js` would force `file.js → search.js`, contradicting the import graph below. `parseFileHash` joins it because both own the same hash-URL format and changing one without the other is a bug.

## Tasks

### Task 1: Extract leaf modules with no internal dependencies

**Context:** `internal/web/static/app.js`

**Files:**
- Create: `internal/web/static/api.js`, `internal/web/static/dom.js`, `internal/web/static/query.js`

**Steps:**

1. [ ] Create `api.js` containing the `api` function verbatim from `app.js:26-40`, exported:
   ```js
   export async function api(path, signal) { /* body unchanged */ }
   ```
2. [ ] Create `dom.js` containing, verbatim: the `$` helper (`app.js:3`), `ICONS` (104-112), `icon` (114-120), `copyBtn` (122-150), `markMatches` (69-98), `note` (545-550), `errorBox` (552-557), and `plural` (256). Export `$`, `icon`, `copyBtn`, `markMatches`, `note`, `errorBox`, `plural`. Keep `COPY_FEEDBACK_MS` (line 14) here — `copyBtn` is its only consumer.
3. [ ] Create `query.js` containing `bareTokens` (50-67), `CHIPS` (489-497), and `insertAtom` (509-519). The chip-building loop at `app.js:499-507` becomes the body of an exported `initHints(hintsEl, searchInput)` function, since it currently runs at module top level against DOM globals. Export `bareTokens` and `initHints`.
4. [ ] Create `routes.js` containing `fileHash` (155-158) and `parseFileHash` (262-273) verbatim, both exported. No imports.
5. [ ] Preserve every comment. The comments explain non-obvious security and UX decisions (`app.js:43-44`, `69-70`, `100-102`, `118`, `133`) and are the most valuable thing in the file.

**Verify:**
```bash
node --version   # must be >= 22.7; older versions parse .js as CommonJS and
                 # report a false SyntaxError on every `export`
for f in api dom query routes; do node --check internal/web/static/$f.js || echo "FAILED: $f"; done
# Expected: no output (syntax OK). Verified working on v22.21.
```

### Task 2: Extract view modules and the entry point

**Context:** `internal/web/static/app.js`, the modules from Task 1

**Files:**
- Create: `internal/web/static/search.js`, `internal/web/static/file.js`, `internal/web/static/repos.js`, `internal/web/static/main.js`
- Delete: `internal/web/static/app.js`
- Modify: `internal/web/static/index.html`

**Steps:**

1. [ ] Create `search.js` with `runSearch` (160-190), `renderResults` (192-211), `fileCard` (213-237), `renderStats` (239-254), and `SEARCH_LIMIT` (12). Import `api` from `./api.js`, `fileHash` from `./routes.js`, and `copyBtn`, `markMatches`, `note`, `errorBox`, `plural` from `./dom.js`. Export `runSearch` and `initSearch`.

   Module-level state (`searchCtl`, line 17) stays module-private in `search.js`. `resultsEl` and `statsEl` are currently file-level DOM refs; take them as arguments to an exported `initSearch({resultsEl, statsEl})` that stores them module-privately, rather than re-querying the DOM in each module.

2. [ ] Create `file.js` with `showFile` (275-304), `fileHeader` (306-328), `EDITOR_LABELS` (330), `githubUrl` (332-336), `editorUrl` (338-341), `fileActions` (343-370), `refreshFileActions` (372-379), `plainPre` (381-409), `targetLine` (411-418), `friendlyFileError` (420-433), and the gutter click listener (437-450). Import `fileHash` from `./routes.js`. Module-private state: `fileCtl`, `currentFile`, `currentFileData`.

   `currentFile` is read by the gutter listener and by `refreshFileActions`, and **written by the router** in `main.js` (`app.js:466`, `472`, `478`). Export a `setCurrentFile(loc)` setter and a `getCurrentFile()` reader rather than exporting the binding — an exported `let` is read-only to importers and assigning to it from `main.js` will throw.

   Also export `resetFile()`, covering exactly what the router does at `app.js:478-482`:
   ```js
   export function resetFile() {
     currentFile = null;
     currentFileData = null;
     if (fileCtl) fileCtl.abort();
     fileEl.replaceChildren();
   }
   ```
   Without it `main.js` cannot abort the in-flight file fetch or clear the pane, and the "zero behaviour change" goal fails silently — a slow file response would paint over the results view.

   Follow the same `initFile(fileEl)` pattern for the DOM ref. Export `showFile`, `targetLine`, `refreshFileActions`, `setCurrentFile`, `getCurrentFile`, `resetFile`, `initFile`.

3. [ ] Create `repos.js` with `loadRepos` (521-540), taking the badge element via an exported `loadRepos(repoBadge)` parameter. Import `api` from `./api.js` and `plural` from `./dom.js`.

4. [ ] Create `main.js` as the entry point, containing in this order:
   - element refs (`app.js:5-10`) via `$` from `./dom.js`
   - `savedScroll` (21)
   - the `route` function (455-484), calling `parseFileHash` from `./routes.js`, `setCurrentFile`/`getCurrentFile`/`showFile`/`targetLine`/`refreshFileActions`/`resetFile` from `./file.js`
   - search input listeners (559-569) and the document keydown handler (571-582)
   - init block (587-593), including `initSearch`, `initFile`, `initHints`, and `loadRepos`

5. [ ] Delete `app.js`.

6. [ ] Change `index.html:28` to:
   ```html
   <script type="module" src="/main.js"></script>
   ```

7. [ ] Update the `static.go` doc comment (lines 8-10) — it names `app.js` explicitly and would be stale.

8. [ ] Check `TestUIStaticAssets` (`api_test.go:375-401`) for a hardcoded `/app.js` reference. If it asserts on `app.js`, update it to `main.js` and add an assertion that the response `Content-Type` starts with `text/javascript`.

**Verify:**
```bash
cd /Users/broderick.westrope/dev/helse/muninn
for f in internal/web/static/*.js; do node --check "$f" || echo "FAILED: $f"; done  # needs node >= 22.7
go build ./... && go test ./internal/web/...
wc -l internal/web/static/*.js
# Expected: all syntax OK, tests pass, every module under ~250 lines
```

### Task 3: Confirm no circular imports and no stale references

**Context:** all modules in `internal/web/static/`

**Steps:**

1. [ ] Verify the import graph is acyclic. Intended direction: `main.js` → {`search.js`, `file.js`, `repos.js`, `query.js`} → {`routes.js`, `dom.js`, `api.js`}. `dom.js`, `api.js`, and `routes.js` import nothing local.

   `search.js` and `file.js` must not import each other. `runSearch` sets `location.hash` (`app.js:189`) rather than calling into `file.js`, and the router in `main.js` is what bridges them — preserve that indirection.

2. [ ] Grep for stale references:
   ```bash
   rg -n "app\.js" internal/ README.md
   ```
   Expected: no matches outside the plan and spec files.

3. [ ] Confirm no module reaches for a DOM element it no longer owns. Each module receives its elements through its `init*` function; only `main.js` may query the document:
   ```bash
   rg -n "\\\$\('#" internal/web/static/
   ```
   Expected: matches in `main.js` only.

4. [ ] Confirm the graph mechanically — no module imports a module that imports it back:
   ```bash
   rg -n "^import .* from '\./" internal/web/static/
   ```
   Read the output and check it against the intended direction above.

**Verify:**
```bash
go test ./internal/web/... && rg -c "app.js" internal/web/static/ ; echo "exit=$?"
# Expected: tests pass, no app.js references remain (rg exit=1)
```

## Manual Verification

Run `go run . web` and confirm every behaviour is unchanged. This is the entire point of the phase — do not skip it.

- [ ] Page loads with no console errors (module loading fails silently in some cases — check the console explicitly)
- [ ] Typing in the search box debounces and returns results
- [ ] Enter runs the search immediately
- [ ] `/` focuses and selects the search input
- [ ] Syntax hint chips insert their atom at the caret; `"..."` places the caret between the quotes
- [ ] Clicking a result row opens the file view scrolled to that line
- [ ] Esc returns to results, and the results scroll position is restored
- [ ] Clicking a gutter line number updates the URL without a history entry
- [ ] Copy buttons copy and briefly show a checkmark
- [ ] The GitHub link points at the indexed commit and the target line
- [ ] The repo badge shows the repo count and index age
- [ ] A deep link (`#/file/owner/name/path:42`) loads directly into the file view
- [ ] Reloading on a deep link keeps the file view rather than yanking to results

## PR

Open a PR for human review. Title it as a refactor and state plainly that no behaviour should change, so the reviewer knows to read for drift rather than for design.
