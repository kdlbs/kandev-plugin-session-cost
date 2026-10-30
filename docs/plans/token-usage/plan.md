---
created: 2026-09-13
status: complete
requirements:
  - REQ-COSTS-TOKEN-USAGE-001
  - REQ-COSTS-TOKEN-USAGE-002
  - REQ-COSTS-TOKEN-USAGE-003
system_design:
  - ../../../../kandev/docs/specs/costs/system-design/token-usage.md
legacy_specs: []
---

# Implementation Plan: Session Cost Usage Collection

## Retained-history follow-up

This package's completed results describe its earlier contract.
The [core Retained Usage History package](../../../../kandev/docs/plans/usage-history/plan.md)
owns new retained bindings, collector interoperability, org/user attribution, and bounded statistics queries.
Its [Task 06](../../../../kandev/docs/plans/usage-history/task-06-session-cost-adapter.md)
owns this plugin's new adapter and import compatibility changes, which remain pending.
Record the finalized SDK pin and exact integration results there. Earlier done statuses do not establish the new contract.

## Overview

Connect Session Cost to Kandev's core usage service, then add optional collection and saved-cost display.
An explicit historical import shares the same bounded collector.
This package expands [Kandev Task 03](../../../../kandev/docs/plans/token-usage/task-03-session-cost.md) into plugin-local work orders.

The [requirements](../../../../kandev/docs/specs/costs/requirements/token-usage.md), [system design](../../../../kandev/docs/specs/costs/system-design/token-usage.md), and [ADR](../../../../kandev/docs/decisions/2026-09-13-core-session-usage-projections.md) remain authoritative.
This package owns implementation sequence and verification, not a second product contract.
The assumption check reuses the user's settled choices. No new approval question is necessary.

## Scope

### In scope

- Typed Host usage reads and writes, source normalization and idempotent batches.
- Optional collection, efficient session discovery, final collection and restart recovery.
- Saved values during refresh, failure recovery, keyboard and phone disclosure.
- Explicit historical import, collection settings, diagnostics and user documentation.

### Out of scope

- Kandev schema migrations, source-selection policy, native statistics UI and new protocol collectors.
- Direct database access, native-ledger mutations, remote transcript transport and invoice reconciliation.
- Publishing and installation into a user's instance.

## Repository and dependency contract

Commands run from the plugin repository root unless a command explicitly changes directory.
The supported development layout has Kandev at `../kandev`, matching the current `go.mod` replacement.
Links traverse the same sibling layout. A standalone checkout needs that sibling for SDK builds and contract references.

The host prerequisites are implemented in the sibling Kandev repository:

- [Core accounting](../../../../kandev/docs/plans/token-usage/task-01-core-usage.md): tables, service, source selection and dated tokscale fixture evidence.
- [Usage APIs](../../../../kandev/docs/plans/token-usage/task-02-usage-apis.md): typed Host read/write APIs, authorized actions and bounded session queries.

The implementation uses the generated Host SDK contract. It does not fall back to direct SQL. An old Host returns explicit upgrade guidance and does not acknowledge persisted collection.

All four plugin work orders complete the parent Task 03.
[Kandev Task 04](../../../../kandev/docs/plans/token-usage/task-04-native-page.md) owns the native statistics page.
Both plans record matching plugin integration results before either marks that boundary complete.

## Technical approach

### Existing implementation

`server/plugin.go` has `sessionCost` and `resolveACPSessionID`. The legacy `session-cost` webhook returns fixed upgrade guidance.
Current saved reads and refreshes use the authenticated `session-usage` action.
`runSessionModels` executes a cumulative session/model report. The existing lookup reads only the first 200 sessions.
`server/tokscale.go` owns the injected `runner`, command resolution and the pinned npx fallback.
`ui/bundle.js` uses the shared host React instance. `test/bundle.test.mjs` supplies a local React harness.
`Makefile` already provides `test`, `vet`, and `package-host` targets.

### Host integration and report coordination

Add usage capabilities and server-owned saved-read/refresh actions using the released Host contract.
Use a shared report coordinator for manual requests, timers and import.
At most one tokscale invocation runs per plugin instance, including installation probes.
Compatible requests share a result. Different report scopes queue with bounded capacity.

The adapter preserves nullable counts, cost basis, source coverage and transcript identity.
Normalize cache and reasoning categories according to source semantics before producing the core DTO.
Convert decimal cost to the existing integer subcent unit once, with checked bounds.
Do not relabel tokscale message counts as Kandev user turns.
Saved core records do not necessarily have tokscale message counts, so unsupported per-turn values remain unavailable.

Submit only changed records in bounded batches. Persist source revisions and pending batch identity before submission.
An uncertain write retries the same identity. A newer correction replaces prior source values.
Only core storage decides which source contributes to canonical totals.
Never add external cumulative values to `task_usage_events` or its rollups.

### Collection and recovery

Add `collect_statistics=false` and `collection_interval_minutes=5`, with integer validation and minimum one minute.
When disabled, manual calculations still work but do not persist new measurements.
Saved DB values remain readable. Enabled manual refresh writes before canonical readback.

One worker starts after Host injection and stops before settings restart or process exit.
Lifecycle events mark pending work. SQL-filtered reconciliation repairs missing events and restart gaps.
Session discovery follows all pages and covers eligible sessions beyond the focused tab.
Final collection retries account for delayed transcript writes.
An empty eligible set starts no scheduled command.

Keep checkpoints in Host state with `state: true`. Keep measurements in core usage tables.
The worker owns retries, timeout, subprocess-tree cancellation and write serialization.
A failed command preserves saved values and records a bounded diagnostic.

