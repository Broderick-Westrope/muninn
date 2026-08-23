# Phase 2: Sync Engine

> **Status:** DRAFT
> **Spec:** `plans/design-2026-08-23-muninn.md`
> **Depends on:** Phase 1 merged

Delivers `muninn sync` (GitHub discovery → mirror reconciliation → Zoekt indexing → status), `muninn status`, and launchd install/uninstall.

## Specification (excerpt)

**Goal:** A scheduled, self-contained sync that reconciles 200+ repos: clones new, fetches existing, indexes changed, GCs removed — with per-repo failure isolation and full observability. launchd agent installable with everything (token, ctags path) baked in.

**Success Criteria:**

- [ ] Sync against a small test config produces bare mirrors + Zoekt shards with symbols
- [ ] Removing a repo from config and re-syncing deletes its mirror and shards
- [ ] A repo that fails to fetch is recorded in status; other repos still complete
- [ ] `muninn sync --install` writes a working launchd agent; `--uninstall` removes it
- [ ] Peak indexing concurrency is capped (default 2)

## Context Loading

_Run before starting:_

```bash
read /Users/broderick.westrope/dev/helse/muninn/plans/design-2026-08-23-muninn.md
read /Users/broderick.westrope/dev/helse/muninn/internal/config/config.go
read /Users/broderick.westrope/dev/helse/muninn/internal/status/status.go
read /Users/broderick.westrope/dev/helse/muninn/internal/xdg/xdg.go
```

**Zoekt pinning (do first, applies to Tasks 3–4):** add `github.com/sourcegraph/zoekt` pinned to a specific recent commit (`go get github.com/sourcegraph/zoekt@<commit>`; record the commit in a comment in `go.mod`). Before writing wrapper code, read the pinned version's packages to confirm current API names — the layout churns (e.g. `build` → `index`, searcher constructors moved between `shards`/`search`). Key entry points to locate: git repo indexing (`gitindex.IndexGitRepo` + `index.Options` or equivalents), ctags configuration on the index options, and shard naming conventions in the index dir.

## Discovery Tasks

### Task 1: GitHub repo discovery

**Files:**
- Create: `internal/discover/github.go`, `internal/discover/github_test.go`

**Steps:**

1. [ ] Define the resolved repo type used by the rest of sync:

```go
type Repo struct {
    FullName      string // "owner/name"
    CloneURL      string // https
    DefaultBranch string
    Archived      bool
}
```

2. [ ] `Discover(ctx, cfg *config.Config, token string) ([]Repo, error)`:
   - For each connection org: `GET /orgs/{org}/repos?per_page=100` with pagination (plain `net/http` + token header; no heavy SDK needed).
   - Append ad-hoc `repos` entries via `GET /repos/{owner}/{name}`.
   - Filter: drop `Archived` when `exclude.archived`; drop names in `exclude.repos` (exact match on `owner/name`).
   - Deterministic order (sort by FullName).
3. [ ] Rate-limit courtesy: respect `Retry-After` on 403/429 with one retry; otherwise fail with the API error body included.
4. [ ] Tests with `httptest.Server`: pagination across 3 pages, archived + explicit exclusion, ad-hoc repo addition, 404 on ad-hoc repo returns actionable error.

**Verify:**
```bash
go test ./internal/discover/
# Expected: PASS
```

## Mirror Tasks

### Task 2: Mirror manager (clone/fetch/GC, indexed refs)

**Files:**
- Create: `internal/mirror/mirror.go`, `internal/mirror/mirror_test.go`

**Steps:**

