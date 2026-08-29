# Phase 1: Git Plumbing & Pinning Completion

> **Status:** DRAFT
> Part of `plans/impl-2026-08-29-git-history/` — see README for phase overview.

## Specification

**Problem:** Three gaps block safe history tools: (1) git subprocesses inherit user/system gitconfig, which silently changes semantics (`grep.patternType`, `log.showSignature`, `blame.ignoreRevsFile`) and have no deadline; (2) `gc.auto=0` with no explicit gc means unbounded object accumulation, but naive gc would prune previously indexed commits that live MCP sessions still reference; (3) `checkCommit` maps every rev failure to `ErrIndexMismatch`, so a typo'd SHA gets "run `muninn sync`" advice.

Already shipped (do not re-implement): narrowed fetch refspec `+refs/heads/*:refs/heads/*`, `gc.auto=0` at clone + `assertConfig` self-heal, `MarkIndexed` writing `refs/muninn/indexed` after successful index.

**Goal:** A shared `internal/gitcmd` package every git-invoking component uses (hermetic env, deadline, error classification); git validated at startup; sync maintains two pin generations and runs gc safely after pinning.

**Scope:** In: gitcmd package, gitfile/mirror migration to it, git startup validation for the MCP server and sync, two-generation pin, threshold-based gc, error taxonomy. Out: any MCP tool changes (phase 2), go-git (never), changes to zoekt indexing.

**Success Criteria:**

- [ ] Hermetic-config test: a poisoned global gitconfig (`grep.patternType=perl`, `log.showSignature=true`) does not affect any gitcmd invocation.
- [ ] Pinning survives hostile maintenance: force-push remote → sync → `git gc --prune=now` on the mirror → reads at both current and previous indexed commits still succeed.
- [ ] Index-failure consistency: indexing fails after fetch → status still points at the old commit and that commit is still pinned.
- [ ] Unknown rev and index/mirror mismatch produce distinct errors with correct remedies.
- [ ] All gitcmd invocations respect a deadline; an artificially slow git command returns a timeout error, not a hang.

## Context Loading

_Run before starting:_

```bash
read internal/gitfile/gitfile.go        # runGit/runGitRaw/checkCommit to migrate
read internal/mirror/mirror.go          # runGit(env), assertConfig, MarkIndexed
read internal/sync/sync.go              # Mirror interface, syncRepo flow
read internal/ctags/ctags.go            # validation precedent to mirror for git
read internal/cli/mcp.go                # MCP server startup (where git validation hooks in)
read plans/design-2026-08-29-git-history.md
```

## Git Runner Tasks

### Task 1: Create `internal/gitcmd` package

**Context:** `internal/gitfile/gitfile.go:320-340`, `internal/mirror/mirror.go:225-240`

**Files:**
- Create: `internal/gitcmd/gitcmd.go`
- Create: `internal/gitcmd/gitcmd_test.go`

**Steps:**

1. [ ] Create `internal/gitcmd/gitcmd.go` with:
   - `const DefaultTimeout = 15 * time.Second`.
   - `type Runner struct { Timeout time.Duration; ExtraEnv []string }` (zero value usable; 0 timeout means DefaultTimeout).
   - `func (r Runner) Run(ctx context.Context, args ...string) (string, error)` — trimmed stdout (current `runGit` semantics).
   - `func (r Runner) RunRaw(ctx context.Context, args ...string) (string, error)` — verbatim stdout (current `runGitRaw` semantics).
   - Both: wrap ctx with `context.WithTimeout` (respecting an earlier caller deadline), run `git` via `exec.CommandContext`, env = `os.Environ()` + `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_TERMINAL_PROMPT=0` + `r.ExtraEnv`. On error include trimmed stderr (current format: `git %s: %w (stderr: %s)`).
   - On deadline expiry return an error wrapping `ErrTimeout` (`var ErrTimeout = errors.New("git command timed out")`) so callers can offer narrowing advice.
   - `func Validate() error` — mirrors `ctags.Validate` shape: `git version` must run and print `git version`; return an actionable error ("install git / ensure it is on PATH") otherwise.
