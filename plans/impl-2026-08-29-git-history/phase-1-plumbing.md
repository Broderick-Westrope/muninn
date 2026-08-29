# Phase 1: Git Plumbing & Pinning Completion

> **Status:** DRAFT
> Part of `plans/impl-2026-08-29-git-history/` — see README for phase overview.

## Specification

**Problem:** Three gaps block safe history tools: (1) git subprocesses inherit user/system gitconfig, which silently changes semantics (`grep.patternType`, `log.showSignature`, `blame.ignoreRevsFile`) and have no deadline; (2) `gc.auto=0` with no explicit gc means unbounded object accumulation, but naive gc would prune previously indexed commits that live MCP sessions still reference; (3) `checkCommit` maps every rev failure to `ErrIndexMismatch`, so a typo'd SHA gets "run `muninn sync`" advice.

Already shipped (do not re-implement): narrowed fetch refspec `+refs/heads/*:refs/heads/*`, `gc.auto=0` at clone + `assertConfig` self-heal, `MarkIndexed` writing `refs/muninn/indexed` after successful index.

**Goal:** A shared `internal/gitcmd` package every git-invoking component uses (hermetic env, deadline, exit-code-aware errors, partial output); git validated at startup with a version floor; sync maintains two pin generations and runs gc safely after pinning.

**Scope:** In: gitcmd package, gitfile/mirror migration to it, git startup validation for the MCP server and sync, two-generation pin, threshold-based gc, error taxonomy (unknown rev AND unknown path). Out: any MCP tool changes (phase 2), go-git (never), changes to zoekt indexing.

**Success Criteria:**

- [ ] Hermetic-config test: a syntactically invalid global gitconfig (hard-fails any config-reading git command — proven by a control assertion using raw `exec`) does not affect any gitcmd invocation.
- [ ] Pinning survives hostile maintenance: force-push remote → sync → `git gc --prune=now` on the mirror → reads at both current and previous indexed commits still succeed.
- [ ] Index-failure consistency: indexing fails after fetch → status still points at the old commit and that commit is still pinned.
- [ ] Unknown rev, unknown path, and index/mirror mismatch produce distinct errors with correct remedies.
- [ ] All gitcmd invocations respect a deadline; a deliberately slow fake `git` returns `ErrTimeout`, not a hang, and partial stdout is preserved.
- [ ] Startup validation rejects git < 2.32 with an actionable message.

## Context Loading

_Run before starting:_

```bash
read internal/gitfile/gitfile.go        # runGit/runGitRaw/checkCommit to migrate
read internal/mirror/mirror.go          # runGit(env), assertConfig, MarkIndexed
read internal/sync/sync.go              # Mirror interface, syncRepo flow
read internal/ctags/ctags.go            # validation precedent to mirror for git
read internal/cli/mcp.go                # MCP server startup (where git validation hooks in)
read internal/cli/sync.go
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
   - `type Error struct { Args []string; ExitCode int; Stderr string; Err error }` implementing `error` + `Unwrap`. Callers need `ExitCode` because git uses exit 1 as a legitimate answer in places (`merge-base` with disjoint histories) — string-matching stderr is not an API.
   - `var ErrTimeout = errors.New("git command timed out")`.
   - Methods, all wrapping ctx with `context.WithTimeout` (respecting an earlier caller deadline) and setting `cmd.WaitDelay = 5 * time.Second` (a killed git can leave grandchildren like `pack-objects` holding the stdout pipe; without WaitDelay, `Wait` hangs — the exact failure the timeout exists to prevent):
     - `Run(ctx, args ...string) (string, error)` — trimmed stdout.
     - `RunRaw(ctx, args ...string) (string, error)` — verbatim stdout.
     - `RunStdin(ctx, stdin string, args ...string) (string, error)` — trimmed, with stdin provided (needed by Task 3's ref transaction and by tests).
     - All return captured stdout **even on error** (partial output on timeout is a hard phase-2 dependency for labeled partial results; document this in the method comments). On deadline expiry the returned error wraps `ErrTimeout`.
   - Environment construction: start from `os.Environ()` **filtered** — drop every `GIT_*` variable except an allowlist (`GIT_TRACE*` for debugging); an MCP server launched from an editor hook with `GIT_DIR`/`GIT_WORK_TREE`/`GIT_INDEX_FILE`/`GIT_OBJECT_DIRECTORY` set must not operate on the wrong repo. Then append: `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_TERMINAL_PROMPT=0`, `GIT_CONFIG_COUNT=1`, `GIT_CONFIG_KEY_0=safe.directory`, `GIT_CONFIG_VALUE_0=*` (wiping global config also wipes any `safe.directory` entries; mirrors owned by a different uid — Docker, shared volumes — must keep working), then `r.ExtraEnv`.
   - `func Validate() error` — run `git version`, parse `git version X.Y.Z`, require ≥ 2.32 (`GIT_CONFIG_GLOBAL` support — silently ignored on older git, which would make hermeticity a lie; 2.32 also covers `--end-of-options`, `%as`, and `update-ref --stdin` needs). Actionable error naming the found version and the floor.
2. [ ] Tests (testify `require`; note: tests using `t.Setenv` must NOT use `t.Parallel()` — Go forbids the combination; keep env-dependent tests serial and the rest parallel):
   - Hermetic env: write a **syntactically invalid** gitconfig to a temp file; control assertion first — raw `exec.Command("git", "config", "--get", "user.name")` with `GIT_CONFIG_GLOBAL` pointing at it fails — then the same probe through `Runner` succeeds because the poisoned file is never read.
   - Env filtering: `t.Setenv("GIT_DIR", "/nonexistent")`; a `Runner.Run(ctx, "-C", scratchRepo, "rev-parse", "HEAD")` still works.
   - Timeout: create a fake `git` shell script (`#!/bin/sh\nsleep 10`) in a temp dir prepended to `PATH` via `t.Setenv`; `Runner{Timeout: 100 * time.Millisecond}` returns an error wrapping `ErrTimeout` well before 10s. (A real blocking git invocation is not reliably constructible: with `cmd.Stdin` unset, exec gives the child `/dev/null`, so `cat-file --batch` exits instantly.)
   - Partial output: fake `git` printing a line then sleeping; assert the printed line is returned alongside `ErrTimeout`.
   - Exit code: a failing real git command yields `*Error` with `ExitCode` set and stderr text included.
   - `Validate()` succeeds on the test machine; version-parse rejects a fake `git` printing `git version 2.20.0`.

