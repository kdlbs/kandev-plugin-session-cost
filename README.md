# Session Cost

A [Kandev](https://github.com/kdlbs/kandev) plugin that shows the current
session's spend, cost per turn, and per-model usage in the chat composer. The
backend reads the local agent transcript through the [tokscale](https://github.com/junhoyeo/tokscale)
CLI.

## Screenshots

Session cost details on hover or focus, with the total, cost per turn, token
totals, and per-model breakdown:

![Session cost details](https://raw.githubusercontent.com/kdlbs/kandev-plugin-session-cost/f4c484a3f3f5d84e80baa8950732c98c76ed954a/session-cost-popover.png)

The action appears in the chat composer toolbar:

![Session cost action in the chat bar](https://raw.githubusercontent.com/kdlbs/kandev-plugin-session-cost/f4c484a3f3f5d84e80baa8950732c98c76ed954a/session-cost-toolbar.png)

Settings are generated from `manifest.yaml`:

![Session Cost settings](https://raw.githubusercontent.com/kdlbs/kandev-plugin-session-cost/f4c484a3f3f5d84e80baa8950732c98c76ed954a/session-cost-settings.png)

## Behavior

- The action registers once in `chat-input-actions`. Hover or keyboard focus
  starts one request for the active session. Click or tap pins the details open.
- The accessible action label stays “Session cost” when the displayed amount
  changes. On hosts with `host.ui.Action`, the host owns the action's size,
  spacing, focus style, and icon box. Hosts without Action use the plugin's
  existing Button fallback.
- Pinned details show the total, cost per turn, input/output/cache-read token
  totals, and per-model costs and token counts. Click or tap the action again,
  click outside, or press Escape to close them.
- Refresh is available while details are pinned. It recalculates the active
  session once and remains disabled until the request finishes.
- The total changes from green to amber at `warn_threshold` and to red at
  `high_threshold`.
- The backend maps the Kandev session ID to the agent transcript ID. The UI
  sends only Kandev task and session IDs.
- If tokscale is unavailable, the details explain how to configure its command.

The action label and details are registered with Kandev's plugin translation
API. English is the fallback; Portuguese (Portugal) is also included.

## Requirements and permissions

The manifest uses plugin API v1 and requests read-only access to Kandev
sessions (`api_read: ["sessions"]`). It does not request credentials or write
access. The plugin runs tokscale on the machine hosting the Kandev plugin
backend, so tokscale must be installed there or available through the configured
command.

The package contains server binaries for Linux amd64 and arm64, macOS amd64 and
arm64, and Windows amd64. The UI uses the host's React instance and does not
bundle a second React runtime.

The Go and frontend SDK source pin is
`570600439036e81f8e9e1c63f15c4abce8a6c846`, which includes host PR #3943 and
`host.ui.Action`. This is a source revision, not a Kandev release number. The
UI detects Action and retains the Button path for older hosts. The manifest
does not declare a minimum Kandev version. Packaged UI checks passed against
Kandev `v0.97.0` and the older `v0.86.0` reference; these tested versions do not
establish a declared support floor.

## Settings

Open **Settings → Plugins → Session Cost**:

- `command` sets the tokscale command or path. Empty uses `tokscale` from PATH,
  then the pinned `npx -y tokscale@4.15.1` fallback.
- `warn_threshold` is the USD amount at which the displayed total turns amber.
  Its default is 1.
- `high_threshold` is the USD amount at which the displayed total turns red.
  Its default is 10.

## Install

Download the package from a GitHub Release and upload it in **Settings →
Plugins → Install plugin**. You can also install the local package through the
plugin API:

```sh
curl -F package=@kandev-session-cost-0.4.0.tar.gz http://localhost:8080/api/plugins/install
```

## Develop and verify

Use Go 1.26 and Node.js 24. The Go module resolves the unpublished Kandev SDK
from a sibling checkout at `../kandev/apps/backend`. Create a private checkout
for this plugin worktree and pin it to the source revision above:

```sh
git clone --filter=blob:none https://github.com/kdlbs/kandev.git ../kandev
git -C ../kandev checkout --detach 570600439036e81f8e9e1c63f15c4abce8a6c846
```

Do not point the module replacement at a floating branch. The host checkout is
outside the plugin tree and can be shared by Go builds, package checks, and the
disposable host smoke test.

Run the local checks from the plugin root:

```sh
make check-format
go mod tidy
git diff --exit-code -- go.mod go.sum
make vet
make test
make build
make package-host
make package
make verify-package-host
make verify-package
```

`make test` runs the Go suite, UI bundle behavior tests, JavaScript syntax
check, and negative package/release verifier tests. `make verify-package-host`
checks the package for the current machine. `make verify-package` builds and
checks all five declared platforms, the UI bundle, manifest identity, exact
file inventory, and checksums. `make package-host` and `make package` create
the corresponding archives without the additional verification step.

The UI has no build step or npm dependencies. The UI tests use fake webhook
data. They do not run tokscale or read a real agent database.

## Packaged browser smoke test

Build the disposable Kandev host and its E2E fixture package:

```sh
PLUGIN_ROOT=$PWD
HOST_ROOT=$PLUGIN_ROOT/../kandev
(cd "$HOST_ROOT" && make build-backend build-web-e2e build-e2e-plugin-package)
(cd "$HOST_ROOT/apps" && pnpm install --frozen-lockfile)
```

Run both desktop and phone checks against the package. The tests install the
archive through the host UI and use fake cost data:

```sh
(cd "$HOST_ROOT/apps/web" && \
  NODE_OPTIONS='--import=tsx' \
  SESSION_COST_PACKAGE_PATH="$PLUGIN_ROOT/kandev-session-cost-0.4.0.tar.gz" \
  SESSION_COST_EXPECT_ACTION=1 \
  pnpm exec playwright test --config "$PLUGIN_ROOT/test/host-action-smoke.playwright.config.mjs")
```

For an older host checkout, set `KANDEV_HOST_ROOT` to its path and set
`SESSION_COST_EXPECT_ACTION=0`. Build that host with the same commands first.
The checked fallback reference is Kandev `v0.86.0`.

The stable release check also uses this same package against Kandev `v0.97.0`
([release](https://github.com/kdlbs/kandev/releases/tag/v0.97.0), source commit
`e43881c7555372897b57ec51c705f1e05da43c40`). Verify the published Linux x64
full-runtime archive with its `.sha256` file. Set `PLUGIN_ROOT` to this
checkout, `HOST_ROOT` to a source checkout at that exact commit, `TASK_TMP` to
a short task-owned temporary directory, and `E2E_RUNTIME` to a subdirectory of
`TASK_TMP`. Build the release source's E2E web assets and `mock-agent` fixture
(`pnpm install --frozen-lockfile`, `make build-web-e2e`, and
`make -C apps/backend build-mock-agent`). `agentctl` looks for `mock-agent`
beside the runtime executable, so copy the verified runtime to a disposable
directory and add that fixture beside it; do not modify the downloaded
release archive. For example:

```sh
mkdir -p "$TASK_TMP" "$E2E_RUNTIME"
tar -xzf /path/to/kandev-linux-x64-full.tar.gz -C "$TASK_TMP"
cp -a "$TASK_TMP/kandev/." "$E2E_RUNTIME/"
cp "$HOST_ROOT/apps/backend/bin/mock-agent" "$E2E_RUNTIME/bin/mock-agent"
chmod +x "$E2E_RUNTIME/bin/mock-agent"
```

Set `KANDEV_E2E_BIN` to `$E2E_RUNTIME/bin/kandev` and `KANDEV_HOST_ROOT` to
`$HOST_ROOT`. Put `$E2E_RUNTIME/bin` first on `PATH`. Use isolated `HOME`, XDG
data/config/cache directories, `TMPDIR`, and Playwright port offsets, and run
the two projects separately with `--workers=1`:

```sh
mkdir -p "$TASK_TMP/home" "$TASK_TMP/config" "$TASK_TMP/data" \
  "$TASK_TMP/cache" "$TASK_TMP/tmp" "$TASK_TMP/artifacts"

env HOME="$TASK_TMP/home" XDG_CONFIG_HOME="$TASK_TMP/config" \
  XDG_DATA_HOME="$TASK_TMP/data" XDG_CACHE_HOME="$TASK_TMP/cache" \
  TMPDIR="$TASK_TMP/tmp" PATH="$E2E_RUNTIME/bin:$PATH" \
  NODE_OPTIONS='--import=tsx' KANDEV_HOST_ROOT="$HOST_ROOT" \
  KANDEV_E2E_BIN="$E2E_RUNTIME/bin/kandev" \
  SESSION_COST_SMOKE_ARTIFACT_DIR="$TASK_TMP/artifacts" \
  SESSION_COST_PACKAGE_PATH="$PLUGIN_ROOT/kandev-session-cost-0.4.0.tar.gz" \
  SESSION_COST_EXPECT_ACTION=1 E2E_PORT_OFFSET=22 \
  pnpm exec playwright test --config "$PLUGIN_ROOT/test/host-action-smoke.playwright.config.mjs" \
    --project=chromium --workers=1 --retries=0

# Repeat with E2E_PORT_OFFSET=23 and --project=mobile-chrome.
```

Each successful run attaches and saves a task-composer screenshot under
`$TASK_TMP/artifacts`.

## CI and releases

Pull requests check module tidiness, Go formatting, vet, Go and UI tests, and
both host-only and full platform packages. CI, package builds, and releases
read the same immutable SDK source pin from `.kandev-sdk-ref`. Node is set up
explicitly for the UI tests.

Release automation runs from `main`. Select a patch, minor, or major bump, then
use its dry run before a release. The workflow builds and validates the proposed
version before it pushes release metadata or a tag. A pushed tag must match the
manifest, Makefile, and packaged manifest before the workflow publishes a
GitHub Release with the archive and `checksums.txt`.

The stable-host validation condition for this package is met on Kandev `v0.97.0`,
which includes PR #3943. Keep the package parked in its draft PR until the
maintainer authorizes merge and the remaining overlapping plugin work is
reconciled.

## License

See [LICENSE](LICENSE).