2. [ ] Tests (`t.Parallel()`, testify `require`):
   - Hermetic env: write a poisoned gitconfig to a temp home (`t.Setenv("HOME", dir)` plus `GIT_CONFIG_GLOBAL` unset scenario), set `log.showSignature=true` and an alias, assert a `Run` of `git config --get log.showSignature` inside a scratch repo returns nothing.
   - Timeout: `Runner{Timeout: 100 * time.Millisecond}` running a git command against a FIFO-backed or deliberately huge input (simplest: `git -C <repo> log --all` on a repo is too fast — instead run `git cat-file --batch` which blocks on stdin) returns an error wrapping `ErrTimeout`.
   - Stderr propagation: a failing command's error contains stderr text.
   - `Validate()` succeeds on the test machine.

**Verify:**
```bash
gofumpt -w internal/gitcmd && go test ./internal/gitcmd/
# Expected: all tests pass
```

### Task 2: Migrate `gitfile` and `mirror` to gitcmd

**Context:** `internal/gitfile/gitfile.go`, `internal/mirror/mirror.go`

**Files:**
- Modify: `internal/gitfile/gitfile.go` (delete local `runGit`/`runGitRaw`, use `gitcmd.Runner`)
- Modify: `internal/mirror/mirror.go` (delete local `runGit`, use `gitcmd.Runner` with `ExtraEnv: authEnv(token)`)

**Steps:**

1. [ ] `gitfile`: package-level `var runner gitcmd.Runner`; replace `runGit(ctx, ...)` → `runner.Run(ctx, ...)`, `runGitRaw` → `runner.RunRaw`. Keep `checkCommit`/`isMissingObject`/`shortSHA` in place (taxonomy handled in Task 5).
2. [ ] `mirror`: replace `runGit(ctx, env, args...)` with per-call `gitcmd.Runner{ExtraEnv: env}.Run(ctx, args...)`. **Exception:** clone/fetch of large repos legitimately exceeds 15s — construct those calls with `Runner{Timeout: 10 * time.Minute, ExtraEnv: env}`. Choose one named constant in `mirror` (`fetchTimeout`).
3. [ ] Run the full existing test suites for both packages; behavior must be identical (the hermetic env is the only observable change, and no existing test depends on user config).

**Verify:**
```bash
go test ./internal/gitfile/ ./internal/mirror/ ./internal/sync/
# Expected: all pass, no behavior change
```

## Sync & Pinning Tasks

### Task 3: Two-generation pin refs

**Context:** `internal/mirror/mirror.go` (`MarkIndexed`), `internal/sync/sync.go` (call site ~line 276)

**Files:**
- Modify: `internal/mirror/mirror.go`
- Modify: `internal/mirror/mirror_test.go`

**Steps:**

