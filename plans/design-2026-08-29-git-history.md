# Muninn Design Spec: Git History Tools

**Problem:** Muninn's seven v1 tools cover current-content search well, but agents cannot answer history questions — "when/why did this change", "what did this commit do", "who wrote this line", "when was this string introduced/removed". Sourcebot (`list_commits`, `get_diff`), Sourcegraph (`commit_search`, `diff_search`), and GitHub's MCP server (`list_commits`, `get_commit`, `search_commits`) all treat these as core. Google's developer-search study (Sadowski et al. 2015) puts history queries among the top code-search intents. Zoekt indexes zero history, so this is net-new capability with no overlap with existing tools.

**Goal:** Three new MCP tools, thin wrappers over system git against the bare mirrors:

- `search_commits` — git log with filters, including pickaxe (`-S`/`-G`) content-change search.
- `get_diff` — commit details (one rev) or comparison (two revs), with agent-safe truncation.
- `blame` — line attribution pinned to the indexed commit by default.

Three tools, not five: description tokens are paid by every agent session, and each merged tool covers its siblings' use cases with one extra parameter. (An earlier draft had five history tools plus a symbol-graph workstream; see "Out" for what was cut and why.)

**Prerequisite fix (ships first, independent of the tools): commit pinning that survives prune, gc, and mirror fetch.**

Two failure modes currently threaten the indexed commit's objects, silently breaking the existing v1 line-number guarantee (`read_file`/`list_tree` pinned to the indexed commit) — every history tool inherits the same exposure:

1. After a force-push or branch deletion upstream, the indexed commit becomes unreachable; any gc/repack (including fetch-triggered auto-gc) prunes its objects.
2. A naive pin ref does not survive mirror fetches: a `--mirror` clone's refspec is `+refs/*:refs/*`, so any prune-enabled fetch **deletes every local ref not on the remote** — including `refs/muninn/*`. The pin would be destroyed by the very sync job that maintains it (git-fetch PRUNING docs describe exactly this hazard).

The fix, in full:

- **Exclude pin refs from the mirror refspec**: add negative refspec `remote.origin.fetch = ^refs/muninn/*` (git ≥ 2.29) at clone time, and retrofit onto existing mirrors during sync.
- **Two-generation pin**: sync maintains `refs/muninn/indexed` (current) and `refs/muninn/indexed-prev` (previous). Latest-only is insufficient: a live MCP session may hold line numbers/SHAs against the previous indexed commit while sync moves the pin — hourly launchd sync plus long agent sessions makes this routine. Two generations plus the existing "status file re-read per tool call" behavior (`internal/mcp/server.go` never caches the status) covers live sessions across one sync boundary; sessions spanning two syncs get the accurate `ErrIndexMismatch` remedy.
- **Pin before index, not after**: sync updates the pin ref immediately **after fetch, before indexing**. An index failure (zoekt error, disk full) must not leave the fetched commit unpinned while the status file still references the old one.
- **Disable auto-gc** (`gc.auto=0`) at clone time and retrofit during sync.
- **Explicit maintenance**: unbounded `gc.auto=0` means unbounded object accumulation on force-push-heavy mirrors. Sync runs `git gc` on a mirror only **after** the pin refs are updated (pinned commits stay reachable), and only periodically (e.g. every N syncs or when loose-object count crosses a threshold — decide during planning). This is the designed answer to "when does gc ever run": after pinning, never before.
- **Error taxonomy**: a rev that fails to resolve distinguishes "unknown ref/SHA" (typo, never-fetched ref) from "index/mirror mismatch" (existing `ErrIndexMismatch`, "run `muninn sync`" remedy), and the mismatch message must be accurate about cause.

**Scope:**

In:

