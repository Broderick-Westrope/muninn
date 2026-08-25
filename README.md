# muninn

A single Go binary that indexes your GitHub repos for code search and serves them to agents over MCP. It syncs bare mirrors on a schedule (via launchd), builds Zoekt search shards, and exposes grep, glob, file access, and symbol lookup tools over stdio. Includes an on-demand local web UI for interactive search.

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
  }
}
```

Token resolution: `auth.githubToken` in config → the `token.env` env var → `gh auth token`.

## Usage

```bash
muninn sync              # reconcile repos: fetch mirrors, build index, GC removed
muninn sync --install    # schedule syncs via launchd (bakes token + ctags path into config)
muninn status            # last sync result, per-repo failures, agent status
muninn search "sym:NewServer case:yes repo:muninn"   # terminal search (zoekt syntax)
muninn mcp               # MCP server over stdio
muninn web               # local web UI at 127.0.0.1:7576 (Ctrl-C to stop)
```

### MCP

```json
{ "mcpServers": { "muninn": { "command": "muninn", "args": ["mcp"] } } }
```

Tools: `grep`, `glob`, `list_repos`, `read_file`, `list_tree`, `find_symbol_definitions`, `find_symbol_references`. File reads are pinned to the indexed commit so line numbers always match search results. `find_symbol_references` is approximate (text search excluding definition sites).

## Data

XDG paths: mirrors + index under `~/.local/share/muninn/`, sync status + logs under `~/.local/state/muninn/`. Nothing stays resident between sessions — the MCP server lives only as long as the agent session, the web UI only until Ctrl-C, and sync is a short-lived scheduled job.
