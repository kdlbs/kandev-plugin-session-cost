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
- Pinned details show the total cost, cost per turn, and token counts for input,
  output, cache reads, cache writes, and reasoning. They also show each model
  and its cost. Click or tap the action again, click outside, or press Escape
  to close the details.
- Optional statistics collection is disabled by default. When enabled, it
  saves measurements every five minutes (minimum one minute). The settings
  card offers an explicit, resumable historical import using dated buckets.
- Saved canonical usage loads before an explicit refresh. Unknown cost remains
  unavailable rather than appearing as zero.
- Refresh is available while details are pinned. Reports run in the background
  and share one subprocess with collection and import. Details poll every two
  seconds, retain previous values on failure, and stop polling after 130 seconds.
  Closing details cancels the browser request; reopening reads saved usage again.
  Each request has a ten-second timeout. Reports cache successful data for
  thirty seconds. Shutdown cancels and drains the shared subprocess. Sessions
  without a transcript do not start a report.
- The total changes from green to amber at `warn_threshold` and to red at
  `high_threshold`.
- The backend maps the Kandev session ID to the agent transcript ID. The UI
  sends only Kandev task and session IDs.
- If tokscale is unavailable, the details explain how to configure its command.

The action label and details are registered with Kandev's plugin translation
API. English is the fallback; Portuguese (Portugal) is also included.

## Requirements and permissions

The manifest uses plugin API v1 and requests read-only access to Kandev
sessions, tasks, and saved usage (`api_read: ["sessions", "tasks", "session_usage"]`),
usage writes (`api_write: ["session_usage"]`), and durable state. It does not
request credentials. The plugin runs tokscale on the machine that hosts its
backend, so tokscale must be installed there or available through the configured
command.

The package contains server binaries for Linux amd64 and arm64, macOS amd64 and
arm64, and Windows amd64. The UI uses the host's React instance and does not
bundle a second React runtime.

The Go and frontend SDK source pin is public Kandev commit
`9a751dd7d88a12307ee3a23130651cb6a0b8f2d3`; its Go `pluginsdk` tree is
`a9a0a09dc8b30ac02876ccc7e46eb18102079600`. The pinned SDK includes caller
context propagation and the retained usage-history APIs used by this plugin.
This source revision is not a Kandev release number. The UI detects Action and
retains the Button path for older hosts. The manifest does not declare a minimum
Kandev version, and this plugin does not claim a release floor. The exact `0.5.0`
archive was accepted by tagged `v0.97.0` and `v0.86.0` Hosts. Installed browser
checks received upgrade guidance through the v0.97 POST action and the v0.86
legacy GET route. These checks do not establish retained-usage support on those
older Hosts.

## Retained usage development

Collection and import use `HostUsageHistory` and its versioned observation API.
The Host supplies durable subject bindings and owns actor attribution and collector identity.
The plugin saves bindings and pending revisions in Host state before report admission.
An uncertain response replays the same revision and digest after restart.
Reports contain at most 200 observations.

### Host trust and action scope

Session Cost is privileged installed backend code. Kandev gates its Host RPCs by the declared manifest capabilities. This gate is not an operating-system sandbox.
The generic `HostUsageHistory` RPC uses the installed plugin identity. It does not carry the user who invoked a browser action.
The manifest grants `api_read: ["session_usage"]` for reads and `api_write: ["session_usage"]` for observation writes.

The authenticated `session-usage` action receives workspace, task, and session selectors from Kandev's verified action context.
Kandev checks the action selectors before dispatch. The action body cannot replace those selectors.
Before a canonical read, Session Cost asks the Host for a current target in the verified workspace.
It matches the task, session, and any saved subject binding.
If the history interface or point-query acknowledgement is absent, the action returns upgrade guidance before reads or source commands.
Denied or malformed scope fails closed.

### Legacy webhook behavior

The manifest declares the `session-cost` GET webhook as `authenticated`.
The handler returns HTTP 200 with this fixed body for that key:

```json
{"compatibility":"upgrade_required","error":"Use the authenticated session-usage action to view or refresh usage."}
```

The handler returns 404 for a nil request or an unknown webhook key.
It does not read the query, headers, or body. It makes no Host API or tokscale calls.
Use the authenticated `session-usage` action to read or refresh real usage.

