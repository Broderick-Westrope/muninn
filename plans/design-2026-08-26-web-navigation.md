# Web UI Navigation Design Spec

**Problem:** The web UI is search-only and has no way to move around code once you land on it. Four concrete gaps:

1. **No file navigation.** Opening a file is a dead end — you can only reach a sibling or neighbouring file by going back to results and searching again. There is no way to see a file in the context of its directory.
2. **No result filtering.** A broad query across 200+ indexed repos returns a flat list capped at 100 matches. Narrowing it requires hand-typing zoekt atoms (`repo:`, `lang:`), which the `#hints` chips teach but do not make discoverable — you must already know which repo you want.
3. **Repo names are visually subordinate to their own children.** `.repo-group h2` renders 12px/600 in `var(--muted)` (style.css:152-157) while the file paths beneath it render 12px mono in `var(--accent)`. The group header is quieter than its contents, so the eye reads paths first. Worse, the header is not sticky: scrolling a 100-match result set puts it off-screen, so mid-scroll there is no indication which repo is being read.
4. **Open-in-editor lands in an arbitrary window.** The `cursor://file/<path>:<line>` URL scheme has no way to express a workspace folder, so the editor routes the file to whichever window was last focused. The file opens without its repo loaded, which means no language server, no project search, and no go-to-definition — the entire reason for opening in an editor rather than reading it in the browser.

**Goal:** The web UI becomes a code *browser* as well as a code search. A persistent left sidebar carries a lazily-loaded file tree in the file view and result facets in the results view. Repo identity stays visible while scrolling. Open-in-editor opens the file in a window with its repo loaded.

## Scope

**In:**

- **Sidebar shell.** Layout changes from two mutually exclusive views (`body[data-view]` toggling `#results`/`#file`) to a persistent shell: sidebar + content pane. Sidebar content is view-coupled and swaps with `data-view`.
- **File tree** (file view only), rooted at the open file's repo, expanded to reveal the open file, lazily fetched one directory at a time.
- **Result facets** (results view only): repo and file-extension, derived from the current result set, multi-select and toggleable.
- **Repo prominence**: strengthened, sticky result group headers with match counts.
- **Open-in-editor via the editor CLI**, so the repo is loaded in the target window.
- **`app.js` split into ES modules.** It is 593 lines today; this change adds a sidebar shell, tree rendering, facet rendering, and the open-in-editor call, which would push a single file past ~900 lines. Split along the seams already marked by its section comments. `static.go` embeds the whole directory, so native ES modules need no build step.
- **`main` max-width cap removed** (style.css:148).

**Out:**

- Standalone repo browsing (a tree with no file open, or a repo picker in the results view). The tree is a navigation aid within a repo reached via search, not a general file browser.
- Path-prefix facets (top-level directory within a repo). Useful in a monorepo, but only meaningful once a single repo is selected; revisit after the sidebar exists.
- Bidirectional sync between typed query atoms and facet chips. Facet state lives in its own URL params and never rewrites the query string (see Design Decisions).
- Facet counts computed over the full uncapped match set. Counts describe the returned results only.
- Forced `--new-window` on editor launch.
- Any editor CLI option surface beyond the single fixed argv below.
- A sidebar collapse toggle.

## Constraints

- **No build step.** Assets are embedded via `static.go` and served directly. Native ES modules only; no bundler, no transpiler, no npm.
- **Line numbers must stay pinned to the indexed commit.** Tree listings read from the bare mirror at the repo's indexed commit, exactly as `handleFile` does via `resolveIndexedCommit`. The status file is never cached (server.go:30-35).
- **Loopback-only, unauthenticated.** The server serves an unauthenticated index of private code. The new write-ish endpoint must not widen this surface (see Design Decisions → CSRF).
- **No user data through `innerHTML`.** The existing code builds DOM with `textContent`/`append` throughout and reserves `innerHTML` for chroma output and the static `ICONS` table (app.js:118, 298). Tree and facet rendering must hold that line.
- **Bounded git work per request.** A depth-1 listing must not walk an entire monorepo tree.
- **macOS only**, consistent with the rest of muninn (launchd, `open`).

