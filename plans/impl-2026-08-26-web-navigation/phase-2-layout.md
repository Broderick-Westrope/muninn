# Phase 2: Layout Shell and Repo Prominence

> **Status:** DRAFT
> **Depends on:** Phase 1 (ES modules)
> **Delivers:** Width cap removed, `--sticky-top` per view, sticky repo group headers with match counts, and the empty sidebar shell that Phases 3 and 4 fill.

## Specification

**Problem:** Two of the spec's four gaps are pure layout. `main` is capped at `max-width: 1100px` (style.css:148) while `.row code` truncates with an ellipsis (style.css:213-218), so the cap actively discards matched code you could otherwise read. And `.repo-group h2` renders 12px/600 in `var(--muted)` (style.css:152-157) while the paths beneath it render in `var(--accent)` — the group header is quieter than its own children, and it scrolls off screen entirely on a long result set, so mid-scroll there is no indication which repo you are reading.

**Goal:** Results use the full window width. Repo group headers are visually dominant and stay visible while scrolling. A sidebar column exists, correctly positioned and scrollable, ready for the tree and facets.

**Scope:**

In: `main` cap removal, `--sticky-top` variable, sticky strengthened group headers with match counts, sidebar shell markup and CSS, responsive breakpoint, stats footer offset.

Out: anything rendered *inside* the sidebar (Phases 3 and 4). The sidebar is an empty, correctly-sized column at the end of this phase.

**Success Criteria:**

- [ ] `main` carries no `max-width`
- [ ] Sticky offsets use `--sticky-top`; no new rule uses `--header-h` directly
- [ ] Group headers stay visible while scrolling and are not obscured by `#hints`
- [ ] Each group header shows the repo's match count
- [ ] The sidebar scrolls internally rather than scrolling out of view
- [ ] The stats footer is not obscured by the sidebar
- [ ] Below ~900px the sidebar is hidden
- [ ] `savedScroll` still restores the results position
- [ ] `go test ./internal/web/...` passes

## Context Loading

```bash
sed -n '1,60p'    internal/web/static/style.css   # variables, header
sed -n '105,160p' internal/web/static/style.css   # #hints, view switching, main
sed -n '280,310p' internal/web/static/style.css   # footer, .file-head sticky
cat internal/web/static/index.html
rg -n "renderResults|repo-group|savedScroll" internal/web/static/*.js
```

## The `--header-h` Trap

Read this before touching any sticky rule.

`--header-h: 41px` is documented as ".bar height + padding + border" (style.css:19). It does **not** include `#hints`, which sits inside the sticky `<header>` below `.bar` (index.html:21, style.css:110-115).

`.file-head` uses `top: var(--header-h)` (style.css:304) and is correct — but only because `#hints` is `display: none` in the file view (style.css:135-139). Group headers exist only in the **results** view, where `#hints` is visible. Reusing `--header-h` there slides them under the chips row.

Hence `--sticky-top`: `var(--header-h)` in the file view, bar-plus-hints in the results view.

## Tasks

### Task 1: Introduce `--sticky-top` and remove the width cap

**Context:** `internal/web/static/style.css`

**Files:**
- Modify: `internal/web/static/style.css`

**Steps:**

1. [ ] Add a `--hints-h` variable next to `--header-h` (style.css:19) and derive `--sticky-top`. `#hints` is `padding: 0 12px 6px` around 11px chips with `padding: 1px 7px`, which computes to roughly 24px — **measure the rendered height in devtools** rather than trusting that arithmetic, and record the measured value in a comment the way `--header-h` does:
   ```css
   --header-h: 41px; /* .bar height + padding + border; .file-head sticks below it */
   --hints-h: 24px;  /* measured #hints chip row; visible in the results view only */
   --sticky-top: calc(var(--header-h) + var(--hints-h));
   ```
   If the measurement disagrees with 24px, use the measured value — a wrong offset here is exactly the bug this variable exists to prevent, and it shows up as a sliver of scrolling content above each sticky header.
