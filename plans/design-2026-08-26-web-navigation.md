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
- **Result facets** (results view only): repo and file-extension, multi-select and toggleable, with a stable facet list computed server-side from a facet-free pass (see Design Decisions).
- **Repo prominence**: strengthened, sticky result group headers with match counts.
- **Open-in-editor via the editor CLI**, so the repo is loaded in the target window.
- **`app.js` split into ES modules.** It is 593 lines today; this change adds a sidebar shell, tree rendering, facet rendering, and the open-in-editor call, which would push a single file past ~900 lines. Split along the seams already marked by its section comments. `static.go` embeds the whole directory and serves via `http.FileServer`, which maps `.js` to `text/javascript` — acceptable for modules, so no build step and no MIME change. `index.html:28` changes to `<script type="module" src="/main.js">`.
- **`gitfile.ListDir`**, a new function returning the immediate children of one directory (non-recursive `ls-tree`, no self-entry). See Design Decisions.
- **`main` max-width cap removed** (style.css:148).

**Out:**

- Standalone repo browsing (a tree with no file open, or a repo picker in the results view). The tree is a navigation aid within a repo reached via search, not a general file browser.
- Path-prefix facets (top-level directory within a repo). Useful in a monorepo, but only meaningful once a single repo is selected; revisit after the sidebar exists.
- Bidirectional sync between typed query atoms and facet chips. Facet state lives in its own URL params and never rewrites the query string (see Design Decisions).
- Facet counts computed over the whole index. Counts describe matches within the aggregation cap.
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

The repo has no JavaScript test infrastructure (no `package.json`, no runner) and the no-npm constraint above rules out adding a headless-browser harness, so **client-side behaviour is verified manually** and marked *(manual)*. Unmarked criteria are Go tests: handler tests via `httptest` against `Handler()`, and `gitfile` tests against a fixture mirror. This split is deliberate — the Go boundary is where the contracts worth regression-testing live.

### Tree

- [ ] Opening a file from search results shows a tree in the sidebar, rooted at that file's repo, with every ancestor directory of the file expanded and the file itself marked as current. *(manual)*
- [ ] Clicking a file in the tree navigates to it without a full page reload, and the tree keeps its expansion state across that navigation. *(manual)*
- [ ] Expanding a directory fetches only that directory's immediate children. *(manual)*
- [ ] A tree listing against a monorepo-scale repo issues a non-recursive `git ls-tree` and returns only that directory's entries.
- [ ] `gitfile.ListTree`'s existing behaviour is unchanged: `TestListTreeDepth` and the MCP `list_tree` tool output are unaffected.
- [ ] Navigating rapidly between files in different repos never renders a stale repo's tree. *(manual)*
- [ ] Navigating to a file in a different repo resets the tree; navigating within the same repo preserves expansion state and any in-flight expand. *(manual)*
- [ ] A repo with no local checkout still renders a full tree. *(manual)*
- [ ] `ListDir` uses a trailing-slash pathspec and returns repo-relative child paths, excluding the anchor directory itself.
- [ ] `ListDir` returns `fs.ErrNotExist` for a nonexistent path and `ErrNotDir` for a path naming a file; `/api/tree` maps them to 404 and 400.
- [ ] `/api/tree` maps `ErrIndexMismatch` to 409.
- [ ] A directory exceeding `maxEntries` returns `truncated: true`, and the client discloses it on that node. *(server: automated; disclosure: manual)*
- [ ] A failed directory expand shows an inline error with a retry on that node, not an empty directory. *(manual)*
- [ ] Returning to the results view replaces the tree with facets; no tree is shown when no file is open. *(manual)*

### Facets

