# muninn

A single Go binary that indexes your GitHub repos for code search and serves them to agents over MCP. It syncs bare mirrors on a schedule (via launchd), builds Zoekt search shards, and exposes grep, glob, file access, and symbol lookup tools over stdio — with an on-demand local web UI for interactive search and near-zero idle memory cost.

## Install

```bash
go install github.com/broderick-westrope/muninn@latest
```

## Usage

```bash
muninn sync      # reconcile repos: fetch mirrors, build index
muninn status    # show last sync result
muninn mcp       # run the MCP server over stdio
muninn search    # search from the terminal
muninn web       # start the local web UI
```

Configuration lives at `~/.config/muninn/config.json` (override with `--config`).
