# Configuration

muninn reads `~/.config/muninn/config.json`, or `$XDG_CONFIG_HOME/muninn/config.json`, or whatever you pass to `--config`.

## Example

```json
{
  "connections": {
    "github": {
      "type": "github",
      "orgs": ["your-org"],
      "repos": ["you/side-project"],
      "exclude": { "archived": true, "repos": ["your-org/skip-me"] }
    }
  },
  "sync": { "intervalMinutes": 60, "retryIntervalMinutes": 10 },
  "editor": { "scheme": "cursor", "roots": ["~/dev"] }
}
```

## Fields

### `connections`

A map of named connections. At least one must list an org or a repo.

| Field | Meaning |
|---|---|
| `type` | Must be `"github"`, the only source muninn supports. |
| `orgs` | GitHub organizations to index every repo of. A personal account isn't an organization: list its repos in `repos` instead. |
| `repos` | Extra repos, as `owner/name`. |
| `token.env` | Optional. An environment variable holding a GitHub token (see [Tokens](#tokens)). |
| `exclude.archived` | Skip archived repos. |
| `exclude.repos` | Repos to skip, as `owner/name`. Matched exactly, including case. |

### `sync`

| Field | Default | Meaning |
|---|---|---|
| `intervalMinutes` | 60 | How long after a sync the next one is due. |
| `retryIntervalMinutes` | 10 | How soon to try again after a failed sync. Capped at `intervalMinutes`. |

Every run reads these to decide whether a sync is due, but how often launchd wakes muninn to check is fixed when you run `muninn sync --install`. Raising either value takes effect straight away; lowering one needs `muninn sync --install` again. [scheduling.md](scheduling.md) explains how the two interact.

### `editor`

| Field | Default | Meaning |
|---|---|---|
| `scheme` | `"cursor"` | Which editor the web UI opens files in: `"cursor"` or `"vscode"`. |
| `roots` | none | Directories to scan, two levels deep, for local checkouts of indexed repos. The web UI only offers "Open in editor" for repos it finds here. `~` is expanded. |

The checkout can be on a different commit from the index, so a file can open at a line that has since moved.

### Written by muninn

`muninn sync --install` records two absolute paths, because launchd's `PATH` doesn't include Homebrew:

| Field | Meaning |
|---|---|
| `ctags.path` | The universal-ctags binary. |
| `auth.ghPath` | The GitHub CLI, if installed. |

To do that, `--install` rewrites the whole file. The rewrite fills in defaults (such as `sync` and `editor.scheme`), expands `~` in `editor.roots` to absolute paths, and drops any field muninn doesn't know. Keep a copy if you share the file with another tool.

`auth.githubToken` also exists, but avoid it (see below). muninn never writes a token into the config.

## Tokens

Each run looks for a GitHub token in this order:

1. `auth.githubToken` in the config
2. the variable named by `token.env`
3. `gh auth token`

Use `gh`. It resolves a fresh token on every run, so scheduled syncs keep working through token rotation and expiry. The other two sources break a launchd schedule in ways that are easy to miss:

- **A token only in your shell environment** is invisible to launchd, so scheduled syncs fail even though `muninn sync` works in your terminal.
- **A token in `auth.githubToken`** takes precedence over `gh` and never rotates, so every sync fails once it expires.

`muninn sync --install` warns about both. If an older muninn wrote a token into your config, delete `auth.githubToken` to get per-run resolution back.

## Coming from Sourcebot

The config is a subset of [Sourcebot](https://github.com/sourcebot-dev/sourcebot)'s v3 schema, so a simple Sourcebot config works as it is. muninn supports:

- `github` connections only; any other `type` fails validation
- `orgs` and `repos`
- `token.env`, but not other token sources
- `exclude.archived` and `exclude.repos` (exact match)

Other Sourcebot fields, such as `models`, `users`, `topics`, `revisions`, `exclude.forks` and `exclude.topics`, are ignored without a warning. An `exclude.forks: true` therefore doesn't stop forks being indexed; list them in `exclude.repos` instead. `muninn sync --install` also drops ignored fields when it rewrites the file (see [Written by muninn](#written-by-muninn)), so keep muninn's config separate from the one Sourcebot uses.