### Saved display and import

Separate saved reads from slow calculations so the first response never waits for tokscale.
Opening details reads the current session's saved record and requests refresh through the coordinator.
Use session-keyed request fencing and retain saved values through progress and failure.
A no-data response differs from an unavailable Host or DB response.

An explicit settings action queues historical import after collection is enabled.
Import resumes checkpoints, exposes progress, and yields to active/final work.
Use the verified dated export from host Task 01.
The documented fallback queues one source-local day per sweep under the same interval and process budget.
Never use collection time to label historical consumption. Undated lifetime data stays undated.
Only uniquely attributed Kandev transcripts enter a write batch.

## ASCII UI preview

### UI-04: Session Cost refresh states

Entry: chat cost icon. Desktop uses a popover. Phone uses an explicit touch drawer.

```text
Saved + refresh                 No saved measurement
SESSION COST                    SESSION COST
$1.24 estimated                 Calculating cost...
Input / Output / Cache
Saved 2 minutes ago             Failed initial calculation
Refreshing...                   Could not calculate. [Retry]

Saved + failed refresh
$1.24 estimated
Saved 2 minutes ago
Refresh failed. [Retry]
```

The amount is illustrative. The structure and preservation of saved values are required.
Desktop controls retain standard density. Phone drawers have one scroll body and a close control.
Keyboard focus returns to the cost trigger after dismissal.

### UI-P01: Phone cost details

```text
Chat composer                  [Cost]

+----------------------------------+
| Session Cost             [Close] |
| $1.24 estimated                  |
| Saved 2 minutes ago              |
| Input / Output / Cache           |
| Model breakdown                  |
| Refreshing...                    |
| [Refresh]                        |
+----------------------------------+
```

The existing trigger remains the entry point. One drawer body scrolls above the bottom safe area.
Refresh stays visible but disabled during an active request. Failed refresh restores the retry action.
Phone controls have at least 44px hit areas. Desktop controls keep the standard 28px size.
No interaction depends on hover. Shared data and request identity apply to both presentations.
These views cover AC-COSTS-TOKEN-USAGE-003.1 through .4.

### UI-P02: Collection settings and historical import

Entry: Settings > Plugins > Session Cost. Desktop uses the existing plugin settings page.

```text
Session Cost
Collect token statistics          [Off / On]
Collection interval (minutes)     [5]
[Save]

Historical usage
[Import local history]            enabled only after collection is on
Progress: 12 dates complete, 3 pending
[Cancel import]
Last successful collection: 2 minutes ago
```

Phone composition uses the same settings route, one form column and a single scrolling body.

```text
Session Cost
Collect token statistics [On]
Interval (minutes)       [5]
[Save]
Historical usage
12 dates complete, 3 pending
[Cancel import]
```

Disabled collection disables import and explains how to enable it.
An incompatible Host shows an upgrade message. Failed import preserves progress and offers Resume.
Cancellation stops queued import work and retains accepted measurements.
Settings save uses the existing page save lifecycle. It never starts historical import implicitly.
The import uses a plugin-owned settings contribution and supported authenticated actions.
No new native Token Usage controls belong to the plugin.

Control order, explicit import, retained progress and touch access are structural requirements.
Copy and sample counts are illustrative. Labels use the plugin translation API.
These views cover AC-COSTS-TOKEN-USAGE-002.1, .2 and .6.


## Tests

The table is the acceptance coverage map. Executed evidence is recorded in the verification results and task records below.
AC suffixes expand to `AC-COSTS-TOKEN-USAGE-<suffix>`.

| ACs | Planned evidence |
| --- | --- |
| 001.2, 001.4 | `server/usage_test.go`: `TestUsageNormalizationAndIdempotency` |
| 001.6 | `server/actions_test.go`: `TestUsageActionSessionAuthorization`; host API tests own capability enforcement |
| 001.7 | `server/usage_test.go`: `TestCanonicalReadbackDoesNotAddNativeUsage` |
| 002.1, 002.2 | `server/collector_test.go`: `TestCollectionSettings` |
| 002.3, 002.4 | `server/report_test.go`: `TestSharedReportAndProcessLimit`; `server/collector_test.go`: `TestIdleSkip` |
| 002.5, 002.6, 002.7 | `server/collector_test.go`: `TestFinalCollectionRecoveryAndAttribution` |
| 002.6, 002.7 | `server/import_test.go`: `TestHistoricalImportResumeAndCoverage` |
| 003.1, 003.2, 003.3 | `test/bundle.test.mjs`: `saved usage survives refresh and ignores old session responses` |
| 003.4 | `test/bundle.test.mjs`: `touch and keyboard expose cost and refresh` plus installed-plugin E2E |

Preserve the existing session-change, pinning, threshold and explicit-refresh tests.
Extend the fake Host for typed usage APIs. Fake command output avoids real user transcripts or billable agents.
Use controllable clocks and request completion signals for worker tests.
A race-enabled Go run checks simultaneous manual refresh, timer ticks, shutdown and import.

## E2E tests

Browser tests live in Kandev because its harness owns plugin installation and session fixtures.
The package tarball must supply the actual changed bundle and server. A mock UI is not equivalent evidence.
Use a deterministic tokscale fixture command and seed core saved usage through supported fixture/service APIs.