**Verify:**
```bash
gofumpt -w internal/gitcmd && go test ./internal/gitcmd/
# Expected: all tests pass, including timeout returning under 1s
```

### Task 2: Migrate `gitfile` and `mirror` to gitcmd

**Context:** `internal/gitfile/gitfile.go`, `internal/mirror/mirror.go`

**Files:**
- Modify: `internal/gitfile/gitfile.go`
- Modify: `internal/mirror/mirror.go`

**Steps:**

1. [ ] `gitfile`: replace local `runGit`/`runGitRaw` with thin wrappers over a package-level **immutable** `gitcmd.Runner{}` value (zero-config; gitfile never needs auth or long timeouts). Keeping the existing function signatures makes this a mechanical diff. Keep `checkCommit`/`isMissingObject`/`shortSHA` in place (taxonomy handled in Task 5).
2. [ ] `mirror`: replace `runGit(ctx, env, args...)` with `gitcmd.Runner{ExtraEnv: env}.Run(ctx, args...)`. Clone/fetch/gc legitimately exceed 15s: use named constants `fetchTimeout = 10 * time.Minute` for `Ensure`'s clone and fetch calls.
3. [ ] Auth-path caveat, verified explicitly: mirrors authenticate via `authEnv(token)` (ExtraEnv), NOT via global `credential.helper` — but hermetic config also kills global `url.insteadOf` rewrites and credential helpers for anyone relying on them. Confirm `authEnv` covers the fetch path (it does today — it is the only mechanism sync uses) and add a comment in `Ensure` stating global helpers are deliberately unavailable.
4. [ ] Run the full existing suites for gitfile, mirror, and sync; behavior must be identical.

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

