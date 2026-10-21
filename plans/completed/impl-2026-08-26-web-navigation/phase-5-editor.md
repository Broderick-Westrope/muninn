# Phase 5: Open in Editor

> **Status:** DRAFT
> **Depends on:** Phase 1 (ES modules) — parallel with Phases 2, 3, 4
> **Delivers:** Scheme→binary map, `POST /api/open` with a CSRF guard and path containment, and client wiring with a URL-scheme fallback.

## Specification

**Problem:** `cursor://file/<path>:<line>` has no way to express a workspace folder, so the editor routes the file to whichever window was last focused. The file opens without its repo loaded — no language server, no project search, no go-to-definition, which is the entire reason for opening in an editor rather than reading it in the browser.

**Goal:** Clicking the editor action opens the file in a window with the repo's checkout loaded as the workspace folder, scrolled to the target line.

**Scope:**

In: scheme→binary map, `POST /api/open`, CSRF guard, symlink-resolved path containment, fixed argv, client call with URL-scheme fallback, status-specific UI copy.

Out: forced `--new-window`, any editor CLI option surface beyond the single fixed argv, configurable arguments.

**Security note:** This is the highest-risk phase in the plan. It gives an unauthenticated loopback server the ability to launch a local process. Read the whole file before writing code, and do not simplify away the guards.

**Success Criteria:**

- [ ] With `editor.scheme: "vscode"` the launched binary is `code`, not `vscode`
- [ ] Every status-table row is reachable and returns the stated code
- [ ] A `POST` with no `Sec-Fetch-Site` header is rejected
- [ ] A `POST` with a non-JSON content type is rejected
- [ ] The CSRF guard is enforced inside `Handler()`, so `httptest` exercises it
- [ ] Containment holds on a separator boundary: `/dev/repo` does not admit `/dev/repo-other`
- [ ] The path passed to `--goto` is the symlink-resolved path that was validated
- [ ] No process is launched from a shell string
- [ ] `go test ./internal/web/...` passes

## Context Loading

```bash
sed -n '30,75p'   internal/web/server.go     # Server struct, Handler, editorScheme
sed -n '76,115p'  internal/web/server.go     # hostCheck — note it is NOT in Handler()
sed -n '160,180p' internal/web/api.go        # localFile, the checkouts map
cat internal/web/checkouts.go
sed -n '55,62p'   internal/config/config.go  # EditorConfig
sed -n '330,380p' internal/web/static/app.js # fileActions, editorUrl (now file.js)
```

## Why `hostCheck` Does Not Cover This

`hostCheck` (server.go:76-91) blocks DNS rebinding, not CSRF. A form POST from `evil.com` to `127.0.0.1:7576` carries `Host: 127.0.0.1:7576`, which `hostAllowed` accepts. Without a separate guard, any page the user visits can make their editor open arbitrary files.

`hostCheck` is also applied in `Serve` (server.go:129), **not** in `Handler()` (server.go:66-74) — so `httptest` tests that hit `Handler()` bypass it entirely. The new CSRF guard must live inside `Handler()` or the handler itself, or the rejection tests will pass while testing nothing.

## The Argv

```
<binary> <checkoutDir> --goto <resolvedFile>:<line>
```

Fixed. No `--new-window`: given a bare folder argument the CLI focuses an existing window holding that folder and spawns one only if none exists, which is the intent ("a window with the repo loaded") without accumulating a window per click.

`exec.Command` with an argv slice, never a shell string. Both path arguments are absolute, so neither can be read as an option flag, and `line` is an integer — no request value ever becomes an argv element on its own.

## Backend Tasks

### Task 1: Scheme→binary map and editor launch

**Context:** `internal/web/server.go`, `internal/config/config.go`

**Files:**
- Create: `internal/web/editor.go`
- Test: `internal/web/editor_test.go`

**Steps:**

1. [ ] Create `editor.go` with the map and launch logic. `editor.scheme` validates as `"cursor"` or `"vscode"` (config.go:100-101) and is currently only ever used to build a URL (api.go:72, app.js:338-341). **There is no executable named `vscode`** — VS Code's CLI is `code`. Verified: `cursor` and `code` resolve on this machine, `vscode` does not. Without this map every `vscode` user silently takes the fallback path forever:
   ```go
   // editorBinaries maps a configured editor scheme to its CLI executable.
   // The scheme doubles as a URL scheme for the client-side fallback, but
   // it is not an executable name: VS Code's CLI is "code".
   var editorBinaries = map[string]string{
       "cursor": "cursor",
       "vscode": "code",
   }
   ```

