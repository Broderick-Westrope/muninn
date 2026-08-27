# Phase 4: Result Facets

> **Status:** DRAFT
> **Depends on:** Phase 3 (sidebar occupancy pattern; both phases touch the sidebar)
> **Delivers:** `Options.FileFilter`, server-side facet aggregation in `/api/search`, and a toggleable repo/extension facet panel in the results sidebar.

## Specification

**Problem:** A broad query across 200+ indexed repos returns a flat list capped at 100 matches. Narrowing it requires hand-typing zoekt atoms, which the `#hints` chips teach but do not make discoverable — you must already know which repo you want.

**Goal:** The results sidebar lists the repos and file extensions present for the query, with counts. Clicking toggles a filter. Multiple selections combine (OR within a category, AND across). The typed query is never rewritten.

**Scope:**

In: `Options.FileFilter`, facet aggregation pass, `repo`/`ext` params on `/api/search` and in the page URL, facet panel UI, param validation and caps.

Out: path-prefix facets, bidirectional query↔facet sync, per-category facet-excluding counts, index-wide counts.

**Success Criteria:**

- [ ] `/api/search` returns a `facets` block computed from a pass that ignores the request's own facet params
- [ ] Selecting a repo does not change any facet count
- [ ] A facet count is never lower than the rows displayed for that value
- [ ] Extensionless files and dotfiles share one "no extension" bucket; `.GO` and `.go` share one bucket
- [ ] A facet applied to a query with a top-level `or` filters both sides
- [ ] A repo name containing a regex metacharacter filters to exactly that repo
- [ ] Params beyond the caps are rejected with 400
- [ ] Selections survive reload and appear in the URL
- [ ] The search input is never programmatically rewritten
- [ ] `go test ./internal/search/... ./internal/web/...` passes

## Context Loading

```bash
sed -n '50,100p' internal/search/search.go      # Search: query.Parse, NewAnd, Simplify
cat internal/search/types.go
sed -n '84,110p'  internal/web/api.go            # handleSearch
sed -n '272,320p' internal/web/api.go            # trimResult
sed -n '260,280p' internal/mcp/tools_search.go   # QuoteMeta alternation precedent
rg -n "runSearch|renderStats|searchCtl|DEBOUNCE_MS" internal/web/static/*.js
```

## Two Traps, Both Load-Bearing

**1. String concatenation silently breaks `or` queries.** Zoekt's `or` binds looser than implicit AND, so appending ` file:\.go$` to `foo or bar` parses as `foo OR (bar AND file:\.go$)` — the facet filters half the results and reports nothing wrong. `RepoFilter` already avoids this by parsing first, then `query.NewAnd` (search.go:54-64). Every facet filter must compose the same way.

Verified against the pinned zoekt: `query.Parse` accepts a grouped, anchored, alternated `file:` regex as a single `file_regex` atom, and `query.NewAnd(orQuery, fileAtom)` yields `(and (or …) file_regex:…)` — the AST-level guarantee holds.

**2. Deriving facet values from the filtered results collapses on the second click.** Select repo A → results contain only repo A → every other repo facet vanishes. Multi-select becomes unbuildable, and a zero-result combination leaves no chip to click to undo. The facet universe must therefore come from a pass that ignores the facet selection.

## Counting Unit: Line Matches, Not Files

The spec requires a facet count never to be lower than the rows displayed for that value. A row is a **line match** — `fileCard` renders one per line (`app.js:224-235`) and `trimResult` counts line matches against the limit (api.go:285-313). So facet counts count line matches too; counting files would show "1" beside 40 rows for one heavily-matching file. Since the aggregation cap (1000) is well above the display limit (100), the count stays ≥ the rows shown.

## Fixture Work Comes First

Both existing fixtures are single-repo and all-`.go`:

- `search_test.go` `fixtureIndex` (47-71): one repo, two files, both `.go`
- `api_test.go` `newFixture` (62-108): one repo, `widget.go`, `other.go`, `data.bin`, `big.txt`

That cannot test any facet behaviour that matters. Worse, `TestSearchFileFilterComposition` — the `or`-precedence regression test — **cannot fail against the buggy implementation** on an all-`.go` fixture, because the correct and broken parses return the same files. Task 0 fixes this before anything else.

Note `search_test.go:245`, `api_test.go:410` and `api_test.go:433` all assert `len(repos) != 1`, so adding a second repo to a shared fixture breaks them.

## Backend Tasks

### Task 0: Fixture groundwork

**Context:** `internal/search/search_test.go`, `internal/web/api_test.go`

**Files:**
- Modify: `internal/search/search_test.go`, `internal/web/api_test.go`

