# Phase 2: History Tools (`internal/githistory` + MCP)

> **Status:** COMPLETED
> Part of `plans/impl-2026-08-29-git-history/` — see README. Requires phase 1 merged (`internal/gitcmd` with partial output + exit codes + `RunStdin`, `ResolveRev`/`ErrUnknownRev`/`ErrUnknownPath`, two-gen pins).

## Specification

**Problem:** Agents cannot answer "when/why did this change", "what did this commit do", "who wrote this line" over the indexed estate.

**Goal:** Three MCP tools — `search_commits`, `get_diff`, `blame` — per the spec (`plans/design-2026-08-29-git-history.md`), with agent-safe defaults and hard output caps.

**Scope:** In: `internal/githistory` core package, three MCP tool handlers, server registration, instructions update. Out: cross-repo search, CLI commands, matched-hunk pickaxe output, symbol-graph tools.

**Success Criteria** (from the spec, phase-2 subset):

- [ ] Flagship scenario: fixture where `Foo` changed in commit C behind merge M — `search_commits(changed_literal:"Foo")` surfaces M annotated as a merge; `first_parent:false` surfaces C; `get_diff(rev:C)` shows the change.
- [ ] `get_diff` on a **merge commit** shows the first-parent diff, not an empty combined diff.
- [ ] `blame` line numbers agree exactly with `read_file` at the indexed commit.
- [ ] Lockfile-heavy diff: source diffs intact, lockfiles as stat lines, budget respected.
- [ ] Descendant/swapped revs under `merge_base:true` produce the warning header, not a bare empty diff.
- [ ] Truncated output never contains a partial hunk; every emitted per-file patch is whole (verified with `git apply --check` on the raw patches).
- [ ] A single over-budget file yields a stat line + notice.
- [ ] Timeout on pickaxe returns labeled partial results, never a hang.
- [ ] Renamed-file history continues past the rename for single-path queries.

## Context Loading

_Run before starting:_

```bash
read plans/design-2026-08-29-git-history.md   # authoritative tool contracts
read internal/gitcmd/gitcmd.go                # Runner, Error{ExitCode}, ErrTimeout, RunStdin (phase 1)
read internal/gitfile/gitfile.go              # ResolveRev, ErrUnknownRev, ErrUnknownPath, checkCommit, shortSHA
read internal/mcp/server.go                   # registration, textHandler, instructions
read internal/mcp/tools_file.go               # arg struct + description + resolveIndexedCommit conventions
read internal/mcp/tools_search.go             # clampLimit/clampNote/formatGrep conventions
```

## githistory Core Tasks

All in `internal/githistory`. Shared decisions:

- Every function takes `(ctx, mirrorDir, ...)`; caller-supplied revs go through `gitfile.ResolveRev` first.
- **Literal pathspecs**: the package Runner env includes `GIT_LITERAL_PATHSPECS=1` — `path` params are literal paths, never globs (also what `--follow` requires). Tool descriptions say so.
- Per-op timeouts: `logTimeout = 15s` (partial results supported), `diffTimeout = 60s`, `blameTimeout = 60s` (no partial semantics — a timeout is an error naming the narrowing options; state this in the error text).
- Fixture: shared `newFixtureRepo(t)` in `fixture_test.go` shells git to build a bare mirror with scripted history: linear commits, a side branch merged with `--no-ff` (merge M containing change C to `Foo`), a rename, a lockfile commit, a binary file, a root commit (empty `%P` — parsers must accept it). All test-relevant commits stay reachable from branches so test clones see them.

### Task 1: `SearchCommits`

**Files:**
- Create: `internal/githistory/githistory.go` (package doc, shared consts, `Commit` struct)
- Create: `internal/githistory/log.go`
- Create: `internal/githistory/log_test.go`
- Create: `internal/githistory/fixture_test.go`

**Steps:**