2. [ ] Add sentinels and the launch function:
   ```go
   // ErrEditorCLINotFound reports that the editor CLI is absent from PATH,
   // which is routine: it is a separately-installed shell command. The
   // client falls back to the URL scheme.
   var ErrEditorCLINotFound = errors.New("editor CLI not found on PATH")

   // launchEditor opens file at line in an editor window with dir loaded as
   // the workspace folder. The argv is fixed and every element is built by
   // the caller from server-side state; nothing is interpolated into a shell.
   func launchEditor(scheme, dir, file string, line int) error {
       bin, ok := editorBinaries[scheme]
       if !ok {
           return fmt.Errorf("unknown editor scheme %q", scheme)
       }
       path, err := exec.LookPath(bin)
       if err != nil {
           return fmt.Errorf("%s: %w", bin, ErrEditorCLINotFound)
       }
       cmd := exec.Command(path, dir, "--goto", fmt.Sprintf("%s:%d", file, line))
       if err := cmd.Start(); err != nil {
           return fmt.Errorf("launching %s: %w", bin, err)
       }
       // Reap the child so it never lingers as a zombie (same pattern as
       // the browser launch in cli/web.go:71-74).
       go func() { _ = cmd.Wait() }()
       return nil
   }
   ```

3. [ ] Add `resolveInCheckout`, the containment check. It returns **both** the resolved root and the resolved target: the root is the folder argument passed to the editor, and reconstructing it at the call site is how you end up opening the wrong directory. Symlink resolution on both sides, and a separator-boundary comparison — a raw `strings.HasPrefix` would let `/dev/repo-other` pass a `/dev/repo` check:
   ```go
   // resolveInCheckout resolves a repo-relative path inside a checkout and
   // verifies it stays within it, returning the resolved checkout root and
   // the resolved target. Both sides go through EvalSymlinks so a symlink
   // inside the checkout cannot redirect the launch outside it, and the
   // returned paths are the resolved ones — what gets validated is what gets
   // opened. A missing file yields fs.ErrNotExist, which is routine: the
   // checkout is frequently on a different commit than the index.
   func resolveInCheckout(checkoutDir, relPath string) (root, target string, err error) {
       root, err = filepath.EvalSymlinks(checkoutDir)
       if err != nil {
           return "", "", fmt.Errorf("resolving checkout %s: %w", checkoutDir, err)
       }
       target, err = filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(relPath)))
       if err != nil {
           return "", "", err // fs.ErrNotExist when absent from the checkout
       }
       if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
           return "", "", fmt.Errorf("path %q escapes checkout %s", relPath, root)
       }
       return root, target, nil
   }
   ```
   TOCTOU between check and exec is not a meaningful risk: the threat model is a malicious web page, which cannot create symlinks, and a local process that could swap one can exec the editor directly.

4. [ ] Tests in `editor_test.go`:
   - `TestEditorBinaries`: `vscode` maps to `code`, `cursor` to `cursor`
   - `TestResolveInCheckoutEscape`: `../outside` → error
   - `TestResolveInCheckoutSymlinkEscape`: a symlink inside the checkout pointing at a file outside → error (create with `os.Symlink` in a `t.TempDir()`)
   - `TestResolveInCheckoutSiblingPrefix`: checkout `<tmp>/repo`, target `<tmp>/repo-other/f` → error. This is the separator-boundary case.
   - `TestResolveInCheckoutMissing`: absent file → `errors.Is(err, fs.ErrNotExist)`
   - `TestResolveInCheckoutOK`: a real file resolves, and the returned `root` is the resolved checkout dir (on macOS `t.TempDir()` under `/var` resolves to `/private/var`, so assert against the resolved form, not the raw one)

**Verify:**
```bash
go test ./internal/web/... -run 'TestEditor|TestResolveInCheckout' -v
```

### Task 2: `POST /api/open` and the CSRF guard

**Context:** `internal/web/api.go`, `internal/web/server.go`, `internal/web/editor.go`

**Files:**
- Modify: `internal/web/api.go`, `internal/web/server.go`
- Test: `internal/web/api_test.go`

**Steps:**

1. [ ] Add the request type and handler in `api.go`:
   ```go
   // openRequest is the JSON body of POST /api/open. The client sends a
   // repo/path pair; the server resolves the checkout itself and never
   // accepts a filesystem path from the request.
   type openRequest struct {
       Repo string `json:"repo"`
       Path string `json:"path"`
       Line int    `json:"line"`
   }
   ```