### Captured ownership and metric meaning

The Host captures workspace, organization, user, and actor evidence from trusted admission data. The plugin cannot assign or rewrite those values.
Later user, task-owner, or workspace-owner changes do not change captured attribution. A workspace transfer does not grant a new organization access to history captured by the former organization.
Task or session deletion removes the live link and keeps retained usage. Explicit workspace or organization erasure removes the affected history from the live database.

The Host provides a source-neutral metric view. It selects token and cost evidence independently of the collector.
The Host sets `TokenMetricAbsent` and `CostMetricAbsent` separately. An absent metric contributes nothing. An applicable `nil` amount stays unknown, while a measured zero stays zero.
The Host supplies currency and cost basis for applicable cost rows.
The plugin does not convert currencies or add unlike currencies. It calculates cost per turn only when canonical Kandev turn data is available.
Plugin state keeps bounded bindings, revisions, and digests. It does not keep another copy of canonical amounts or transcripts.

The tokscale adapter preserves exact model labels, nullable token categories,
and USD amounts at 10,000 units per dollar. Tokscale costs remain estimates.
Source client labels do not become model providers.
Unknown token subset relationships, work identities, and pricing provenance remain absent.
The retained SDK has no turn-count field. Tokscale message counts do not establish Kandev turns.
Only canonical Kandev turn evidence produces saved cost per turn.
The source version identifies the adapter contract, not an unobserved installed tokscale version.

Explicit source record IDs support model and provider corrections.
Explicit supersession retires an earlier record in the same source.
Tokscale's ordinary grouped export lacks immutable row IDs.
Its fallback group identity excludes provider labels, but a changed model label cannot prove retirement.
Missing rows never become zero or imply a complete replacement.
Ambiguous provider groups without distinct source record IDs fail before admission.

Import checkpoints retain an immutable snapshot, keyset cursor, target page, and pending source dates.
A deleted live session keeps its retained binding and supported transcript context.
Trusted retained session dates and source coverage bounds allow dated import after live session deletion.
Incomplete bounds remain unknown. Targets without trustworthy dates use lifetime coverage outside dated charts.
The checkpoint retains source date ranges and one pending day, including for long intervals.
Missing diagnostics count each target once after its page finishes.
The sample stays bounded at 100 IDs. Legacy diagnostics carry an explicit reliability warning.
A new import replaces legacy mutable cursor progress.

