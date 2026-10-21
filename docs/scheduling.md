# Scheduling and freshness

`muninn sync --install` installs a launchd agent that keeps the index fresh. `muninn sync --uninstall` removes it, and `muninn status` shows whether it's loaded and when it last succeeded.

## When a sync runs

The agent wakes every `sync.retryIntervalMinutes` and runs `muninn sync --if-due --no-repair`. Most wakes exit immediately; the check decides whether there is work, timing from when the last run finished:

| Last run | Next sync is due |
|---|---|
| Worked | `sync.intervalMinutes` later (default 60) |
| Broke or was interrupted | `sync.retryIntervalMinutes` later (default 10) |

The quick retry means an expired token or a wake with no network costs minutes, not an hour.

Each run reads both values from the config, but the wake interval is written into the plist at install time. Raising either value works straight away. Lowering one needs `muninn sync --install` again, or launchd keeps waking at the old, slower rate.

## What counts as broken

Only a total failure puts the scheduler on the retry cadence: bad credentials, missing ctags, a discovery that returned no repos, or a run that died mid-flight. A run where some repos failed still shows as failed in `muninn status`, with the failures listed, but for scheduling it counts as having worked, since it fetched everything it could reach. Without that distinction, one permanently broken repo would make muninn refetch every repo every few minutes, forever.

A broken sync is never recorded as a fresh index. A discovery that returns nothing, a clock that moved backwards and a run that died mid-flight are all reported rather than saved as success.

## Per-repo freshness

Staleness is tracked per repo, from that repo's last successful sync, so one unreachable repo doesn't age the rest of the index.

- `read_file`, `list_tree` and the history tools warn when the repo you touched is behind.
- `list_repos` shows every repo's age.
- `muninn status` shows the last run, its per-repo failures, and every repo with no successful sync in 24 hours.

## One sync at a time

Runs are serialized by an advisory lock on `~/.local/state/muninn/sync.lock`. A run that finds the lock held exits without doing anything, so two syncs never touch the mirrors or the index at once. On a filesystem without `flock`, the run proceeds with a warning instead of aborting.

## Surviving upgrades

The plist records the binary path as installed (for example `/opt/homebrew/bin/muninn`) instead of resolving symlinks. A Homebrew upgrade repoints that symlink, so launchd keeps running the new version rather than a deleted versioned path.

If the binary the plist names goes missing anyway, the next interactive `muninn sync` reinstalls the agent. It only does this when the binary is missing, so a `go run` or a scratch build can't take over the schedule. `muninn status` reports the agent's real state by asking launchd whether the job is loaded, rather than trusting the plist on disk.