2. [ ] Implement `handleOpen` with this exact status mapping. It matches the spec's table — if you change one, change both:

   | Condition | Status |
   |---|---|
   | CSRF guard rejection | 403 |
   | Malformed body, `line` < 1 | 400 |
   | Unknown repo (absent from checkouts) | 404 |
   | Path escapes the checkout (including via symlink) | 403 |
   | File absent from the checkout | 404 |
   | Resolved path contains `:` | 501 |
   | CLI not on `PATH` | 501 |
   | Exec failed | 500 |
   | Success | 204 |

   ```go
   func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
       if err := checkCSRF(r); err != nil {
           writeError(w, http.StatusForbidden, err.Error())
           return
       }
       var req openRequest
       if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
           writeError(w, http.StatusBadRequest, "invalid request body")
           return
       }
       // Rejected rather than coerced: a zero line means the client built a
       // bad request, and silently opening line 1 would hide that.
       if req.Line < 1 {
           writeError(w, http.StatusBadRequest, "line must be a positive integer")
           return
       }
       checkout, ok := s.checkouts[strings.ToLower(req.Repo)]
       if !ok {
           writeError(w, http.StatusNotFound, fmt.Sprintf("no local checkout for %q", req.Repo))
           return
       }
       root, target, err := resolveInCheckout(checkout, req.Path)
       switch {
       case errors.Is(err, fs.ErrNotExist):
           writeError(w, http.StatusNotFound,
               fmt.Sprintf("%q is not in the local checkout (it may be on a different commit)", req.Path))
           return
       case err != nil:
           writeError(w, http.StatusForbidden, err.Error())
           return
       }
       // VS Code and Cursor split the --goto value on ":" and read the first
       // numeric segment as a line number, so a path containing a colon can
       // resolve to the wrong file. Decline and let the client fall back.
       if strings.Contains(target, ":") {
           writeError(w, http.StatusNotImplemented, "path contains a colon; cannot be passed to --goto")
           return
       }
       // root is the workspace folder: the resolved checkout root, so the
       // folder and the file agree. Never a parent or a derived path.
       switch err := s.launch(s.editorScheme, root, target, req.Line); {
       case errors.Is(err, ErrEditorCLINotFound):
           writeError(w, http.StatusNotImplemented, err.Error())
           return
       case err != nil:
           writeError(w, http.StatusInternalServerError, err.Error())
           return
       }
       w.WriteHeader(http.StatusNoContent)
   }
   ```
   `s.launch` is a `func(scheme, dir, file string, line int) error` field on `Server`, defaulting to `launchEditor` in `New` — that indirection is what makes the success path testable without executing a real editor.

3. [ ] Add the CSRF guard. Both checks are required, and **absent `Sec-Fetch-Site` is rejected**, not allowed — otherwise any client that omits the header bypasses the guard:
   ```go
   // checkCSRF guards the one endpoint that launches a local process.
   // hostCheck does not cover this: a form POST from a malicious page
   // carries an allowed Host header. Sec-Fetch-Site is browser-set and not
   // forgeable by page script; requiring JSON blocks simple form POSTs and
   // forces a CORS preflight for cross-origin fetch. An absent header is
   // rejected rather than trusted.
   func checkCSRF(r *http.Request) error {
       if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
           return errors.New("forbidden: cross-origin or unknown request origin")
       }
       if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
           return errors.New("forbidden: expected application/json")
       }
       return nil
   }
   ```
   Note in the PR that Safari before 16.4 sends no `Sec-Fetch-Site` and those users get the 403 path — the client copy must say so.

4. [ ] Register in `Handler()` (server.go:66-74), so `httptest` exercises the guard:
   ```go
   mux.HandleFunc("POST /api/open", s.handleOpen)
   ```