1. [ ] `type Commit struct { SHA, AuthorDate, Author, Subject string; IsMerge bool }`; `type LogOptions struct { Rev, Author, Since, Until, Path, Message, ChangedLiteral, ChangedRegex string; FirstParent *bool; Limit int }` (nil `FirstParent` = true).
2. [ ] `SearchCommits(ctx, mirrorDir string, opts LogOptions) (commits []Commit, truncated, timedOut bool, err error)`:
   - Validate: `ChangedLiteral`/`ChangedRegex` mutually exclusive; `Path` rejected if leading `-` or `:`; resolve `Rev` via `gitfile.ResolveRev`.
   - argv: `log`, `--format=%H%x09%as%x09%an%x09%P%x09%s`, `-n <limit+1>`, `--first-parent` unless disabled, `--author=`, `--since=`/`--until=`, `--grep=`, `-S<literal>`/`-G<regex>` (single concatenated tokens), `--follow` iff `Path != ""`, `--end-of-options`, `<sha>`, and `-- <path>` if set.
   - Parse with `strings.SplitN(line, "\t", 5)` — **subjects may contain tabs**; subject is last field so SplitN preserves it. `%P` field: empty for root commits (accept), space-separated for merges → `IsMerge = len(fields) > 1` after `strings.Fields`.
   - On `gitcmd.ErrTimeout`: parse the complete lines from the partial stdout (phase 1 guarantees output on error), return `timedOut = true`.
   - `len > limit` → truncate, `truncated = true`.
3. [ ] Tests: first-parent default excludes merge-side commits, `false` includes them; `-S` finds introduce + remove commits; `--follow` walks past the rename; root-commit line parses; tab-containing subject parses; mutual exclusion + injection guards; since/until; limit/truncated; timeout partial (fake slow git via PATH, as in gitcmd tests).

**Verify:**
```bash
go test ./internal/githistory/ -run TestSearchCommits
```

### Task 2: `GetDiff`

**Files:**
- Create: `internal/githistory/diff.go`
- Create: `internal/githistory/diff_test.go`

**Steps:**