**Steps:**

1. [ ] Extend `fixtureIndex` (search_test.go:47-71) with files that make extension facets meaningful: a `.ts` file and an extensionless file (e.g. `Makefile`), both containing a token the other files also contain so a single query matches across extensions. Additive to the file map — confirm no existing test asserts an exact file count.

2. [ ] Add a **second repo** behind a variant constructor rather than changing `fixtureIndex`. `IndexRepo` is called per repo against the same `indexDir` (search_test.go:66), so a second call with a different name and mirror suffices:
   ```go
   // fixtureIndexMultiRepo indexes acme/widget plus a second repo whose name
   // contains a regex metacharacter, so facet alternations can be tested for
   // both multi-repo OR and QuoteMeta escaping.
   func fixtureIndexMultiRepo(t *testing.T) (indexDir, commit string)
   ```
   Name the second repo `acme/my.lib` — the dot is the escaping test.

3. [ ] Add the same variant for the web fixture: `newFixtureMultiRepo`, writing both repos into the status file (extend `writeStatus`, api_test.go:112-127, to take a repo→commit map).

4. [ ] Leave `TestAPIRepos`, `TestAPIReposStale`, and `TestListRepos` on the single-repo fixture so their `len(repos) != 1` assertions keep passing.

**Verify:**
```bash
go test ./internal/search/... ./internal/web/...
# Expected: every existing test still passes; new fixtures compile but are unused
```

### Task 1: `Options.FileFilter` and the facet aggregation pass

**Context:** `internal/search/search.go`, `internal/search/types.go`

**Files:**
- Modify: `internal/search/types.go`, `internal/search/search.go`
- Test: `internal/search/search_test.go`

**Steps:**

1. [ ] Add `FileFilter` to `Options` (types.go:6-18), documented alongside `RepoFilter`:
   ```go
   // FileFilter is an optional file-path regexp ANDed into the query.
   // Composed at the query AST level, never concatenated onto Query:
   // zoekt's `or` binds looser than implicit AND, so a concatenated atom
   // would silently fail to filter one side of a disjunction.
   FileFilter string
   ```

2. [ ] Apply it in `Search`, immediately after the `RepoFilter` block (search.go:58-64). `query.Repo` takes a compiled `*regexp.Regexp`, but zoekt's filename node wants a parsed `*syntax.Regexp` plus explicit flags — so route through `query.Parse` instead of reaching for that API:
   ```go
   if opts.FileFilter != "" {
       fq, err := query.Parse("file:" + opts.FileFilter)
       if err != nil {
           return nil, fmt.Errorf("parsing file filter %q: %w", opts.FileFilter, err)
       }
       q = query.NewAnd(q, fq)
   }
   ```
   This is the exact path zoekt's own `file:` atom takes, so there is no second regex API and no case-sensitivity flag to get wrong.

   `FileFilter` is always server-constructed, never user input, so a parse failure is a **server bug**. The handler maps it to 500, not the 400 that `handleSearch` returns for user query parse errors (api.go:105-108) — see Task 2.

3. [ ] Add the facet types to **`types.go`** (where the package keeps its public types) and `Aggregate` to `search.go`:
   ```go
   // Facets counts line matches per repo and per file extension for a query,
   // ignoring any facet filters. Aggregation runs at its own cap, independent
   // of the display limits, so a count is never lower than the rows a
   // filtered search displays. Truncated reports that the cap was hit, so the
   // value list is not exhaustive.
   type Facets struct {
       Repos     []FacetValue
       Exts      []FacetValue
       Truncated bool
   }

   // FacetValue is one facet bucket: a value and how many matching lines
   // carry it. Value is "" in Exts for files with no extension.
   type FacetValue struct {
       Value string
       Count int
   }
   ```
   ```go
   // Aggregate runs q with no facet filters and buckets the matches by repo
   // and by file extension. Counts are line matches, matching what the UI
   // renders as rows.
   func (s *Searcher) Aggregate(ctx context.Context, q string, maxResults int) (*Facets, error)
   ```
   Name the parameter `q`, not `query` — `query` is the imported zoekt package (search.go:16) and would be shadowed.

   Implement by calling `Search` with `Options{Query: q, MaxResults: maxResults}` — no `RepoFilter`, no `FileFilter` — then bucketing. Count `len(f.Lines)` per file, falling back to 1 for a filename-only match, mirroring `trimResult`'s rule (api.go:293-297). Set `Truncated` from the result's `Truncated`.

   Document the known limitation: the cap counts line matches, so one hot repo can consume the budget and narrow the value list. `Truncated` is what discloses it.

