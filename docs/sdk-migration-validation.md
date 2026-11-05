# Public SDK migration validation

Validated on 2026-11-05, starting from default branch commit
`d58fa01bc101d92ea409dd41999ab6f06200cefe`. No repository `AGENTS.md` exists.
The parent repository's `create-kandev-plugin` workflow, public authoring guide,
and approved template migration handoff informed this change.

The [public SDK release](https://github.com/kdlbs/kandev/releases/tag/pluginsdk/v0.1.0)
resolves `github.com/kdlbs/kandev/pluginsdk v0.1.0` from source commit
`f0e2e774cfe2df8a0d15e4c21dea7b48580e38fb`. Its installed
`sdk-release.json` declares `sdk_version: 0.1.0`, `host_release: 0.99.0`,
`min_kandev_version: 0.99.0`, and `api_versions: [1, 2]`.

Only Go files consume the SDK. There are no generated protocol imports,
`package.json`, frontend SDK dependencies, or npm/pnpm lockfiles in this
repository. The committed plain JavaScript bundle is unchanged. No npm install
or nonexistent frontend typecheck command is claimed. Plugin API v1, version
0.4.1, capabilities, webhook, settings, and all five platform declarations are
preserved. New packages inherit the published SDK's host floor; old installed
binaries are unaffected by source import changes.

## Repository checks

Environment: Linux amd64, Go 1.26.6, Node 24.19.0.

```sh
go mod tidy
make check-format vet test build
make sdk-check
```

Passed. `make test` runs `go test ./server/...`, all 26 JavaScript behavior tests,
`node --check ui/bundle.js`, and package/release verifier tests. Compatibility
output: `SDK 0.1.0: compatible (minimum Kandev 0.99.0)`.

Before the migration, `go test ./server/...` failed because the old replacement
pointed to missing `../kandev/apps/backend`. The package and release verifiers
also rejected current packaging output. New normalized-YAML regression cases
failed with `unexpected plugin id: missing` and `package id missing` before the
fix; they then passed with existing negative cases. Host-only packages accept
the exact current platform or the prior full-platform declaration shape. Full
packages still require all five platforms.

## Tooling boundary

`.kandev-pack-ref` pins backend-owned packaging tooling at
`f0e2e774cfe2df8a0d15e4c21dea7b48580e38fb`, independently of the public SDK pin.
CI test/vet jobs no longer check out Kandev. Packaging/release workflows retain
`apps/backend` and root `pluginsdk/` in their tooling checkout because the
backend itself uses that local module replacement. The plugin module has no
replacement or backend tooling dependency. Packaging still requires this
explicit input and is not checkout-free.

A private tooling checkout was created outside the plugin repository:

```sh
git clone --filter=blob:none --no-checkout https://github.com/kdlbs/kandev.git /tmp/session-cost-sdk-migration/tooling
git -C /tmp/session-cost-sdk-migration/tooling sparse-checkout set apps/backend pluginsdk
git -C /tmp/session-cost-sdk-migration/tooling checkout --detach f0e2e774cfe2df8a0d15e4c21dea7b48580e38fb
make verify-package-host KANDEV_PACK_ROOT=/tmp/session-cost-sdk-migration/tooling
CGO_ENABLED=0 make verify-package KANDEV_PACK_ROOT=/tmp/session-cost-sdk-migration/tooling
sh scripts/verify-release-version.sh v0.4.1 kandev-session-cost-0.4.1.tar.gz
```

The initial full-package attempt without `CGO_ENABLED=0` failed while compiling
arm64 `runtime/cgo` with the host assembler. This plugin does not need CGO;
portable cross-build verification explicitly disables it. Only Linux amd64 is
executed; the other platform binaries are compile/package checks.

## Standalone checkout

A clean clone at `/tmp/session-cost-sdk-migration/standalone/plugin` has no
`../kandev` sibling and no Go workspace. It initially contained implementation
commit `43e9045`, then fast-forwarded to `77bbb03` (preserving the default Make
build target) before package checks. Public dependency resolution used an empty
module cache, the public Go proxy, and checksum database:

```sh
git clone --no-hardlinks . /tmp/session-cost-sdk-migration/standalone/plugin
cd /tmp/session-cost-sdk-migration/standalone/plugin
export GOWORK=off
export GOPROXY=https://proxy.golang.org
export GOSUMDB=sum.golang.org
export GOMODCACHE=/tmp/session-cost-sdk-migration/clean-modcache
test ! -e ../kandev
go mod download
go mod tidy
git diff --exit-code -- go.mod go.sum
go mod verify
go list -m all
make check-format vet test sdk-check build
git status --porcelain
```

Passed, with no module-file changes and a clean worktree. `go mod verify`
reported `all modules verified`; the graph selects
`github.com/kdlbs/kandev/pluginsdk v0.1.0` with no backend module or replacement.
The full module graph and command logs are retained under the task-owned
`/tmp/session-cost-sdk-migration/` directory. Checks use actual repository
commands, including all 26 UI tests; there is no recipe subproject.