1. [ ] Types: `DiffOptions struct { Rev, Base, Path string; Patch, MergeBase, StatOnly, IncludeGenerated *bool }` (Patch default: false single-rev, true two-rev; MergeBase default true two-rev). Result: `Diff struct { Meta CommitMeta; MergeBaseSHA string; Ahead, Behind int; Files []FileDiff; OmittedStats []string; Warning string }`; `FileDiff { Path, Patch, StatLine string; Binary, Generated bool }`.
2. [ ] Single-rev mode: metadata via `git show --no-patch --format=...`. Content: **merge commits get the first-parent diff `rev^1..rev` explicitly** — bare `git show` on a merge emits combined-diff format, which is empty for clean merges and would silently report "nothing changed" for exactly the commits agents inspect most. Detect via parent count (`%P`). Non-merges: `rev^..rev` (root commit: use `git show` semantics or diff against the empty tree `4b825dc642...`).
3. [ ] Two-rev mode: resolve both revs; `git merge-base <base> <rev>` — **exit code 1 with empty stdout is a legitimate "no merge base" answer** (disjoint histories), distinguished via `gitcmd.Error.ExitCode`, not treated as failure: fall back to two-dot with a Warning line. Otherwise diff `mb..rev` when MergeBase (≡ `base...rev`) else `base..rev`; `git rev-list --count --left-right base...rev` for Ahead/Behind; Warning set when diff is empty or mb ≠ base (spec wording: swapped-arguments hint).
4. [ ] File handling: get the file list + stats from `git diff --numstat -z` (binary files show `-\t-`); then fetch patches with `git diff --patch` and split on `\ndiff --git ` boundaries. Generated paths (package var `generatedPatterns`: `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `go.sum`, `*.pb.go`, `Cargo.lock` — matched on basename/glob) and binaries → stat line only, unless `IncludeGenerated`.
5. [ ] Budget: `diffByteBudget = 64 << 10`, **total across the rendered patch sections**. Files in git's order: append whole patches while they fit; a file whose whole patch doesn't fit in the remaining budget → stat line + per-file notice (even the first file — spec: never a partial hunk, budget never violated; the notice names the path-filter escape hatch).
6. [ ] Tests: single-rev metadata + stat; **merge-commit diff is non-empty and equals `rev^1..rev`**; two-rev merge-base vs two-dot on divergent branches; disjoint-history fallback (fixture: orphan branch via `checkout --orphan`); descendant-rev empty diff → Warning; swapped endpoints → Warning; lockfile stat-only + source patch intact; over-budget single file → stat + notice; binary → stat only; patch-wholeness: for a truncation scenario, apply each emitted `FileDiff.Patch` (raw, pre-render) with `git apply --check` in a scratch **local-path clone** of the fixture (`git clone /path/to/mirror` — plain-path clone hardlinks/copies the whole object store; do NOT use `file://` URL, which packs only reachable objects) checked out at the pre-image rev.

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
2. [ ] `git blame --line-porcelain [-L start,end] --end-of-options <sha> -- <path>` under `blameTimeout`. Porcelain parsing: header `<sha> <origLine> <finalLine> [<numLines>]`, tag lines (`author`, `author-time` → format `2006-01-02`), content line prefixed `\t` ends each record. Map git's "no such path" failure through `gitfile.ClassifyPathErr` → `ErrUnknownPath`.
3. [ ] Tests: full-file blame agrees line-for-line (number + content) with `gitfile.ReadFile` at the same rev; `-L` range exact; older rev differs appropriately; missing path → `ErrUnknownPath`; `-L` past EOF → clear error (git exits 128; surface the stderr message).

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

1. [ ] `SearchCommitsArgs{Repo, Author, Since, Until, Path, Message, ChangedLiteral, ChangedRegex, Rev string; FirstParent *bool; Limit int}` → resolve repo via `s.resolveIndexedCommit` (default `Rev` = indexed commit), call `githistory.SearchCommits`, render `sha  date  author  subject` lines. Merge rows are **always** annotated `(merge)`; when a pickaxe filter was used the annotation extends to `(merge — rerun with first_parent: false for the underlying commit)` because that is the case where the mainline hit hides the "why". Truncation notice; timeout notice ("partial results: timed out after Xs; narrow with path or since/until"); `clampLimit(30, 100)` + `clampNote`.
2. [ ] `GetDiffArgs{Repo, Rev, Base, Path string; Patch, MergeBase, StatOnly, IncludeGenerated *bool}` → default `Rev` to indexed commit; render: header block (endpoints, merge-base, ahead/behind, Warning when set), file sections, `[omitted: ...]` stat block for `OmittedStats`. Description documents direction (base → rev), merge-base default + `merge_base:false` for point-to-point, literal paths, and budget behavior.
3. [ ] `BlameArgs{Repo, Path, Rev string; StartLine, EndLine int}` → default `Rev` to indexed commit; output `line: short-sha date author | content` (inline always — no legend for v1); line cap default 200 / max 500 with notice suggesting a range. Description steers to ranges and states the indexed-commit default.
4. [ ] Error rendering: `ErrUnknownRev` and `ErrUnknownPath` surface as tool errors with their remedial text; `ErrIndexMismatch` keeps its "run `muninn sync`" text; `gitcmd.ErrTimeout` on diff/blame → "timed out; narrow with a path filter / line range / closer revs".
5. [ ] Descriptions (one paragraph each, existing tone): `search_commits` — commit-date filtering note, `-S` literal vs `-G` regex (+ slower), first-parent default, literal paths; `get_diff` — direction, three-dot default, merge-commit = first-parent diff; `blame` — pinned default, range steering.
6. [ ] Register in `mcpServer()` via `textHandler`; update package doc ("seven v1 tools" → ten) and `instructions` const (history tools operate on full fetched history; file reads stay pinned).
7. [ ] Handler tests in `tools_file_test.go` style: happy path per tool against a fixture mirror + status file; unknown repo; unknown rev/path error text; blame/read_file agreement at the MCP layer; merge annotation rendering both with and without pickaxe.

**Verify:**
```bash
go test ./internal/mcp/
```

### Task 5: Staleness parity + docs

**Files:**
- Modify: `internal/mcp/tools_history.go`
- Modify: `README.md` (tool list)

**Steps:**

1. [ ] Inspect how existing tools consume `staleAfter` (check `list_repos` and whether `grep`/`read_file` append anything). Apply the **identical** mechanism to the three history tools. Concrete check: with a status file older than `staleAfter`, `search_commits` output contains the same staleness text `list_repos` produces for the same fixture (assert string equality of the warning fragment in a test).
2. [ ] Update README tool table with the three new tools.

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

## Review notes

Devils-advocate review (round 1) caught: `git show` on merge commits emitting empty combined diffs (now first-parent diff, new success criterion + fixture case); `merge-base` exit 1 being a legitimate answer needing `ExitCode` (disjoint-history fallback added); `%s` subjects containing tabs (SplitN mandated) and empty `%P` on root commits; pathspec globbing ambiguity (GIT_LITERAL_PATHSPECS=1); `git apply --check` needing raw pre-render patches and a plain-path clone (file:// URL clones only pack reachable objects); 64 KiB budget semantics pinned (total, stat-fallback even for the first file); per-op timeouts for diff/blame (no partials — explicit error text); merge annotation gating rationale made explicit (always `(merge)`, extended advice only under pickaxe); staleness parity made concretely testable.