2. [ ] Override `--sticky-top` for the file view, where `#hints` is hidden:
   ```css
   body[data-view="file"] {
     --sticky-top: var(--header-h);
   }
   ```
3. [ ] Change `.file-head` (style.css:302-304) from `top: var(--header-h)` to `top: var(--sticky-top)`. Equivalent today, but it keeps every sticky rule on one variable so a future change cannot desync them.
4. [ ] Remove `max-width: 1100px` from `main` (style.css:148), keeping the padding.

**Verify:**
```bash
go run . web
# Results view: a long matching line shows more characters than before.
# File view: the file header still sticks directly below the header bar.
```

### Task 2: Sidebar shell markup and layout

**Context:** `internal/web/static/index.html`, `internal/web/static/style.css`

**Files:**
- Modify: `internal/web/static/index.html`, `internal/web/static/style.css`

**Steps:**

1. [ ] Add the sidebar to `index.html`, before `#results`/`#file`, inside `<main>`. **Two sibling containers, not one shared node** — Phase 3's tree and Phase 4's facets each own theirs, toggled by `data-view` exactly like `#results`/`#file` already are (style.css:135-143). One shared node would let a slow tree response `replaceChildren` over the facet panel after the user pressed Esc:
   ```html
   <main>
     <aside id="sidebar" aria-label="Navigation">
       <div id="sidebar-facets"></div>
       <div id="sidebar-tree"></div>
     </aside>
     <div id="content">
       <div id="results" role="region" aria-label="Search results"></div>
       <div id="file" role="region" aria-label="File view"></div>
     </div>
   </main>
   ```
   Add the matching view rules next to the existing ones:
   ```css
   body[data-view="file"]    #sidebar-facets,
   body[data-view="results"] #sidebar-tree { display: none; }
   ```
   The `#content` wrapper gives `main` exactly two grid children; without it the grid would have to place `#results` and `#file` individually and the `data-view` display toggles would fight the grid.

2. [ ] Lay out `main` as a two-column grid:
   ```css
   main {
     display: grid;
     grid-template-columns: var(--sidebar-w) minmax(0, 1fr);
     padding: 8px 12px 40px;
   }
   ```
   Add `--sidebar-w: 260px` to the variables block. `minmax(0, 1fr)` rather than `1fr` is required: a bare `1fr` has `min-width: auto`, so the wide `<pre>` in the file view would blow the column out instead of scrolling inside it.

3. [ ] Make the sidebar sticky and internally scrollable:
   ```css
   #sidebar {
     position: sticky;
     top: var(--sticky-top);
     align-self: start;
     max-height: calc(100vh - var(--sticky-top));
     overflow-y: auto;
     padding-right: 12px;
   }
   ```
   `align-self: start` matters — a grid item defaults to stretching to row height, and a stretched item cannot stick.

4. [ ] Offset the stats footer so the sidebar does not cover it. `footer` is `position: fixed; left: 0` (style.css:284-294). Place this rule **after** the existing footer block — equal specificity, so source order decides — and match `main`'s 12px padding:
   ```css
   footer {
     left: calc(var(--sidebar-w) + 12px);
   }
   ```

5. [ ] Check `.file-head`'s `margin: 0 -12px` (style.css:310). It deliberately bleeds to the edges of the old full-width `main`; under the grid it will bleed 12px of opaque `var(--bg)` at `z-index: 5` leftward over the sticky sidebar. Drop the left negative margin (`margin: 0 -12px 0 0`) or scope it to the content column.

6. [ ] Hide the sidebar below the breakpoint and collapse to one column:
   ```css
   @media (max-width: 900px) {
     main { grid-template-columns: minmax(0, 1fr); }
     #sidebar { display: none; }
     footer { left: 0; }
   }
   ```

