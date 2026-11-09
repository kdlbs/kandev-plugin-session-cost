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

Standalone host-only and full packaging also passed with `GOWORK=off`,
`CGO_ENABLED=0`, and `KANDEV_PACK_ROOT=/tmp/session-cost-sdk-migration/tooling`:

```sh
make verify-package-host KANDEV_PACK_ROOT=/tmp/session-cost-sdk-migration/tooling
make verify-package KANDEV_PACK_ROOT=/tmp/session-cost-sdk-migration/tooling
sh scripts/verify-release-version.sh v0.4.1 kandev-session-cost-0.4.1.tar.gz
```

The full standalone archive SHA-256 is
`6dcb32b9b8588ee26bf0198dd3080c2a51f34b27a49c606fec1a0874795bba01`.
Its exact inventory is:

```text
manifest.yaml
server/plugin-darwin-amd64
server/plugin-darwin-arm64
server/plugin-linux-amd64
server/plugin-linux-arm64
server/plugin-windows-amd64.exe
ui/bundle.js
checksums.txt
```

All seven internal checksums passed. Semantic inspection with the tooling
checkout's installed YAML parser confirmed that every source manifest value is
preserved in the full packaged manifest, including API v1, the 0.99.0 floor,
permissions, config schema, webhook and platforms. The packaged UI matches
`origin/main:ui/bundle.js` byte for byte. `go version -m` on the packaged Linux
binary selects public `github.com/kdlbs/kandev/pluginsdk v0.1.0`.

## Minimum-host browser smoke

