# Phase 3: File Tree

> **Status:** DRAFT
> **Depends on:** Phase 2 (sidebar shell)
> **Delivers:** `gitfile.ListDir`, `GET /api/tree`, and a lazily-expanding file tree in the sidebar.

## Specification

**Problem:** Opening a file is a dead end. Reaching a sibling or neighbouring file means returning to results and searching again; there is no way to see a file in the context of its directory.

**Goal:** Opening a file shows a tree in the sidebar, rooted at that file's repo, expanded to reveal the file. Directories expand on click, fetching only their immediate children.

**Scope:**

In: `gitfile.ListDir`, `GET /api/tree`, tree rendering, ancestor-spine fan-out on file open, per-node loading and error state, abort scoped to repo change.

Out: standalone browsing (no tree without an open file), path-prefix facets, any results-view sidebar content.

**Success Criteria:**

- [ ] `ListDir` uses a trailing-slash pathspec and returns repo-relative child paths, excluding the anchor
- [ ] `ListDir` returns `fs.ErrNotExist` for a missing path and `ErrNotDir` for a file path
- [ ] `/api/tree` maps those to 404 and 400, and `ErrIndexMismatch` to 409
- [ ] `ListTree`'s behaviour is unchanged: `TestListTreeDepth` and MCP `list_tree` output unaffected
- [ ] A directory over `maxEntries` returns `truncated: true`
- [ ] Tree renders for a repo with no local checkout
- [ ] Rapid cross-repo navigation never renders a stale tree
- [ ] Same-repo navigation preserves expansion state
- [ ] `go test ./internal/gitfile/... ./internal/web/...` passes

## Context Loading

```bash
sed -n '117,225p' internal/gitfile/gitfile.go     # ListTree, parseTreeEntry, relativeDepth
sed -n '226,258p' internal/gitfile/gitfile.go     # checkCommit, runGit
sed -n '200,260p' internal/gitfile/gitfile_test.go # TestListTreeDepth (contract to preserve)
sed -n '40,80p'  internal/gitfile/gitfile_test.go  # fixtureMirror
sed -n '112,160p' internal/web/api.go              # handleFile, resolveIndexedCommit
grep -n "ListTree" internal/mcp/tools_file.go      # the other caller
```

## Why a New Function, Not a Flag on `ListTree`

`ListTree` runs `git ls-tree -r -t` and filters by depth (gitfile.go:136, 154-156), so a depth-1 listing walks the whole tree. It cannot simply drop `-r`, because its contract **includes the anchor**: `relativeDepth("a", "a")` returns 1 (gitfile.go:210-216), so `ListTree(…, "a", 1, 0)` yields `a`, `a/b`, `a/d.txt`. `TestListTreeDepth` asserts exactly 3 entries (gitfile_test.go:234-241) and MCP `list_tree` shares the contract.

Verified pathspec behaviour against this repo:

```
git ls-tree --long <commit> -- internal/web    → 1 entry: the tree itself
git ls-tree --long <commit> -- internal/web/   → the children, repo-relative paths
```

So children need the trailing slash — which then makes "missing directory" and "path is a file" indistinguishable, since a blob pathspec with a slash matches nothing. Hence the two-call probe below.

## Backend Tasks

### Task 1: `gitfile.ListDir`

**Context:** `internal/gitfile/gitfile.go`, `internal/gitfile/gitfile_test.go`

**Files:**
- Modify: `internal/gitfile/gitfile.go`
- Test: `internal/gitfile/gitfile_test.go`

**Steps:**

1. [ ] Add an `ErrNotDir` sentinel beside `ErrFileTooLarge` (gitfile.go:27):
   ```go
   // ErrNotDir is returned by ListDir when the path names a file rather
   // than a directory.
   var ErrNotDir = errors.New("path is a file, not a directory")
   ```