| Host test path | Project | Scenarios / ACs |
| --- | --- | --- |
| `tests/plugins/session-cost-refresh.spec.ts` | `chromium` | Saved cost during delayed refresh, error, empty initial state, session switch and keyboard: 003.1 through .4 |
| `tests/plugins/mobile-session-cost-refresh.spec.ts` | `mobile-chrome` | Actual drawer, touch refresh, focus return and containment: 003.4 |
| `tests/plugins/session-cost-collection.spec.ts` | `chromium` | Settings off/on, validation, explicit import, cancel/resume and no implicit historical scan: 002.1, .2, .6 |
| `tests/plugins/mobile-session-cost-collection.spec.ts` | `mobile-chrome` | Same settings/import workflow with reachable 44px controls: 002.1, .6 |

Paths start at `../kandev/apps/web/e2e/`.
The host collector work order owns registration of these companion scenarios in the umbrella matrix.
Managed runners rebuild assets, use one worker per shard, and use causal waits instead of arbitrary sleeps.

## Work orders

- [x] [Task 01: Usage adapter and shared refresh](task-01-usage-adapter.md)
- [x] [Task 02: Optional background collection](task-02-background-collection.md)
- [x] [Task 03: Saved cost and touch details](task-03-saved-cost-ui.md)
- [x] [Task 04: Historical import and setup](task-04-historical-import.md)

Dependency order: Host 01 -> Host 02 -> Plugin 01 -> Plugin 02 -> Plugin 03 -> Plugin 04.
All work is sequential. No work order creates another Kandev task or session.

## Verification results

Feature verification completed on 2026-09-13.

- `make test vet build package-host` passes from the plugin repository.
- `go test -race ./server/...` passes.
- `node --test test/bundle.test.mjs` passes all 12 tests, and `node --check ui/bundle.js` passes.
- The Kandev web type check, ESLint with zero warnings, localization check, Prettier check, focused Vitest suite (24 tests) and production build pass.
- Kandev changed backend package tests and `go build ./cmd/kandev` pass. The full backend run was attempted; only existing environment-sensitive process probe tests remained failing. Config and launcher tests pass with inherited `KANDEV_*` configuration variables unset.
- Kandev and plugin documentation validation passes, including the 36 specification linter tests and full specification lint.

No installed-plugin browser E2E scenarios were added or run. No live tokscale archive benchmark was run. Historical import preserves lifetime data as undated when the current tokscale aggregate output has no source dates.

## Native table data dependency

Kandev's model/provider and daily/monthly tables depend on preserved provider identity and dated coverage.
Collector identity is not provider identity. The adapter keeps those fields separate.
The export fixture must verify that providers do not merge within a session/model report.
Missing provider identity remains unknown. The adapter does not infer it from a model name.
Historical import preserves the source-local date and timezone for monthly aggregation in core.
The plugin does not calculate aggregate Cost/1M or render the native tables.

`server/usage_test.go:TestUsagePreservesProviderIdentity` covers provider collisions and missing provider values.
`server/import_test.go:TestImportPreservesDateCoverage` covers dated rows across month and timezone boundaries.
These cases run through the existing `make test` commands in Tasks 01 and 04.
They supply data for AC-COSTS-TOKEN-USAGE-004.8 through .12, owned by the core work package.

## Table refinement validation

The model/provider and daily/monthly refinement passed documentation validation on 2026-09-13.
The host package maps all 35 ACs. The plugin package retains all 11 collection/display ACs and 15 total mapped references.
Local links and whitespace checks passed in both packages.
Kandev catalog validation found 266 decisions and 822 specifications. All 36 linter tests and the full specification lint passed.
Commands ran from the Kandev repository root after an initial workspace-root invocation could not locate the specification configuration.
The E2E tables remain the follow-up browser coverage map. The checks listed above are the executed product and integration evidence.

## Risks

- The additive Host usage contract must remain compatible with older plugin hosts; the plugin reports an upgrade requirement when the capability is absent.
- A source-local daily export cannot accurately split arbitrary timezone boundaries.
- Shared/resumed transcript attribution must not duplicate a lifetime total.
- Canceling an npx parent alone can leave child processes alive.
- Missing local executor transcripts remain missing coverage, not measured zero.
- Existing cost-per-turn labels rely on tokscale message counts and need precise wording for saved data.

## Main compatibility verification (2026-10-05)

Rebased on the plugin 0.4.0 release. This feature targets 0.5.0 and pins core
SDK commit `2e5fe80abddf8920f085b4828ff782b25aa5885e`.
The toolbar retains the host Action and plugin translations. Package checks
accept normalized YAML and the host-only platform manifest. Full packages
still require all five platforms.

Verification passed:

- `make test vet build verify-package`: Go, bundle, package, and release checks.
- `go test -trimpath -race ./server/... -count=1`: passed.
- `make verify-package-host`: passed.
- Desktop and mobile packaged browser smoke tests: 2 passed with retries disabled.
  The smoke helper checks saved reads and explicit refresh through the authenticated action.

The browser tests used the rebuilt core host and the 0.5.0 archive with
`SESSION_COST_EXPECT_ACTION=1`, `E2E_PORT_OFFSET=27`, `--workers=1`, and `--retries=0`.
These are historical results for the source and archive built on 2026-10-05.
They do not establish the current M3 webhook behavior, current package, or older-Host matrix.

## Review corrections (2026-10-05)

The report coordinator no longer joins a canceled process. Batch rejection
results include all accepted models. Dated collection rotates through active
and terminal dates. Historical import waits for the configured interval
between dated reports and pages, and retains its pending-date checkpoint.
Cancellation cannot overwrite an explicit cancelled state with failure.

