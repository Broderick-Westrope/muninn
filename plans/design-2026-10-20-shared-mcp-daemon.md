# Muninn Design: One Shared MCP Server for Every Client

**Status:** Design, not yet planned for implementation. Numbers marked
"measured" were taken on 2026-10-20 on one machine (316 repos, 875 MB
index, muninn from Homebrew, macOS `footprint`); everything else is
projected.

## TL;DR

Every agent session runs its own `muninn mcp`, and 17 of them cost about
1 GB. Fix it in three phases that each ship on their own, and stop when
the measured footprint is good enough:

1. Give `muninn mcp` a default soft memory limit (about 1 GB to 765 MB).
2. One daemon per user and index, reached over a Unix socket, with
   `muninn mcp` as a forwarder (to about 500 MB).
3. Move the forwarder into a small standard-library-only binary that
   `muninn mcp` execs into (to about 110 MB).

Client configs never change. Phase 3 exists because any process that
links Zoekt pays about 26 MB at package init before `main` runs, so a
forwarder inside `muninn` can't be small.

## Problem

Every MCP client session starts its own `muninn mcp`, and each one opens
its own Zoekt searcher over the whole index. On 2026-10-20 one machine
had 17 of them, one per Anvil instance, at 55–61 MB each. That's about
1 GB of duplicated read-only state, and at a fixed index size it grows
with the number of agent sessions. Anvil's ADR 0005 (cross-instance
sharing) decided that muninn's sharing belongs in muninn so every MCP
client benefits, which rules out solving it inside Anvil.

### What one process costs

Measured scenarios, each a separate run, so the rows are not an additive
breakdown:

| Process | Footprint | Live heap after GC |
| --- | --- | --- |
| `muninn mcp`, idle after `initialize` | 61 MB | |
| `muninn mcp` after two `grep` calls | 71 MB | |
| `muninn mcp` with `GOMEMLIMIT=32MiB`, after two `grep` calls | 45 MB | |
| Searcher opened over all 316 shards, one search run | | 22 MB |
| Go binary that only imports the `zoekt` root package | 26 MB | 12–13 MB |
| Go binary that only imports `internal/cli` | 26–30 MB | 13 MB |
| Standard-library-only Go binary copying stdin to a Unix socket | 3 MB | |

Shard pages are file-backed and clean, so the OS already shares them and
they don't appear in footprint.

The package-init cost can't be avoided from inside muninn: the `zoekt`
root package imports `query`, which imports `languages` and with it
go-enry's tables, and `search` also reaches go-enry through `index`. Go
runs every linked package's `init` before `main`, so an early branch in
`main` doesn't help.

### Projected totals for 17 sessions

| After | Per session | Shared | Total |
| --- | --- | --- | --- |
| Today | 61 MB | | ~1,040 MB |
| Phase 1 (memory limit) | ~45 MB | | ~765 MB |
| Phase 2 (daemon, forwarder in `muninn`) | ~26 MB | ~61 MB | ~500 MB |
| Phase 3 (forwarder in its own binary) | ~3 MB | ~61 MB | ~110 MB |

The target is that muninn's total footprint stops depending on how many
sessions are open, which only phase 3 delivers. Phases 1 and 2 are worth
shipping on their way there, and if the owner decides 500 MB is fine,
the work stops after phase 2.

## Goals

- Many MCP clients share one searcher.
- `{"command": "muninn", "args": ["mcp"]}` keeps working unchanged in
  every stdio client (Anvil, Claude Code, Cursor).
- If the daemon can't be used, a session behaves exactly as it does
  today.

## Non-Goals

- Changing sync, indexing, or tool output.
- MCP over HTTP. No current client needs it, and on loopback it needs
  authentication that a Unix socket in a `0700` directory gets from the
  OS. It can be added to the same daemon later if a client appears that
  can't spawn a process.
