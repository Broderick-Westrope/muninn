# Muninn Design Idea: One Shared MCP Server for Every Client

**Status:** Idea, captured for later. Not planned or measured beyond the
numbers below.

**Problem:** Every MCP client session starts its own `muninn mcp`, and each
one opens its own Zoekt searcher over the whole index. On 2026-10-20 one
machine had 17 of them, one per Anvil instance, each with a physical
footprint of 55–60 MB (RSS 55–142 MB; the larger figures include about
88 MB of memory-mapped shard pages, which the OS already shares). That's
about 1 GB for 17 identical copies of the same read-only state, and it
grows with the number of agent sessions, not with the size of the index.
Anvil's ADR 0005 (cross-instance sharing) decided that muninn's sharing
belongs in muninn, so every MCP client benefits, not only Anvil.

**Goal:** Many MCP clients share one searcher. Client configs don't
change (`{"command": "muninn", "args": ["mcp"]}` keeps working), and
nothing about sync, the index, or tool behaviour changes.

## Why It Fits

The code is already close:

- `mcp.Server` holds no per-session state. Every handler re-reads the
  status file per call, and `search.Searcher` watches the index directory,
  so one long-lived process already picks up a concurrent sync.
- The go-sdk serves many sessions from one `sdk.Server`: `Server.Connect`
  takes any transport, including a `net.Conn` wrapped in an `IOTransport`,
  and `NewStreamableHTTPHandler` serves MCP over HTTP.
- Sync runs separately under launchd and takes its own lock, so a
  long-lived reader doesn't change who writes the index.

## Options

| Option | How clients reach it | Lifecycle | Verdict |
| --- | --- | --- | --- |
| **A. Forwarder plus on-demand daemon** (gopls `-remote=auto` style) | `muninn mcp` stays the stdio entry point; it connects to a daemon over a Unix socket and copies bytes both ways | Started by the first forwarder, exits after an idle period with no connections | **Preferred** |
| B. MCP over HTTP (`muninn serve`) | Clients configure a URL | Someone has to keep it running: a launchd agent, or the user | Later, as an extra endpoint on the same daemon |
| C. Keep one process per session | As today | As today | What this replaces |

A keeps today's config and works for every stdio client (Anvil, Claude
Code, Cursor). B is simpler to build but moves lifecycle onto the user,
and HTTP on loopback needs authentication that a Unix socket in a `0700`
directory gets from the OS (muninn is macOS-only for scheduling, so Unix
permissions are the access control that matters).

## Shape of Option A

- **Daemon:** a hidden `muninn mcp --daemon` (name to be decided) that
  opens the searcher once, listens on a Unix socket under
  `xdg.StateDir()`, and runs one MCP session per connection against one
  shared `sdk.Server`. It exits after N minutes with no connections, which
  keeps the README's "nothing stays resident between sessions" true in
  spirit; that line needs rewording.
- **Forwarder:** `muninn mcp` connects to the socket, starting the daemon
  if nobody answers (under `internal/lock` so two forwarders don't both
  start one), then pipes stdin/stdout to the connection. It never opens
  the index, so it should stay small (target under 10 MB; not measured).
- **Fallback:** if the daemon can't be started or reached before any MCP
  bytes are sent, the forwarder serves in-process exactly as today. After
  bytes have flowed it does not switch over, because the session's
  `initialize` has already happened on the daemon.
- **Version identity:** the socket name includes muninn's version and a
  build identity, so after `brew upgrade` new forwarders start a new
  daemon and the old one drains and exits idle. Same-version only means
  no wire or index-format compatibility to maintain.

## Open Questions

- **Daemon crash:** every connected session drops at once. Should the
  forwarder exit (the client sees the MCP server die, as today when a
  `muninn mcp` crashes), or reconnect and replay `initialize`? Exiting is
  simpler and honest; replay is invisible but fragile.
- **Idle timeout:** long enough that agent sessions opening and closing
  don't restart the daemon constantly (shard loading time decides this;
  not measured).
- **Concurrency:** one searcher serving 17 sessions. Zoekt's searcher is
  built for concurrent queries, but heavy `-G` pickaxe history calls run
  `git` per call and may need a limit.
- **Logging:** the daemon has no stderr for a client to show; it should
  log to `xdg.LogPath()`'s directory.
- **Is HTTP (option B) wanted at all**, or does the forwarder cover every
  real client?

## Success Measure

With 17 clients, total footprint is one daemon (about 60 MB, plus
whatever concurrency adds) and 17 forwarders, against about 1 GB today.
Measure before and after with `footprint` on the same set of sessions;
the saving is projected until then.

## Out

- Changing sync, indexing, or tool output.
- Windows or Linux service integration.
- Remote (non-loopback) access.