4. [ ] Add `extOf`, with the bucketing rules the spec pins down:
   ```go
   // extOf returns the lowercased extension of a path's basename, or ""
   // when it has none. A leading dot does not start an extension, so
   // ".gitignore" and "Makefile" both bucket as "".
   func extOf(path string) string {
       base := path
       if i := strings.LastIndexByte(base, '/'); i >= 0 {
           base = base[i+1:]
       }
       i := strings.LastIndexByte(base, '.')
       if i <= 0 { // -1 none; 0 leading dot (dotfile)
           return ""
       }
       return strings.ToLower(base[i+1:])
   }
   ```

5. [ ] Sort both slices by count descending, then value ascending, so the order is stable across identical requests.

6. [ ] Tests in `search_test.go`, on the Task 0 fixtures:
   - `TestFileFilterParses`: assert `query.Parse("file:" + facetExtFilter([]string{"go", ""}))` returns no error. Write this **first** — if zoekt's lexer terminated the `file:` value at `(` or `|`, the entire extension-facet mechanism would fail at parse time. (Verified working against the pinned version; pin it with a test.)
   - `TestSearchFileFilterComposition`: a query with a top-level `or` matching files of two different extensions, plus `FileFilter` for one of them, returns only that extension — from **both** sides of the disjunction. Confirm it fails against a concatenation implementation before fixing it; on an all-`.go` fixture it cannot.
   - `TestExtOf`: table covering `a/b/c.go`→`go`, `Makefile`→``, `.gitignore`→``, `a/.gitignore`→``, `x.TAR.GZ`→`gz`, `dir.d/file`→``
   - `TestAggregateIgnoresFilters`: counts identical whether or not filters would narrow
   - `TestAggregateCountsLines`: a file with several matching lines contributes that many, not 1
   - `TestSearchFileFilter` already exists for the `file:` atom (search_test.go:131) — pick a non-colliding name.

**Verify:**
```bash
go test ./internal/search/... -v
# Expected: all pass, including the or-composition test
```

### Task 2: Facet params and the `facets` block on `/api/search`

**Context:** `internal/web/api.go`, `internal/web/api_test.go`

**Files:**
- Modify: `internal/web/api.go`
- Test: `internal/web/api_test.go`

**Steps:**

1. [ ] Add caps to the const block (api.go:21-32):
   ```go
   // maxFacetRepos and maxFacetExts bound the facet params: each value
   // becomes a regex alternation branch compiled on every search.
   maxFacetRepos = 50
   maxFacetExts  = 20
   // facetAggregateLimit is the cap for the facet-free aggregation pass.
   // Higher than the display limit so a facet count is never lower than
   // the rows a filtered search displays.
   facetAggregateLimit = 1000
   ```

2. [ ] Add response types beside `searchResponse` (api.go:34-60) and a `Facets facetsJSON \`json:"facets"\`` field on it:
   ```go
   type facetsJSON struct {
       Repos     []facetValueJSON `json:"repos"`
       Exts      []facetValueJSON `json:"exts"`
       Truncated bool             `json:"truncated"`
   }

   type facetValueJSON struct {
       Value string `json:"value"` // "" in exts means no extension
       Count int    `json:"count"`
   }
   ```

3. [ ] Parse and validate the params in `handleSearch` (api.go:87-110). Values are comma-separated:
   ```go
   repos, err := parseFacetParam(r.URL.Query().Get("repo"), maxFacetRepos, "repo")
   if err != nil { writeError(w, http.StatusBadRequest, err.Error()); return }
   exts, err := parseFacetParam(r.URL.Query().Get("ext"), maxFacetExts, "ext")
   if err != nil { writeError(w, http.StatusBadRequest, err.Error()); return }
   ```
   `parseFacetParam` splits on `,`, drops empties, rejects over the cap with a message naming the cap, and returns the values. Extension values must additionally match `^[A-Za-z0-9_+-]*$` (empty is legal — the no-extension bucket) so a value cannot smuggle regex structure past `QuoteMeta`.

