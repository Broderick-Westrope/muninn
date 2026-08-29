# Phase 2: History Tools (`internal/githistory` + MCP)

> **Status:** DRAFT
> Part of `plans/impl-2026-08-29-git-history/` — see README. Requires phase 1 merged (`internal/gitcmd`, `ResolveRev`, two-gen pins).

## Specification

**Problem:** Agents cannot answer "when/why did this change", "what did this commit do", "who wrote this line" over the indexed estate.

**Goal:** Three MCP tools — `search_commits`, `get_diff`, `blame` — per the spec (`plans/design-2026-08-29-git-history.md`), with agent-safe defaults and hard output caps.

**Scope:** In: `internal/githistory` core package, three MCP tool handlers, server registration, instructions update. Out: cross-repo search, CLI commands, matched-hunk pickaxe output, symbol-graph tools.

**Success Criteria** (from the spec, phase-2 subset):

- [ ] Flagship scenario: fixture where `Foo` changed in commit C behind merge M — `search_commits(changed_literal:"Foo")` surfaces M annotated as a merge; `first_parent:false` surfaces C; `get_diff(rev:C)` shows the change.
- [ ] `blame` line numbers agree exactly with `read_file` at the indexed commit.
- [ ] Lockfile-heavy diff: source diffs intact, lockfiles as stat lines, budget respected.
- [ ] Descendant/swapped revs under `merge_base:true` produce the warning header, not a bare empty diff.
- [ ] Truncated diffs are always whole per-file patches (`git apply --check` against a temp worktree at the pre-image rev; binary files never emitted as patches).
- [ ] A single over-budget file yields a stat line + notice, never a partial hunk.
- [ ] Timeout on pickaxe returns labeled partial results, never a hang.
- [ ] Renamed-file history continues past the rename for single-path queries.

## Context Loading

_Run before starting:_

```bash
read plans/design-2026-08-29-git-history.md   # authoritative tool contracts
read internal/gitcmd/gitcmd.go                # Runner, ErrTimeout (phase 1)
read internal/gitfile/gitfile.go              # ResolveRev, ErrUnknownRev, checkCommit, shortSHA
read internal/mcp/server.go                   # registration, textHandler, instructions
read internal/mcp/tools_file.go               # arg struct + description + resolveIndexedCommit conventions
read internal/mcp/tools_search.go             # clampLimit/clampNote/formatGrep conventions
```

## githistory Core Tasks

All in `internal/githistory`, one test file per source file, fixtures built with helper `newFixtureRepo(t)` that shells git to create a bare mirror with a scripted history (branches, a merge, a rename, a lockfile commit, a binary file). Reuse `gitcmd.Runner`; every function takes `(ctx, mirrorDir, ...)` and validates caller-supplied revs via `gitfile.ResolveRev` first.

### Task 1: `SearchCommits`

**Files:**
- Create: `internal/githistory/githistory.go` (package doc, shared types, `Commit` struct)
- Create: `internal/githistory/log.go`
- Create: `internal/githistory/log_test.go`
- Create: `internal/githistory/fixture_test.go` (shared `newFixtureRepo`)

**Steps:**