1. [ ] Rewrite `MarkIndexed(ctx, dir, sha)`: read the current value of `refs/muninn/indexed` (`rev-parse --verify --quiet`, missing is fine); if it exists and differs from `sha`, write both refs in a single `git update-ref --stdin` transaction fed via stdin:
   ```
   start
   update refs/muninn/indexed-prev <old-sha>
   update refs/muninn/indexed <sha>
   prepare
   commit
   ```
   If no previous ref exists, just update `refs/muninn/indexed`. If old == sha, no-op.
   (gitcmd needs stdin support: add `RunStdin(ctx, stdin string, args ...string)` to Task 1's Runner — do it now if not already present.)
2. [ ] Test: index commit A → ref at A, no prev; index commit B → ref at B, prev at A; index B again → unchanged; force-push scenario — mirror with A unreachable from the new head still resolves A via `indexed-prev` after a `git gc --prune=now`.
3. [ ] Test the flagship guarantee end-to-end at the mirror level: create repo, commit A, mark indexed; force-rewrite to commit B, fetch with prune, mark indexed; run `git gc --prune=now`; `git cat-file -e A^{commit}` and `B^{commit}` both succeed.

**Verify:**
```bash
go test ./internal/mirror/ -run TestMarkIndexed
# Expected: pass, including the gc --prune=now case
```

### Task 4: Explicit gc after pinning

**Context:** `internal/sync/sync.go` (`syncRepo`, Mirror interface), `internal/mirror/mirror.go`

**Files:**
- Modify: `internal/mirror/mirror.go` (add `MaybeGC`)
- Modify: `internal/sync/sync.go` (call after `MarkIndexed`, failure is non-fatal)
- Modify: `internal/sync/sync_test.go`

**Steps:**

1. [ ] Add `(m *Manager) MaybeGC(ctx context.Context, dir string) (ran bool, err error)`: run `git count-objects -v`, parse the `count:` (loose objects) line; if over threshold (package const `gcLooseObjectThreshold = 5000`), run `git gc --quiet` (not `--prune=now`: default 2-week grace protects any commit a >two-sync-old session might still name explicitly). Use a long-timeout Runner (`gcTimeout = 10 * time.Minute`).
2. [ ] Add `MaybeGC` to the sync `Mirror` interface and call it in `syncRepo` **after** `MarkIndexed` succeeds. A gc failure is recorded on the repo's status error field but must not mark the repo unfetched/unindexed — extend `status.RepoStatus` handling only insofar as appending to the existing `Error` string; do not change the schema.
3. [ ] Tests: stub Mirror asserts `MaybeGC` is called after `MarkIndexed` and only on successful index; a `MaybeGC` error does not flip `Indexed`/`Fetched` to false. Manager test: repo under threshold → `ran == false`; over threshold (generate loose objects by committing many small files without packing) → `ran == true` and loose count drops.

**Verify:**
```bash
go test ./internal/mirror/ ./internal/sync/
# Expected: pass; gc ordering asserted
```

### Task 5: Rev-error taxonomy

**Context:** `internal/gitfile/gitfile.go` (`checkCommit`, `ErrIndexMismatch`)

**Files:**
- Modify: `internal/gitfile/gitfile.go`
- Modify: `internal/gitfile/gitfile_test.go`

**Steps:**

1. [ ] Add `var ErrUnknownRev = errors.New("unknown revision")` in `gitfile`. Add `ResolveRev(ctx, mirrorDir, rev string) (sha string, err error)`: reject revs starting with `-` (`fmt.Errorf("invalid rev %q: %w", rev, ErrUnknownRev)`); run `git rev-parse --verify --end-of-options <rev>^{commit}`; on failure return an error wrapping `ErrUnknownRev` ("rev %q not found in mirror; it may not exist upstream or predates the last sync").
2. [ ] `checkCommit` keeps its current contract (status-file commits → `ErrIndexMismatch`) — it is only ever called with commits muninn itself recorded, so mismatch is the correct diagnosis there. Document this split in both functions' comments: `checkCommit` for muninn-recorded commits, `ResolveRev` for caller-supplied revs.
3. [ ] Tests: `ResolveRev` with a valid SHA, a valid short SHA, a branch name, an unknown SHA (wraps `ErrUnknownRev`), a `-` prefixed rev (rejected without invoking git — assert via injection-shaped input like `--upload-pack=/bin/true`).

**Verify:**
```bash
go test ./internal/gitfile/
# Expected: pass
```

## Startup Validation Tasks

### Task 6: Validate git at MCP-server and sync startup

**Context:** `internal/cli/mcp.go`, `internal/cli/sync.go`, `internal/ctags/ctags.go` (precedent)

**Files:**
- Modify: `internal/cli/mcp.go`
- Modify: `internal/cli/sync.go`
- Modify: `internal/cli/` tests if startup is covered there

**Steps:**

1. [ ] Call `gitcmd.Validate()` before serving MCP / before running sync; on failure exit with the actionable error (same UX as the ctags hard-prerequisite failure).
2. [ ] Test: validation error propagates (stub by pointing `PATH` at an empty dir via `t.Setenv` in a small test around whatever seam exists; if no clean seam, cover `Validate` behavior in gitcmd only and keep CLI wiring untested but trivial).

**Verify:**
```bash
go test ./... && go build .
# Expected: full suite green
```

## Final Verification

```bash
gofumpt -w . && go test ./... && go build .
```

Create PR for human review; phase 2 starts after merge.
