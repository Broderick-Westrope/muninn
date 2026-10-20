# muninn

A single Go binary that indexes your GitHub repos for code search and serves them to agents over MCP. It syncs bare mirrors on a schedule (via launchd), builds [Zoekt](https://github.com/sourcegraph/zoekt) search shards, and exposes grep, glob, file access, symbol lookup, and git history tools (commit search, diff, blame) over stdio. Includes an on-demand local web UI for interactive search.

Named for Odin's raven that flies out over the world and brings back memory.

**WHY?** I really enjoyed using [Sourcebot](https://github.com/sourcebot-dev/sourcebot) for many months. I ran it locally in Docker via compose. But I thought it was using too much memory and I was running pretty hot overall. So muninn has a much lower memory footprint. It doesn't need Redis or Postgres since it's more minimal and built for running locally rather than having to consider enterprise scale. It also gives me the ability to be flexible with the web UI which I'd like to integrate better with tools I use (namely GitHub and Cursor).

## Requirements

- macOS (launchd scheduling; core is portable)
- `git`
- universal-ctags with `+interactive` (`brew install universal-ctags`) — hard requirement for symbol search

## Install

```bash
brew install broderick-westrope/tap/muninn
# OR
go install github.com/broderick-westrope/muninn@latest
```

## Configure

`~/.config/muninn/config.json` (a compatible subset of Sourcebot's schema — an existing Sourcebot config migrates with trivial edits):

```json
{
  "connections": {
    "github": {
      "type": "github",
      "orgs": ["your-org"],
      "repos": ["you/extra-repo"],
      "token": { "env": "GITHUB_TOKEN" },
      "exclude": { "archived": true, "repos": ["your-org/skip-me"] }
    }
  },
  "sync": { "intervalMinutes": 60, "retryIntervalMinutes": 10 }
}
```

Token resolution, per run: `auth.githubToken` in config → the `token.env` env var
→ `gh auth token`. `gh` is preferred for scheduled syncs because it re-resolves
every run and so survives token rotation and expiry. muninn never writes a
token to the config itself; `--install` records the *path* to `gh` (and to
`ctags`) because launchd's PATH excludes Homebrew.

`--install` warns about the two sources a scheduled run cannot rely on: a token
reachable only through your shell environment (invisible to a launchd job), and
one stored in `auth.githubToken` (it takes precedence over `gh` and does not
rotate, so every sync fails once it expires). If an older muninn baked a token
into your config, delete that field to get per-run resolution back.

## Usage

```bash
muninn sync              # reconcile repos: fetch mirrors, build index, GC removed
muninn sync --if-due     # sync only if the index is stale (what the agent runs)
muninn sync --install    # schedule syncs via launchd (records ctags + gh paths)
muninn status            # last run, last success, per-repo failures, agent status
muninn search "sym:NewServer case:yes repo:muninn"   # terminal search (zoekt syntax)
muninn mcp               # MCP server over stdio
muninn web               # local web UI at 127.0.0.1:7576 (Ctrl-C to stop)
```

### Scheduling

The launchd agent wakes every `sync.retryIntervalMinutes` and runs `sync
--if-due --no-repair`, which decides whether there is work to do:

- Fresh (a success within `sync.intervalMinutes`, default 60): exit immediately.
- Stale: sync.
- Stale *and* the last run failed or was interrupted: reattempt every
  `sync.retryIntervalMinutes` (default 10) instead of waiting out the full
  interval, so an expired token or a wake with no network costs minutes rather
  than an hour.

Because the cadence lives in the config rather than the plist, changing it does
not need a reinstall. Runs are serialized by an advisory lock on
`~/.local/state/muninn/sync.lock`: a run that finds it held exits without doing
anything, so two syncs never touch the mirrors or the index at once. (On a
filesystem that does not support `flock` the run proceeds with a warning
instead of aborting.)

The plist records the binary path as installed (e.g. `/opt/homebrew/bin/muninn`)
rather than resolving it through symlinks, so a package upgrade that repoints
that symlink does not leave launchd spawning a deleted versioned path. If the
binary the plist names goes missing anyway, an interactive `muninn sync`
reinstalls the agent — only in that case, so that a `go run` or scratch build
cannot capture the schedule. `muninn status` reports the agent's real state,
asking launchd whether the job is loaded rather than trusting the plist on
disk.

Staleness is per repo, measured from that repo's last successful sync, so one
unreachable repo does not age the rest of the index. `read_file`, `list_tree`
and the history tools warn when *the repo you touched* is behind; `list_repos`
shows an age column for every repo and is the fleet view; `muninn status` is the
full picture.

A run that fetched everything it could reach is not treated as a broken sync:
only a total failure (bad credentials, missing ctags, a discovery that returned
nothing) or a run that died mid-flight switches the scheduler to the retry
cadence. That distinction is what stops one permanently failing repo from
refetching the whole fleet every few minutes forever.

A broken sync cannot masquerade as a fresh index: a discovery that returns
nothing, a clock that has moved backwards, and a run that died mid-flight are
all reported rather than recorded as success.

### MCP

```json
{ "mcpServers": { "muninn": { "command": "muninn", "args": ["mcp"] } } }
```

Tools: `grep`, `glob`, `list_repos`, `read_file`, `list_tree`, `find_symbol_definitions`, `find_symbol_references`, `search_commits`, `get_diff`, `blame`. File reads are pinned to the indexed commit so line numbers always match search results. `find_symbol_references` is approximate (text search excluding definition sites). `search_commits` filters commit history (author, date, message, path, pickaxe content-change search); `get_diff` shows one commit or compares two revs with agent-safe truncation; `blame` attributes lines at the indexed commit by default so its line numbers agree with `read_file`.

Every session shares one background server per index, so opening more agent sessions costs about 3 MB each instead of a full copy of the searcher. `muninn mcp` starts that server on demand and hands its stdio to it through `muninn-mcp-proxy`, which ships alongside `muninn`. The server exits after 5 minutes with no sessions and logs to `~/.local/state/muninn/mcp.log`. If it can't be reached, the session is served in-process as before; `muninn mcp --in-process` always does that.

## Data

XDG paths: mirrors + index under `~/.local/share/muninn/`, sync status + logs under `~/.local/state/muninn/`. Nothing stays resident for long: the shared MCP server exits 5 minutes after its last session ends, the web UI runs only until Ctrl-C, and sync is a short-lived scheduled job.
