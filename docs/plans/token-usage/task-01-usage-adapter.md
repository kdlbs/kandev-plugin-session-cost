---
id: "01-usage-adapter"
title: "Usage adapter and shared refresh"
status: done
wave: 1
depends_on: []
plan: "plan.md"
requirements:
  - REQ-COSTS-TOKEN-USAGE-001
  - REQ-COSTS-TOKEN-USAGE-002
acceptance_criteria:
  - AC-COSTS-TOKEN-USAGE-001.2
  - AC-COSTS-TOKEN-USAGE-001.4
  - AC-COSTS-TOKEN-USAGE-001.6
  - AC-COSTS-TOKEN-USAGE-001.7
  - AC-COSTS-TOKEN-USAGE-002.2
  - AC-COSTS-TOKEN-USAGE-002.3
  - AC-COSTS-TOKEN-USAGE-002.7
system_design:
  - ../../../../kandev/docs/specs/costs/system-design/token-usage.md
---

# Task 01: Usage adapter and shared refresh

## Summary

Add a typed core-usage adapter and one report coordinator. Manual refresh can save measurements and read canonical values through supported Host APIs.

## In scope

- Declare usage capabilities and authenticated saved-read/refresh actions. Use final SDK names from the host dependency.
- Add normalization, bounded batches, revision checkpoints and explicit compatibility errors.
- Route every tokscale invocation through one coordinator, including concurrent refresh and install probes.
- Preserve manual calculation when collection is off. No scheduled worker starts in this task.

## Out of scope

- Scheduled collection, historical import UI and native accounting policy.

## Acceptance

- Duplicate reports and uncertain write retries reuse their identity without duplicate persisted usage.
- Saved reads do not await a tokscale process, and unauthorized session actions cannot expose global Host data.
- Concurrent compatible requests share a report, and all report scopes obey one active subprocess.

## Verification

The package validation block was run from the plugin repository root; the executed results are recorded below.

```bash
make test vet build package-host
go test -race ./server/...
```

## Files likely touched

- `manifest.yaml`
- `go.mod`
- `go.sum`
- `server/plugin.go`
- `server/tokscale.go`
- `server/usage.go (new)`
- `server/report.go (new)`
- `server/actions.go (new)`
- `server/plugin_test.go`
- `server/usage_test.go (new)`
- `server/report_test.go (new)`
- `server/actions_test.go (new)`

## Dependencies

Host Task 01 and Host Task 02. No plugin-local predecessor.

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

Host Task 01 and Task 02 must finish first. Unknown or incompatible Host support must not silently acknowledge writes.

## Parallelism

`sequential`

## Inputs

- [Requirements](../../../../kandev/docs/specs/costs/requirements/token-usage.md) and frontmatter acceptance IDs.
- [System design](../../../../kandev/docs/specs/costs/system-design/token-usage.md).
- [Parent work package](../../../../kandev/docs/plans/token-usage/plan.md).
- Existing source and test seams in [Technical approach](plan.md#technical-approach).

## Results

Implemented typed Host usage integration, shared report coordination, nullable tokscale field preservation, stable source identity, changed-only idempotent writes, saved reads and refresh actions.

Validation: `make test vet build package-host` passed; `go test -race ./server/...` passed; bundle tests passed 12/12; and `node --check ui/bundle.js` passed.