4. [ ] Build the filters. **`regexp.QuoteMeta` every value** — a repo named `acme/my.library` would otherwise match characters it should not (precedent: mcp/tools_search.go:269):
   ```go
   // facetRepoFilter builds an anchored alternation over exact repo names.
   func facetRepoFilter(repos []string) string {
       if len(repos) == 0 { return "" }
       alts := make([]string, len(repos))
       for i, r := range repos { alts[i] = "^" + regexp.QuoteMeta(r) + "$" }
       return "(" + strings.Join(alts, "|") + ")"
   }

   // facetExtFilter builds an alternation over file extensions. The empty
   // value means "no extension": a basename with no dot other than a leading
   // one, so both "Makefile" and ".gitignore" qualify — matching extOf.
   func facetExtFilter(exts []string) string {
       if len(exts) == 0 { return "" }
       alts := make([]string, 0, len(exts))
       for _, e := range exts {
           if e == "" {
               alts = append(alts, `(^|/)\.?[^/.]+$`)
               continue
           }
           alts = append(alts, `\.`+regexp.QuoteMeta(e)+`$`)
       }
       return "(" + strings.Join(alts, "|") + ")"
   }
   ```
   The no-extension branch is `(^|/)\.?[^/.]+$` and the details matter. `[^/.]+` forbids dots, so it cannot run past an extension; the optional leading `\.?` is what admits dotfiles. Verified against nine paths:

   | path | matches |
   |---|---|
   | `Makefile`, `a/Makefile`, `LICENSE` | yes |
   | `.gitignore`, `a/.gitignore` | yes |
   | `b.go`, `a/b.go`, `a.b.c` | no |
   | `dir.d/file` | yes (basename has no extension) |

   The obvious-looking `(^|/)[^/.][^/]*$` is **wrong in both directions** — `[^/]*` permits dots so it matches `a/b.go`, and the leading `[^/.]` excludes `.gitignore`, which `extOf` buckets as `""`. Filter and counts would disagree.

   Use the `grafana/regexp` import that `search.go` already uses, not stdlib, for consistency with the package it feeds.

5. [ ] Run both passes and assemble the response:
   ```go
   res, err := s.searcher.Search(r.Context(), search.Options{
       Query:      q,
       RepoFilter: facetRepoFilter(repos),
       FileFilter: facetExtFilter(exts),
       MaxResults: limit + 1,
   })
   if err != nil { writeError(w, http.StatusBadRequest, err.Error()); return }

   facets, err := s.searcher.Aggregate(r.Context(), q, facetAggregateLimit)
   if err != nil { writeError(w, http.StatusInternalServerError, err.Error()); return }

   out := trimResult(res, limit)
   out.Facets = toFacetsJSON(facets)
   writeJSON(w, http.StatusOK, out)
   ```
   One request, so there is no state where results rendered but the sidebar did not.

   **Always run the aggregation pass.** Do not "optimise" it away when no facet is selected by reusing `res`: the results pass runs at `limit + 1` (~51) while aggregation runs at 1000, so counts derived from `res` would be computed over a smaller universe and would **change on the user's first click** — destroying the one invariant this whole design exists to guarantee ("selecting a repo does not change any facet count"). The extra pass is the cost of that guarantee on a local tool; pay it.

   Note the asymmetric error mapping: the results pass can fail on a bad user query (400), but `Aggregate` runs the same query with no filters, so a failure there is a server fault (500). Do not copy the 400.

6. [ ] Tests in `api_test.go`, on `newFixtureMultiRepo` from Task 0:
   - `TestAPISearchFacets`: response carries repo and ext buckets with counts
   - `TestAPISearchFacetsIgnoreSelection`: counts identical with and without `repo=`. **The fixture must produce more matches than `limit + 1`** or this passes even with the skip-the-second-pass bug present — use an explicit small `limit` param to force the gap.
   - `TestAPISearchFacetCountNotBelowRows`: a file with several matching lines — its repo's facet count is ≥ the rows returned
   - `TestAPISearchFacetFilter`: `repo=acme/widget` narrows results; a non-matching repo returns none
   - `TestAPISearchFacetTwoRepos`: `repo=acme/widget,acme/my.lib` returns files from **both** (the OR-within-category criterion)
   - `TestAPISearchFacetIntersection`: `repo=acme/widget&ext=go` returns only `.go` files from that repo (the AND-across-categories criterion)
   - `TestAPISearchFacetRegexMeta`: `repo=acme/my.lib` matches only that repo, not a hypothetical `acme/myxlib` — the `QuoteMeta` test
   - `TestAPISearchFacetExtFilter`: `ext=go` returns only `.go`; `ext=` returns only the extensionless file; `ext=go,ts` returns both extensions
   - `TestAPISearchFacetWithTypedAtom`: a query already containing `repo:` composes with an `ext=` param rather than conflicting
   - `TestAPISearchFacetCaps`: 51 repos → 400, 21 exts → 400
   - `TestAPISearchFacetExtValidation`: `ext=go$|.*` → 400

**Verify:**
```bash
go test ./internal/web/... -run TestAPISearchFacet -v
go build ./... && go test ./...
```

## Frontend Tasks

### Task 3: Facet panel and URL state