Saved amounts come only from canonical Host reads.
Accepted source receipts do not establish the selected canonical amount.
Canonical token and cost applicability comes from the Host.
Each total includes only rows where that metric applies.
Applicable nil values remain unknown, including on older Hosts that omit applicability flags.
Exact model/provider display groups use the same rule.
Currency and cost basis come only from applicable cost rows.
Cost per turn still requires known canonical Kandev turn evidence.
Projection status, generation, and pending metadata control saved usage and polling.
Only a complete authorized page sequence with one publication supplies saved amounts.
Canonical cursors stay opaque and use the same filter on every page.
A changed publication restarts at the first page, at most twice within one three-second Host budget.
Each attempt stops after 100 pages. Partial pages never become the saved selection.
A ready publication can remain visible while projection work is pending, including after plugin restart.
Those rows come from the Host's last completed publication, with the same generation on every page.
A preparing, mixed, or failed read supplies no accounting fallback from another request or owner.
An empty preparing result shows unavailable usage. It does not become a measured zero.
The plugin stores no canonical amount snapshots in memory or generic Host state.
It keeps only 64 request-order stamps to reject older overlapping reads.
A durable completion marker makes already-migrated restarts use point reads only.
Canonical saved reads do not wait for legacy cleanup.
Initial legacy cleanup uses optional Host state-key pages, with no whole-archive fallback.
Each batch processes at most 200 keys within the three-second Host budget.
A durable queue records each remaining page key. The opaque continuation advances only after the whole page finishes.
One cancellable worker continues larger archives and starts collection after cleanup completes.
Cleanup removes legacy canonical snapshots and strips observation payloads from receipt checkpoints.
Unsupported Hosts require an upgrade for cleanup and collection. Authorized canonical reads remain independent.
Receipt state retains binding/item identities, revision, digest, and acceptance status for exact retries.
Generic Host state has no captured-organization erase contract. It must not hold another accounting copy.
Unsupported RPCs or schema versions show upgrade guidance. Saved values require an authorized Host read.
Invalid queries show an explicit error. Authorization denial clears displayed saved values.
Collection remains disabled by default. Manual calculation still works while collection is disabled.
Historical import status exposes only a narrow view after the Host revalidates its captured-scope snapshot.
Revoked snapshots discard former-scope diagnostics and prevent an old run from restoring the checkpoint.
Target and accepted-receipt reuse also require current scope validation.
Observed active days receive bounded rollover stabilization after midnight, including after restart or terminal transition.
The queue retains up to three actually observed days and shares the existing fair date rotation and subprocess budget.
Unobserved history still belongs to explicit import.
Raw calculation and collection require a current Host target with `SourceScopeStatus` set to `eligible`.
Absent, unknown, or mixed source scope stays unavailable, including manual calculation with collection disabled.
Live session IDs and dates cannot bypass that decision. Cached eligibility and accepted receipts do not supply current authority.
When a saved selection exists, disabled-collection refresh keeps its canonical amounts.
Current/raw eligibility uses one exact `CurrentTarget` point lookup within its Host budget.
The response must acknowledge point support, even when empty. Older Hosts cannot substitute archive enumeration.
Known bindings remain exact constraints after restart, deletion, or pruning. Revoked bindings stay quarantined.
Historical target discovery can prepare metadata before freezing import membership.
Each historical discovery request repeats the same initial query at most four times within its Host budget.
Preparation runs no tokscale command and never counts as an empty or completed import.
The UI shows preparation, preserves authorized saved values, and supports polling and import cancellation.
Actual Host source-safety and indexed bounded-visit proof remain pending the coordinator's backend handoff.
The existing subprocess coordinator owns refresh, collection, import, cancellation, and shutdown.

Phase B checks passed against the local root SDK on 2026-10-29:

- `GOMAXPROCS=2 GOFLAGS=-p=1 make check-format vet test`: Go checks and 47 UI tests passed.
- `GOMAXPROCS=2 go test -p 1 -race ./server/... -run 'TestUsageHistory|TestReport|TestCollector|TestHistorical' -count=1`: passed.
- `GOMAXPROCS=2 GOFLAGS=-p=1 make -j1 build verify-package-host verify-package`: host and all five platform packages passed.

Receipts are in `/tmp/kandev-plugin-session-cost-phase-b/` for this task.
The final source pin, public SDK metadata, real Host integration, and packaged browser checks remain pending.
These local results do not establish a minimum supported Host version or complete Task 06.

Phase C metadata alignment checks also passed on 2026-10-29:

- Focused Go regressions covered trusted retained dates, unknown bounds, projection preparation, generation consistency, restart, and Kandev turn evidence.
- `GOMAXPROCS=2 GOFLAGS=-p=1 make -j1 check-format vet test`: passed, including 49 UI tests.
- `GOMAXPROCS=2 go test -p 1 -race ./server/... -run 'TestUsageHistoryProjection|TestUsageHistoryCanonicalKandevTurns|TestUsageHistoryTokscaleMessage|TestHistorical|TestResponsiveAction|TestReport' -count=1`: passed.

Phase C receipts are in `/tmp/kandev-plugin-session-cost-phase-c/`.
The existing package archive contains Phase B. Rebuild it before integrated package or browser checks.
Real Host, lifecycle, uninstall, package/browser checks, and the final SDK pin remain pending.

Phase D adds SDK broker fixtures for publication restarts, unchanged cursors and filters, cancellation, denied reads, and unsupported Hosts.
Privacy regressions cover ownership transfer, erasure without generic-state cleanup, legacy snapshot removal, and exact pending-receipt replay.
Refresh actions preserve failed admission/readback states when the source report is ready.
The full Go/vet/format checks and all 51 UI tests passed with `GOMAXPROCS=1` and `GOFLAGS=-p=1`.
Exact focused, race, tidy, and failure receipts are in `/tmp/kandev-plugin-session-cost-phase-d/verification/`.
Desktop and phone package fixtures cover actual admission/readback, restart, archive, deletion, import, disablement, and uninstall retention.
Those installed-browser checks remain unrun. The current archive still contains Phase B.
Rebuild it after all source changes and the coordinator's final reachable pin.
These source checks do not establish a released SDK version or a minimum supported Host version.