The command runner cancels descendants and bounds pipe cleanup. Windows uses
Node/ICU to resolve the system IANA timezone. If Node is absent, set `TZ` to an
IANA timezone. Dated writes fail when the source timezone cannot be resolved.

Verification: the Go race suite, 17 bundle tests, vet, build, all-platform
package checks, and package/release negative tests passed. Regression tests
cover canceled report joining, complete rejected batches, dated scheduling,
cancelled import state, resumed pending dates, interval waits, timezone/date
alignment, and descendant shutdown. Documentation now names actual tests and
describes ticker reconciliation instead of unimplemented lifecycle events.


## Compatibility with Session Cost 0.4.1

The 0.5.0 branch includes the responsive lookup changes from main. Toolbar
requests use the authenticated saved-usage action and return pending reports
without waiting for tokscale. Polls retain the pending state over older saved
usage, then persist the accepted result and read canonical usage. The process
owns report cancellation, the 30-second cache and the failure cooldown. Dated
collection and import share the same serialized subprocess runner. Shutdown
cancels and drains that runner, including work started outside the toolbar.

The UI preserves bounded polling, request cancellation, reopening recovery,
unknown costs and the extra token categories. The installed-package smoke test
uses a blocked command fixture, two transcript identities, an explicit refresh
and failure recovery on desktop and mobile. Unit tests cover both the upstream
responsive behavior and the statistics integration.


## Retained adapter Phase B (2026-10-29)

The follow-up link above remains the owner of final integration and compatibility evidence.
Phase B implements the optional `HostUsageHistory` writer against the local root SDK.
Core checkout HEAD is `423bb207241cfd72d45154f8931867a3873943a4`, with the local Task03 SDK changes.
This working tree is not an immutable published SDK revision.
The plugin source pin remains `9aecf156c65e7522a43f2a1c8310737179c25d70` until the coordinator supplies its replacement.

The adapter retains Host bindings, exact nullable amounts, estimated costs, and independent source dimensions.
Durable pending revisions support uncertain-response replay.
Stable source records support dimension corrections and explicit supersession.
Partial absence never retires a record.
Ordinary tokscale model groups cannot prove model-label replacement without stable record evidence.
Import retains snapshot identity, keyset position, exact page membership, and pending dates after restart or live deletion.
Missing counts use page-local deduplication beyond the 100-ID sample.
Legacy diagnostics carry an explicit reliability warning.
Canonical reads supply all saved toolbar amounts.
Unsupported RPCs and schema versions preserve saved values and show upgrade guidance.

Checks passed with `GOMAXPROCS=2` and serial Go package compilation:

- `GOFLAGS=-p=1 make check-format vet test`: Go checks, 47 UI tests, and package/release negative tests passed.
- `go test -p 1 -race ./server/... -run 'TestUsageHistory|TestReport|TestCollector|TestHistorical' -count=1`: passed.
- `GOFLAGS=-p=1 make -j1 build verify-package-host verify-package`: host and all five platform packages passed.

Receipts are in `/tmp/kandev-plugin-session-cost-phase-b/`.
At Phase B handoff, the SDK did not expose projection generations. Phase C consumes the new metadata.
The retained observation SDK still has no turn-count field.
Saved cost per turn requires canonical turn evidence. Existing canonical values remain visible during projection updates.
Real Host admission, source selection, canonical projection, deletion/uninstall retention, and browser checks remain pending.
SDK publication metadata and the final reachable core pin also remain pending.
Task 06 remains open for coordinator review and integrated verification.

## Retained adapter Phase D preparation (2026-10-29)

The self-verification section below supersedes the provisional cache behavior and test status in this preparation record.

Canonical pagination passes each opaque cursor unchanged with the same filter.
A changed publication permits two restarts from the first page within one three-second Host budget.
Each attempt permits at most 100 pages. Cancellation ends the read, and incomplete pages never replace a completed selection.
Preparing reads retain the completed selection without invoking tokscale, including explicit refresh requests.
Invalid queries return an explicit failure. Unsupported Hosts retain saved values with upgrade guidance.
Authorization denial blocks source calculation and cached fallback, including post-write readback and plugin restart.
The action preserves denied HTTP status codes so the UI can clear earlier values.

Eleven new SDK broker test functions cover the actual Host client/server and protobuf adapters without a backend or subprocess.
Two new bundle tests cover invalid-query copy and clearing saved values after denied access.
The desktop and phone smoke suites each add a packaged Session Cost retention flow.
They cover actual admission, canonical readback, source namespace separation, restart, archive, session/task deletion,
retained import, disable/enable, uninstall, and denied action scope.
Core-owned native pruning, payload retention, authenticated scope, and projection recovery suites remain integration prerequisites.

The benchmark resource window permits code and light syntax checks only.
Formatting, JavaScript syntax, and whitespace checks passed. All new Go, UI behavior, package, and browser tests remain unrun.
Planned commands, file hashes, and integration gates are in `/tmp/kandev-plugin-session-cost-phase-d/`.
The immutable SDK pin and manifest remain unchanged. No released SDK version or minimum Host floor is claimed.
The package archive still contains Phase B. Its final rebuild follows all source changes and the coordinator's reachable core pin.
Task 06 remains open. Phase D is ready for plugin checks after the coordinator grants the resource window.


## Retained adapter Phase C (2026-10-29)

The plugin consumes the additive fields in the local root SDK.
`UsageCollectionTargetV1` supplies trusted session and coverage timestamps.
`PageInfo` supplies projection status, generation, and pending state.