2. [ ] Add `ListDir`. Two git calls for a non-empty path (anchor probe, then children), one for the root:
   ```go
   // ListDir returns the immediate children of path at commit in the bare
   // mirror at mirrorDir, without the anchor directory itself. Path "" lists
   // the repo root. At most maxEntries entries are returned (0 means
   // unlimited); total reports how many exist, so total > len(entries) means
   // the listing was truncated.
   //
   // Unlike ListTree this is non-recursive: git walks one tree object rather
   // than the whole commit. Listing children requires a trailing-slash
   // pathspec, which matches nothing for a blob, so a non-empty path is first
   // probed without the slash to tell a missing path from a file. An
   // unreachable commit yields ErrIndexMismatch, a missing path
   // fs.ErrNotExist, and a file path ErrNotDir.
   func ListDir(ctx context.Context, mirrorDir, commit, path string, maxEntries int) (entries []TreeEntry, total int, err error) {
       if err := checkCommit(ctx, mirrorDir, commit); err != nil {
           return nil, 0, err
       }
       prefix := strings.Trim(path, "/")
       if prefix != "" {
           anchor, err := runGit(ctx, "-C", mirrorDir, "ls-tree", "--long", "-z", commit, "--", prefix)
           if err != nil {
               return nil, 0, fmt.Errorf("probing %q at commit %s: %w", path, shortSHA(commit), err)
           }
           record, _, _ := strings.Cut(anchor, "\x00")
           if record == "" {
               return nil, 0, fmt.Errorf("path %q not found at commit %s: %w", path, shortSHA(commit), fs.ErrNotExist)
           }
           entry, err := parseTreeEntry(record)
           if err != nil {
               return nil, 0, fmt.Errorf("probing %q at commit %s: %w", path, shortSHA(commit), err)
           }
           if entry.Type != "dir" {
               return nil, 0, fmt.Errorf("path %q at commit %s: %w", path, shortSHA(commit), ErrNotDir)
           }
       }

       args := []string{"-C", mirrorDir, "ls-tree", "--long", "-z", commit}
       if prefix != "" {
           args = append(args, "--", prefix+"/")
       }
       out, err := runGit(ctx, args...)
       if err != nil {
           return nil, 0, fmt.Errorf("listing directory %q at commit %s: %w", path, shortSHA(commit), err)
       }
       for _, record := range strings.Split(out, "\x00") {
           if record == "" {
               continue
           }
           total++
           if maxEntries > 0 && len(entries) >= maxEntries {
               continue
           }
           entry, err := parseTreeEntry(record)
           if err != nil {
               return nil, 0, fmt.Errorf("listing directory %q at commit %s: %w", path, shortSHA(commit), err)
           }
           entries = append(entries, entry)
       }
       return entries, total, nil
   }
   ```
   No recursion means no depth filtering, so `relativeDepth` is not involved and `ListTree` is untouched.

   Three details in that snippet are correct but look wrong — do not "fix" them:
   - `strings.Cut(anchor, "\x00")` isolates one record because `runGit` trims whitespace only, and `\x00` is not whitespace (gitfile.go:243-246).
   - `parseTreeEntry` handles a **tree** record: `--long` emits `-` in the size column, and `strconv.ParseInt` is only reached on the `blob` branch (gitfile.go:190-204).
   - `ls-tree` exits 0 with empty output for a non-matching pathspec, so the `record == ""` → `fs.ErrNotExist` branch is reachable rather than dead. This is the same inference `ListTree` already makes (gitfile.go:168-174).

3. [ ] Add tests to `gitfile_test.go` using the existing `fixtureMirror` (its tree is `file.txt`, `top.txt`, `a/d.txt`, `a/b/c.txt`):
   - `TestListDirRoot`: `ListDir(ctx, mirror, commit1, "", 0)` returns exactly `a`, `file.txt`, `top.txt` — `a` typed `dir`, the others `file`, and **not** `a/d.txt` or `a/b/c.txt`
   - `TestListDirSubdir`: `ListDir(…, "a", 0)` returns exactly `a/b` and `a/d.txt`, repo-relative, with no `a` self-entry — this is the contrast with `ListTree`'s 3
   - `TestListDirNotFound`: `ListDir(…, "nope", 0)` returns an error satisfying `errors.Is(err, fs.ErrNotExist)`
   - `TestListDirNotDir`: `ListDir(…, "file.txt", 0)` returns an error satisfying `errors.Is(err, ErrNotDir)`
   - `TestListDirMaxEntries`: `ListDir(…, "", 2)` returns 2 entries with `total == 3`
   - `TestListDirIndexMismatch`: a bogus commit returns `ErrIndexMismatch` (mirror the existing `ListTree` mismatch test)

4. [ ] Re-run `TestListTreeDepth` and confirm it is untouched — this is the regression the whole design avoids.

**Verify:**
```bash
go test ./internal/gitfile/... -run 'TestListDir|TestListTree' -v
# Expected: all new TestListDir* pass; TestListTreeDepth still passes unchanged
```

### Task 2: `GET /api/tree`

**Context:** `internal/web/api.go`, `internal/web/server.go`, `internal/web/api_test.go`

**Files:**
- Modify: `internal/web/api.go`, `internal/web/server.go`
- Test: `internal/web/api_test.go`

**Steps:**

1. [ ] Add response types beside `fileResponse` (api.go:62-73):
   ```go
   // treeResponse is the JSON shape of /api/tree.
   type treeResponse struct {
       Entries   []treeEntryJSON `json:"entries"`
       Truncated bool            `json:"truncated"`
   }

   // treeEntryJSON is one child of a listed directory.
   type treeEntryJSON struct {
       Path string `json:"path"` // repo-relative
       Type string `json:"type"` // "file" or "dir"
       Size int64  `json:"size"` // 0 for directories
   }
   ```

