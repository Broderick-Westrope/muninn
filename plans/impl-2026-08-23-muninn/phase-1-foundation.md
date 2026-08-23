# Phase 1: Foundation

> **Status:** COMPLETED
> **Spec:** `plans/design-2026-08-23-muninn.md`

Delivers the module scaffold and the primitives every other phase builds on: config loading (Sourcebot-compatible subset), XDG path resolution, the sync status file with atomic writes, and universal-ctags detection/validation.

## Specification (excerpt)

**Goal:** `go build ./...` produces a `muninn` binary with subcommand skeleton; `internal/` packages for config, paths, status, and ctags exist with full test coverage.

**Success Criteria:**

- [ ] `muninn` binary builds and prints help for `sync`, `status`, `mcp`, `search`, `web` subcommands (stubs OK)
- [ ] Config loader accepts a trivially-migrated copy of the user's existing Sourcebot `config.json`
- [ ] Status file survives concurrent read-during-write (atomic rename)
- [ ] ctags validation rejects BSD ctags and universal-ctags without `+interactive`

## Context Loading

_Run before starting:_

```bash
read /Users/broderick.westrope/dev/helse/muninn/plans/design-2026-08-23-muninn.md
read /Users/broderick.westrope/dev/helse/sourcebot/config.json   # schema to stay compatible with
```

## Scaffold Tasks

### Task 1: Module scaffold and CLI skeleton

**Files:**
- Create: `go.mod` (module `github.com/broderick-westrope/muninn`, Go 1.24+)
- Create: `main.go`
- Create: `internal/cli/root.go` (+ one file per subcommand)
- Create: `.gitignore`, `README.md` (one-paragraph description + install/usage stub)

**Steps:**

1. [ ] `go mod init github.com/broderick-westrope/muninn`
2. [ ] Add `github.com/spf13/cobra` and wire a root command with subcommands `sync`, `status`, `mcp`, `search`, `web` — each returning "not implemented" for now. `--config` global flag overrides the default config path.
3. [ ] `main.go` calls `cli.Execute()` and exits non-zero on error.

**Verify:**
```bash
cd /Users/broderick.westrope/dev/helse/muninn && go build ./... && go run . --help
# Expected: help output listing sync, status, mcp, search, web
```

### Task 2: XDG paths package

**Files:**
- Create: `internal/xdg/xdg.go`, `internal/xdg/xdg_test.go`

**Steps:**

1. [ ] Implement `ConfigPath() string` → `$XDG_CONFIG_HOME/muninn/config.json` (default `~/.config/muninn/config.json`); `DataDir() string` → `$XDG_DATA_HOME/muninn` (default `~/.local/share/muninn`); `StateDir() string` → `$XDG_STATE_HOME/muninn` (default `~/.local/state/muninn`).
2. [ ] Derived helpers: `MirrorsDir()` = `DataDir()/mirrors`, `IndexDir()` = `DataDir()/index`, `StatusPath()` = `StateDir()/status.json`, `LogPath()` = `StateDir()/sync.log`.
3. [ ] `EnsureDirs() error` creates all with `0700`.
4. [ ] Tests: env-var overrides respected; defaults expand `$HOME`.

**Verify:**
```bash
go test ./internal/xdg/
# Expected: PASS
```

## Config Tasks

### Task 3: Config package (Sourcebot-compatible subset)

**Context:** `/Users/broderick.westrope/dev/helse/sourcebot/config.json`

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`, `internal/config/testdata/sourcebot.json` (copy of user's config minus the `models` block and with a fake token env)

**Steps:**

1. [ ] Define types mirroring the Sourcebot v3 subset:

```go
type Config struct {
    Schema      string                `json:"$schema,omitempty"`
    Connections map[string]Connection `json:"connections"`
    // Muninn-specific, written by `sync --install` / first run:
    Auth  AuthConfig  `json:"auth,omitempty"`
    Ctags CtagsConfig `json:"ctags,omitempty"`
    Sync  SyncConfig  `json:"sync,omitempty"`
}

