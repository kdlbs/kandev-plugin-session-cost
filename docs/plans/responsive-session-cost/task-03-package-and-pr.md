---
status: done
requirements:
  - REQ-SESSION-COST-LOOKUP-001
  - REQ-SESSION-COST-RECOVERY-002
system_design:
  - ../../specs/session-cost/system-design/responsive-lookups.md
---

# Package Verification and PR Delivery

## Outcome and dependencies

Verify the packaged fix on a disposable host, update operator docs, and open a PR against main.
Depends on tasks 01 and 02.
Design: [responsive lookups](../../specs/session-cost/system-design/responsive-lookups.md).
Covers all criteria for REQ-SESSION-COST-LOOKUP-001 and REQ-SESSION-COST-RECOVERY-002 through combined evidence.

## Owned files and scope

Update `README.md` for progress, stale values, refresh, failure recovery, and snapshot restart behavior.
Update plan and work-order results. Retain existing package verification and release identity.
Do not modify the user's running installation or the Kandev task worktree.

## Pinned SDK setup

Run this shared prerequisite before task 01. It does not wait for the delivery work in task 03.
Run from the plugin root. Retain this shell's variables for subsequent commands.

```sh
PLUGIN_ROOT="$PWD"
FIX_TMP="$(mktemp -d /tmp/session-cost-fix.XXXXXX)"
SDK_ROOT="$FIX_TMP/kandev"
SDK_REF="$(cat .kandev-sdk-ref)"
git clone --filter=blob:none https://github.com/kdlbs/kandev.git "$SDK_ROOT"
git -C "$SDK_ROOT" checkout --detach "$SDK_REF"
(cd "$FIX_TMP" && GOWORK=off go work init "$PLUGIN_ROOT" "$SDK_ROOT/apps/backend")
export GOWORK="$FIX_TMP/go.work"
test "$(git -C "$SDK_ROOT" rev-parse HEAD)" = "$SDK_REF"
```

The external workspace selects the pinned SDK module for build and test commands only.
It avoids repinning or replacing the current task's Kandev worktree.

## Exact source and artifact checks

From the plugin root, with the workspace above:

```sh
make check-format
cp go.mod "$FIX_TMP/plugin.mod"
cp go.sum "$FIX_TMP/plugin.sum"
GOWORK=off go mod edit -modfile="$FIX_TMP/plugin.mod" -replace "github.com/kandev/kandev=$SDK_ROOT/apps/backend"
GOWORK=off go mod tidy -modfile="$FIX_TMP/plugin.mod"
GOWORK=off go mod edit -modfile="$FIX_TMP/plugin.mod" -replace "github.com/kandev/kandev=../kandev/apps/backend"
diff -u go.mod "$FIX_TMP/plugin.mod"
diff -u go.sum "$FIX_TMP/plugin.sum"
git diff --exit-code -- go.mod go.sum
make vet
make test
go test -race ./server/... -count=1
make build
make verify-package-host KANDEV_SDK="$SDK_ROOT/apps/backend"
make verify-package KANDEV_SDK="$SDK_ROOT/apps/backend"
git diff --check
```

`go mod tidy` ignores workspace replacements. The temporary module copy checks tidiness against the pinned SDK without changing tracked files.

## Disposable packaged checks

Use the pinned source, an isolated home, mock agents, and a fixture cost command.
The fixture must block report output and then complete it without a real transcript scan.
Test the real installed webhook for prompt pending responses and shared calculation.
Browser response fixtures additionally cover an HTML gateway response and JSON failures.
Run both existing browser projects. Assert pending-to-ready, stale values, retry, stopped polling, and session isolation.
Preserve existing Quick Chat, task chat, keyboard, disable/re-enable, and viewport containment coverage.

The pinned SDK accepts `E2E_PORT_OFFSET` values from 0 through 29. Use two unused values in this range.