**Context:** `internal/web/static/search.js`, `main.js`, the sidebar from Phase 2

**Files:**
- Create: `internal/web/static/facets.js`
- Modify: `internal/web/static/search.js`, `internal/web/static/main.js`, `internal/web/static/style.css`

**Steps:**

1. [ ] Create `facets.js` owning facet selection state as two `Set`s (`repos`, `exts`), read from and written to the URL. **`facets.js` must not import `search.js`** — `runSearch` needs `facetParams()` and `toggleFacet` needs to re-search, which would be a cycle, and Phase 1's stated graph forbids it. Inject the callback from `main.js` instead:
   ```js
   // Facet state lives in its own URL params, never in ?q= — the search
   // input is never programmatically rewritten.
   export function readFacetsFromURL() { /* parse ?repo= and ?ext= */ }
   export function facetParams() { /* -> "&repo=a,b&ext=go" for the API call */ }
   export function renderFacets(facets) { /* build the panel */ }
   // onToggle is runSearch, passed in by main.js so this module never
   // imports search.js (that would be a cycle).
   export function initFacets({ facetsEl, onToggle }) { }
   ```
   Sync the URL with `history.replaceState`, matching how `runSearch` already handles `?q=` (`app.js:161-164`), so facet toggles do not pile up history entries.

   `facetsEl` is `#sidebar-facets` from Phase 2 — its own container, so the tree never overwrites it.

2. [ ] Include the params in the search request. `runSearch` takes them as an **argument** rather than importing `facets.js`, keeping the dependency one-way (`main.js` → both):
   ```js
   // in main.js
   runSearch(searchInput.value.trim(), true, facetParams());
   // in search.js
   `/api/search?q=${encodeURIComponent(q)}&limit=${SEARCH_LIMIT}${params}`
   ```
   `runSearch` also calls `renderFacets(data.facets)` — pass that in at init the same way, or have `main.js` wire it as a post-search callback. Either is fine; importing `facets.js` from `search.js` is not.

3. [ ] Render chips as the **union of the selection and the returned values**. The universe normally contains every selected value, but a URL carrying `repo=X` that does not match the query would not — the union is what guarantees a selection is always deselectable. A selected value absent from the universe renders with a count of 0, still clickable to remove.

4. [ ] A facet toggle re-searches **immediately**, bypassing the 200ms input debounce (`app.js:559-562`) — it is a click, not typing. Reuse the existing `searchCtl` abort so a fast double-toggle cannot render out of order.

5. [ ] Label the no-extension bucket explicitly (e.g. "no extension") rather than showing an empty chip.

6. [ ] Show `facets.truncated` in the panel as a note that the value list is not exhaustive. Keep it distinct from the results-level `truncated` warning in the stats footer (`app.js:248-253`) — the two describe different caps.

7. [ ] Render the panel into `#sidebar-facets`. Phase 2's `data-view` CSS already hides it in the file view, so no manual clearing is needed.

8. [ ] Sequence `readFacetsFromURL()` **before** the initial `runSearch(initialQ)` in `main.js`'s init block (`app.js:587-590`). Without that ordering the first search fires with no facet params and a reloaded filtered URL renders unfiltered results.

9. [ ] Style chips on the existing `.chip` pattern (style.css:118-131) with a clear selected state — do not rely on colour alone.

**Verify:**
```bash
go run . web
# Search something broad:
# - Sidebar lists repos and extensions with counts
# - Click a repo: results narrow, URL gains ?repo=, other repos STAY listed
# - Click a second repo: results include both
# - Click again: deselects
# - Reload: selections restored from the URL
# - Search input never changes on its own
```

## Manual Verification

- [ ] Facet counts do not change when a facet is selected
- [ ] After selecting one repo, other repos remain listed and clickable
- [ ] Two repos selected by clicking returns results from both
- [ ] Repo + extension together narrows to the intersection
- [ ] A zero-result combination still renders the selected chips, and clicking one recovers
- [ ] A URL with a `repo` value that does not match the query still renders a removable chip
- [ ] No facet count is lower than the rows shown for it
- [ ] Toggling re-searches immediately, without waiting out the debounce
- [ ] The search input is never rewritten
- [ ] A hand-typed `repo:` atom composes with facet selections
- [ ] Selections are shareable via URL
- [ ] Facet truncation note is distinct from the results truncation warning
- [ ] Opening a file swaps the sidebar to the tree; returning restores the facets

## PR

Open a PR for human review. Point at `TestSearchFileFilterComposition` explicitly — the `or` precedence bug it guards is invisible in manual testing and is the single most important thing in this phase.