1. [ ] Rewrite `MarkIndexed(ctx, dir, sha)`: read the current `refs/muninn/indexed` (`rev-parse --verify --quiet`; missing is fine). If old == sha, no-op. Otherwise write via one `update-ref --stdin` batch through `RunStdin` — a plain batch of `update` lines is already applied atomically; do NOT use `start`/`prepare`/`commit` verbs (unnecessary, and the plain form works on older git). Include the old value as a CAS guard so concurrent syncs can't interleave (read-then-write is otherwise TOCTOU):
   ```
   update refs/muninn/indexed-prev <old-sha> <whatever indexed-prev currently is, or empty if absent>
   update refs/muninn/indexed <sha> <old-sha>
   ```
   With no previous ref: single `update refs/muninn/indexed <sha> ` line (empty old value = must-not-exist... verify git's exact empty-oldvalue semantics for "create"; use `create` verb if cleaner). A CAS failure returns an error; sync already records per-repo errors.
2. [ ] Test: index commit A → ref at A, no prev; index B → ref at B, prev at A; index B again → no-op; CAS failure path (move the ref behind MarkIndexed's back between read and write is hard to orchestrate — instead test the batch directly: stale old-value in the transaction fails).
3. [ ] Hostile-maintenance test at the mirror level: repo with commit A marked indexed; force-rewrite upstream to B; `Ensure` (fetch --prune); mark B indexed; run `git gc --prune=now` in the mirror; `git cat-file -e A^{commit}` and `B^{commit}` both succeed (A is held by `indexed-prev`).

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

1. [ ] Add field `GCLooseObjectThreshold int` to `mirror.Manager` (0 = default 5000) — injectable so tests use ~5 instead of generating thousands of objects. Add `(m *Manager) MaybeGC(ctx, dir string) (ran bool, err error)`: `git count-objects -v`, parse `count:` (loose objects); over threshold → `git gc --quiet` with `gcTimeout = 10 * time.Minute` Runner (NOT `--prune=now`: the default 2-week grace additionally protects any commit a >two-sync-old session might still name explicitly). Comment: a killed gc can leave tmp packs; git self-heals on the next run, acceptable.
2. [ ] Add `MaybeGC` to the sync `Mirror` interface, call in `syncRepo` **after** `MarkIndexed` succeeds. A gc failure appends to the repo's status `Error` string but must not flip `Fetched`/`Indexed`.
3. [ ] Tests: sync stub asserts `MaybeGC` called after `MarkIndexed`, only on successful index, and its error doesn't flip flags. Manager test with threshold 5: generate loose objects via one `git hash-object -w --stdin-paths` invocation (NOT thousands of process spawns, NOT fast-import — fast-import writes packs, not loose objects); assert `ran == true` and loose count drops; under threshold → `ran == false`.

**Verify:**
```bash
go test ./internal/mirror/ ./internal/sync/
# Expected: pass; gc ordering asserted; manager gc test completes in seconds
```

### Task 5: Rev and path error taxonomy

**Context:** `internal/gitfile/gitfile.go` (`checkCommit`, `ErrIndexMismatch`, `isMissingObject`)

**Files:**
- Modify: `internal/gitfile/gitfile.go`
- Modify: `internal/gitfile/gitfile_test.go`

**Steps:**

1. [ ] Add `var ErrUnknownRev = errors.New("unknown revision")`. Add `ResolveRev(ctx, mirrorDir, rev string) (sha string, err error)`: reject empty and leading-`-` revs without invoking git; run `git rev-parse --verify --end-of-options <rev>^{commit}`; failure wraps `ErrUnknownRev` ("rev %q not found in mirror; it may not exist upstream or predates the last sync"). Branch names resolve via `refs/heads/*`; SHAs and (auto-followed) tags resolve if present — no special-casing.
2. [ ] Add `var ErrUnknownPath = errors.New("path not found at revision")` and `func ClassifyPathErr(err error) error` (or extend `isMissingObject`) mapping git's `fatal: no such path`, `does not exist in`, `Not a valid object name` variants — phase 2's blame/log handlers must render these as user errors ("path X does not exist at rev Y"), not internal failures. Existing `ReadFile`'s `fs.ErrNotExist` mapping stays; new code paths get the same treatment through the shared helper.
3. [ ] Contract split documented in comments: `checkCommit` is only for muninn-recorded commits (status file) → `ErrIndexMismatch` correct; `ResolveRev` is for caller-supplied revs → `ErrUnknownRev`.
4. [ ] Tests: valid full SHA, short SHA, branch name; unknown SHA wraps `ErrUnknownRev`; `-`-prefixed rev (e.g. `--upload-pack=/bin/true`) rejected without git being invoked (assert via a PATH-less environment or a fake git that fails the test if executed); path-error classification against real git stderr strings.

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

**Steps:**

1. [ ] Call `gitcmd.Validate()` before serving MCP / before running sync; on failure exit with the actionable error (same UX as the ctags hard-prerequisite failure). Version floor 2.32 enforced by Validate (Task 1).
2. [ ] Coverage: version-floor and parse behavior are tested in gitcmd (Task 1); CLI wiring is a two-line call — cover only if an existing CLI test seam makes it free.

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

## Review notes

Devils-advocate review (round 1) caught: `GIT_CONFIG_GLOBAL` silently ignored pre-2.32 (added version floor); hermeticity killing `safe.directory` (added GIT_CONFIG_COUNT injection) and inherited `GIT_DIR` leakage (added env filtering); the original timeout test being un-runnable (`cat-file --batch` gets `/dev/null` stdin and exits instantly — replaced with fake-git approach); missing `cmd.WaitDelay`; `RunStdin`/partial-output/exit-codes being hard phase-2 dependencies (promoted into Task 1); `update-ref` transaction verbs unnecessary + TOCTOU (plain batch + CAS old-values); fast-import writing packs not loose objects (hash-object --stdin-paths + injectable threshold); missing path-error taxonomy (Task 5 extended); `t.Setenv`/`t.Parallel` incompatibility.