R1 state cleanup passed 13 focused broker/privacy tests, normally and under race, against the frozen state-key SDK.
The archive fixture includes 453 keys per namespace, large values, restart, deletion, cancellation, and interrupted checkpoint writes.
Completed archives perform no key enumeration. Exact uncertain receipt revisions and digests survive cleanup and restart.
All 17 R1 review fixtures pass. The mixed-tenant source contract and actual Host proof remain pending.
Current receipts and hashes are in `/tmp/kandev-plugin-session-cost-review-r1/state-keys/`.

The typed source-safety checkpoint passed the full Go suite normally and under race: 130 test functions and 74 subtests.
All 56 Node tests, format, vet, syntax, and module tidy checks passed.
Regressions cover unsafe source status, revocation during calculation, preparing discovery, cancellation, and import restart.
Exact commands, red/green receipts, and current hashes are in `/tmp/kandev-plugin-session-cost-review-r1/source-safety/`.
That checkpoint preceded the point-query changes below.
These plugin fixtures do not establish actual Host safety, a release floor, or final integration.

The point-query checkpoint passed the full Go suite and full race suite: 141 test functions and 96 subtests each.
All 56 Node tests and format, vet, tidy, syntax, and diff checks passed.
Current/raw checks make no archive traversal. Immutable historical import keeps its separate snapshot/keyset enumeration.
The broker harness now gives client shutdown one owner, fixing the duplicate-stop race found by the full race run.
Receipts, exact commands, and hashes are in `/tmp/kandev-plugin-session-cost-review-r1/point-segments/`.

Replacement adapters can supply explicit `Segment` identities. Declared segments require schema 2 and participate in the payload digest.
Revision, correction, retry, and partial-absence handling preserve those supplied identities. Unsupported Hosts never cause an unsegmented downgrade.
Unsegmented writes still use schema 1. That checkpoint preceded the approved pinned producer contract below.
Actual backend segment selection and projection remain pending. This checkpoint does not close R1-I or Task 06.

Ordinary Tokscale session/model rows now declare schema 2 using the versioned `tokscale-session-model-v1` decomposition.
Member and record identities use the exact producer `sessionId` and exported normalized model.
Provider, client, binding, date, and timezone do not distinguish members.
Exact rollout IDs remain distinct when the existing suffix join maps them to one canonical ACP transcript.
The canonical ACP ID supplies Host context and the membership-checkpoint key; it never substitutes for a producer key.
Models-only DTOs cannot declare producer identity and remain unavailable for this adapter.

A metadata-only checkpoint reserves up to 1024 member hashes before any report, including partial or uncertain writes.
Its key is the canonical transcript plus source date/timezone. Different dates have independent membership.
A disappearing member blocks the entire snapshot and preserves stale accepted values.
Rejected new names do not enter the baseline; restoring the prior groups permits recovery after restart.
No missing row becomes a zero, tombstone, or guessed rename. No accounting amounts enter these checkpoints.
Explicit stable-record adapters retain their separate correction rules without gaining inferred disjointness.

Merged provider labels stay raw metadata with unavailable provider attribution. The first-contributor `client` is also raw metadata.
The source-defined token total is derived only for the recognized pinned invocation with every category present and safe exact addition.
Partial fields, unknown commands, and overflow retain an unknown total. Message counts still do not establish Kandev turns.
The [pinned fixture notes](server/testdata/README.md) link the primary source and document this codec.
Producer receipts and frozen hashes are in `/tmp/kandev-plugin-session-cost-review-r1/tokscale-producer/`.
The corrected producer checkpoint passed 152 Go functions and 105 subtests, both normally and under race.
All 58 Node tests, formatting, vet, module tidy, and syntax checks passed with one Go process.
Actual Host, legacy compatibility, lifecycle, package/browser, and final source-pin gates remain pending.