**Verify:**
```bash
go run . web
# A 260px empty column sits left of the results; content fills the rest.
# Resize below 900px: the sidebar disappears and the footer spans full width.
# File view: a long code line scrolls horizontally inside the content
# column rather than widening the page.
```

### Task 3: Sticky, strengthened repo group headers with match counts

**Context:** `internal/web/static/style.css`, `internal/web/static/search.js`

**Files:**
- Modify: `internal/web/static/style.css`, `internal/web/static/search.js`

**Steps:**

1. [ ] In `renderResults` (moved to `search.js` in Phase 1, originally `app.js:192-211`), replace the plain `h.textContent = repo` with a split owner/name plus a count. The repo string is `owner/name`; `strings.Cut`-style splitting on the first `/`:
   ```js
   const h = document.createElement('h2');
   const [owner, ...rest] = repo.split('/');
   const ownerEl = document.createElement('span');
   ownerEl.className = 'repo-owner';
   ownerEl.textContent = owner + '/';
   const nameEl = document.createElement('span');
   nameEl.className = 'repo-name';
   nameEl.textContent = rest.join('/');
   const count = files.reduce((n, f) => n + (f.lines.length || 1), 0);
   const countEl = document.createElement('span');
   countEl.className = 'repo-count';
   countEl.textContent = `${count} ${plural(count, 'match', 'matches')}`;
   h.append(ownerEl, nameEl, countEl);
   ```
   `f.lines.length || 1` mirrors the server's own counting rule — a filename-only match has no lines but is still one result (`api.go:293-297`).

   Build with `textContent`/`append`, never `innerHTML`: repo names are index data and the codebase holds this line everywhere (see the comment at `app.js:100-102`).

   Note the owner and name spans must render as one unbroken string (`helse/pilot-engine`), so the flex `gap` below must not apply between them — see the CSS.

2. [ ] Style the header so it outranks the `var(--accent)` paths beneath it, and stick it. No flex `gap` — it would insert a space inside `helse/pilot-engine`; the count is pushed right with `margin-left: auto` instead:
   ```css
   .repo-group h2 {
     position: sticky;
     top: var(--sticky-top);
     z-index: 4;
     display: flex;
     align-items: baseline;
     margin: 14px 0 6px;
     padding: 4px 0;
     background: var(--bg);
     font-size: 13px;
     font-weight: 600;
     color: var(--fg);
   }

   .repo-owner { color: var(--muted); font-weight: 400; }
   .repo-name  { color: var(--fg); }
   .repo-count { margin-left: auto; padding-left: 8px; font-weight: 400; color: var(--muted); font-size: 11px; }
   ```
   `z-index: 4` sits below `.file-head`'s 5 and the header's 10 (style.css:54, 303). `background: var(--bg)` is required — a transparent sticky header lets result rows show through as they scroll under it.

**Verify:**
```bash
go run . web
# Search something broad enough to return several repos with many matches.
# - Header reads "helse/" muted, "pilot-engine" strong, count right-aligned
# - Scrolling holds the header below the chips row, NOT under it
# - The header is opaque; rows do not bleed through
# - The next repo's header pushes the previous one up
```

## Manual Verification

- [ ] Group header is clearly more prominent than the file paths beneath it
- [ ] Group header sticks below the `#hints` chip row, fully visible
- [ ] Match counts match the row counts in each group
- [ ] File view: `.file-head` still sticks correctly (no `#hints` in this view)
- [ ] Esc from a file restores the previous results scroll position
- [ ] Content uses full window width; long result lines show more than before
- [ ] Footer is not covered by the sidebar
- [ ] Below 900px the sidebar hides and the footer spans full width
- [ ] Dark mode: sticky header background matches the page (check `prefers-color-scheme: dark`)

## PR

Open a PR for human review. Include before/after screenshots at a wide viewport and mid-scroll — this phase is almost entirely visual and screenshots are the review.