- [ ] `/api/search` returns a `facets` block with repo and extension values and counts, computed from a facet-free pass that ignores the request's own facet params.
- [ ] Facet counts are unaffected by the facet selection: selecting a repo does not change any facet count.
- [ ] A facet count is never lower than the number of results displayed for that value.
- [ ] A file with no dot in its basename and a dotfile such as `.gitignore` both fall in the "no extension" bucket; `.GO` and `.go` share one bucket.
- [ ] The results sidebar renders every repo and extension in the `facets` block with its count. *(manual)*
- [ ] Clicking a repo facet narrows results to that repo; clicking it again restores the unfiltered results. *(manual)*
- [ ] After selecting one repo facet, the other repo facets remain listed and clickable. *(manual)*
- [ ] Selecting two repo facets by clicking, one after the other, returns results from both (OR within a category).
- [ ] Selecting a repo facet and an extension facet returns only files matching both (AND across categories).
- [ ] A facet combination that yields zero results still renders the selected chips, so the selection can be undone without editing the URL. *(manual)*
- [ ] Loading a URL whose `repo` value does not match the query still renders that value as a deselectable chip. *(manual)*
- [ ] A facet toggle re-searches without waiting for the input debounce, and reuses the single existing `AbortController`. *(manual)*
- [ ] `facets.truncated` is reported separately from the results-level `truncated`, and the sidebar discloses it. *(server: automated; disclosure: manual)*
- [ ] A facet applied to a query containing a top-level `or` filters *both* sides of the disjunction.
- [ ] A repo whose name contains a regex metacharacter filters to exactly that repo.
- [ ] Facet params beyond the documented caps are rejected with 400.
- [ ] Facet selections survive a page reload and are present in the shareable URL. *(manual)*
- [ ] The search input is never programmatically rewritten by a facet interaction. *(manual)*
- [ ] A hand-typed `repo:` atom in the query composes with facet selections rather than conflicting with them.

### Repo prominence

- [ ] Result group headers render at a higher contrast and larger size than the `.card-path` links beneath them, with the owner segment de-emphasised relative to the name. *(manual)*
- [ ] A group header remains visible while scrolling past its own file cards, and is not obscured by the `#hints` row. *(manual)*
- [ ] Each group header shows the repo's match count within the current results. *(manual)*

### Open in editor

