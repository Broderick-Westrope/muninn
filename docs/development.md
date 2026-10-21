# Development

```bash
go build ./...
go test ./...
```

`go run . <command>` runs a local build. It won't take over the launchd schedule: `muninn sync` only reinstalls the agent when the binary the plist names has gone missing.

## The web UI

The UI in `internal/web/static` is plain ES modules and CSS, embedded into the binary with `go:embed`. There is no build step and no `package.json`, and the browser loads the modules directly. The pieces with real logic (palette matching, the lookup lifecycle, selection and Enter handling, line-range parsing) live in DOM-free modules so they can be tested without a browser.

Their tests are in `internal/web/static/_test/*.test.mjs`, which `go:embed` skips, so they are never served. `go test ./internal/web/` runs them through `node --test` when `node` 20.19+ or 22.7+ is on your `PATH`, and skips them otherwise. Those versions are the first that load `.js` files written as ES modules without a `package.json` `"type"` field. To run them directly:

```bash
node --test internal/web/static/_test/*.test.mjs
```

No CI workflow runs the tests, so a skip means they didn't run.

To try UI changes, rebuild and run `muninn web`. Embedded assets are served with `Cache-Control: no-cache`, so a reload picks up the new build.

## Releases

Pushing a `v*` tag runs `.github/workflows/release.yaml`, which uses GoReleaser to:

- build `muninn` and `muninn-mcp-proxy` for macOS on arm64 and amd64
- publish a GitHub release with a changelog that leaves out `docs:` and `test:` commits
- update the formula in [Broderick-Westrope/homebrew-tap](https://github.com/Broderick-Westrope/homebrew-tap)

```bash
git tag vX.Y.Z
git push origin vX.Y.Z
```

Tags follow semver: bump the minor version for new features and the patch version for fixes. The workflow needs a `GH_PAT` secret with write access to this repo and the tap, since the default token can't push to another repo.