On 2026-11-08, both projects passed against the verified published
[v0.99.0 Linux x64 full runtime](https://github.com/kdlbs/kandev/releases/tag/v0.99.0).
The archive SHA-256 was
`24df61be8750fdf3ef151243f6385a4d14b0e3c84a409b6b11a1ef5fee3237be`;
`sha256sum -c kandev-linux-x64-full.tar.gz.sha256` passed before extraction.
The downloaded archive was preserved. Its extracted disposable runtime gained
only the source-matched `mock-agent` test fixture beside its executables.

The tooling checkout at the same source commit was expanded for test support:

```sh
git -C /tmp/session-cost-sdk-migration/tooling sparse-checkout add apps/web apps/packages scripts
(cd /tmp/session-cost-sdk-migration/tooling/apps && pnpm install --frozen-lockfile)
make -C /tmp/session-cost-sdk-migration/tooling build-web-e2e
make -C /tmp/session-cost-sdk-migration/tooling/apps/backend build-mock-agent
make -C /tmp/session-cost-sdk-migration/tooling build-e2e-plugin-package
make -C /tmp/session-cost-sdk-migration/tooling build-e2e-delivery-fixture
GOMAXPROCS=2 GOGC=25 GOFLAGS=-p=1 make -C /tmp/session-cost-sdk-migration/tooling build-e2e-send-now-fixture
cp /tmp/session-cost-sdk-migration/tooling/apps/backend/bin/mock-agent /tmp/session-cost-sdk-migration/runtime/kandev/bin/mock-agent
```

The first send-now fixture build was killed during compilation; the final
command above completed with reduced compiler parallelism/memory use. The first
browser launch lacked 22 shared libraries on NixOS. These were resolved from
already installed Nix store outputs into the task-local `browser-library-path`
file used below. No host checkout or parent process environment was modified.

The smoke driver initially expected an older host's `quick-chat-start` control.
v0.99.0 uses `quick-chat-send` with an opening prompt. The driver now supports
both controls, preserves the disabled-submit and no-usage-request-before-session
assertions, and records/asserts actual host health before installation.

Exact successful runner (Node 24.19.0, Playwright 1.61.1, pnpm 9.15.9 for the host):

```sh
TASK_TMP=/tmp/scsdk-smoke
HOST_ROOT=/tmp/session-cost-sdk-migration/tooling
E2E_RUNTIME=/tmp/session-cost-sdk-migration/runtime/kandev
PLUGIN_ROOT=/tmp/session-cost-sdk-migration/standalone/plugin
mkdir -p "$TASK_TMP"/{home,config,data,cache,tmp,artifacts}
cd "$HOST_ROOT/apps/web"
for project in chromium mobile-chrome; do
  case "$project" in chromium) offset=26;; mobile-chrome) offset=27;; esac
  env -i LD_LIBRARY_PATH="$(cat /tmp/session-cost-sdk-migration/browser-library-path)" \
    LANG=C.UTF-8 NIX_LD="${NIX_LD-}" NIX_LD_LIBRARY_PATH="${NIX_LD_LIBRARY_PATH-}" \
    HOME="$TASK_TMP/home" XDG_CONFIG_HOME="$TASK_TMP/config" \
    XDG_DATA_HOME="$TASK_TMP/data" XDG_CACHE_HOME="$TASK_TMP/cache" \
    TMPDIR="$TASK_TMP/tmp" PATH="$E2E_RUNTIME/bin:$PATH" \
    PLAYWRIGHT_BROWSERS_PATH=/home/zeval/.cache/ms-playwright \
    NODE_OPTIONS='--import=tsx' KANDEV_HOST_ROOT="$HOST_ROOT" \
    KANDEV_E2E_BIN="$E2E_RUNTIME/bin/kandev" \
    SESSION_COST_SMOKE_ARTIFACT_DIR="$TASK_TMP/artifacts" \
    SESSION_COST_PACKAGE_PATH="$PLUGIN_ROOT/kandev-session-cost-0.4.1.tar.gz" \
    SESSION_COST_EXPECT_ACTION=1 SESSION_COST_EXPECT_HOST_VERSION=0.99.0 \
    KANDEV_SERVER_HOST=127.0.0.1 E2E_PORT_OFFSET="$offset" \
    node node_modules/@playwright/test/cli.js test \
      --config "$PLUGIN_ROOT/test/host-action-smoke.playwright.config.mjs" \
      --project="$project" --workers=1 --retries=0
done
```

Results: desktop `1 passed (35.9s)`; phone `1 passed (33.8s)`, with no retries.
Both live `/health` responses reported `version: v0.99.0`, `status: ok`,
`service: kandev`, `mode: websocket+http`. Starting with `env -i` removes inherited
`KANDEV_BUNDLE_DIR`, `KANDEV_VERSION`, internal config, service, session, and
supervisor selection variables. Fixture backends use separate temporary HOME,
SQLite databases, worktrees, agentctl ports, and loopback listeners.

The tests upload the full standalone package through Settings, configure the
plugin (including restart), run real Host session reads/webhook requests against
mock-agent sessions, verify scoped costs for two sessions, pin/close disclosure,
refresh/poll/failure/retry, disable/re-enable, and desktop/phone composer geometry.
A controlled tokscale fixture provides report data; this is not a live tokscale
provider/transcript integration test. Other platform binaries were not executed.

Committed evidence:

- [Desktop health](evidence/sdk-v0.1.0/desktop-host-health.json)
- [Phone health](evidence/sdk-v0.1.0/phone-host-health.json)
- [Desktop composer](evidence/sdk-v0.1.0/session-cost-desktop-task-composer.png)
- [Phone composer](evidence/sdk-v0.1.0/session-cost-phone-task-composer.png)

Full logs remain under `/tmp/session-cost-sdk-migration/`. After the smoke driver
updates, `make check-format vet test sdk-check build` passed again. No plugin
release, catalog update, merge, or author notification was performed.

## Review follow-up

On 2026-11-09, Greptile identified that independently optional quote characters
accepted malformed YAML identity values. New negative cases reproduced that
failure before the fix. Identity/version/path patterns now match plain or fully
quoted values as separate alternatives. Regression cases reject both unmatched
opening and closing quotes in package IDs, executable paths, source versions,
and archive IDs/versions, including archives with freshly generated checksums.

The following checks passed after the fix:

```sh
sh scripts/test-verify-package.sh
sh scripts/test-verify-release-version.sh
make test
sh scripts/verify-package.sh /tmp/session-cost-sdk-migration/package-inspect full
sh scripts/verify-release-version.sh v0.4.1 /tmp/session-cost-sdk-migration/standalone/plugin/kandev-session-cost-0.4.1.tar.gz
```

The source/runtime payload is unchanged by this verifier fix; the previously
validated standalone full archive passes the stricter checks.