Deleted targets use complete trusted date bounds without a live session lookup or an earlier plugin checkpoint.
Source coverage ends remain exclusive. Missing or invalid endpoints do not acquire a collection-time date.
Import checkpoints retain bounded target ranges and one pending source day.
Existing page membership, snapshot identity, and keyset progress survive resume.

A completed canonical selection replaces the saved snapshot only after all pages report consistent metadata.
Preparing, pending, unknown, or mixed-generation pages cannot replace that snapshot with partial values.
The snapshot survives plugin restart. An empty preparing read remains unavailable and continues bounded polling.
Generation tokens stay opaque strings. Saved and pending generation fields identify different views during projection lag.
Authorization errors do not replay cached selected values.
Disabled collection still permits manual calculation after an empty ready read.
Tokscale message counts never populate Kandev turns. Canonical Kandev turn evidence still supports cost per turn.
The existing observation revision, receipt, digest, and collector namespace rules remain unchanged.

Checks passed with serial Go compilation and `GOMAXPROCS=2`:

- Focused metadata, trusted-date, refresh, and turn regressions passed.
- `GOFLAGS=-p=1 make -j1 check-format vet test`: passed, including 49 UI tests.
- `go test -p 1 -race ./server/... -run 'TestUsageHistoryProjection|TestUsageHistoryCanonicalKandevTurns|TestUsageHistoryTokscaleMessage|TestHistorical|TestResponsiveAction|TestReport' -count=1`: passed.

Receipts and local SDK hashes are in `/tmp/kandev-plugin-session-cost-phase-c/`.
The existing package archive predates Phase C. Package rebuild and browser verification remain pending integration.
Real Host admission/readback, lifecycle/uninstall checks, and the final reachable SDK pin also remain pending.
Task 06 remains open for coordinator review and integrated verification.


## Retained adapter Phase D self-verification (2026-10-29)

The actual regression runs reproduced cursor restart, invalid-query, authorization, page-bound, and UI privacy failures in the earlier inputs.
A stronger preparing-read regression checks that no asynchronous source work was scheduled.
A six-case action regression exposed failed admission/readback states being overwritten by a ready source report.
The plugin now keeps those failures and their error codes. Rejected source amounts never replace the selected Host values.

Privacy regressions also reproduced replay of a former owner's snapshot and recoverable accounting copies in generic Host state.
Generic state has no captured organization ownership or erasure contract.
The plugin now stores no canonical accounting snapshots and strips full payloads from old observation checkpoints before use.
Only revision/digest/acceptance and opaque binding/item metadata remain in receipt state.
The legacy uncertain-receipt fixture proves an identical revision/payload digest is retried and accepted retries are suppressed after restart.

Saved usage comes from complete, consistent, authorized Host pages, including the last completed publication while work is pending.
The current Host uses ready publication metadata with pending=true for that last completed selection.
Preparing or unknown metadata, failed/mixed page sequences, unsupported canonical reads, and obsolete overlapping reads return unavailable amounts.
A new owner's preparing read cannot reuse the previous owner's values, including after process restart.
The metadata fixtures now model that actual published-view boundary. Exact visible amounts remain asserted for valid pending publications and restart.
Invalid queries and denied reads retain explicit failures. No SDK or source pin change was made.

The full plugin Go/vet/format checks and 51 Node behavior tests passed with GOMAXPROCS=1 and Go package parallelism one.
Focused tests cover 12 SDK broker functions, four privacy functions, and six readback-failure action cases.
Exact commands, meaningful red logs, race/tidy results, final file hashes, and remaining gates are in `/tmp/kandev-plugin-session-cost-phase-d/verification/`.
This is plugin fixture evidence. Actual Host lifecycle/privacy, package rebuild, installed desktop/mobile, and final reachable SDK pin checks remain pending.
The general Host-state erasure contract for nondisplayable receipt and import-progress identifiers remains a core API limitation.
No accounting payload depends on that generic-state erasure path. Task 06 remains open for coordinator review and final integration.


## Coordinator review R1 source work (2026-10-29)

Current-format startup now uses a durable completion marker, with a real SDK broker restart regression.
Canonical reads no longer depend on legacy cleanup success.
Initial unmarked archive cleanup now uses the frozen optional Host state-key API.
Each call processes at most 200 keys within the existing three-second Host budget.
The durable checkpoint stores one bounded page and its remaining keys.
Each successful cleanup operation checkpoints that key before continuing.
The opaque page continuation advances only after every key on that page finishes.
A failed progress write repeats one idempotent operation after restart.
One cancellable worker continues larger archives and starts collection after cleanup succeeds.
Unsupported or denied enumeration never falls back to whole-namespace ListState.

Import actions revalidate the saved immutable snapshot against the Host's current captured scope before displaying or resuming it.
The public status view excludes target identities, transcripts, cursors, snapshot/run IDs, date maps and missing-ID samples.
Revoked checkpoints are deleted and in-flight revoked runs cannot restore them.
Cached binding and accepted-receipt reuse revalidate scope; a revoked cumulative transcript cannot silently move to a new binding.

The rollover regression reproduced a Dec31 bucket staying at 10 after late pre-midnight usage raised it to 20.
Bounded observed-day stabilization now writes revision 2 across restart and terminal transition while current-day values progress.
No arbitrary days between an old observation and restart are scanned.

Focused scope, startup broker, erasure and rollover regressions passed before the coordinator opened the SDK/protogen change window.
A broader affected Go run also passed at that checkpoint.
The later shared-transcript source escalation adds common raw-source preflight work and disabled-collection canonical-only fallback.
Those raw-display and homogeneous-source fixtures now pass against the current SDK.
Their core semantics remain provisional until the source-safety contract handoff and actual Host checks.
Current Node checks passed all 53 tests, with meaningful red/green source-context guidance regressions.