2. [ ] Add `maxTreeEntries = 2000` as a **`Server` field**, defaulted in `New`, not a package const. A const cannot be lowered from a test, so the truncation path would be unreachable without a 2000-entry fixture. Document that it bounds payload and DOM for a pathological generated directory.

3. [ ] Add `handleTree`, following `handleFile`'s error-mapping shape (api.go:128-142):
   ```go
   // handleTree serves GET /api/tree?repo=<owner/name>&path=<dir>, listing
   // one directory's immediate children pinned to the repo's indexed commit.
   func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
       repo := r.URL.Query().Get("repo")
       dir := r.URL.Query().Get("path") // "" lists the repo root
       if !validTreePath(dir) {
           writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid path %q", dir))
           return
       }
       mirrorDir, commit, herr := s.resolveIndexedCommit(repo)
       if herr != nil {
           writeError(w, herr.code, herr.msg)
           return
       }
       entries, total, err := gitfile.ListDir(r.Context(), mirrorDir, commit, dir, s.maxTreeEntries)
       switch {
       case errors.Is(err, gitfile.ErrNotDir):
           writeError(w, http.StatusBadRequest, err.Error())
           return
       case errors.Is(err, fs.ErrNotExist):
           writeError(w, http.StatusNotFound, err.Error())
           return
       case errors.Is(err, gitfile.ErrIndexMismatch):
           writeError(w, http.StatusConflict, err.Error())
           return
       case err != nil:
           writeError(w, http.StatusInternalServerError, err.Error())
           return
       }
       out := treeResponse{Entries: make([]treeEntryJSON, 0, len(entries))}
       for _, e := range entries {
           out.Entries = append(out.Entries, treeEntryJSON{Path: e.Path, Type: e.Type, Size: e.Size})
       }
       out.Truncated = total > len(entries)
       writeJSON(w, http.StatusOK, out)
   }
   ```
   Order matters: check `ErrNotDir` before `fs.ErrNotExist`. Both are plain sentinels here so they cannot collide, but the ordering documents intent and survives a future wrap.

   `validTreePath` rejects a dot segment and a leading `:`. The path goes into a git pathspec, and pathspec magic (`:(exclude)`, glob characters) stays active after `--`. This is not a filesystem escape — it addresses a tree object, not a path on disk — but `..` should be a 400 rather than a 500, and magic should not be reachable:
   ```go
   // validTreePath rejects tree paths that would reach git pathspec magic
   // or a dot segment. "" (the repo root) is valid.
   func validTreePath(p string) bool {
       if strings.HasPrefix(p, ":") {
           return false
       }
       for _, seg := range strings.Split(p, "/") {
           if seg == "." || seg == ".." {
               return false
           }
       }
       return true
   }
   ```
   Note in the PR that `ListTree` has the same latent pathspec behaviour on its MCP path; out of scope here, worth a follow-up.

4. [ ] Register the route in `Handler()` (server.go:66-74):
   ```go
   mux.HandleFunc("GET /api/tree", s.handleTree)
   ```

5. [ ] Extend `newFixture` (api_test.go:62) with a subdirectory — the current repo is flat (`widget.go`, `other.go`, `data.bin`, `big.txt`) and cannot exercise `type: "dir"`. Add `pkg/sub.go` to the fixture file map; it is additive and breaks no existing assertion.

6. [ ] Add handler tests:
   - `TestAPITree`: root listing returns the four files plus `pkg` typed `dir`, `truncated` false
   - `TestAPITreeSubdir`: `path=pkg` returns only `pkg/sub.go`, repo-relative, with no `pkg` self-entry
   - `TestAPITreeNotFound`: `path=nope` → 404
   - `TestAPITreeNotDir`: `path=widget.go` → 400
   - `TestAPITreeInvalidPath`: `path=../x` and `path=:(exclude)x` → 400
   - `TestAPITreeInvalidRepo`: reuse the `TestAPIFileInvalidRepo` table (api_test.go:272-294) — bad owner/name forms → 400
   - `TestAPITreeUnknownRepo`: a repo absent from the status file → 404
   - `TestAPITreeIndexMismatch`: rewrite the status file to a bogus commit (reuse `writeStatus`, api_test.go:112) → 409
   - `TestAPITreeTruncated`: set `srv.maxTreeEntries = 2` → 2 entries and `truncated: true`

**Verify:**
```bash
go test ./internal/web/... -run TestAPITree -v
go build ./... && go test ./...
# Expected: all nine tree handler tests pass, whole suite green
```

## Frontend Tasks

