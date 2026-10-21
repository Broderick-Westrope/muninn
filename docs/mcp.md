# MCP

`muninn mcp` serves the index to coding agents over stdio.

```json
{ "mcpServers": { "muninn": { "command": "muninn", "args": ["mcp"] } } }
```

For Claude Code: `claude mcp add muninn -- muninn mcp`.

## Tools

Every read is pinned to the commit the repo was indexed at, so line numbers from `grep`, `read_file` and `blame` always agree. Content can lag a repo's latest commit until the next sync, and tools that read a repo warn when it hasn't synced recently.

| Tool | What it does |
|---|---|
| `grep` | Regex search (RE2) across every repo. The pattern is a Zoekt query, so `repo:`, `file:`, `sym:`, `lang:` and `case:` atoms combine with it. |
| `glob` | Find files by path glob (`**/*.go`, `src/**/*.{ts,tsx}`), case-insensitive. |
| `list_repos` | Indexed repos with branch, indexed commit and how long ago each synced. |
| `read_file` | Read a file, or a line range of it. |
| `list_tree` | List a directory. |
| `find_symbol_definitions` | Where an exact identifier is defined, from ctags. |
| `find_symbol_references` | Where an exact identifier is used, excluding definitions. This is a text search, so treat results as leads: it can't tell shadowed names, comments or strings apart. |
| `search_commits` | Filter one repo's history by author, date, message, path, or a change to a string or regex (pickaxe). |
| `get_diff` | Show one commit, or compare two revisions, with truncation sized for an agent's context. |
| `blame` | Attribute lines to the commit that introduced them, at the indexed commit by default. |

## The shared server

Every session shares one background server per index, so each extra agent session costs about 3 MB instead of a full copy of the searcher. 17 sessions measured about 127 MB, down from about 1 GB when each loaded its own.

`muninn mcp` starts the server on demand and hands its stdio over to it through `muninn-mcp-proxy`, a small standard-library-only binary that ships alongside `muninn`. Without the proxy next to `muninn`, sessions still share the server but each costs about 30 MB.

- **Lifetime.** The server exits 5 minutes after its last session ends.
- **Logs.** The server logs to `mcp.log` in the state directory (`~/.local/state/muninn/` by default).
- **Fallback.** If the server can't be reached within 5 seconds, the session is served in-process, and says why on stderr.

## Debugging

`muninn mcp --in-process` always serves in-process, which rules the shared server out when something goes wrong.

`pkill -f 'muninn mcp --daemon'` stops a running server. That ends every open session with an error, and the next session starts a fresh server.
