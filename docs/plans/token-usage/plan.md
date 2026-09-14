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

`server/plugin.go` has `sessionCost`, `resolveACPSessionID`, and the `session-cost` webhook.
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