Receipts and source-ready hashes are in `/tmp/kandev-plugin-session-cost-review-r1/`.
The state-key checkpoint has separate receipts and hashes in `state-keys/` below that directory.
Thirteen cleanup/privacy tests passed normally and under race. All 17 R1 review fixtures passed.
The real SDK broker archive test uses 453 keys per namespace and large legacy values.
It checks partial-page restart, deletion, exact retry identity, namespace isolation, and no whole-archive enumeration.
Additional fixtures cover capability denial, unsupported Hosts, cancellation, hidden payload removal, and shutdown.
No state-key API gap remains. The authoritative mixed-tenant source contract remains pending in `core-contract-gaps.txt`.
No SDK/core changes, package/browser run, source pin change, release metadata, commit or push was performed.
Task 06 and R1 remain open until concrete core handoff, same-snapshot Go/full/race checks and final integration gates.

## R1 typed source-context SDK checkpoint (2026-10-29)

The plugin consumes the actual additive `SourceScopeStatus` and target-page `Preparing` fields.
Only a current explicit `eligible` response permits aggregate calculation or collection.
Absent, unknown, mixed, and unrecognized values stay unavailable across manual, legacy, scheduled, and import paths.
Unsafe source context cannot recover a transcript or date range through live session metadata.
Cached bindings and accepted receipts require another Host target read before reuse.
Calculation results are withheld if the Host revokes source eligibility during the report.
Canonical saved values still come from a fresh authorized complete Host publication.

Preparing pages contain no membership, snapshot, upper bound, or cursor.
The plugin validates that shape and repeats only the identical initial query, at most four times per short Host budget.
Import preparation checkpoints no target identities, source dates, counts, or missing samples.
A cancellable metadata worker advances later preparation passes without invoking tokscale.
Membership freezes only on a non-preparing response and is checkpointed before external collection.
Same-scope legacy import pages can resume after a current eligible response authorizes the exact retained binding.
The UI polls preparation, preserves canonical selections, and keeps import cancellation available.

Meaningful Go and Node red regressions preceded the fixes.
The full plugin Go suite passed normally and under race: 130 test functions and 74 subtests.
All 56 Node tests, format/vet, syntax, module tidy, and diff checks passed with `GOMAXPROCS=1` and `GOFLAGS=-p=1`.
Current receipts, exact commands, and hashes are in `/tmp/kandev-plugin-session-cost-review-r1/source-safety/`.

At this checkpoint, the bounded current-target lookup performance finding remained open.
The point-query checkpoint below replaces current/raw traversal. Actual backend bounded-visit proof remains pending.
The concrete query-filter proposal is in `source-safety/bounded-lookup-gap.txt` below the receipts directory.
Worker3 owns the actual API and backend implementation. No query fields were guessed or added here.
Actual Host mixed-tenant/shared-subject safety and final backend integration remain unverified by this checkpoint.
The final reachable SDK pin, release truth, package rebuild, and installed desktop/mobile checks remain gated.
No core/SDK writes, pin/release changes, package/browser runs, or Git mutations occurred. Task 06 and R1 remain open.

## Point SDK and segment adapter checkpoint (2026-10-29)

The plugin consumes the actual point SDK checkpoint from `/tmp/usage-history-worker4-segments/POINT-SDK-DDL-CHECKPOINT.md`.
Worker2's live-versus-retained lookup contract was also read from `/tmp/usage-history-agent2-current-target-api-checkpoint.md`.
Neither checkpoint constitutes final backend proof or a release floor.

All current/raw eligibility uses one bounded `CurrentTarget` query with workspace and exact subject/transcript constraints.
A known retained binding is included as an exact constraint, including after deletion or pruning.
The plugin requires the response ACK and zero/one target, with no snapshot, cursor, preparation, or enumeration state.
Missing ACK means unsupported. Empty, denied, unknown, mixed, or mismatched results never trigger archive fallback.
Revoked bindings remain quarantined across process restart, preventing cumulative relabeling after ID reuse.
Historical membership and its captured-scope status checks keep the separate immutable snapshot/keyset API.

Generic adapters can supply explicit `UsageSegmentIdentityV1` declarations.
The plugin validates both IDs, chooses schema 2 when a pending declaration is present, and retains the declaration in digests and revisions.
Unsegmented pending batches remain schema 1. An unsupported schema 2 result never downgrades to unsegmented writes.
Four declared members sharing a SourceRecordID remain distinct across two models and two providers.
Downward and dimension corrections retain supplied segment IDs. Lifetime/day views and subject bindings also retain those IDs.
Partial absence sends no tombstones or complete-set assertion.
At that checkpoint, the producer decision remained open.
The approved pinned producer rule and implementation below supersede its ordinary-group fallback.

Meaningful point and segment red regressions preceded the fixes.
The full Go suite passed, then the full race run found a duplicate shutdown in the SDK broker harness.
Client cleanup now owns shutdown and awaits server completion. No assertions or deadlines were weakened.
The affected broker tests passed five race runs. The final full race suite also passed.
Final counts are 141 Go test functions and 96 subtests, plus all 56 Node tests.
Format, vet, module tidy, syntax, and diff checks passed with `GOMAXPROCS=1` and `GOFLAGS=-p=1`.
Current receipts, exact commands, and source hashes are in `/tmp/kandev-plugin-session-cost-review-r1/point-segments/`.