## Retained-history integration status at the E2/E3a checkpoint (2026-10-30, NOT COMPLETE)

E2 prepared an installed Host test matrix for native token usage, external reported cost, corrections, nullable values, currencies, action scope, and retention.
E2R and E2R2 prepared fail-closed handling for Hosts that lack the history interface or current-target acknowledgement.
E2T prepared a corrected revision sequence for the live smoke helper. The live smoke did not run.

The E2 preparation receipts record `node --check`, formatting, and whitespace checks only. They contain no Go tests or Host integration from the C2 benchmark window.
After E3a corrected the legacy Host fixture, the full `server/...` Go suite passed normally and under race. Vet, tidy, format, and all 61 Node tests also passed.
The E3a checks use the sibling local SDK and do not establish the final pin, build/package verification, or real Host integration. Receipt: `/tmp/usage-history-luna-E3a-ready.json`.

H3/K3/J3 prepared installed checks for authorization, delivery/session pruning, ownership transfer, erasure, and plugin-state privacy. These are preparations, not installed Host passes.
J4/L3 are adapting the live smoke helper. No live helper result is ready.
Core C3 is running the full SQLite benchmark with 1,000,000 native and 100,000 external rows for ten samples. The full PostgreSQL run remains pending.
The core A3 current-main candidate is prepared but not adopted into primary, so the final reachable SDK pin remains open.
The final package rebuild, actual Host selection and lifecycle checks, and installed desktop/phone browser results also remain pending. Task 06 is still open.
E2 preparation receipts: `/tmp/usage-history-luna-E2-prep.json`, `/tmp/usage-history-luna-E2R-prep.json`, `/tmp/usage-history-luna-E2R2-prep.json`, and `/tmp/usage-history-luna-E2T-prep.json`.

## Retained-history preparation checkpoint (E2–E4, superseded by E5)

The M3 handler returns fixed upgrade guidance for the legacy GET webhook. The plugin regression proves that this handler makes no Host or CLI calls.
The manifest marks the webhook `authenticated`. The source smoke helper asserts HTTP 401 for anonymous access, but that check has not run against an installed Host.

Q3 passed the full `server/...` Go suite normally and under race. The focused M3 guard, `go vet`, and `go mod tidy -diff` also passed.
The bundle tests passed 61 cases. The state-inspection tests passed 7 cases. The JavaScript syntax checks passed.
Receipt: `/tmp/usage-history-luna-Q3-ready.json`. M3 source hashes and its handler contract are in `/tmp/usage-history-luna-M3-ready.json`.

The retained-usage smoke uses the authenticated `session-usage` action. Its legacy case expects only the fixed GET guidance body.
The prepared selectors use `SESSION_COST_EXPECT_ACTION=1` with `SESSION_COST_EXPECT_UPGRADE_REQUIRED=1` for Kandev `v0.97.0`.
They use `SESSION_COST_EXPECT_ACTION=0` with the same upgrade selector for Kandev `v0.86.0`.
Neither older-Host matrix has run against the current package. The installed Host, package, browser, and anonymous-401 checks remain pending.
The selector receipts are `/tmp/usage-history-luna-L4-ready.json` and `/tmp/usage-history-luna-O3-ready.json`.

At this checkpoint the final SDK pin, package, and installed Host checks were still pending. The E5 results below supersede that status.

## Initial retained-history status (E5, 2026-10-30; superseded by E6)

The plugin pins public Kandev commit `9a751dd7d88a12307ee3a23130651cb6a0b8f2d3`. The Go `pluginsdk` tree is `a9a0a09dc8b30ac02876ccc7e46eb18102079600`. Commit `1eb3bb1f944820150636cbeef66365f2994004b9` contains the plugin code. No minimum Kandev release is claimed.

The full `server/...` Go suite passed normal and race runs. `go vet`, module tidy, Go formatting, JavaScript syntax checks, and all 68 Node tests passed. The 68 Node tests include 61 bundle tests and 7 state-inspection tests.

The `0.5.0` archive passed `make verify-package` for all five supported platform binaries. Its SHA-256 is `b53001dc639cddb36c757e798f7c492ec3b2c8fc7543fc73bddf3be35bfb1a9e`.