type Connection struct {
    Type    string   `json:"type"` // "github" only in v1
    Orgs    []string `json:"orgs,omitempty"`
    Repos   []string `json:"repos,omitempty"` // ad-hoc "owner/name" additions
    Token   *TokenRef `json:"token,omitempty"`
    Exclude *Exclude  `json:"exclude,omitempty"`
}

type TokenRef struct{ Env string `json:"env,omitempty"` }
type Exclude struct {
    Archived bool     `json:"archived,omitempty"`
    Repos    []string `json:"repos,omitempty"`
}
type AuthConfig struct{ GitHubToken string `json:"githubToken,omitempty"` }        // baked at install; 0600 file perms enforced
type CtagsConfig struct{ Path string `json:"path,omitempty"` }                     // absolute path baked at install
type SyncConfig struct{ IntervalMinutes int `json:"intervalMinutes,omitempty"` }   // default 60
```

2. [ ] `Load(path string) (*Config, error)`: parse, validate (`type` must be `github`; at least one org or repo), apply defaults. Unknown top-level fields (e.g. Sourcebot's `models`) are ignored, not errors.
3. [ ] `ResolveToken(c *Config) (string, error)` precedence: `auth.githubToken` → `token.env` env var → `gh auth token` (exec, trimmed) → error with actionable message.
4. [ ] `Save(path string, c *Config) error` writes with `0600` via temp-file + rename.
5. [ ] Test: `testdata/sourcebot.json` (real schema incl. an ignored `models` block) loads without error; exclusion list parsed; token precedence covered with env manipulation.

**Verify:**
```bash
go test ./internal/config/
# Expected: PASS, including sourcebot.json compatibility test
```

## State Tasks

### Task 4: Status file with atomic writes

**Files:**
- Create: `internal/status/status.go`, `internal/status/status_test.go`

**Steps:**

1. [ ] Types:

```go
type SyncStatus struct {
    StartedAt   time.Time             `json:"startedAt"`
    FinishedAt  time.Time             `json:"finishedAt"`
    Success     bool                  `json:"success"`
    Repos       map[string]RepoStatus `json:"repos"` // key: "owner/name"
}
type RepoStatus struct {
    Fetched       bool   `json:"fetched"`
    Indexed       bool   `json:"indexed"`
    IndexedCommit string `json:"indexedCommit,omitempty"` // full SHA
    Error         string `json:"error,omitempty"`
}
```

2. [ ] `Write(path string, s *SyncStatus) error`: marshal → write `path+".tmp"` → `os.Rename` (atomic; safe against a live MCP reader per spec implementation notes).
3. [ ] `Read(path string) (*SyncStatus, error)`; distinguish not-exist (never synced) from corrupt.
4. [ ] `Age(s *SyncStatus) time.Duration` helper for staleness checks.
5. [ ] Test: write-then-read roundtrip; concurrent reader never sees partial JSON (loop read during repeated writes).

**Verify:**
```bash
go test ./internal/status/ -race
# Expected: PASS
```

### Task 5: ctags detection and validation

**Files:**
- Create: `internal/ctags/ctags.go`, `internal/ctags/ctags_test.go`

**Steps:**

1. [ ] `Resolve(configured string) (string, error)`: if `configured` non-empty validate it directly; else probe, in order: `universal-ctags`, `ctags` on PATH, then `/opt/homebrew/bin/ctags`, `/usr/local/bin/ctags` (launchd's default PATH excludes Homebrew — spec Q2 fix).
2. [ ] `Validate(path string) error`: run `<path> --version` and require output containing `Universal Ctags`; run `<path> --list-features` and require `interactive` (Zoekt's parser requires `+interactive` sandbox/json mode). Return errors that name the failing check and suggest `brew install universal-ctags`.
3. [ ] Tests use fake scripts in a temp dir simulating: BSD ctags (reject), universal-ctags without interactive (reject), valid (accept).

**Verify:**
```bash
go test ./internal/ctags/
# Expected: PASS; on a machine with brew ctags, `go run . status` may later surface the resolved path
```

## Verify Phase

```bash
cd /Users/broderick.westrope/dev/helse/muninn && go vet ./... && go test ./... -race && go build ./...
# Expected: all PASS, binary builds
```

Create PR (or reviewed commit) for human review.