## Success Criteria

- [ ] Opening a file from search results shows a tree in the sidebar, rooted at that file's repo, with every ancestor directory of the file expanded and the file itself marked as current.
- [ ] Clicking a file in the tree navigates to it without a full page reload, and the tree keeps its expansion state across that navigation.
- [ ] Expanding a directory fetches only that directory's immediate children.
- [ ] A depth-1 tree request against a monorepo-scale repo issues a non-recursive `git ls-tree` and returns only that directory's entries.
- [ ] Returning to the results view replaces the tree with facets; no tree is shown when no file is open.
- [ ] The results sidebar lists every repo present in the current results with a match count, and every file extension present with a match count.
- [ ] Clicking a repo facet narrows results to that repo; clicking it again restores the unfiltered results.
- [ ] Selecting two repo facets returns results from both (OR within a category).
- [ ] Selecting a repo facet and an extension facet returns only files matching both (AND across categories).
- [ ] Facet selections survive a page reload and are present in the shareable URL.
- [ ] The search input is never programmatically rewritten by a facet interaction.
- [ ] A hand-typed `repo:` atom in the query composes with facet selections rather than conflicting with them.
- [ ] Result group headers are visually stronger than the file paths beneath them and remain visible while scrolling a long result set.
- [ ] Each group header shows the repo's match count within the current results.
- [ ] Clicking the editor action opens the file in an editor window with the repo's checkout loaded as the workspace folder, scrolled to the target line.
- [ ] Clicking the editor action twice for two files in the same repo reuses one window rather than spawning two.
- [ ] With the editor CLI absent from `PATH`, the editor action falls back to the `cursor://`/`vscode://` URL scheme and the UI discloses the degraded behaviour.
- [ ] A cross-origin `POST` to the open endpoint is rejected.
- [ ] The open endpoint refuses a repo absent from the scanned checkouts, and refuses a path that resolves outside its checkout directory.
- [ ] No editor process is launched from a shell string; the argv is fixed and built server-side.
- [ ] Matching lines in results use the full window width, showing more of each line than the previous 1100px cap allowed.
- [ ] Below a ~900px viewport the sidebar is hidden and the content pane uses the full width.
- [ ] `app.js` is replaced by focused ES modules, each under ~250 lines.
- [ ] Existing tests pass; new endpoints have handler tests covering success, validation failure, and rejection paths.

## Design Decisions

### Sidebar is view-coupled, not independently navigable

The sidebar's content is a function of `data-view`: facets in results, tree in file view. Leaving the file view flips the sidebar back to facets.

*Alternative declined — user-switchable Tree|Filters tabs in both views:* a tree in the results view needs a "current repo" that no longer exists once you have left the file, which forces a repo picker and a second source of truth for "which repo am I browsing".

*Alternative declined — tree as a primary browse mode with a repo picker:* this makes muninn a general code browser, a materially larger surface than the stated goal. The tree here is for walking siblings and neighbours of a file you arrived at by searching.

### Tree fetches lazily, one directory per request

New endpoint `GET /api/tree?repo=<owner/name>&path=<dir>` returns the immediate children of one directory, pinned to the indexed commit. On file open the client requests each ancestor segment of the file's path to render the expanded spine (`internal/web/static/app.js` → root, `internal`, `internal/web`, `internal/web/static`); each subsequent expand is one request.

Paths are rarely deeper than 5-6 segments and the requests are parallel against a local mirror, so the N-requests-on-open cost is acceptable. If it proves slow, a server-side spine walk returning all ancestors in one response is a drop-in change — the client's per-directory data shape is unchanged.