Five installed-package tests ran against core commit `9a751dd7d88a12307ee3a23130651cb6a0b8f2d3`. The backend used `KANDEV_FEATURES_WORKSPACE_COLLABORATION=true`. Playwright used one worker and zero retries.

Desktop passed the toolbar test. Retained usage and authenticated multi-user tests returned errors. Phone passed the toolbar test. Its retained-usage test returned an error.

Each retained-usage test expected `CostMetricAbsent=true` for a 150-token row. The Host returned `false`. The Host accepted a report for 1200 subcents. The stats API returned `cost_subcents=null`.

These errors stopped the flows before pruning, uninstall, ownership transfer, or erasure checks. These lifecycle and privacy results are unverified. The tests did not change plugin assertions.

The Go suite includes `TestLegacySessionCostWebhookIsStaticAndDoesNotCallHostOrCLI`. This test proves that the legacy webhook makes no Host or CLI calls. The installed anonymous-webhook HTTP 401 check did not run.

The Host reduction sets `CostMetricAbsent` from whether a cost source was selected. This matches the test results, but remains an inference. A focused backend test can confirm this cause.

See `/tmp/usage-history-E5-core-findings.json` for the Host finding. See `/tmp/usage-history-luna-E5-ready.json` for package, command, hash, and test results.

The cached Kandev `v0.97.0` binary was available. The current test fixture returned HTTP 404 for a workspace PATCH request before package upload.
The matching cached source is a sparse checkout. It omits tracked `apps/tsconfig.json`. Its E2E web build stopped when Vite could not find that file.
These results do not show if v0.97 accepts the archive or returns `upgrade_required`. No v0.86 binary or source checkout was available. That test did not run.
Earlier toolbar-only checks do not show current-package compatibility. No older release floor is claimed.

The parent owns the full-scale core benchmark and final integration review. Task 06 remains open until the Host behavior is corrected and lifecycle and privacy checks pass.

## Current integration and compatibility status (E6, 2026-10-31)

The tested archive is `kandev-session-cost-0.5.0.tar.gz`, SHA-256
`b53001dc639cddb36c757e798f7c492ec3b2c8fc7543fc73bddf3be35bfb1a9e`. It was
built from plugin source commit `1eb3bb1f944820150636cbeef66365f2994004b9` and
pins Kandev SDK commit `9a751dd7d88a12307ee3a23130651cb6a0b8f2d3`.

C6 confirmed a PostgreSQL precision bug. PostgreSQL rounded the point-coverage
timestamps for the native event and matching reported price to the same
microsecond. The overlap check then discarded the matching price. Core commit
`98580f996c3867c7db9e87fabc0c321c4f1586e9` introduces the fix and its regression.
C6's focused normal and race runs passed with SQLite and PostgreSQL 18.6.
Receipt: `/tmp/usage-history-luna-C6-metric-ready.json`.

The plugin fixtures now follow the confirmed metric contract. A native row with
150 known tokens and no price has applicable unknown cost
(`CostMetricAbsent=false`, `CostSubcents=nil`). The positive aggregate-price
case uses the native event's exact source ID, model, provider, and observation
time. A positive but unrelated price keeps aggregate cost unknown. Zero,
unknown-token, partial-cost, and mixed-currency assertions remain in the smoke.

Five installed-package browser tests passed on the coordinated v0.98.0-264 Host
build. The desktop action and authenticated multi-user tests passed in run r7;
desktop retention passed in r12; both phone tests passed in run r2. Playwright
used one worker, zero retries, and the normal artifact checks. The retained flow
reached restart, archive, task/session deletion, uninstall, and retained stats
display. The stats page showed two unavailable sessions, 300 tokens, and $0.24,
with no links to deleted tasks or sessions. The authenticated test passed
anonymous legacy GET denial (HTTP 401), cross-workspace denial, and fixed
upgrade guidance for authenticated users. The guidance exposed no usage values
or target IDs, ran no Tokscale fixture command, and left usage totals unchanged.

The Tokscale fixture returned an empty report. The import status completed and
accounted for every processed target as missing. This verifies bounded import
status and diagnostics; it does not show that Tokscale data was imported.

The same unchanged archive was accepted by the exact tagged older Hosts:

- Kandev `v0.97.0`, source `e43881c7555372897b57ec51c705f1e05da43c40`:
  upload returned HTTP 201. The installed desktop and phone UI checks used the
  POST action and received `upgrade_required`. Backend binary SHA-256:
  `254985bbdc8a54896eca93893512445f1d1fff86006ae5afd2e9f230dd6505e5`.
  The exact tagged E2E web build followed restoration of tracked
  `apps/tsconfig.json`, omitted by the sparse checkout. Its index SHA-256 is
  `996ce058a2a14bae86b95511a3fd52e5bb67ff49acec8764850aca9e03720227`.
- Kandev `v0.86.0`, official tag source
  `50a65247dd8871e5857c0cdefb50c127c790360b`: upload returned HTTP 201 and the
  manifest reported version `0.5.0` with active status. The installed desktop
  UI used the legacy GET route and received the fixed `upgrade_required`
  response. Backend binary SHA-256:
  `30b2bef8a6c355e7fe7b1f0bf5674b9a0c83e0d48924d46584450d34adfe98a9`.
  Its tagged E2E web index SHA-256 is
  `63cf2843e0c62a83c38c8d8d52d8735f669fa55075c6eb6241e347294037009d`.

These older-Host checks prove archive acceptance and the tested upgrade-guidance
routes. They do not prove retained-usage support on either older Host, and they
do not establish a minimum Kandev release. The v0.86 phone route was not run.

The current Host browser logs are `/tmp/usage-history-luna-E6-managed-chromium-r7.log`,
`/tmp/usage-history-luna-E6-managed-chromium-r12.log`, and
`/tmp/usage-history-luna-E6-managed-mobile-r2.log`. The v0.97 logs are
`/tmp/usage-history-luna-E6-v097-api-probe.log`,
`/tmp/usage-history-luna-E6-v097-desktop-r1.log`, and
`/tmp/usage-history-luna-E6-v097-phone-r1.log`. The v0.86 API and desktop logs
are `/tmp/session-cost-e6-v086-api-r3/backend-stdout.log` and
`/tmp/usage-history-luna-E6-v086-desktop-r1.log`.

E6 changed only plugin smoke fixtures and this evidence. The package archive and
production plugin source are unchanged. The full source test suites from E5 were
not repeated. Parent review and CI remain pending.

## Settings

Open **Settings → Plugins → Session Cost**:

- `command` sets the tokscale command or path. Empty uses `tokscale` from PATH,
  then the pinned `npx -y tokscale@4.15.1` fallback.
- `warn_threshold` is the USD amount at which the displayed total turns amber.
  Its default is 1.
- `high_threshold` is the USD amount at which the displayed total turns red.
  Its default is 10.
- `collect_statistics` enables collection. Its default is false.
- `collection_interval_minutes` sets the interval. Its default is 5 and its
  minimum is 1. Historical import requires collection to be enabled.

## Install

Download the package from a GitHub Release and upload it in **Settings →
Plugins → Install plugin**. You can also install the local package through the
plugin API:

```sh
curl -F package=@kandev-session-cost-0.5.0.tar.gz http://localhost:8080/api/plugins/install
```

## Develop and verify

Use Go 1.26 and Node.js 24. The Go module resolves the unpublished Kandev SDK
from the sibling module at `../kandev/pluginsdk`. Package targets run
`plugin-pack` from `../kandev/apps/backend` with that checkout's dependencies.

Retained adapter development currently uses the coordinator's local SDK checkout.
The immutable source pin above still selects the earlier baseline.
The bootstrap commands below select that baseline, which lacks the new root module.
The coordinator must supply the final core revision before CI or release checks can use this pin:


```sh
git clone --filter=blob:none https://github.com/kdlbs/kandev.git ../kandev
git -C ../kandev checkout --detach 9aecf156c65e7522a43f2a1c8310737179c25d70
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

The UI has no build step or npm dependencies. The UI tests use fake API
responses. They do not run tokscale or read a real agent database.

## Packaged browser smoke test

Build the disposable Kandev host and its E2E fixture package:

```sh
PLUGIN_ROOT=$PWD
HOST_ROOT=$PLUGIN_ROOT/../kandev
(cd "$HOST_ROOT" && make build-backend build-web-e2e build-e2e-plugin-package)
(cd "$HOST_ROOT/apps" && pnpm install --frozen-lockfile)
```

Run both projects separately against the rebuilt package, after core integration is available.
The tests install the archive through the host UI and replace only the external tokscale command.
The retained flow uses real Host admission, canonical reads, and lifecycle operations.
It also checks that a different plugin's source-owned read cannot expose Session Cost observations.
The Host binaries, web build, and E2E fixture package must come from the same core source.

```sh
(cd "$HOST_ROOT/apps/web" && \
  NODE_OPTIONS='--import=tsx' \
  GOMAXPROCS=2 GOFLAGS=-p=1 KANDEV_HOST_ROOT="$HOST_ROOT" \
  SESSION_COST_PACKAGE_PATH="$PLUGIN_ROOT/kandev-session-cost-0.5.0.tar.gz" \
  SESSION_COST_EXPECT_ACTION=1 E2E_PORT_OFFSET=22 \
  pnpm exec playwright test --config "$PLUGIN_ROOT/test/host-action-smoke.playwright.config.mjs" \
    --project=chromium --workers=1 --retries=0)

(cd "$HOST_ROOT/apps/web" && \
  NODE_OPTIONS='--import=tsx' \
  GOMAXPROCS=2 GOFLAGS=-p=1 KANDEV_HOST_ROOT="$HOST_ROOT" \
  SESSION_COST_PACKAGE_PATH="$PLUGIN_ROOT/kandev-session-cost-0.5.0.tar.gz" \
  SESSION_COST_EXPECT_ACTION=1 E2E_PORT_OFFSET=23 \
  pnpm exec playwright test --config "$PLUGIN_ROOT/test/host-action-smoke.playwright.config.mjs" \
    --project=mobile-chrome --workers=1 --retries=0)
```

For the Kandev `v0.86.0` reference, set `HOST_ROOT` to its checkout and
build that Host with the commands above. Build the current plugin archive first.
Select the legacy GET upgrade-guidance case with these settings:

```sh
(cd "$HOST_ROOT/apps/web" && \
  NODE_OPTIONS='--import=tsx' KANDEV_HOST_ROOT="$HOST_ROOT" \
  SESSION_COST_PACKAGE_PATH="$PLUGIN_ROOT/kandev-session-cost-0.5.0.tar.gz" \
  SESSION_COST_EXPECT_ACTION=0 SESSION_COST_EXPECT_UPGRADE_REQUIRED=1 E2E_PORT_OFFSET=24 \
  pnpm exec playwright test --config "$PLUGIN_ROOT/test/host-action-smoke.playwright.config.mjs" \
    --project=chromium --workers=1 --retries=0 --grep 'returns upgrade_required')

# Repeat with E2E_PORT_OFFSET=25 and --project=mobile-chrome.
```

This case checks the fixed GET guidance only. It does not establish retained-usage support on that Host.

To check action upgrade guidance against Kandev `v0.97.0`
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
  SESSION_COST_PACKAGE_PATH="$PLUGIN_ROOT/kandev-session-cost-0.5.0.tar.gz" \
  SESSION_COST_EXPECT_ACTION=1 SESSION_COST_EXPECT_UPGRADE_REQUIRED=1 E2E_PORT_OFFSET=22 \
  pnpm exec playwright test --config "$PLUGIN_ROOT/test/host-action-smoke.playwright.config.mjs" \
    --project=chromium --workers=1 --retries=0 --grep 'returns upgrade_required'

# Repeat with E2E_PORT_OFFSET=23, --project=mobile-chrome, and the same grep.
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

The statistics APIs require core PR #3660. The earlier stable-host smoke
checks covered the toolbar Action contract; they do not establish support for
the new collection and saved-usage APIs. Merge the core dependency before this PR.

## License

See [LICENSE](LICENSE).

Parent review corrected the archive digest transcription and reran the seven state-inspection tests: all passed. Runtime plugin source and archive remain unchanged. Durable verification and log hashes are in `docs/plans/token-usage/evidence/e6-integration-20261031/`. Public CI and the core performance gates remain pending.