- `search_commits` — `git log` on one repo's mirror. Filters, all optional and composable:
  - `author` (`--author`), `since`/`until` (`--since`/`--until`; note these filter on **commit date** while output shows author date — the description says so), `path` (pathspec), `message` (`--grep`).
  - `changed_literal` — pickaxe `-S<string>`: commits where the occurrence count of the **literal** string changed (named to stop agents passing regexes). `changed_regex` — `-G<regex>`: commits whose diff has a hunk matching the regex. Mutually exclusive; the description explains `-S` vs `-G` semantics and notes `-G` is substantially slower.
  - `rev` — start point; defaults to the indexed commit. `first_parent` — defaults **true** so merge-heavy repos return mainline history. When a pickaxe hit under `first_parent: true` is a merge commit, the output row is annotated `(merge — rerun with first_parent: false for the underlying commit)`; otherwise the flagship "when *and why*" question dead-ends on "Merge pull request #123".
  - Single `path` gets `--follow` so file history doesn't silently stop at renames; the description notes multi-path queries do not follow renames.
  - Output: `sha  date  author  subject` lines. Default limit 30, max 100, truncation notice.
  - Per-repo only. Cross-repo pickaxe over 200 mirrors is a different performance universe; the tool errors if `repo` is missing.
- `get_diff` — one tool for both "show this commit" and "compare two revs":
  - `rev` only: commit metadata (sha, author, date, full message) + `--stat`; `patch: true` adds the diff.
  - `rev` + `base`: diff **from base to rev** (direction stated in the description and echoed in the output header). `patch` defaults **true** in two-rev mode (comparing is the point), `stat_only` available.
  - `merge_base` (default **true** in two-rev mode): three-dot semantics (`base...rev`). To prevent the silent-empty-diff trap (`A...B` where one rev descends from the other, or swapped arguments), the output **always** begins with a header: resolved endpoints, the computed merge-base, and an explicit warning line when the diff is empty or when merge-base ≠ base ("rev is X commits ahead of base; empty diff means rev descends from base — did you mean merge_base: false or swapped arguments?"). `merge_base: false` gives two-dot for literal point-to-point tree comparison.
  - Optional `path` filter.
  - **Truncation at file boundaries, never mid-hunk**: whole per-file diffs until the byte budget (64 KiB) is spent, then remaining files as `--stat` lines with a notice. A single file whose diff alone exceeds the budget is emitted as a stat line with a per-file notice ("diff exceeds budget; use path filter with a smaller range or read the file at each rev") — the budget is never violated. Binary files always appear as stat lines.
  - Generated/lockfile paths (`package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `go.sum`, `*.pb.go`, `Cargo.lock`) are stat-only by default; `include_generated: true` overrides.
  - Always `--no-ext-diff --no-color`; `-z` where filenames are parsed.
- `blame` — `git blame --line-porcelain <rev> -- <path>`:
  - `rev` defaults to the **indexed commit** so line numbers agree exactly with `read_file`/`grep`. Explicit revs accepted.
  - `start_line`/`end_line` map to `-L`. The description strongly steers agents to blame ranges: full-file blame is truncated at the line cap (default 200 output lines, max 500) with a notice suggesting a range.
  - Output: `line: short-sha date author | content`, author/date deduped into a legend only above a repetition threshold (prototype; if the legend costs more tokens than it saves, stay inline).

Out (this iteration, possible later):

- **Symbol-graph tools (`repo_map`, `symbol_neighbors`) — cut.** Adversarial review showed the cited evidence (LocAgent) describes AST-resolved edges, not ctags spans + word-boundary regex; the approximate version produces noise that costs agent context instead of saving it, and reference-count ranking required thousands of zoekt queries per call while rewarding name commonness over importance. Revisit only with tree-sitter-resolved edges for specific languages and a precision eval defined up front.
- Cross-repo `search_commits`.
- Cross-rev blame chaining ("blame the version before this commit").
- `shortlog`/contributor-stats tools.
- CLI parity commands (`muninn log`, `muninn blame`) — MCP is the primary surface.
- Returning matched hunks inline from pickaxe (`-S ... -p`) — evaluate after usage data.
- Semantic/embedding search, `ask_codebase`, precise SCIP/LSP navigation (previously ruled out).

**Constraints:**

- **Shell out to system git**, never go-git: go-git blame is pathologically slow (go-git#14), and Gitea/OpenGrok both shell out. `internal/gitfile` already establishes the pattern (`runGit`/`runGitRaw`).
- **Hermetic git invocations**: every git subprocess runs with `GIT_CONFIG_GLOBAL=/dev/null` and `GIT_CONFIG_NOSYSTEM=1`. User/system config silently changes tool semantics otherwise (`grep.patternType=perl` alters `--grep`, `log.showSignature` pollutes output, `blame.ignoreRevsFile`, `diff.algorithm`, `log.follow`). Repo-local config (where muninn writes `gc.auto`, refspecs) still applies. Retrofit this onto the existing `gitfile` helpers too.
- **Global subprocess timeout**: every git invocation — not just pickaxe — runs under a deadline (default 15s, generous for reads on mirrors). Diff between distant revs or blame on pathological files can run minutes; a hung tool call is the worst agent experience. On expiry, tools that can return labeled partials do (`search_commits` reads log output incrementally and keeps commits parsed so far); others return a clear timeout error naming the narrowing options (path filter, line range, closer revs).
- **Validate system git at startup** the same way ctags is validated: fail loudly with a remedy. Require git ≥ 2.29 (negative refspec support for the pinning fix).
- **Argument-injection safety**: all user-supplied values pass as separate argv entries (never shell-interpolated — existing pattern); `rev` **and** `base` are validated via `git rev-parse --verify` and rejected if they start with `-`; `path` values starting with `-` are rejected and pathspec magic (`:` prefix) is disallowed; `repo` is validated against the known-repo set from the status file (no path traversal); `--` separates revs from paths in every invocation that accepts both.
- All operations are read-only and work on bare mirrors: log, show, diff-between-revs, blame-at-rev, and pickaxe need no working tree. Blame **must** pass an explicit rev (bare repos have no worktree HEAD default).
- Every tool enforces output caps with truncation notices (same discipline as v1 tools). Patch output capped by bytes at file boundaries; log and blame output by lines.
- Zero-idle property unchanged: no new resident processes, no new storage. New sync work is two `update-ref` calls per indexed repo plus periodic explicit gc — negligible and bounded.
- Tool count goes 7 → 10. Each description must earn its tokens: short, example-driven, explicit about defaults (pinned revs, first-parent, merge-base, diff direction).

**Success Criteria:**

- [ ] **Pinning survives hostile maintenance**: fixture test force-pushes the remote, runs sync, then runs `git fetch --prune` and `git gc --prune=now` on the mirror — and `read_file`/`blame` at both the current and previous indexed commits still work. (A test without explicit gc/prune verifies nothing: unreachable objects survive by default for weeks.)
- [ ] **Pin-before-index**: fixture test makes indexing fail after fetch; the fetched commit is pinned anyway and the status file still points at the old (also pinned) commit.
- [ ] **Flagship scenario, concretely**: fixture repo where function `Foo`'s implementation changed in commit C (behind a merge M). `search_commits(changed_literal: "Foo", first_parent: true)` surfaces M with the merge annotation; rerunning with `first_parent: false` surfaces C; `get_diff(rev: C)` shows the change. Test asserts this exact retrieval chain.
- [ ] `blame` line numbers agree exactly with `read_file` for the same path at the indexed commit (fixture asserts equality).
- [ ] `get_diff` on a lockfile-heavy commit returns source diffs intact and lockfiles as stat lines within budget.
- [ ] Two-rev `get_diff` where `rev` descends from `base` under `merge_base: true` produces the empty-diff warning header (test asserts the warning, not just emptiness); swapped-endpoint case covered.
- [ ] Truncated diffs are always syntactically whole: for each emitted per-file text diff in truncated output, `git apply --check` succeeds against a temp worktree checked out at the pre-image rev (binary files excluded — they are never emitted as patches).
- [ ] A single file whose diff exceeds the whole budget yields a stat line + notice, never a partial hunk and never budget violation.
- [ ] Unfiltered pickaxe on the largest indexed repo (pilot-engine) either completes or returns labeled partial results within the timeout; never hangs. Same harness asserts `get_diff` and `blame` respect the global timeout.
- [ ] `search_commits` on a merge-heavy fixture returns mainline commits under the default; renamed-file history continues past the rename when a single `path` is given.
- [ ] Hermetic-config test: a poisoned `~/.gitconfig` (`grep.patternType=perl`, `log.showSignature=true`) does not change any tool's output.
- [ ] All new tools carry the v1 staleness warning; git validation failure at startup produces an actionable error naming the minimum version.

**Design Decisions:**

- **Three tools, not five**: `get_commit`/`get_diff` merged (one rev vs two — same output machinery); `list_commits`/pickaxe merged (pickaxe is a log filter, not a different operation). The "one whitelisted read-only `git` tool" alternative was considered — agents know git — but rejected: parameter-shaped tools enforce pinned-rev defaults, truncation, timeouts, hermetic config, and lockfile handling; a passthrough cannot.
- **Two-generation pin ref + negative refspec**: latest-only pinning contradicts the concurrency model muninn already commits to (live MCP sessions during sync); the negative refspec is what makes any pin ref survive mirror fetches at all. Both are cheap; neither is optional.
- **`first_parent` defaults true, with merge annotation**: 30 result slots on merge-heavy repos otherwise fill with merge noise; the annotation keeps the "why" reachable in one follow-up call.
- **`merge_base` defaults true, with mandatory endpoint header**: three-dot answers "what does this branch change", but silently returns empty for descendant/swapped revs — the header converts a trap into a self-explaining result.
- **File-boundary diff truncation**: a byte cap that cuts mid-hunk hands agents a malformed patch. Whole files + stat-remainder is strictly more useful at the same budget.
- **Blame defaults pinned to the indexed commit**: preserves the v1 line-number guarantee; blame is the only line-number-bearing history output.
- **Hermetic config + global timeout as blanket constraints**: correctness and liveness properties should not be per-tool opt-ins.
- **Explicit gc after pinning**: the only ordering in which maintenance is safe; makes "when does gc run" a designed answer instead of an accident.
- **New `internal/githistory` package** mirroring `internal/gitfile`'s shape (context-first, explicit errors). Extract `runGit`/`runGitRaw`/`checkCommit` into a shared internal package — three consumers (gitfile, githistory, sync) now justify it; the hermetic-env change lands in the shared helper so gitfile inherits it.

**Implementation Notes (resolve during planning):**

- Confirm the current sync's prune behavior (`remote update --prune`? config?) and make prune explicit + ordered after the negative-refspec retrofit, so the first post-upgrade sync cannot delete pins before the refspec lands.
- Pin-ref reconciliation: repo removed from config → reconciliation GC deletes mirror (pins go with it); nothing extra needed. Verify `indexed-prev` is updated atomically with `indexed` (read old value, two `update-ref` calls; a `--stdin` transaction gives atomicity).
- gc cadence: pick the trigger (every N syncs vs `git count-objects -v` threshold); measure gc time on pilot-engine before choosing.
- Pickaxe/log incremental read: parse commits as they stream; on deadline, kill the process group and label the partial. Verify git flushes per-commit with `--no-pager`.
- Byte budget accounting for `get_diff`: `--numstat` pre-pass vs stream-and-cut at file boundaries; prototype both, prefer single invocation if boundary detection is reliable (`diff --git ` markers with `-z` filename parsing).
- `--follow` interacts badly with some filter combinations (`--follow` requires exactly one pathspec); enforce single-path when following, error otherwise.
- Lockfile pattern list: package-level var, not config.
- `search_commits` format string: `%h%x09%as%x09%an%x09%s` (short sha, author date, author, subject); confirm tab-safety of subjects is a non-issue (subject is last field).
- Blame legend threshold: prototype on a real high-churn file before committing to the format.

**Evidence Base:**

- Sourcebot MCP tools: https://docs.sourcebot.dev/docs/features/mcp-server (`list_commits`, `get_diff` shipped there)
- Sourcegraph MCP: https://sourcegraph.com/docs/api/mcp (`commit_search`, `diff_search` first-class)
- GitHub MCP server: https://github.com/github/github-mcp-server (`list_commits`, `get_commit`, `search_commits`)
- Developer search intents: Sadowski, Stolee, Elbaum — "How Developers Search for Code" (Google, 2015)
- go-git blame performance: https://github.com/go-git/go-git/issues/14
- Pickaxe semantics: https://git-scm.com/docs/git-log (`-S`/`-G`)
- Mirror-refspec prune hazard: https://git-scm.com/docs/git-fetch (PRUNING section); negative refspecs: git 2.29 release notes