1. [ ] Layout: `xdg.MirrorsDir()/<owner>/<name>.git` (bare). All git ops shell out to system `git` with `exec.CommandContext`.
2. [ ] `Ensure(ctx, repo discover.Repo, token string) (created bool, err error)`:
   - **Narrow refspec, not `--mirror`**: `--mirror` implies `+refs/*:refs/*`, so `fetch --prune` would (a) delete `refs/muninn/indexed` on every sync (it matches `refs/*` and doesn't exist on the remote), and (b) pull GitHub's `refs/pull/*`, bloating disk/fetch time across 200+ repos. Instead: `git clone --bare <url> <dir>` then set `remote.origin.fetch = +refs/heads/*:refs/heads/*`; existing → `git -C <dir> fetch --prune origin`. Prune now only touches `refs/heads/*`; `refs/muninn/*` is never in the fetch refspec's destination.
   - Token injected via env, never argv (visible in `ps`) or disk: `GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraHeader GIT_CONFIG_VALUE_0="Authorization: Bearer <token>"` on the command's env. **Implementation note:** the actual header is `Authorization: Basic <base64(x-access-token:<token>)>` because GitHub's git HTTP endpoint rejects `Bearer` for OAuth tokens — do not regress to Bearer.
   - On clone, set `git config gc.auto 0` (backstop protecting indexed commits from auto-gc — spec implementation note; `refs/muninn/indexed` is the primary mechanism).
3. [ ] `HeadCommit(dir, defaultBranch string) (string, error)` → `git rev-parse refs/heads/<branch>` (mirror clones map remote branches directly under refs/heads).
4. [ ] `MarkIndexed(dir, sha string) error` → `git update-ref refs/muninn/indexed <sha>` (keeps the indexed commit reachable across force-pushes).
5. [ ] `List() ([]string, error)` returns `owner/name` for every mirror on disk; `Remove(fullName string) error` deletes the mirror dir.
6. [ ] Tests against local fixture repos created with `git init` in `t.TempDir()` (no network): clone(file:// URL), fetch after new commit, and — critically — **MarkIndexed → upstream force-push → `fetch --prune` → `refs/muninn/indexed` still present and its commit still readable after `git gc --prune=now`** (this is the regression the narrow refspec exists to prevent).

**Verify:**
```bash
go test ./internal/mirror/
# Expected: PASS (uses local git; requires git on PATH)
```

## Index Tasks

### Task 3: Zoekt indexer wrapper

**Files:**
- Create: `internal/index/indexer.go`, `internal/index/indexer_test.go`
- Modify: `go.mod` (pinned zoekt)

**Steps:**

1. [ ] `Indexer` struct configured with `IndexDir`, `CtagsPath`, `SizeMax` (default 1 MiB per file, matching zoekt defaults), and repo metadata. Public API takes/returns only muninn types — **no zoekt types exported** (spec decision).
2. [ ] `IndexRepo(ctx, mirrorDir, fullName, branch, commit string) error`:
   - Use the pinned zoekt git-index entry point (confirm exact API per Context Loading note) with: single branch, repo name = `fullName`, ctags path passed explicitly (not via PATH lookup), shard output under `xdg.IndexDir()`.
   - Zoekt skips work if the shard is already current for the commit — rely on that for incremental syncs, but verify the behavior at the pinned commit and document it in a code comment.
3. [ ] `CleanTmp() error`: remove `*.tmp` files in IndexDir (crash recovery, spec requirement).
4. [ ] `RemoveShards(fullName string) error`: delete shards belonging to a repo (match zoekt's shard-name convention for the repo; confirm at pinned commit).
5. [ ] `ListIndexed() (map[string]string, error)`: repo → indexed commit, derived from shard metadata (used as a cross-check against the status file).
6. [ ] Test: index a tiny fixture git repo (with a Go file so ctags emits symbols) into a temp IndexDir; assert shard file(s) exist; RemoveShards deletes them; CleanTmp removes a planted `.tmp`. Skip test with a clear message if universal-ctags is not installed.

**Verify:**
```bash
go test ./internal/index/
# Expected: PASS (requires universal-ctags; test self-skips otherwise)
```

## Orchestration Tasks

### Task 4: `muninn sync` reconciliation + `muninn status`

**Context:** all packages above

**Files:**
- Create: `internal/sync/sync.go`, `internal/sync/sync_test.go`
- Modify: `internal/cli/sync.go`, `internal/cli/status.go`

**Steps:**

1. [ ] `sync.Run(ctx, cfg) (*status.SyncStatus, error)` pipeline:
   - **Seed from previous status**: read the existing status file first; each repo's new `RepoStatus` starts from the previous one so `IndexedCommit` is **carried forward when fetch succeeds but indexing fails** (the spec's read_file pinning guarantee depends on this) and when discovery/whole-run fails.
   - Validate ctags (`ctags.Resolve`/`Validate`) — **fail the whole run loudly if invalid** (hard prereq).
   - Discover repos. On discovery failure, abort reconciliation (can't GC safely without the full list) and write a failed status **that retains the previous per-repo entries**.
   - Reconcile: `desired = discovered`, `actual = mirror.List() ∪ indexer.ListIndexed()`. For removed repos: `mirror.Remove` + `indexer.RemoveShards`.
   - `indexer.CleanTmp()`.
   - Per repo (worker pool, fetch concurrency 8, **index concurrency capped at 2** — spec constraint): Ensure mirror → HeadCommit → IndexRepo → MarkIndexed → update `IndexedCommit`. Any error is captured into `RepoStatus.Error` and prior `IndexedCommit` retained; the run continues (per-repo isolation). Invariant to state in code: `IndexRepo` indexes the branch tip, which equals the `HeadCommit` we record only because no concurrent fetch of the same repo can occur within a run.
   - **Write the status file incrementally** (atomic rename each time) after each repo completes, so a live MCP session's shard/status skew window is seconds, not the whole run; final write sets `FinishedAt` and `Success = all repos OK`.
2. [ ] `muninn sync` command: load config, run pipeline, print one-line summary (`synced 212 repos, 2 failed — see muninn status`), exit non-zero only on total failure (not per-repo failures, so launchd doesn't mark the job crashed for one bad repo).
3. [ ] `muninn status` command: read status file; print last run time/age, success, and a table of failed repos with errors; note "never synced" if no status file.
4. [ ] Test `sync.Run` end-to-end with local file:// fixture repos and a stubbed discovery (inject via interface): happy path; one repo's mirror dir made unreadable → other repos still indexed + error recorded; repo removed from discovery → mirror+shards GC'd; **fetch-OK-but-index-fail → previous `IndexedCommit` carried forward**; discovery failure → previous per-repo entries retained.

**Verify:**
```bash
go test ./internal/sync/ && go build ./...
# Expected: PASS
# Manual acceptance (optional, network): create ~/.config/muninn/config.json with 2 public repos, run `go run . sync`, then `go run . status`
```

### Task 5: launchd install/uninstall

**Files:**
- Create: `internal/launchd/launchd.go`, `internal/launchd/launchd_test.go`
- Modify: `internal/cli/sync.go` (`--install`, `--uninstall` flags)

**Steps:**

1. [ ] `--install` flow (spec: everything baked at install time):
   - Resolve + validate ctags; resolve token (`config.ResolveToken`); persist both into config via `config.Save` (0600).
   - Render plist to `~/Library/LaunchAgents/dev.broderick-westrope.muninn.plist`:
     - `ProgramArguments`: absolute path of the current binary (`os.Executable`, resolve symlinks) + `sync --config <abs config path>`
     - `StartInterval`: `sync.intervalMinutes * 60` (default 3600)
     - `StandardOutPath`/`StandardErrorPath`: `xdg.LogPath()`
     - `RunAtLoad: false`
   - `launchctl bootstrap gui/$UID <plist>` (fall back to `launchctl load` on older macOS); print what was installed and when it next runs. **Idempotent**: if the job is already loaded, `bootout` first, then `bootstrap` (bare bootstrap errors on a loaded job).
2. [ ] `--uninstall`: `launchctl bootout gui/$UID/<label>` + remove plist; idempotent.
3. [ ] Plist rendering via `text/template` into a golden-file test (no launchctl in tests); launchctl invocations behind a small interface so they can be stubbed.
4. [ ] `muninn status` addition: note whether the launchd agent is installed (plist exists).

**Verify:**
```bash
go test ./internal/launchd/ && go build ./...
# Expected: PASS
# Manual acceptance: `go run . sync --install`, `launchctl print gui/$UID/dev.broderick-westrope.muninn` shows the job, then `--uninstall` removes it
```

## Verify Phase

```bash
cd /Users/broderick.westrope/dev/helse/muninn && go vet ./... && go test ./... -race && go build ./...
# Expected: all PASS
# Full acceptance (network + ctags): point config at eucalyptusvc org, run sync, confirm mirrors+shards exist and `muninn status` reports success; measure wall time + peak RSS (spec: bounded, warm target <15 min)
```

Create PR (or reviewed commit) for human review.