5. [ ] Tests in `api_test.go`. Add a `post` helper — modelled on `get` (api_test.go:129-143) but **not** asserting a JSON content type, since the success path is a bodyless 204. It sets `Sec-Fetch-Site: same-origin` and `Content-Type: application/json` by default, with per-test overrides. Every row of the status table needs one:
   - `TestAPIOpenSuccess`: injected `launch` recorder → 204, and the recorded argv is `{scheme, resolvedRoot, resolvedTarget, line}`. Assert the folder argument is the **checkout root**, not its parent — this is the guard against the `filepath.Dir` mistake.
   - `TestAPIOpenCSRFMissingHeader`: no `Sec-Fetch-Site` → 403
   - `TestAPIOpenCSRFCrossSite`: `Sec-Fetch-Site: cross-site` → 403
   - `TestAPIOpenCSRFFormContentType`: `application/x-www-form-urlencoded` → 403
   - `TestAPIOpenMalformedBody`: `{` → 400
   - `TestAPIOpenZeroLine`: `line: 0` → 400
   - `TestAPIOpenUnknownRepo`: repo absent from checkouts → 404
   - `TestAPIOpenMissingFile`: valid repo, absent path → 404
   - `TestAPIOpenEscape`: `../outside` → 403
   - `TestAPIOpenColonPath`: a checkout containing a file with `:` in its name → 501
   - `TestAPIOpenCLIMissing`: injected `launch` returning `ErrEditorCLINotFound` → 501
   - `TestAPIOpenExecFailed`: injected `launch` returning a generic error → 500

   `newFixture` currently passes `nil` checkouts and `""` scheme (api_test.go:107). Add a variant, or extend it, that wires a real temp checkout and a `cursor` scheme — `TestAPIFileLocalPath` (api_test.go:438) already builds one, so follow its setup.

**Verify:**
```bash
go test ./internal/web/... -run TestAPIOpen -v
go build ./... && go test ./...
```

## Frontend Tasks

### Task 3: Client call with URL-scheme fallback

**Context:** `internal/web/static/file.js` (from Phase 1)

**Files:**
- Modify: `internal/web/static/file.js`

**Steps:**

1. [ ] Change the editor action in `fileActions` (originally `app.js:361-368`) from a plain anchor into a button that POSTs. Keep the existing `editorUrl` helper — it becomes the fallback, not the primary path.
   ```js
   const res = await fetch('/api/open', {
     method: 'POST',
     headers: { 'Content-Type': 'application/json' },
     body: JSON.stringify({ repo: loc.repo, path: loc.path, line: loc.line || 1 }),
   });
   ```
   The browser sets `Sec-Fetch-Site: same-origin` automatically for a same-origin fetch; do not try to set it (it is a forbidden header name and the attempt is silently dropped).

2. [ ] Branch on status, matching the server's mapping:
   - `204` — done, no UI change
   - `501` — fall back to `editorUrl(loc, data)` via `location.href`, and set a tooltip explaining the degraded behaviour (no repo loaded; install the editor's shell command to fix)
   - `404` — inline message: the file is not in the local checkout, which may be on a different commit
   - `403` — inline message telling the user to reload the page (covers both the CSRF guard and Safari < 16.4 sending no `Sec-Fetch-Site`)
   - `500` — inline error from the response body, **no fallback**: the URL scheme would not fix an exec failure

3. [ ] Keep the existing `refreshFileActions` behaviour (`app.js:372-379`) working — clicking a gutter line number updates the target line, and the editor button must pick up the new line. Since the button now reads `loc`/`currentFile` at click time rather than baking a URL into an `href`, this gets simpler; make sure the fallback URL is still computed at click time.

4. [ ] Reuse the existing error rendering (`errorBox` from `dom.js`) rather than inventing a new inline-message pattern.

**Verify:**
```bash
go run . web
# With the editor CLI installed:
#   Click the editor button -> the file opens in a window with the repo loaded
# Without it (temporarily rename the binary or run with a stripped PATH):
#   Click -> falls back to the URL scheme, tooltip explains why
```

## Manual Verification

- [ ] Editor button opens the file with the repo loaded as the workspace folder, at the right line
- [ ] Clicking twice for two files in the same repo reuses one window (depends on the user's `window.openFoldersInNewWindow` setting — note the setting in the PR)
- [ ] Clicking a gutter line number then the editor button opens at the new line
- [ ] With `editor.scheme: "vscode"` configured, `code` is launched
- [ ] With the CLI removed from `PATH`, the fallback fires and the tooltip explains the degradation
- [ ] A file absent from the local checkout shows the "different commit" message, not a crash
- [ ] A repo with no local checkout shows no editor button at all (existing `localFile` behaviour, api.go:165-175)

## PR

Open a PR for human review and **ask for a security-focused review**. Direct the reviewer at: the CSRF guard living inside `Handler()`, absent-header rejection, the separator-boundary containment check, symlink resolution on both sides, and the fixed argv with no shell interpolation.