- Windows or Linux service integration, and remote access.
- Fixing the pre-existing risks listed under [Risks](#risks), beyond
  making them visible.

## Alternatives Considered

| Option | Why not (or not alone) |
| --- | --- |
| GC tuning only | Kept as phase 1, but the 22 MB live heap per process is a floor it can't go below |
| Open the searcher lazily on first search | Saves the searcher only for sessions that never search, does nothing about init cost, and stops saving after the first search unless eviction is added; sessions that never search haven't been shown to be common |
| Share inside the client (one muninn per Anvil install) | Helps only that client; ADR 0005 put sharing in muninn |
| Configure clients to run the proxy directly | Allowed as an optional fast path once phase 3 exists, but can't be the default without changing every client config |

## Phase 1: Default Memory Limit

`muninn mcp` calls `debug.SetMemoryLimit` at startup unless the user has
set `GOMEMLIMIT`. The value needs a short benchmark (idle footprint, a
burst of representative tool calls, GC CPU, and latency) at 32, 48, and
64 MiB. It applies only to `muninn mcp`, never to `muninn sync`, whose
indexing legitimately needs more. The limit is soft: live data can still
exceed it, and it says nothing about mmap'd shards, so it's a GC hint
and not a ceiling. Phase 2's daemon reuses it.

## Phase 2: Shared Daemon

```mermaid
sequenceDiagram
    participant C as MCP client
    participant F as muninn mcp (forwarder)
    participant S as muninn mcp --daemon-start
    participant D as daemon
    C->>F: spawn, stdio pipes
    F->>F: validate git and config, resolve identity
    F->>D: dial socket, send admission request
    alt no daemon or draining
        F->>S: run and wait
        S->>D: start detached, then exit
        F->>D: retry admission for up to 2 s
    end
    alt admitted
        C-->>D: bytes copied both ways
    else still not admitted
        F->>F: serve in-process (today's behaviour)
    end
```

### Forwarder

`muninn mcp` does what it does today up to the searcher: it validates
`git` and the config, so misconfiguration still fails fast in the client.
Then it resolves the session's identity, asks a daemon to admit it, and
on success copies stdin to the socket and the socket to stdout. The
forwarding code lives in its own package that imports only the standard
library, so phase 3 can lift it into a separate binary unchanged.

**Fallback boundary.** The forwarder doesn't read a byte of stdin until
it has been admitted. Until then, any failure (no socket, daemon never
started, admission refused, socket path too long, locking unsupported)
means it serves in-process exactly as today. After admission it never
falls back: if the daemon dies mid-session the forwarder exits non-zero,
and the client sees the server die just as it would today if
`muninn mcp` crashed. Replaying `initialize` against a new daemon can't
restore in-flight requests, so it isn't worth the complexity.

### Admission

A successful dial isn't proof of a live daemon: a connection can be
accepted, or still be queued in the listen backlog, just as an idle
daemon decides to exit. So the first thing on every connection is a
one-line exchange in muninn's own format, before any MCP bytes:

- the forwarder sends its identity;
- the daemon, holding the same mutex that guards its idle-to-draining
  transition, replies `ok` and counts the session, or replies
  `draining` (or closes) if it is shutting down or the identity doesn't
  match its own.

Anything other than `ok` within 500 ms counts as not admitted, and the
forwarder retries or falls back. Admission, not dial, is the
point of no return.

### Identity

Today each `muninn mcp` resolves its own paths from its own environment.
A shared daemon keeps the environment of whichever session started it,
so only sessions that would have resolved the same things may share
one. The identity is a hash of:

- the real path of the `muninn` binary (symlinks resolved once, in the
  forwarder), plus its size and mtime;
- the absolute forms of `xdg.IndexDir()`, `xdg.StatusPath()`, and
  `xdg.MirrorsDir()`. The XDG helpers accept relative overrides, so
  they're made absolute against the forwarder's working directory before
  hashing, and passed to the daemon as absolute paths rather than
  re-resolved from its environment.

The path, size, and mtime are a heuristic: they change on `brew upgrade`
(new Cellar path) and on normal rebuilds, but a metadata-preserving
replacement could keep them equal. That is accepted for a personal tool.
After an upgrade, old daemons keep serving their existing sessions and
exit when idle. Like today's long-lived `muninn mcp` processes, an old
daemon may skip shards written in a newer format by a sync that runs in
the meantime.

Config isn't part of the identity, because the MCP server doesn't use
it; the forwarder validates it so errors still surface per session.
`git` is resolved from the daemon's `PATH`. That's accepted: muninn
needs a working `git`, not a particular one.

The socket and lock live at `xdg.StateDir()/mcp-<hash>.sock` and
`.lock`, with a 16-character hash. If the socket path exceeds macOS's
104-byte `sun_path` limit, the forwarder serves in-process.

### Daemon lifecycle

1. Take `mcp-<hash>.lock` with `internal/lock`, retrying for up to 1 s
   (an exiting daemon may still hold it). If it's held, exit 0: another
   daemon owns this identity. If locking is unsupported, exit non-zero;
   the forwarder's admission then fails and it serves in-process.
2. Holding the lock, remove any stale socket file and listen.
3. Register all tools, then accept connections. Tool-list changes are
   broadcast to every session, so the tool set must be fixed before the
   first one connects.
4. Serve each admitted connection as its own session on one shared
   `sdk.Server`.
5. After 5 minutes with no sessions, enter draining under the admission
   mutex, close the listener, remove the socket file, and exit. The lock
   file is never removed; the kernel releases the lock with the process.

Holding the lock for the daemon's life is what makes the socket file
trustworthy: lock free means no live daemon, so a leftover socket is
stale and safe to delete. `internal/lock` opens the file close-on-exec,
so `git` subprocesses don't inherit the lock. Forwarders never touch the
lock; several racing forwarders each start a daemon, one wins, the rest
exit within a second, and every forwarder's admission retry reaches the
winner.

**Detaching.** The forwarder runs `muninn mcp --daemon-start` and waits
for it. That process starts the real daemon in a new session (`Setsid`),
with stdin and stdout on `/dev/null`, stderr appended to
`xdg.StateDir()/mcp.log`, and working directory `/`, then exits at once.
The daemon is then reparented to launchd, so it isn't a descendant of
any client: a client that kills its process group or walks its process
tree on exit can't take down a daemon other sessions depend on, and a
client waiting for EOF on the server's stdout can't hang on a pipe the
daemon holds. App Sandbox and TCC behaviour for clients that run sandboxed
is unverified.

### Session end

Today a client closing stdin doesn't abort the server: the SDK stops
reading, finishes in-flight calls, writes their responses, and then
`Run` returns. The daemon keeps that behaviour.

- **Client closes stdin.** The forwarder half-closes the socket
  (`CloseWrite`) and keeps copying responses to stdout until the daemon
  closes the connection. The daemon's session sees EOF, finishes in-flight
  calls as today, and then closes the connection.
- **Client or forwarder dies.** The daemon sees the same EOF, and its
  next write fails; the SDK cancels every in-flight call on a failed
  write. Work that runs before that write is already bounded by the
  per-operation `git` deadlines (`internal/githistory/githistory.go:17`).
- **Client stops reading stdout.** Every write to a session's connection
  has a 30 s deadline; when one expires the daemon closes that
  connection, which fails later writes and cancels its calls. Without
  this, one stuck client would pin its handler goroutines forever.
- **Malformed input.** A decode error ends the session in the SDK; the
  daemon closes the connection whenever a session ends for any reason.

### What one process serving many sessions changes

- **Panics.** Neither the go-sdk nor `textHandler`
  (`internal/mcp/server.go:178`) recovers panics in tool handlers. Today
  a panic kills one session; in a daemon it would kill all of them.
  `textHandler` must recover and turn a panic into a tool error.
- **History calls.** `search_commits` with `changed_regex` and similar
  calls run `git` per call. A semaphore around `gitcmd`, sized to
  `GOMAXPROCS` and acquired with the call's context so a cancelled call
  stops waiting, stops 17 sessions starting 17 pickaxe scans at once.
  Zoekt's searcher is built for concurrent queries and needs no limit.
- **Queues.** The SDK queues each session's incoming calls without a
  bound. That's per session, as today, and isn't changed here.
- **Logging.** The daemon logs admissions, session ends, panics, and
  Zoekt's shard-load errors to `mcp.log`. Zoekt's informational shard
  loading lines stay discarded.

## Phase 3: Forwarder Binary

`muninn-mcp-proxy` is the phase 2 forwarder package with a `main`. It is
built by goreleaser alongside `muninn` and installed next to it by the
same Homebrew formula.

`muninn mcp` still validates `git` and the config and resolves the
identity, then `syscall.Exec`s the proxy found next to its own resolved
executable. It passes everything explicitly: socket path, identity, the
resolved `muninn` path (used to run `--daemon-start`), and the original
arguments (so `--config` survives). The proxy resolves nothing itself. Exec keeps the pid and the inherited stdio,
nothing has been read from stdin, and the 26 MB of init heap disappears
with the old process image.

When the proxy isn't admitted, it execs back into
`<resolved muninn> mcp --in-process <original arguments>`. The
`--in-process` flag stops that from looping. If the proxy binary is
missing (a `go install` build, say), `muninn mcp` behaves as in phase 2.

## Risks

| Risk | Status |
| --- | --- |
| One crash or out-of-memory kill drops every session at once | Accepted; panic recovery covers the common case |
| A background process outlives the session that started it, for up to 5 idle minutes | Accepted; `README.md:120` ("Nothing stays resident between sessions") needs rewording |
| A shard that fails to load during a sync is never retried: Zoekt records the new file identity before loading it (`search/watcher.go`, `scan`), so the searcher keeps the old shard until the file changes again | Pre-existing in every long-lived `muninn mcp`; a daemon serves every session from the one stale searcher. Logged to `mcp.log`; fixing it is out of scope |
| During a sync, search results and `read_file` can briefly disagree, because the status file and the searcher's shard watcher update independently | Pre-existing and unchanged |
| Exec handoff only works on Unix | Fine while scheduling is macOS-only |

## Testing

- Daemon and forwarder in one test with a temp `XDG_STATE_HOME`: two
  sessions run tool calls concurrently and both get correct results.
- A tool call in flight when the client closes stdin still gets its
  response.
- A stale socket with no lock holder is removed and replaced.
- Two daemons started at once: exactly one serves, and every forwarder
  is admitted by it.
- Idle shutdown racing a connection, including one still in the listen
  backlog: the forwarder is either admitted and served, or refused and
  retries; never admitted and then dropped.
- Daemon killed mid-session: the forwarder exits non-zero and doesn't
  fall back.
- No daemon possible (locking unsupported, path too long): `muninn mcp`
  serves in-process and the existing MCP tests pass through it.
- A handler panic returns a tool error and other sessions keep working.
- A client that stops reading has its connection closed after the write
  deadline, and other sessions are unaffected.
- Phase 3: `--config` and the other original arguments survive both
  exec hops.

## Success Measure

After each phase, open the same 17 sessions, run the same burst of tool
calls, and record `footprint` for every muninn process, idle and during
the burst, against the projections above. Phase 3's target is under
150 MB total. Report measured and projected numbers separately.

## Open Questions

- **Budget.** Is about 500 MB after phase 2 acceptable, or is phase 3's
  near-flat cost per session wanted? This decides whether the second
  binary gets built.
- **Idle timeout.** 5 minutes is a guess. Opening the searcher took
  68 ms warm (measured), which suggests a short timeout costs little,
  but a cold daemon start (process start, page cache cold, first
  `ctags` or mirror access) hasn't been measured.
- **Proxy discovery.** `muninn-mcp-proxy` next to the real path of
  `muninn` works for Homebrew and goreleaser archives. Is there any
  install path where the two could end up in different directories?