### Task 3: Tree rendering in the sidebar

**Context:** `internal/web/static/`, the modules from Phase 1, the sidebar shell from Phase 2

**Files:**
- Create: `internal/web/static/tree.js`
- Modify: `internal/web/static/main.js`, `internal/web/static/style.css`

**Steps:**

1. [ ] Create `tree.js` owning all tree state and rendering. Module-private state:
   ```js
   let treeRepo = null;   // repo the current tree belongs to
   let treeCtl = null;    // AbortController, replaced on repo change
   const expanded = new Map(); // dir path -> {entries, truncated} once loaded
   ```

2. [ ] Export `showTree(loc)`, called by the router on file navigation:
   - If `loc.repo !== treeRepo`: abort `treeCtl`, clear `expanded`, set `treeRepo = loc.repo`, create a fresh `AbortController`. **Do not abort on same-repo navigation** — that would cancel an expand the user just clicked (spec decision).
   - Compute the ancestor spine from `loc.path`: for `internal/web/static/app.js`, that is `['', 'internal', 'internal/web', 'internal/web/static']`.
   - Fetch any spine directory not already in `expanded`, in parallel via `Promise.allSettled` so one failure does not sink the rest.
   - Render, marking `loc.path` as current.

3. [ ] Export `initTree(treeEl)` for the DOM ref (`#sidebar-tree`), matching the Phase 1 pattern.

4. [ ] Fetch through the shared `api` helper from `./api.js`, passing `treeCtl.signal`:
   ```js
   const data = await api(
     `/api/tree?repo=${encodeURIComponent(repo)}&path=${encodeURIComponent(dir)}`,
     treeCtl.signal,
   );
   ```
   Guard every render against a stale response. After an `await`, check **both** that the repo still matches and that the file view is still active:
   ```js
   if (repo !== treeRepo || document.body.dataset.view !== 'file') return;
   ```
   The repo check alone does not cover pressing Esc back to results — a slow response would then paint the tree while the user is looking at the facet panel. `AbortError` must be swallowed silently, as `showFile` already does (`app.js:289`).

5. [ ] Render entries as nested `<ul>`/`<li>` into **`#sidebar-tree`** (the container Phase 2 added; the facet panel owns `#sidebar-facets`, so the two never `replaceChildren` the same node). Directories are buttons that toggle expansion; files are anchors using `fileHash(repo, path)` from `./routes.js`, so tree navigation goes through the existing router and needs no special-casing. Sort directories before files, then alphabetically. Display the basename, not the full path. Build with `textContent`/`append` — never `innerHTML`.

6. [ ] Per-node state, all three required by the spec:
   - **loading**: a directory awaiting its response shows an inline indicator
   - **error**: a failed expand shows an inline message on that node with a retry button, never an empty directory
   - **truncated**: a node whose response has `truncated: true` shows a "listing truncated" note

7. [ ] Wire into the router in `main.js`: call `showTree(loc)` where the file view is entered. The `data-view` CSS from Phase 2 already hides `#sidebar-tree` in the results view, so no explicit clearing is needed — but keep the rendered tree in place rather than tearing it down, so returning to the same repo's file view is instant.

8. [ ] Add tree CSS: monospace at the existing 12px scale, current file highlighted with `var(--accent)`, disclosure indicator for directories, indentation via nested `<ul>` padding. Nothing may fight the sidebar's `overflow-y: auto` from Phase 2 — no `overflow: hidden` on ancestors.

**Verify:**
```bash
go run . web
# Open a file from search results:
# - Tree appears rooted at that repo, ancestors expanded, file marked
# - Clicking a sibling file navigates without a page reload
# - Clicking a collapsed directory loads and expands its children
# - Network tab: one /api/tree request per ancestor on open, one per expand
```

## Manual Verification

- [ ] Tree renders rooted at the open file's repo with the spine expanded and the file marked
- [ ] Clicking a tree file navigates; the tree keeps its expansion state
- [ ] Expanding a directory issues exactly one request for that directory
- [ ] Navigating to a file in a **different** repo resets the tree
- [ ] Navigating within the same repo preserves expansion, including an in-flight expand
- [ ] Rapidly opening files across repos never leaves a stale tree (throttle the network to force it)
- [ ] A repo with no local checkout still renders a full tree
- [ ] A failed expand shows an inline error with a working retry
- [ ] Returning to results clears the sidebar
- [ ] A tree taller than the viewport scrolls inside the sidebar
- [ ] Deep-linking straight to a file URL builds the tree correctly on load

## PR

Open a PR for human review. Call out the `ListDir` vs `ListTree` split explicitly — a reviewer's first instinct will be "why not a flag on `ListTree`", and the anchor self-entry contract is the answer.
