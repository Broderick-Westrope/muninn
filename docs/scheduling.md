# Scheduling and freshness

`muninn sync --install` installs a launchd agent that keeps the index fresh. `muninn sync --uninstall` removes it, and `muninn status` shows whether it's loaded and when it last succeeded.

## When a sync runs

The agent wakes every `sync.retryIntervalMinutes` and runs `muninn sync --if-due --no-repair`. Most wakes exit immediately; the check decides whether there is work:

| Index state | What happens |
|---|---|
| Fresh (a success within `sync.intervalMinutes`, default 60) | Exit. |
| Stale | Sync. |
| Stale, and the last run failed or was interrupted | Sync on every wake, every `sync.retryIntervalMinutes` (default 10). |

The quick retry means an expired token or a wake with no network costs minutes, not an hour. The cadence lives in the config rather than the plist, so changing it doesn't need a reinstall.

## What counts as a failure

Only a total failure switches the scheduler to the retry cadence: bad credentials, missing ctags, a discovery that returned no repos, or a run that died mid-flight. A run that fetched everything it could reach counts as a success even if one repo failed. Without that distinction, one permanently broken repo would make muninn refetch every repo every few minutes, forever.

A broken sync is never recorded as a fresh index. A discovery that returns nothing, a clock that moved backwards and a run that died mid-flight are all reported rather than saved as success.

## Per-repo freshness

Staleness is tracked per repo, from that repo's last successful sync, so one unreachable repo doesn't age the rest of the index.

- `read_file`, `list_tree` and the history tools warn when the repo you touched is behind.
- `list_repos` shows every repo's age.
- `muninn status` shows the full picture, including per-repo failures.

## One sync at a time

Runs are serialized by an advisory lock on `~/.local/state/muninn/sync.lock`. A run that finds the lock held exits without doing anything, so two syncs never touch the mirrors or the index at once. On a filesystem without `flock`, the run proceeds with a warning instead of aborting.

## Surviving upgrades

The plist records the binary path as installed (for example `/opt/homebrew/bin/muninn`) instead of resolving symlinks. A Homebrew upgrade repoints that symlink, so launchd keeps running the new version rather than a deleted versioned path.

If the binary the plist names goes missing anyway, the next interactive `muninn sync` reinstalls the agent. It only does this when the binary is missing, so a `go run` or a scratch build can't take over the schedule. `muninn status` reports the agent's real state by asking launchd whether the job is loaded, rather than trusting the plist on disk.
