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
- **Result facets** (results view only): repo and file-extension, multi-select and toggleable, with a stable facet list drawn from a facet-free search (see Design Decisions).
- **Repo prominence**: strengthened, sticky result group headers with match counts.
- **Open-in-editor via the editor CLI**, so the repo is loaded in the target window.
- **`app.js` split into ES modules.** It is 593 lines today; this change adds a sidebar shell, tree rendering, facet rendering, and the open-in-editor call, which would push a single file past ~900 lines. Split along the seams already marked by its section comments. `static.go` embeds the whole directory and serves via `http.FileServer`, which maps `.js` to `text/javascript` — acceptable for modules, so no build step and no MIME change. `index.html:28` changes to `<script type="module" src="/main.js">`.
- **`gitfile.ListDir`**, a new function returning the immediate children of one directory (non-recursive `ls-tree`, no self-entry). See Design Decisions.
- **`main` max-width cap removed** (style.css:148).

**Out:**

- Standalone repo browsing (a tree with no file open, or a repo picker in the results view). The tree is a navigation aid within a repo reached via search, not a general file browser.
- Path-prefix facets (top-level directory within a repo). Useful in a monorepo, but only meaningful once a single repo is selected; revisit after the sidebar exists.
- Bidirectional sync between typed query atoms and facet chips. Facet state lives in its own URL params and never rewrites the query string (see Design Decisions).
- Facet counts computed over the full uncapped match set. Counts describe the returned (capped) result set only.
- Strictly per-category facet-excluding counts. A single facet-free universe is used instead; see Design Decisions.
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

Automated unless marked *(manual)*.

### Tree

- [ ] Opening a file from search results shows a tree in the sidebar, rooted at that file's repo, with every ancestor directory of the file expanded and the file itself marked as current.
- [ ] Clicking a file in the tree navigates to it without a full page reload, and the tree keeps its expansion state across that navigation.
- [ ] Expanding a directory fetches only that directory's immediate children.
- [ ] A tree listing against a monorepo-scale repo issues a non-recursive `git ls-tree` and returns only that directory's entries.
- [ ] `gitfile.ListTree`'s existing behaviour is unchanged: `TestListTreeDepth` and the MCP `list_tree` tool output are unaffected.
- [ ] Navigating rapidly between files in different repos never renders a stale repo's tree.
- [ ] Navigating to a file in a different repo resets the tree; navigating within the same repo preserves expansion state.
- [ ] A repo with no local checkout still renders a full tree.
- [ ] `ListDir` uses a trailing-slash pathspec and returns repo-relative child paths, excluding the anchor directory itself.
- [ ] `ListDir` on a nonexistent directory returns `fs.ErrNotExist`; `/api/tree` with a `path` naming a file returns 400.
- [ ] A directory exceeding the entry cap returns a truncation signal and the client discloses it on that node.
- [ ] A failed directory expand shows an inline error with a retry on that node, not an empty directory.
- [ ] Returning to the results view replaces the tree with facets; no tree is shown when no file is open.

### Facets

- [ ] The results sidebar lists every repo present in the current results with a match count, and every file extension present with a match count.
- [ ] Clicking a repo facet narrows results to that repo; clicking it again restores the unfiltered results.
- [ ] After selecting one repo facet, the other repo facets remain listed in the sidebar and are still clickable.
- [ ] Selecting two repo facets by clicking, one after the other, returns results from both (OR within a category).
- [ ] Selecting a repo facet and an extension facet returns only files matching both (AND across categories).
- [ ] A facet combination that yields zero results still renders the selected chips, so the selection can be undone without editing the URL.
- [ ] Loading a URL whose `repo` value does not match the query still renders that value as a deselectable chip.
- [ ] With no facets selected, a search issues exactly one search; with any facet selected, exactly two.
- [ ] A facet applied to a query containing a top-level `or` filters *both* sides of the disjunction.
- [ ] A repo whose name contains a regex metacharacter filters to exactly that repo.
- [ ] Facet params beyond the documented caps are rejected with 400.
- [ ] Facet selections survive a page reload and are present in the shareable URL.
- [ ] The search input is never programmatically rewritten by a facet interaction.
- [ ] A hand-typed `repo:` atom in the query composes with facet selections rather than conflicting with them.

### Repo prominence

- [ ] Result group headers render at a higher contrast and larger size than the `.card-path` links beneath them, with the owner segment de-emphasised relative to the name. *(manual)*
- [ ] A group header remains visible while scrolling past its own file cards.
- [ ] Each group header shows the repo's match count within the current results.