*Alternative declined — whole tree in one request:* a monorepo root at full depth is a multi-MB payload and a slow `ls-tree`, and it makes `ListTree`'s `maxEntries` truncation a routine user-facing failure rather than an edge case.

**Targeted fix to code being built on:** `gitfile.ListTree` always runs `git ls-tree -r -t` and filters by depth afterwards (gitfile.go:136, 154-156), so a depth-1 listing walks the entire tree. Add a non-recursive path (omit `-r`) when depth is 1. This is a fix to the function this feature depends on, not unrelated refactoring.

### Facet state lives in dedicated URL params, not in the query string

Facets serialise to their own params — `?q=useQuery&repo=helse/pilot-engine,helse/gql-admin&ext=go` — and `/api/search` gains `repo` and `ext` params that the server ANDs into the zoekt query it builds. `search.Options` already carries `RepoFilter` for exactly this purpose (search/types.go:10-11). The search input displays only what the user typed and is never rewritten.

Semantics: **OR within a category, AND across categories.** Multiple repos become a single alternation; repo and extension selections are ANDed. The query is composed server-side where the syntax is known-correct, rather than assembled by string concatenation in the client.

Extension rather than zoekt `lang:` — extension is derivable from the path with no backend involvement and maps to what "file type" means to a user. The server translates an extension facet to a `file:\.go$`-style atom, which also avoids zoekt's language detection disagreeing with the displayed label.

*Alternative declined — facets rewrite the query string (append-only or reserved-region):* both require an atom-aware query parser and serialiser in the client. Round-tripping arbitrary zoekt syntax — grouped alternations, negations, quoting — back into toggle state is error-prone, and getting it wrong leaves facet chips disagreeing with the results on screen. Dedicated params give one-way data flow (facet state → request) and delete the parser entirely.

*Alternative declined — client-side post-filtering of fetched results:* it can only filter the ≤100 matches already returned, so filtering to a repo silently hides that repo's matches beyond the cap. A filter that hides real matches is worse than no filter.

*Accepted seam:* a hand-typed `repo:helse/gql-admin` filters correctly and composes with facet selections, but does not light up the corresponding facet chip. The two mechanisms are independent rather than in conflict.

### Facet values derive from the current result set

Repo values come from `data.files[].repo`, extension values from the path suffix — both already present in the search response, so no new backend aggregation. Every sidebar entry is therefore something that will actually narrow what is on screen, with a count.

Consequences to handle explicitly: an empty result set yields an empty facet list, and a query that already pins one repo yields a single-entry repo facet. Counts describe the returned results, not index-wide totals; the existing `truncated` warning (app.js:248-253) already discloses when results are capped, and no second disclosure is added.

*Alternative declined — repo facets from `/api/repos`:* listing all indexed repos means a long list of mostly zero-match options for any given query.

*Alternative declined — server-side uncapped aggregation for true counts:* requires a second uncapped zoekt pass on every search.

### Open-in-editor shells out to the editor CLI

The URL scheme cannot express a workspace folder, so a new-window-with-repo-loaded launch is only possible via the CLI. `POST /api/open` takes `{repo, path, line}` and runs a **single fixed argv**:

```
<scheme> <checkoutDir> --goto <absFile>:<line>
```

`exec.Command` with an argv slice, never a shell string. `checkoutDir` comes from `s.checkouts[strings.ToLower(repo)]` — the same map `localFile` already uses (api.go:165-175) — never from the request. The client sends a repo/path pair, the server resolves it, and rejects the request if the repo is absent from the checkouts map or if the resolved absolute path escapes the checkout directory.

Deliberately no option surface: no `--new-window`, no configurable flags, no pass-through arguments. Now that the server invokes a CLI, that invocation stays a constant.

**Folder reuse, not forced new window.** Given a bare folder argument, the VS Code/Cursor CLI focuses an existing window already holding that folder and spawns one only if none exists. The stated intent — a window with the repo loaded — is satisfied without accumulating one window per click, which `--new-window` would do.