```sh
(cd "$SDK_ROOT/apps" && pnpm install --frozen-lockfile)
(cd "$SDK_ROOT" && make build-backend build-web-e2e build-e2e-plugin-package)
export KANDEV_HOST_ROOT="$SDK_ROOT"
export SESSION_COST_PACKAGE_PATH="$PLUGIN_ROOT/kandev-session-cost-0.4.0.tar.gz"
export SESSION_COST_EXPECT_ACTION=1
export NODE_OPTIONS='--import=tsx'
(cd "$SDK_ROOT/apps/web" && E2E_PORT_OFFSET=21 pnpm exec playwright test --config "$PLUGIN_ROOT/test/host-action-smoke.playwright.config.mjs" --project=chromium --workers=1 --retries=0)
(cd "$SDK_ROOT/apps/web" && E2E_PORT_OFFSET=22 pnpm exec playwright test --config "$PLUGIN_ROOT/test/host-action-smoke.playwright.config.mjs" --project=mobile-chrome --workers=1 --retries=0)
```

Use host fixtures that create disposable data. Release ports and terminate only the test-owned instance.
Remove only the owned temporary workspace after all commands finish.

## PR delivery

1. Re-read remote main and PR #10. If their contracts changed, reconcile the fix before committing.
2. Read the task's commit, push, PR, and PR-fixup skills. Apply repository-local templates if present.
3. Commit a Conventional Commit after required checks pass, then push and create the requested PR against main.
4. Write the PR body through a file. State before/after behavior, exact checks, SDK pin, and platform limitations.
5. Keep merge and release disabled. Address observed CI failures or actionable findings through the PR workflow.
6. Report the PR URL. Do not claim CI success while checks remain pending.

## Results

Checks passed against pinned Kandev source `570600439036e81f8e9e1c63f15c4abce8a6c846` using Go 1.26.0 on Linux amd64. The isolated SDK install (`pnpm install --frozen-lockfile`) and SDK backend, web E2E, and plugin-package builds passed.

`make check-format`, temporary pinned-SDK `go mod tidy` comparison, `make vet`, `make test`, `go test -race ./server/... -count=1`, `make build`, `make verify-package-host`, `make verify-package`, a Windows amd64 test-binary cross-compile, both syntax checks, and `git diff --check` passed. The bundle suite passed all 26 tests. Package verification checked manifest identity, inventory, checksums, and Linux amd64, Linux arm64, macOS amd64, macOS arm64, and Windows amd64 binaries. The new cleanup helper tests passed with the race detector.

The disposable packaged host smoke passed on the pre-fixup package on Linux amd64 in desktop Chromium and phone Chrome. Each browser project passed 1 test. The smoke used installed webhook requests and a temporary blocking/failing tokscale fixture. Linux child-process cancellation passed its runtime test; Windows and macOS binaries cross-compiled, but process cancellation was not runtime-tested there. No live installation was used.

The reopen-freshness follow-up passed `node --test test/bundle.test.mjs` (26 tests), `node --check ui/bundle.js`, and `make check-format`. A full five-platform package verification passed against pinned SDK commit `570600439036e81f8e9e1c63f15c4abce8a6c846`; updated package SHA-256: `f019a1b48d98f77253189290c4220821240c3abae275bc2d1fc8cd9744eaca32`. The new regression verifies a normal lookup on reopen, with the previous result visible until the response arrives. The UI layout did not change, so the earlier desktop and phone smoke remain representative for presentation.

Remote `main` was verified at `91be5a0046b869fbb87551d8753ea094e0adc349`. PR #10 remains open at `76e74ad0fa69ba4a254f730ff3c36d5fd5428b5a`; its persistent collection overlaps files in this repair. The PR description records that overlap. Generated package/build outputs and the owned pinned-SDK checkout were removed after verification.

PR #14 remains open against `main`: https://github.com/kdlbs/kandev-plugin-session-cost/pull/14. Use the live PR for exact-head CI and review state. No merge, release, or live-plugin installation was performed.