### Open in editor

- [ ] Clicking the editor action opens the file in an editor window with the repo's checkout loaded as the workspace folder, scrolled to the target line. *(manual)*
- [ ] With `editor.scheme: "vscode"`, the launched binary is `code`, not `vscode`.
- [ ] Clicking the editor action twice for two files in the same repo reuses one window rather than spawning two. *(manual — depends on the user's `window.openFoldersInNewWindow` setting)*
- [ ] With the editor CLI absent from `PATH`, the endpoint returns 501 and the client falls back to the URL scheme with a tooltip disclosing the degraded behaviour.
- [ ] A resolved path containing `:` returns 501 and takes the URL-scheme fallback rather than opening the wrong file.
- [ ] A file absent from the local checkout returns 404 with a distinct message, not an exec failure.
- [ ] With the editor CLI present but failing to launch, the endpoint returns 500 and the UI surfaces the error without falling back.
- [ ] A cross-origin `POST` to the open endpoint is rejected with 403 and a message telling the user to reload.
- [ ] A `POST` with no `Sec-Fetch-Site` header is rejected.
- [ ] A `POST` with a non-JSON content type is rejected.
- [ ] The open endpoint refuses a repo absent from the scanned checkouts, refuses a path that resolves outside its checkout directory, and refuses a path that escapes via a symlink.
- [ ] Containment is enforced on a path-separator boundary: a checkout at `/dev/repo` does not admit `/dev/repo-other`.
- [ ] The path passed to `--goto` is the symlink-resolved path that was validated.
- [ ] No editor process is launched from a shell string; the argv is fixed and built server-side.

### Layout and structure

- [ ] `main` carries no `max-width`, so a result line renders more characters before ellipsis at a 1600px viewport than at 1100px.
- [ ] Below a ~900px viewport the sidebar is hidden and the content pane uses the full width.
- [ ] The sidebar stays fixed while the content pane scrolls, with `<body>` still the scroll container.
- [ ] Returning from a file to results restores the previous results scroll position (`savedScroll` still works).
- [ ] `app.js` is replaced by focused ES modules, each under ~250 lines, loaded via `<script type="module">`.
- [ ] Existing tests pass; new endpoints have handler tests covering success, validation failure, and rejection paths.

## Design Decisions

### Sidebar is view-coupled, not independently navigable

The sidebar's content is a function of `data-view`: facets in results, tree in file view. Leaving the file view flips the sidebar back to facets.

*Alternative declined — user-switchable Tree|Filters tabs in both views:* a tree in the results view needs a "current repo" that no longer exists once you have left the file, which forces a repo picker and a second source of truth for "which repo am I browsing".

*Alternative declined — tree as a primary browse mode with a repo picker:* this makes muninn a general code browser, a materially larger surface than the stated goal. The tree here is for walking siblings and neighbours of a file you arrived at by searching.

### Tree fetches lazily, one directory per request

New endpoint `GET /api/tree?repo=<owner/name>&path=<dir>` returns the immediate children of one directory, pinned to the indexed commit. On file open the client requests each ancestor segment of the file's path to render the expanded spine (`internal/web/static/app.js` → root, `internal`, `internal/web`, `internal/web/static`); each subsequent expand is one request.

Paths are rarely deeper than 5-6 segments and the requests are parallel against a local mirror, so the N-requests-on-open cost is acceptable. If it proves slow, a server-side spine walk returning all ancestors in one response is a drop-in change — the client's per-directory data shape is unchanged.

**Tree requests are cancelled on repo change only.** The existing `searchCtl`/`fileCtl` discipline (app.js:17-18, 166, 278) exists to stop stale responses rendering over newer navigation, and the tree needs the same protection: a fan-out for repo A followed by fast navigation to repo B must not let A's responses land in B's sidebar. But a single controller aborted on every navigation would also cancel a same-repo expand the user just clicked. So the controller is scoped to the tree's repo and aborted when the repo changes, not on every route change. Navigating to a file in a **different repo resets the tree**; navigating within the same repo preserves expansion state and any in-flight expand.

Per-node state is explicit: a directory being expanded shows a loading indicator, and a failed expand (including one ancestor of the open-file fan-out) shows an inline error on that node with a retry, rather than silently rendering as empty.

*Alternative declined — whole tree in one request:* a monorepo root at full depth is a multi-MB payload and a slow `ls-tree`, and it makes `ListTree`'s `maxEntries` truncation a routine user-facing failure rather than an edge case.

**New `gitfile.ListDir` rather than a fast path inside `ListTree`.** `ListTree` runs `git ls-tree -r -t` and filters by depth afterwards (gitfile.go:136, 154-156), so a depth-1 listing walks the whole tree. But it cannot simply drop `-r` when depth is 1: without `-r`, git never emits the anchor directory itself, and `ListTree`'s current contract *includes* it — `relativeDepth("a", "a")` returns 1 (gitfile.go:210-216), so listing `"a"` at depth 1 yields `a`, `a/b`, `a/d.txt`. `TestListTreeDepth` asserts exactly that count (gitfile_test.go:234-241), and the MCP `list_tree` tool (mcp/tools_file.go:96) shares the contract.

So `ListTree` is left untouched, and the tree endpoint uses a new `ListDir(ctx, mirrorDir, commit, path)` returning immediate children only, non-recursive, without a self-entry. "Children of this directory" is a genuinely different contract from `ListTree`'s "everything within N levels of this anchor, anchor included" — a separate function is clearer than a mode flag, breaks no existing caller or test, and keeps MCP output stable. It reuses `checkCommit` and `parseTreeEntry`.

**The pathspec needs a trailing slash.** Verified against this repo: `git ls-tree --long <commit> -- internal/web` returns exactly one entry, the `internal/web` tree itself, and no children. `git ls-tree --long <commit> -- internal/web/` returns the children, with **repo-relative** paths. So the argv is `ls-tree -z --long <commit> -- <prefix>/`, with the pathspec omitted entirely for the repo root. The subtree-object form (`<commit>:internal/web`) is rejected: it returns basenames relative to the subtree, which would break the client's repo-relative path model.

`fs.ErrNotExist` for a missing directory uses the same inference `ListTree` already makes (gitfile.go:168-174): git cannot store empty directories, so empty output for a non-empty prefix means the path does not exist. A `path` that names a **file** rather than a directory returns `400` — a non-recursive listing of a blob pathspec returns the blob, which would otherwise render as a directory containing only itself.

**`ListDir` caps entries** and reports truncation, for the same reason the whole-tree fetch was declined: a single generated directory with tens of thousands of entries would otherwise produce an unbounded payload and DOM. The client discloses truncation on the node.

Symlinks are git blob entries and surface as files, consistent with `parseTreeEntry`'s existing fallback for non-tree/non-blob objects (gitfile.go:200-203). Empty directories cannot exist in git, so no special case is needed.

The tree reads from the bare mirror and is independent of local checkouts: it renders identically for a repo with no checkout (only the editor action is unavailable). A mid-session sync that changes the indexed commit is an accepted transient inconsistency — `resolveIndexedCommit` re-reads the status file per request, so already-rendered nodes reflect the old commit while new expands reflect the new one. Reloading the file view resolves it; the tree does not attempt commit-change detection.

### Facet state lives in dedicated URL params, not in the query string

Facets serialise to their own params — `?q=useQuery&repo=helse/pilot-engine,helse/gql-admin&ext=go` — and `/api/search` gains `repo` and `ext` params that the server ANDs into the zoekt query it builds. `search.Options` already carries `RepoFilter` for exactly this purpose (search/types.go:10-11). The search input displays only what the user typed and is never rewritten.

Semantics: **OR within a category, AND across categories.** Multiple repos become a single alternation; repo and extension selections are ANDed.

**Composition is AST-level, never string concatenation.** This is a correctness requirement, not a style preference: zoekt's `or` binds looser than implicit AND, so appending ` file:\.go$` to the user query `foo or bar` parses as `foo OR (bar AND file:\.go$)` — the facet silently fails to filter half the results. `RepoFilter` already avoids this by parsing first and then `query.NewAnd`-ing a `&query.Repo{}` node (search.go:54-64).

Extension facets get a new `Options.FileFilter`, ANDed the same way but built by a different route: `RepoFilter` compiles to a `*regexp.Regexp` for `query.Repo`, whereas zoekt's filename node wants a parsed `*syntax.Regexp` plus explicit `FileName`/`Content`/case flags. Rather than reach for that API directly, `FileFilter` is a regex string that the search package turns into a node via `query.Parse("file:" + filter)` and then `query.NewAnd`s against the parsed user query. Same AST-level guarantee, no second regex API, and it reuses the exact path zoekt's own `file:` atom takes.

**Facet values are `regexp.QuoteMeta`-escaped** before being joined into an alternation, following the existing precedent at mcp/tools_search.go:269. Unescaped, a repo named `acme/my.library` would match characters it should not.

**Facet params are validated and capped** — at most 50 repo values and 20 extension values, `400` beyond that, and extension values constrained to a conservative character class. An unbounded param list otherwise compiles an arbitrarily large regex on every search.

Extension rather than zoekt `lang:` — extension is derivable from the path with no backend involvement and maps to what "file type" means to a user. Using a `file:`-style regex atom also avoids zoekt's language detection disagreeing with the displayed label.

*Alternative declined — facets rewrite the query string (append-only or reserved-region):* both require an atom-aware query parser and serialiser in the client. Round-tripping arbitrary zoekt syntax — grouped alternations, negations, quoting — back into toggle state is error-prone, and getting it wrong leaves facet chips disagreeing with the results on screen. Dedicated params give one-way data flow (facet state → request) and delete the parser entirely.

*Alternative declined — client-side post-filtering of fetched results:* it can only filter the ≤100 matches already returned, so filtering to a repo silently hides that repo's matches beyond the cap. A filter that hides real matches is worse than no filter.

*Accepted seam:* a hand-typed `repo:helse/gql-admin` filters correctly and composes with facet selections, but does not light up the corresponding facet chip. The two mechanisms are independent rather than in conflict.

### Facet values come from a facet-free search, not the filtered results

The naive version of this — derive facet values from the response you just rendered — collapses on the second click. Selecting repo A filters the search to repo A, so the next response contains only repo A, so every other repo facet **disappears from the sidebar**. Multi-select becomes impossible to build by clicking (falsifying the two-repo criterion above), and selecting a repo/extension pair with no overlap empties the sidebar entirely, leaving the active selections with no chip to click to undo them — an unrecoverable dead end short of hand-editing the URL.

So the facet universe and the results come from **two different searches**:

- **Results** — the query with all facet filters ANDed in. What the content pane renders.
- **Facet universe** — the same query with **no facet filters**, used only for its repo/extension buckets and counts.

This is two searches when any facet is selected and one when none is (the facet-free search is exactly the search already being run), independent of how many categories or values are active. In exchange, the facet list is **stable** — it does not shift under the cursor as you toggle — which is the behaviour that makes multi-select usable at all.

**Selected values are always rendered as chips,** as the union of the selection and the universe. The universe makes every selected value present by construction in the normal case, but a URL carrying `repo=X` where `X` does not match the query would not, so the union is what actually guarantees a selection is always deselectable. No dead end is reachable.

Deliberate imprecision: a strictly correct implementation excludes only the *same* category when computing that category's universe (repo counts should respect an extension selection but ignore the repo selection), which costs one search per selected category. A single facet-free universe means counts are slightly broader than the filtered view. That is a good trade for a stable, predictable list at a fixed cost of two searches, and the counts remain honest about what they are: matches for the query, not for the query-plus-other-facets.

Extension values derive from the path suffix of the universe response; repo values from its `repo` field. Neither needs new backend aggregation. Counts describe the capped result set, not index-wide totals; the existing `truncated` warning (app.js:248-253) already discloses capping and no second disclosure is added.

*Alternative declined — derive facets from the filtered results and merely pin the selected chips:* fixes the dead end but not discovery. You still cannot find a second repo to add, so multi-select stays unbuildable.

*Alternative declined — repo facets from `/api/repos`:* listing all indexed repos means a long list of mostly zero-match options for any given query.

*Alternative declined — per-category facet-excluding aggregation:* strictly correct counts, but one extra search per selected category and a more complex contract, for a precision difference users will not notice.

*Alternative declined — server-side uncapped aggregation for true counts:* requires an uncapped zoekt pass on every search.

### Open-in-editor shells out to the editor CLI

The URL scheme cannot express a workspace folder, so a new-window-with-repo-loaded launch is only possible via the CLI. `POST /api/open` takes `{repo, path, line}` and runs a **single fixed argv**:

```
<binary> <checkoutDir> --goto <resolvedFile>:<line>
```

**`editor.scheme` is not the binary name.** The config validates `scheme` as `"cursor"` or `"vscode"` (config.go:100-101) and it is currently only ever used to build a URL (api.go:72, app.js:338-341). There is no executable named `vscode` — VS Code's CLI is `code`. Verified on this machine: `cursor` and `code` resolve, `vscode` does not. So the server keeps an explicit map, `cursor` → `cursor`, `vscode` → `code`, and the scheme string continues to serve double duty for the URL-scheme fallback only. Without this, every `vscode` user silently gets the fallback path forever and the headline behaviour never happens.

`exec.Command` with an argv slice, never a shell string. `checkoutDir` comes from `s.checkouts[strings.ToLower(repo)]` — the same map `localFile` already uses (api.go:165-175) — never from the request. The client sends a repo/path pair and the server resolves it, rejecting the request when:

- the repo is absent from the checkouts map;
- the resolved path escapes the checkout directory **after `filepath.EvalSymlinks`** on both the checkout dir and the target, so a symlink inside the checkout cannot redirect the launch outside it (containment compared on cleaned, symlink-resolved paths);
- `line` is not a positive integer.

Argv injection is structurally prevented rather than filtered: `checkoutDir` and the target path are both absolute (leading `/`), so neither can be read as an option flag, and `line` is an integer. No request value ever becomes an argv element on its own. Containment is compared on a path-separator boundary, not a raw string prefix, so `/dev/repo` cannot match `/dev/repo-other`. The **resolved** path is what gets exec'd, so there is no gap between the path that was validated and the path that is opened.

Two limits worth stating rather than papering over:

- **`--goto` value parsing.** VS Code and Cursor split the `--goto` value on `:` and read the first numeric segment as a line number. A colon inside a path is therefore usually fine, but a path segment that is entirely numeric (`dir/2024:notes.md`) parses as the wrong file. When the resolved path contains a `:`, the endpoint declines and the client uses the URL-scheme fallback.
- **Resolving `checkoutDir` through symlinks** may hand the editor a different folder path than the one an existing window holds, which weakens window reuse in that case. Correctness of containment wins over reuse.

TOCTOU between the containment check and the exec is not a meaningful risk here: the threat model is a malicious web page, which cannot create symlinks on disk, and a local process that can already exec the editor directly.

Deliberately no option surface: no `--new-window`, no configurable flags, no pass-through arguments. Now that the server invokes a CLI, that invocation stays a constant.

**Folder reuse, not forced new window.** Given a bare folder argument, the VS Code/Cursor CLI focuses an existing window already holding that folder and spawns one only if none exists. The stated intent — a window with the repo loaded — is satisfied without accumulating one window per click, which `--new-window` would do.

**Fallback, and only for CLI-not-found.** The editor CLI is a separately-installed shell command that many users never set up. When it is not on `PATH` the endpoint reports that as a distinct condition and the client falls back to the existing URL-scheme link, with a tooltip explaining the degraded behaviour. Silently doing nothing on click would be inexplicable. Three other outcomes are distinguished from it, each with its own status and message:

| Condition | Status | Client behaviour |
| --- | --- | --- |
| CLI not on `PATH` | `501` | URL-scheme fallback + tooltip |
| Resolved path contains `:` | `501` | URL-scheme fallback + tooltip |
| File absent from the checkout (checkout on a different commit, or deleted since the startup scan) | `404` | Inline message: file not in local checkout |
| CLI found but exec failed | `500` | Inline error, no fallback |
| CSRF guard rejection | `403` | Inline message: reload the page |

The `404` case is expected in normal use, not exceptional: the local checkout is frequently on a different commit than the index, which `localFile` already accounts for on the GET path (api.go:160-175). `filepath.EvalSymlinks` returns `fs.ErrNotExist` for it, so it surfaces as validation rather than an exec failure. The `403` needs its own copy because Safari before 16.4 sends no `Sec-Fetch-Site` and those users get an otherwise-opaque failure.

**CSRF is a genuinely new risk and needs its own guard.** The server currently only reads; this endpoint lets an unauthenticated loopback server launch local processes. `hostCheck` (server.go:76-91) does **not** protect it: a form POST from a malicious page to `127.0.0.1:7576` carries an allowed `Host` header. Guard with two checks:

1. `Sec-Fetch-Site` must be present **and** equal `same-origin`. Absent is rejected, not allowed — otherwise any client that omits the header (older browsers, non-browser HTTP clients) bypasses the guard entirely. The header is browser-set and not forgeable by page script.
2. `Content-Type` must be JSON, which blocks simple form POSTs and forces a CORS preflight for cross-origin `fetch`.

### Repo prominence: stronger and sticky

Raise `.repo-group h2` to full-strength colour and a slightly larger size so it outranks the `var(--accent)` paths beneath it; de-emphasise the owner relative to the name (`helse/` muted, `pilot-engine` strong) so the distinguishing half wins the glance. Add the repo's match count. Stick the header below the existing header bar, following the precedent already set by `.file-head` at `top: var(--header-h)` (style.css:304). Sticky is the part that fixes the scroll problem; the sidebar reinforces repo identity in a second place.

*Alternative declined — inline repo on every file card:* repeats the repo on every card and discards the grouping `renderResults` already builds (app.js:193-208).

*Alternative declined — per-repo accent colours:* a colour system to maintain that stops being legible past a handful of repos.

### Sidebar is always visible; the width cap is removed

The 1100px `main` cap is a prose-readability convention, and there is no prose here. It is actively harmful: `.row code` is `white-space: pre; overflow: hidden; text-overflow: ellipsis` (style.css:213-218), so each matching line is truncated at the pane edge and the cap discards matched code that would otherwise be readable. The file view is a horizontally-scrolling chroma `<pre>`, so extra width means less scrolling. Removing the cap improves both views.

With the cap gone, a ~260px sidebar costs nothing worth a toggle on a normal display, so the sidebar is always visible — no toggle, no persisted collapse state, no keyboard binding. Below a ~900px viewport, where 260px is a real bite, a media query hides the sidebar and gives the content pane full width.

**The shell keeps `<body>` as the scroll container,** with the sidebar `position: sticky`. Two existing behaviours depend on this and would break under independently-scrolling panes: `.file-head` sticks at `top: var(--header-h)` (style.css:304), which is measured against the body scroll, and `savedScroll` reads and writes `window.scrollY`/`window.scrollTo` to restore the results position (app.js:21, 471, 483). A sticky sidebar in a body-scrolled shell gets the desired effect — sidebar stays put, content scrolls — without touching either. The new sticky group headers use the same `top: var(--header-h)` origin for the same reason.

*Alternative declined — collapsible sidebar with persisted state:* only necessary if the content pane were starved, which removing the cap prevents. Skipping it avoids a toggle control, a `localStorage` key, and a breakpoint-versus-user-intent conflict.

*Alternative declined — overlay drawer:* hiding the file while the tree is open defeats the purpose of a tree used for navigation.

*Alternative declined — independently-scrolling sidebar and content panes:* forces rework of `.file-head`'s sticky origin and the `savedScroll` restore for no user-visible gain.

## Context Files

- `internal/web/server.go` — `Server` struct and deps, route table in `Handler` (new `/api/tree`, `/api/open`), `hostCheck` (CSRF context)
- `internal/web/api.go` — handler patterns, `resolveIndexedCommit`, `validRepoPart`, `localFile`, `trimResult`, `writeJSON`/`writeError`
- `internal/web/checkouts.go` — `ScanCheckouts`, the lowercased `owner/name` → checkout path map keying the open endpoint
- `internal/gitfile/gitfile.go` — new `ListDir`; existing `ListTree`/`TreeEntry`/`checkCommit`/`parseTreeEntry`/`relativeDepth` (contract to preserve)
- `internal/gitfile/gitfile_test.go` — `TestListTreeDepth` (the contract the new function must not disturb)
- `internal/mcp/tools_file.go` — `list_tree`, the other `ListTree` caller
- `internal/mcp/tools_search.go` — `regexp.QuoteMeta` alternation precedent (269)
- `internal/web/static/app.js` — module split source: search render (192-256), file view (258-450), routing (452-484), header chips (486-540)
- `internal/web/static/index.html` — shell markup for the sidebar
- `internal/web/static/style.css` — `main` cap (147-150), group headers (152-157), row overflow (213-218), sticky precedent (304), view switching (133-143)
- `internal/web/static.go` — embedded asset serving (ES module MIME types)
- `internal/search/types.go` — `Options.RepoFilter`, and the new `FileFilter` for extension facets
- `internal/search/search.go` — `Search` query composition via `query.NewAnd` (54-64), the pattern facets must follow
- `internal/config/config.go` — `EditorConfig.Scheme` / `.Roots`, and the scheme→binary map the open endpoint needs (`vscode`→`code`)
- `internal/cli/web.go` — server construction and wiring
- `internal/web/api_test.go`, `server_test.go`, `checkouts_test.go` — existing test patterns