**Fallback.** The editor CLI is a separately-installed shell command that many users never set up. When it is not on `PATH` the endpoint reports that distinctly and the client falls back to the existing URL-scheme link, with a tooltip explaining the degraded behaviour. Silently doing nothing on click would be inexplicable.

**CSRF is a genuinely new risk and needs its own guard.** The server currently only reads; this endpoint lets an unauthenticated loopback server launch local processes. `hostCheck` (server.go:76-91) does **not** protect it: a form POST from a malicious page to `127.0.0.1:7576` carries an allowed `Host` header. Guard with `Sec-Fetch-Site: same-origin` (browser-set, not forgeable by page script) plus a JSON content-type requirement, which together defeat simple form POSTs.

### Repo prominence: stronger and sticky

Raise `.repo-group h2` to full-strength colour and a slightly larger size so it outranks the `var(--accent)` paths beneath it; de-emphasise the owner relative to the name (`helse/` muted, `pilot-engine` strong) so the distinguishing half wins the glance. Add the repo's match count. Stick the header below the existing header bar, following the precedent already set by `.file-head` at `top: var(--header-h)` (style.css:304). Sticky is the part that fixes the scroll problem; the sidebar reinforces repo identity in a second place.

*Alternative declined — inline repo on every file card:* repeats the repo on every card and discards the grouping `renderResults` already builds (app.js:193-208).

*Alternative declined — per-repo accent colours:* a colour system to maintain that stops being legible past a handful of repos.

### Sidebar is always visible; the width cap is removed

The 1100px `main` cap is a prose-readability convention, and there is no prose here. It is actively harmful: `.row code` is `white-space: pre; overflow: hidden; text-overflow: ellipsis` (style.css:213-218), so each matching line is truncated at the pane edge and the cap discards matched code that would otherwise be readable. The file view is a horizontally-scrolling chroma `<pre>`, so extra width means less scrolling. Removing the cap improves both views.

With the cap gone, a ~260px sidebar costs nothing worth a toggle on a normal display, so the sidebar is always visible — no toggle, no persisted collapse state, no keyboard binding. Below a ~900px viewport, where 260px is a real bite, a media query hides the sidebar and gives the content pane full width.

*Alternative declined — collapsible sidebar with persisted state:* only necessary if the content pane were starved, which removing the cap prevents. Skipping it avoids a toggle control, a `localStorage` key, and a breakpoint-versus-user-intent conflict.

*Alternative declined — overlay drawer:* hiding the file while the tree is open defeats the purpose of a tree used for navigation.

## Context Files

- `internal/web/server.go` — `Server` struct and deps, route table in `Handler` (new `/api/tree`, `/api/open`), `hostCheck` (CSRF context)
- `internal/web/api.go` — handler patterns, `resolveIndexedCommit`, `validRepoPart`, `localFile`, `trimResult`, `writeJSON`/`writeError`
- `internal/web/checkouts.go` — `ScanCheckouts`, the lowercased `owner/name` → checkout path map keying the open endpoint
- `internal/gitfile/gitfile.go` — `ListTree`, `TreeEntry`, the `-r -t` depth-1 fast path
- `internal/web/static/app.js` — module split source: search render (192-256), file view (258-450), routing (452-484), header chips (486-540)
- `internal/web/static/index.html` — shell markup for the sidebar
- `internal/web/static/style.css` — `main` cap (147-150), group headers (152-157), row overflow (213-218), sticky precedent (304), view switching (133-143)
- `internal/web/static.go` — embedded asset serving (ES module MIME types)
- `internal/search/types.go` — `Options.RepoFilter` for server-side facet composition
- `internal/config/config.go` — `EditorConfig.Scheme` / `.Roots`
- `internal/cli/web.go` — server construction and wiring
- `internal/web/api_test.go`, `server_test.go`, `checkouts_test.go` — existing test patterns