1. [ ] `type Commit struct { SHA, AuthorDate, Author, Subject string; IsMerge bool }` and `type LogOptions struct { Rev, Author, Since, Until, Path, Message, ChangedLiteral, ChangedRegex string; FirstParent *bool; Limit int }` (nil `FirstParent` = true).
2. [ ] `SearchCommits(ctx, mirrorDir string, opts LogOptions) (commits []Commit, truncated bool, timedOut bool, err error)`:
   - Validate: `ChangedLiteral`/`ChangedRegex` mutually exclusive; resolve `Rev` (default: caller passes the indexed commit) via `gitfile.ResolveRev`.
   - Build argv: `log --no-pager? (use git --no-pager log)`, format `%H%x09%as%x09%an%x09%P%x09%s` (parents field drives `IsMerge`: more than one parent), `-n <limit+1>`, `--first-parent` unless disabled, `--author`, `--since`/`--until`, `--grep=<message> --regexp-ignore-case`, `-S<literal>`/`-G<regex>` (as single argv tokens: `-S` + value concatenated), `--follow` iff exactly one `Path`, then `--end-of-options <rev> -- <path>`.
   - `Path` starting with `-` or `:` → error (injection/pathspec-magic guard).
   - Run with `RunRaw`; on `gitcmd.ErrTimeout`, parse whatever complete lines were captured and return `timedOut=true` (Runner must return partial stdout alongside `ErrTimeout` — if phase 1's Runner discards output on error, extend it here with a `RunPartial` variant; note it in the PR).
   - `len > limit` → truncate to limit, `truncated=true`.
3. [ ] Tests against the fixture: default first-parent excludes merge-side commits; `first_parent=false` includes them; pickaxe `-S` finds the commit that introduced a string and the one that removed it; `--follow` walks past the rename; mutual-exclusion and injection guards error; since/until filter; limit + truncated flag.

**Verify:**
```bash
go test ./internal/githistory/ -run TestSearchCommits
```

### Task 2: `GetDiff`

**Files:**
- Create: `internal/githistory/diff.go`
- Create: `internal/githistory/diff_test.go`

**Steps:**

1. [ ] Types: `DiffOptions struct { Rev, Base, Path string; Patch, MergeBase, StatOnly, IncludeGenerated *bool }` (Patch default: false single-rev, true two-rev; MergeBase default true two-rev). Result: `Diff struct { Header CommitMeta; MergeBaseSHA string; Files []FileDiff; TruncatedFiles []StatLine; Warning string }` with `FileDiff { Path string; Patch string; StatLine string; Binary, Generated bool }`.
2. [ ] Single-rev mode: `git show --no-patch --format=...` for metadata + `git show --stat` / `--patch` for content. Two-rev mode: resolve both revs; compute `git merge-base base rev`; diff `mb..rev` when MergeBase (equivalent to `base...rev`), `base..rev` otherwise; always compute ahead/behind (`git rev-list --count --left-right base...rev`) for the header; set `Warning` when the diff is empty or merge-base ≠ base (exact wording from the spec: swapped-arguments hint).
3. [ ] File splitting: run diff with `--patch -z`? — no: parse on `diff --git ` boundaries from `RunRaw` output (git paths with spaces stay within the marker line; use `--src-prefix=a/ --dst-prefix=b/` defaults and split conservatively on `\ndiff --git `). Binary detection: `GIT binary patch` / `Binary files ... differ` → `Binary: true`, patch dropped, stat line kept (from a parallel `--numstat` invocation, one extra git call, acceptable).
4. [ ] Budget: package const `diffByteBudget = 64 << 10`. Walk files in git's order: generated paths (package-level `generatedPatterns` var: `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `go.sum`, `*.pb.go`, `Cargo.lock`) and binaries → stat line unless `IncludeGenerated`; others append whole patch while budget lasts; a file whose patch alone exceeds the remaining budget (or the whole budget when first) → stat line + per-file notice. Never emit a partial hunk.
5. [ ] Tests: single-rev metadata+stat; two-rev merge-base vs two-dot difference (fixture has divergent branches); descendant-rev empty diff sets Warning; swapped endpoints sets Warning; lockfile commit → stat-only for lockfile, patch for source; over-budget single file → stat+notice; binary file → stat only; every emitted `FileDiff.Patch` in a truncation scenario passes `git apply --check` in a temp worktree checked out at the pre-image (`git worktree` is unavailable on bare — use `git clone <mirror> <tmp>` + `git checkout <pre-image>` in the test helper).

**Verify:**
```bash
go test ./internal/githistory/ -run TestGetDiff
```

### Task 3: `Blame`

**Files:**
- Create: `internal/githistory/blame.go`
- Create: `internal/githistory/blame_test.go`

**Steps:**

1. [ ] `BlameOptions struct { Rev, Path string; StartLine, EndLine int }`; `BlameLine struct { Line int; SHA, AuthorDate, Author, Content string }`.
2. [ ] Run `git blame --line-porcelain [-L start,end] --end-of-options <rev> -- <path>`; parse porcelain records (header line `<sha> <orig> <final> [count]`, then tag lines, `\t`-prefixed content line ends the record). Collect `author`, `author-time` (format as date), content, final line number.
3. [ ] Tests: full-file blame line numbers and content match `gitfile.ReadFile` at the same rev line-for-line (the spec's flagship equality criterion); `-L` range returns exactly that range; blame at an older rev differs from the indexed rev appropriately; missing path → error wrapping `fs.ErrNotExist` semantics consistent with `gitfile`.

**Verify:**
```bash
go test ./internal/githistory/ -run TestBlame
```

## MCP Tool Tasks

### Task 4: Tool handlers + registration

**Context:** `internal/mcp/tools_file.go` (conventions), `internal/mcp/server.go`

**Files:**
- Create: `internal/mcp/tools_history.go`
- Create: `internal/mcp/tools_history_test.go`
- Modify: `internal/mcp/server.go` (register 3 tools, update package doc + `instructions`)

**Steps:**

1. [ ] `SearchCommitsArgs{Repo, Author, Since, Until, Path, Message, ChangedLiteral, ChangedRegex, Rev string; FirstParent *bool; Limit int}` → resolve repo via `s.resolveIndexedCommit` (default `Rev` = indexed commit), call `githistory.SearchCommits`, format `sha  date  author  subject` lines with `(merge — rerun with first_parent: false for the underlying commit)` annotation on merge rows **when a pickaxe filter was used**; truncation notice; timeout notice per spec ("partial results: timed out after Xs; narrow with path or since/until"); `clampLimit(30, 100)` + `clampNote`.
2. [ ] `GetDiffArgs{Repo, Rev, Base, Path string; Patch, MergeBase, StatOnly, IncludeGenerated *bool}` → default `Rev` to indexed commit; render: header block (endpoints, merge-base, ahead/behind, Warning line when set), then file sections, then `[truncated: ...]` stat block. Tool description documents direction (base → rev), merge-base default, and budget behavior.
3. [ ] `BlameArgs{Repo, Path, Rev string; StartLine, EndLine int}` → default `Rev` to indexed commit; output `line: short-sha date author | content`; line cap default 200 / max 500 with notice suggesting a range (skip the legend/dedup idea for v1 — inline always; note spec allows this). Description steers to ranges and states the indexed-commit default.
4. [ ] Tool descriptions: one paragraph each, following existing tone; must cover: `search_commits` — commit-date filtering note, `-S` literal vs `-G` regex + slower, first-parent default; `get_diff` — direction, three-dot default + when to set `merge_base:false`; `blame` — pinned default, range steering.
5. [ ] Register in `mcpServer()` with `textHandler`; update the package doc comment ("seven v1 tools" → ten) and the `instructions` const (mention history tools operate on the mirror's full fetched history while file reads stay pinned).
6. [ ] Handler tests mirroring `tools_file_test.go` style: happy path per tool against a fixture mirror + status file; unknown repo error; unknown rev error surfaces `ErrUnknownRev` message; blame/read_file line agreement at the MCP layer.

**Verify:**
```bash
go test ./internal/mcp/
```

### Task 5: Staleness + docs

**Files:**
- Modify: `internal/mcp/tools_history.go` (staleness warning parity)
- Modify: `README.md` (tool list)

**Steps:**

1. [ ] Apply the same staleness-warning mechanism `list_repos` uses (check how `staleAfter` is consumed; if warnings are per-tool, append to history outputs; if only in `list_repos`, add a one-line stale suffix to history tools consistent with whatever `grep` does — match existing behavior exactly, do not invent a new pattern).
2. [ ] Update README tool table with the three new tools and one-line descriptions.

**Verify:**
```bash
gofumpt -w . && go test ./... && go build .
# Expected: full suite green
```

## Final Verification

```bash
go test ./... && go build .
# Manual smoke: `go run . mcp` + drive search_commits/get_diff/blame against a real indexed repo
```

Create PR for human review.
