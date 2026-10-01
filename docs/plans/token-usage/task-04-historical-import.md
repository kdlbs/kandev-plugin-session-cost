---
id: "04-historical-import"
title: "Historical import and setup"
status: done
wave: 4
depends_on: ["02-background-collection", "03-saved-cost-ui"]
plan: "plan.md"
requirements:
  - REQ-COSTS-TOKEN-USAGE-002
acceptance_criteria:
  - AC-COSTS-TOKEN-USAGE-002.1
  - AC-COSTS-TOKEN-USAGE-002.2
  - AC-COSTS-TOKEN-USAGE-002.3
  - AC-COSTS-TOKEN-USAGE-002.6
  - AC-COSTS-TOKEN-USAGE-002.7
system_design:
  - ../../../../kandev/docs/specs/costs/system-design/token-usage.md
---

# Task 04: Historical import and setup

## Summary

Add an explicit historical import through plugin settings. It shares the collector budget and preserves progress across interruption.

## In scope

- Add authenticated import start/status/cancel/resume actions and settings contribution.
- Use verified dated output or the host design daily-query fallback, with active/final work first.
- Persist bounded cursors and progress, skip ambiguous or inaccessible transcripts, and retain accepted data on cancellation.
- Add desktop/phone setup E2E and update README and CHANGELOG with compatibility and retention behavior.

## Out of scope

- Implicit all-history import on enablement, remote collection and changes to the native stats page.

## Acceptance

- Saving collection settings never starts an import. Import is disabled while collection is off.
- Interrupted import resumes its checkpoint without duplicate rows or invented consumption dates.
- Import and active collection share one subprocess, and both desktop and phone expose progress and cancellation.

## ASCII UI preview

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

See the [combined preview](plan.md#ascii-ui-preview). The relevant view labels match the package manifest.

## Verification

The package validation block was run from the plugin repository root; the executed results are recorded below.

```bash
make test vet build package-host
go test -race ./server/...
node --test test/bundle.test.mjs
node --check ui/bundle.js
```

## Files likely touched

- `server/import.go (new)`
- `server/import_test.go (new)`
- `server/collector.go`
- `server/actions.go`
- `manifest.yaml`
- `ui/bundle.js`
- `test/bundle.test.mjs`
- `README.md`
- `CHANGELOG.md`
- `../kandev/apps/web/e2e/tests/plugins/session-cost-collection.spec.ts (new)`
- `../kandev/apps/web/e2e/tests/plugins/mobile-session-cost-collection.spec.ts (new)`

## Dependencies

02-background-collection, 03-saved-cost-ui.

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

## Risks

Undated cumulative data cannot establish exact daily consumption. Keep that coverage explicit instead of assigning it to import time.

## Parallelism

`sequential`

## Inputs

- [Requirements](../../../../kandev/docs/specs/costs/requirements/token-usage.md) and frontmatter acceptance IDs.
- [System design](../../../../kandev/docs/specs/costs/system-design/token-usage.md).
- [Parent work package](../../../../kandev/docs/plans/token-usage/plan.md).
- Existing source and test seams in [Technical approach](plan.md#technical-approach).

## Results

Implemented explicit owner-scoped historical import start/status/cancel actions, resumable Host state checkpoints, bounded page processing, missing transcript diagnostics, undated coverage labeling and the plugin settings component.

Validation: `make test vet build package-host` passed; `go test -race ./server/...` passed; the bundle suite passed 12/12; and `node --check ui/bundle.js` passed. Installed-plugin browser E2E was not run.