Actual backend current-target safety, PostgreSQL/bounded visits, segment selection/projection, and full Host lifecycle proof remain pending.
R1-I and Task 06 are not complete. Final source pin, release truth, package rebuild, and installed desktop/mobile checks remain held.
No core/SDK writes, pin/release edits, package/browser runs, Git mutations, or delegation occurred.

## Pinned Tokscale producer contract (2026-10-29)

The user and Worker4 approved the v4.15.1 session/model group-series declaration.
The exact source and synthetic fixture are documented in `server/testdata/README.md`.
Ordinary rows use schema 2 and `tokscale-session-model-v1`.
Member/record IDs use exact producer session ID plus exported normalized model, excluding provider/client, binding, and temporal coverage.
The existing ACP suffix join does not rewrite producer keys.
Two rollout IDs with one ACP suffix and the same model remain two distinct members.
Models-only DTOs cannot manufacture those keys. Explicit stable-record adapters follow their separate correction rules.

The per-canonical-transcript/date/timezone checkpoint stores at most 1024 member hashes, version, and quarantine state.
It stores no amounts, raw model names, or transcript content and requires no archive enumeration.
One existing process lock covers metadata reservation and every resulting report batch.
Reservations precede admission so lost and partial receipts cannot drop pending members.
Missing members quarantine the whole snapshot before any changed row is written.
Rejected new names do not enter the set; restoring old groups recovers after restart.
No absence creates a tombstone, zero, or guessed rename. Different source dates have independent sets.

Merged providers have unavailable attribution and retain their raw label.
The actual JSON client field remains raw first-contributor metadata, without uniform-client attribution.
Only a recognized pinned invocation with all nonnegative categories and checked exact addition derives the source total.
Partial/unknown/overflow cases remain null. Kandev turns never come from Tokscale message counts.
The UI keeps fresh authorized accepted values during quarantine and clears earlier raw values when no saved selection exists.

Producer receipts, meaningful red/green cases, exact commands, and frozen hashes are in `/tmp/kandev-plugin-session-cost-review-r1/tokscale-producer/`.
The interim exact-producer-ID and explicit-record classification review cases are included there.
Final full Go and race checks passed 152 test functions and 105 subtests each.
All 58 Node tests, format/vet, tidy, and syntax checks passed with `GOMAXPROCS=1` and `GOFLAGS=-p=1`.
Final Host legacy mapping, safety/SQL visits, segment projection, lifecycle, pin, packaging, and installed browser checks remain held.
No core/SDK edits, release/pin edits, Git mutations, backend/package/browser runs, or delegation occurred.

## Canonical metric applicability checkpoint (2026-10-29)

The plugin consumes the approved `TokenMetricAbsent` and `CostMetricAbsent` canonical read flags.
Only the Host assigns applicability. False defaults keep older replies applicable, with nil values unknown.
Token totals, Kandev turns, and costs use their own applicable rows.
Currency and cost basis exclude rows whose cost metric does not apply.
Exact model/provider display groups use the same aggregation rules without merging provider identities.
Unknown applicable amounts taint only their metric or category. Measured zeros remain known.
Partial costs, unknown cost basis, mixed currencies, and unsafe browser numbers remain unavailable.
Exact model cost amounts retain their currency and partial coverage limits.
Cost per turn still requires available cost and known canonical Kandev turns.

Meaningful fake Host and real SDK broker regressions failed before the consumer fix.
Nineteen cases cover split metrics across opaque cursor pages, true unknowns, older defaults, zeros, partial costs, currencies, and exact large values.
The full Go and race suites passed 155 test functions and 143 subtests each.
All 61 Node tests, format/vet, module tidy, and syntax checks passed with `GOMAXPROCS=1` and `GOFLAGS=-p=1`.
Receipts, exact commands, changed files, and source/SDK hashes are in `/tmp/kandev-plugin-session-cost-review-r1/canonical-metrics-r8/`.

Worker4's SDK checkpoint and repository-to-Host applicability test receipts were read without modifying core.
Actual plugin-to-Host split-metric integration and final backend/race signoff remain coordinator gates.
Task 06 remains open. Final source pin, release truth, package rebuild, and installed desktop/mobile checks remain held.
No core/SDK edits, Git mutations, pin/release changes, backend/package/browser runs, or delegation occurred.

## Retained Usage History adapter preparation checkpoint (2026-10-30, NOT COMPLETE)

E2 prepared an installed Host matrix for native tokens, external reported cost, corrections, nullable metrics, currencies, action scope, and retention.
The plugin now rechecks the current task/session binding in the verified workspace before a task-scoped canonical read.
The read must match the current Host target and any saved subject binding. A foreign workspace or replaced binding is denied before the canonical read.

E2R and E2R2 prepared explicit upgrade guidance for Hosts without `HostUsageHistory`, an implemented `CurrentTarget` RPC, or its required acknowledgement.
These unsupported cases return before canonical reads or tokscale calls. Denied or malformed acknowledged responses keep their fail-closed result.
E2T corrected the installed-smoke revision sequence. Its live smoke remains unrun.

`node --check` passed for the smoke helper. The recorded formatting and whitespace checks passed for the prepared Go changes.
The E2 preparation window during C2 did not include Go tests or the full Node suite. After E3a corrected the legacy Host fixture, the full `server/...` Go suite passed normally and under race.
Vet, tidy, format, and all 61 Node tests also passed. These checks use the sibling local SDK.
They do not establish the final pin, package verification, or real Host integration.
Receipt: `/tmp/usage-history-luna-E3a-ready.json`.