- [ ] Clicking the editor action opens the file in an editor window with the repo's checkout loaded as the workspace folder, scrolled to the target line. *(manual)*
- [ ] With `editor.scheme: "vscode"`, the launched binary is `code`, not `vscode`.
- [ ] Clicking the editor action twice for two files in the same repo reuses one window rather than spawning two. *(manual — depends on the user's `window.openFoldersInNewWindow` setting)*
- [ ] Every row of the status table is reachable and returns the stated code: 400 validation, 501 CLI-missing, 501 colon-in-path, 403 containment, 404 file-absent, 500 exec-failure, 403 CSRF.
- [ ] With the editor CLI absent from `PATH`, the client falls back to the URL scheme with a tooltip disclosing the degraded behaviour. *(manual)*
- [ ] A 403 renders a message telling the user to reload, not an opaque failure. *(manual)*
- [ ] A `POST` with no `Sec-Fetch-Site` header is rejected.
- [ ] A `POST` with a non-JSON content type is rejected.
- [ ] The CSRF guard is enforced inside `Handler()`, so `httptest` requests exercise it.
- [ ] Containment is enforced on a path-separator boundary: a checkout at `/dev/repo` does not admit `/dev/repo-other`.
- [ ] The path passed to `--goto` is the symlink-resolved path that was validated.
- [ ] No editor process is launched from a shell string; the argv is fixed and built server-side.

### Layout and structure

- [ ] `main` carries no `max-width`, so a result line renders more characters before ellipsis at a 1600px viewport than at 1100px. *(manual)*
- [ ] Below a ~900px viewport the sidebar is hidden and the content pane uses the full width. *(manual)*
- [ ] The sidebar stays fixed while the content pane scrolls, with `<body>` still the scroll container. *(manual)*
- [ ] A tree taller than the viewport scrolls within the sidebar rather than scrolling the sidebar out of view. *(manual)*
- [ ] Sticky offsets use `--sticky-top`, which differs between the results and file views; no new rule uses `--header-h` directly.
- [ ] The stats footer is not obscured by the sidebar. *(manual)*
- [ ] Returning from a file to results restores the previous results scroll position (`savedScroll` still works). *(manual)*
- [ ] `app.js` is replaced by focused ES modules, each under ~250 lines, loaded via `<script type="module">`.
- [ ] Existing Go tests pass; both new endpoints have handler tests covering success, validation failure, and rejection paths.

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

**The pathspec needs a trailing slash, and the anchor needs a separate probe.** Verified against this repo: `git ls-tree --long <commit> -- internal/web` returns exactly one entry, the `internal/web` tree itself, and no children. `git ls-tree --long <commit> -- internal/web/` returns the children, with **repo-relative** paths. So children come from `ls-tree -z --long <commit> -- <prefix>/`, with the pathspec omitted entirely for the repo root. The subtree-object form (`<commit>:internal/web`) is rejected: it returns basenames relative to the subtree, which would break the client's repo-relative path model.

But the trailing slash makes "does this path exist, and is it a directory?" unanswerable from the children call alone — a blob pathspec with a slash appended matches nothing, which is indistinguishable from a missing directory. So for a non-empty path `ListDir` first probes the anchor with the **slash-free** form, which returns exactly one entry carrying its type, and returns:

- `fs.ErrNotExist` when the probe is empty — handler maps to `404`;
- a distinct `ErrNotDir` sentinel when the probe reports a blob — handler maps to `400`, so a file path cannot render as a directory containing only itself.

Two git calls per expand, one for the root. Expands are user-driven, one per click, against a local mirror.

**`ListDir` takes a `maxEntries` parameter** mirroring `ListTree` (gitfile.go:125) rather than hard-coding, and the handler passes 2000. A single generated directory with tens of thousands of entries would otherwise produce an unbounded payload and DOM. Truncation is reported in the response and the client discloses it on the node.

**Response shape**, following the `searchResponse`/`fileResponse` precedent (api.go:34-73):

```
{ entries: [{path, type, size}], truncated: bool }
```

**Error contract:** `400` invalid repo or path-names-a-file, `404` missing repo/commit/directory, `409` `ErrIndexMismatch` (reachable via `checkCommit`, mapped exactly as `handleFile` does at api.go:133-135, with per-node client copy mirroring app.js:424-425), `500` otherwise.

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

### Facet values are computed server-side from a facet-free pass

The naive version — derive facet values from the response you just rendered — collapses on the second click. Selecting repo A filters the search to repo A, so the next response contains only repo A, so every other repo facet **disappears from the sidebar**. Multi-select becomes impossible to build by clicking, and selecting a repo/extension pair with no overlap empties the sidebar entirely, leaving the active selections with no chip to undo them — an unrecoverable dead end short of hand-editing the URL.

So the facet universe must ignore the facet selection. It is computed **server-side, inside the single `/api/search` request**, which returns a new `facets` block alongside `files`:

```
facets: {
  repos: [{value, count}],
  exts:  [{value, count}],
  truncated: bool
}
```

The handler runs two zoekt passes: the **results** pass (query + facet filters ANDed) and a **facet-free aggregation** pass (query only). Aggregation runs at its own, higher cap — independent of the 100/200 display limits (api.go:24-25) — because it needs breadth of coverage, not content, and returns counts only.

**One request, not two.** This was the deciding factor over a second client-side `fetch`, and it resolves five things at once that a client-side universe leaves dangling:

- **Abort and debounce** — one `searchCtl` (app.js:17, 166-172) keeps working unchanged. A facet toggle re-searches through a path that bypasses the 200ms input debounce (app.js:559-562), since it is a click, not typing.
- **Partial failure** — impossible. One request either succeeds or fails; there is no state where the results rendered but the sidebar did not.
- **Stats footer** — one `data.stats` and one `data.truncated`, so `renderStats` (app.js:239-253) needs no precedence rule between two responses.
- **No redundant work** — the universe depends only on `q`, so a client-side version would re-fetch an identical universe on every toggle and re-render an identical list (flicker), and double zoekt work on every debounce tick.
- **Counts cannot invert** — with a client-side universe capped at 100, selecting a repo whose universe share is 12 could display "12" beside up to 100 result rows. A higher aggregation cap keeps the count ≥ what is displayed.

**Selected values are always rendered as chips,** as the union of the selection and the returned facet values. The universe makes every selected value present in the normal case, but a URL carrying `repo=X` where `X` does not match the query would not, so the union is what guarantees a selection is always deselectable. No dead end is reachable.

**The facet list is still bounded, and says so.** Aggregation is capped, so for a very broad query the value list is the repos and extensions found within that cap, not every repo in the index. `facets.truncated` carries this and the sidebar discloses it — distinct from the results-level `truncated` warning, because the two describe different caps.

Extension bucketing rules, since the edges are ambiguous: extension is the suffix after the last dot in the **basename**, lowercased. A basename with no dot (`Makefile`, `LICENSE`) and a dotfile whose only dot is leading (`.gitignore`) both have **no extension** and are grouped under one explicit "no extension" bucket, which is selectable like any other. Lowercasing matches zoekt's default case-insensitive `file:` behaviour.

Deliberate imprecision: strictly correct faceting excludes only the *same* category when computing that category's universe (repo counts should respect an extension selection but ignore the repo selection), costing one pass per selected category. A single facet-free universe means counts are slightly broader than the filtered view — honest about what they are: matches for the query, not the query-plus-other-facets.

*Alternative declined — derive facets from the filtered results and merely pin the selected chips:* fixes the dead end but not discovery. You still cannot find a second repo to add, so multi-select stays unbuildable.

*Alternative declined — a second client-side search for the universe:* leaves limit, abort, partial failure, stats precedence and count inversion all to be re-decided in the client, for no gain.

*Alternative declined — repo facets from `/api/repos`:* listing all indexed repos means a long list of mostly zero-match options for any given query.

*Alternative declined — per-category facet-excluding aggregation:* strictly correct counts at one extra pass per selected category, for a precision difference users will not notice.

*Alternative declined — uncapped aggregation for index-wide true counts:* an uncapped zoekt pass on every search.

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
| Non-positive `line`, malformed body | `400` | Inline error |
| Unknown repo (absent from the scanned checkouts) | `404` | Inline message: no local checkout for this repo |
| CLI not on `PATH` | `501` | URL-scheme fallback + tooltip |
| Resolved path contains `:` | `501` | URL-scheme fallback + tooltip |
| Path escapes the checkout (including via symlink) | `403` | Inline error |
| File absent from the checkout (checkout on a different commit, or deleted since the startup scan) | `404` | Inline message: file not in local checkout |
| CLI found but exec failed | `500` | Inline error, no fallback |
| CSRF guard rejection | `403` | Inline message: reload the page |
| Success | `204` | No UI change |

The guard itself lives **inside `Handler()`** (server.go:66-74), not in the `Serve` middleware chain where `hostCheck` is applied (server.go:129). Otherwise `httptest` tests that exercise `Handler()` bypass it entirely and the rejection criteria are untestable.

The `404` case is expected in normal use, not exceptional: the local checkout is frequently on a different commit than the index, which `localFile` already accounts for on the GET path (api.go:160-175). `filepath.EvalSymlinks` returns `fs.ErrNotExist` for it, so it surfaces as validation rather than an exec failure. The `403` needs its own copy because Safari before 16.4 sends no `Sec-Fetch-Site` and those users get an otherwise-opaque failure.

**CSRF is a genuinely new risk and needs its own guard.** The server currently only reads; this endpoint lets an unauthenticated loopback server launch local processes. `hostCheck` (server.go:76-91) does **not** protect it: a form POST from a malicious page to `127.0.0.1:7576` carries an allowed `Host` header. Guard with two checks:

1. `Sec-Fetch-Site` must be present **and** equal `same-origin`. Absent is rejected, not allowed — otherwise any client that omits the header (older browsers, non-browser HTTP clients) bypasses the guard entirely. The header is browser-set and not forgeable by page script.
2. `Content-Type` must be JSON, which blocks simple form POSTs and forces a CORS preflight for cross-origin `fetch`.

### Repo prominence: stronger and sticky

Raise `.repo-group h2` to full-strength colour and a slightly larger size so it outranks the `var(--accent)` paths beneath it; de-emphasise the owner relative to the name (`helse/` muted, `pilot-engine` strong) so the distinguishing half wins the glance. Add the repo's match count. Stick the header below the visible header bar using `--sticky-top` (see the layout decision below — **not** `--header-h`, which excludes the `#hints` row that is visible in this view). Sticky is the part that fixes the scroll problem; the sidebar reinforces repo identity in a second place.

*Alternative declined — inline repo on every file card:* repeats the repo on every card and discards the grouping `renderResults` already builds (app.js:193-208).

*Alternative declined — per-repo accent colours:* a colour system to maintain that stops being legible past a handful of repos.

### Sidebar is always visible; the width cap is removed

The 1100px `main` cap is a prose-readability convention, and there is no prose here. It is actively harmful: `.row code` is `white-space: pre; overflow: hidden; text-overflow: ellipsis` (style.css:213-218), so each matching line is truncated at the pane edge and the cap discards matched code that would otherwise be readable. The file view is a horizontally-scrolling chroma `<pre>`, so extra width means less scrolling. Removing the cap improves both views.

With the cap gone, a ~260px sidebar costs nothing worth a toggle on a normal display, so the sidebar is always visible — no toggle, no persisted collapse state, no keyboard binding. Below a ~900px viewport, where 260px is a real bite, a media query hides the sidebar and gives the content pane full width.

**The shell keeps `<body>` as the scroll container,** with the sidebar `position: sticky`. Two existing behaviours depend on this and would break under independently-scrolling panes: `.file-head` sticks at `top: var(--header-h)` (style.css:304), which is measured against the body scroll, and `savedScroll` reads and writes `window.scrollY`/`window.scrollTo` to restore the results position (app.js:21, 471, 483). A sticky sidebar in a body-scrolled shell gets the desired effect — sidebar stays put, content scrolls — without touching either.

**`--header-h` is the wrong sticky origin in the results view, and must not be reused.** It is 41px, documented as `.bar` height + padding + border (style.css:19). `#hints` sits *inside* the sticky `<header>` below `.bar` (index.html:21, style.css:110-115) and is only `display: none` in the **file** view (style.css:135-139) — which is the sole reason `.file-head`'s use of the value is correct. Group headers exist only in the results view, where `#hints` is visible, so sticking them at 41px slides them under the chips row. The fix is a `--sticky-top` variable set per view: `var(--header-h)` in the file view, the full bar-plus-hints height in the results view. Both the sticky group headers and the sticky sidebar use `--sticky-top`, never `--header-h` directly.

**The sidebar scrolls internally.** An expanded tree easily exceeds viewport height, and a sticky element taller than the viewport scrolls away — defeating the point. The sidebar gets `max-height: calc(100vh - var(--sticky-top))` and `overflow-y: auto`. The per-directory entry cap bounds one listing, not the accumulated tree.

**The stats footer needs a left offset.** `footer` is `position: fixed; left: 0` (style.css:284-294) and would otherwise run underneath the sidebar. It is the one existing element the shell change breaks.

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
- `internal/web/static/style.css` — `main` cap (147-150), group headers (152-157), row overflow (213-218), `--header-h` and why it excludes `#hints` (19, 110-115, 135-143), sticky precedent (304), fixed footer (284-294)
- `internal/web/static.go` — embedded asset serving (ES module MIME types)
- `internal/search/types.go` — `Options.RepoFilter`, and the new `FileFilter` for extension facets
- `internal/search/search.go` — `Search` query composition via `query.NewAnd` (54-64), the pattern facets must follow; the facet-free aggregation pass hangs off the same entry point
- `internal/config/config.go` — `EditorConfig.Scheme` / `.Roots`, and the scheme→binary map the open endpoint needs (`vscode`→`code`)
- `internal/cli/web.go` — server construction and wiring
- `internal/web/api_test.go`, `server_test.go`, `checkouts_test.go` — existing test patterns