H3/K3/J3 prepared installed checks for authorization, delivery/session pruning, ownership transfer, erasure, and plugin-state privacy. These are preparations, not installed Host passes.
J4/L3 are adapting the live smoke helper. No live helper result is ready.
Core C3 is running the full SQLite benchmark with 1,000,000 native and 100,000 external rows for ten samples. The full PostgreSQL run remains pending.
The core A3 current-main candidate is prepared but not adopted into primary.
The final reachable SDK pin, package rebuild, actual Host selection/readback, ownership-transfer and erasure flows, uninstall retention, and installed browser checks remain pending.
The existing README pin is still the earlier investigation baseline. It is not a final SDK pin or release floor.
Task 06 remains open. E2 preparation receipts: `/tmp/usage-history-luna-E2-prep.json`, `/tmp/usage-history-luna-E2R-prep.json`, `/tmp/usage-history-luna-E2R2-prep.json`, and `/tmp/usage-history-luna-E2T-prep.json`.

### M3/Q3 source validation update (2026-10-30)

The M3 fix makes the legacy `session-cost` GET webhook return HTTP 200 with fixed upgrade guidance.
It returns 404 for a nil request or an unknown key. It reads no query, header, or body value and makes no Host or CLI call.
The manifest sets webhook access to `authenticated`. Only the authenticated `session-usage` action reads or refreshes real usage after current-target validation in the verified workspace.

Q3 passed the full plugin `server/...` Go suite normally and under race.
The focused M3 guard, `go vet`, and `go mod tidy -diff` passed. The bundle suite passed 61 cases, and the state-inspection suite passed 7 cases.
The JavaScript syntax checks also passed. Q3 changed only a private-marker test fixture, not production code.
Receipts: `/tmp/usage-history-luna-M3-ready.json` and `/tmp/usage-history-luna-Q3-ready.json`.

The updated smoke selectors expect the authenticated action on Kandev `v0.97.0` and the legacy GET path on `v0.86.0`.
Both cases expect `upgrade_required`. They do not claim that either older Host supports retained usage.
The older toolbar checks are historical plugin-source evidence only. They do not prove current-package compatibility.
Selector receipts: `/tmp/usage-history-luna-L4-ready.json` and `/tmp/usage-history-luna-O3-ready.json`.

The anonymous-401 assertion exists in the prepared smoke helper. It has not run against an installed Host.
The package rebuild, current Host usage and lifecycle flows, both older-Host browser matrices, and final browser checks remain pending.
The full core benchmark is incomplete. Task 06 remains open.

## E5 SDK pin and source validation (2026-10-30)

`.kandev-sdk-ref` now pins public Kandev commit `9a751dd7d88a12307ee3a23130651cb6a0b8f2d3`.
The pinned Go `pluginsdk` tree is `a9a0a09dc8b30ac02876ccc7e46eb18102079600`, and the checked-out tree matches it.
The plugin resolves `github.com/kdlbs/kandev/pluginsdk` from the sibling checkout for local builds.

The full plugin `server/...` suite passed normally and under the race detector against this SDK.
`go vet`, `go mod tidy -diff`, Go formatting, all 68 Node tests, and JavaScript syntax checks passed.
Logs: `/tmp/usage-history-luna-E5-go-test.log`, `/tmp/usage-history-luna-E5-go-race.log`, `/tmp/usage-history-luna-E5-go-vet.log`, `/tmp/usage-history-luna-E5-tidy-diff.log`, and `/tmp/usage-history-luna-E5-node-all.log`.

The plugin source commit was `1eb3bb1f944820150636cbeef66365f2994004b9`. `make verify-package` built and verified the `0.5.0` archive for Linux amd64/arm64, macOS amd64/arm64, and Windows amd64. The archive SHA-256 is `b53001dc639cddb36c757e798f7c492ec3b2c8fc7543fc73bddf3be35bfb1a9e`.

Five installed-package tests ran against core commit `9a751dd7d88a12307ee3a23130651cb6a0b8f2d3`. The backend used `KANDEV_FEATURES_WORKSPACE_COLLABORATION=true`. Playwright used one worker and zero retries. Desktop passed 1 test and returned errors for 2 tests. Phone passed 1 test and returned an error for 1 test.

Both retained-usage tests expected `CostMetricAbsent=true` for a 150-token row without cost. The Host returned `false`. The Host accepted a report for 1200 subcents. The public stats API returned `cost_subcents=null`.

These errors stopped the flows before the remaining archive, deletion, uninstall, ownership-transfer, and erasure assertions. Those lifecycle and privacy results remain unverified. The plugin assertions did not change.

The Host reduction sets `CostMetricAbsent` from whether a cost source was selected. This matches the observations, but remains an inference. A focused backend test can confirm the cause. Evidence is in `/tmp/usage-history-E5-core-findings.json` and `/tmp/usage-history-luna-E5-ready.json`.

The Go suite includes `TestLegacySessionCostWebhookIsStaticAndDoesNotCallHostOrCLI`. This source test proves that the legacy webhook makes no Host or CLI calls. The installed anonymous-webhook HTTP 401 test did not run.

The current fixture received HTTP 404 from the v0.97 workspace-update API before package upload. The cached v0.97 source is sparse and excludes tracked `apps/tsconfig.json`. Its E2E web build stopped when Vite could not find that file.

The v0.97 test did not establish archive acceptance or `upgrade_required` behavior. No v0.86 binary or source was available, so that test did not run. No older release floor is claimed.

No minimum Kandev release floor is claimed. The parent owns the full-scale core benchmark and core follow-up. Task 06 remains open pending correction of the Host behavior, a successful integration run, and review.
