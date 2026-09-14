---
id: "02-background-collection"
title: "Optional background collection"
status: done
wave: 2
depends_on: ["01-usage-adapter"]
plan: "plan.md"
requirements:
  - REQ-COSTS-TOKEN-USAGE-002
acceptance_criteria:
  - AC-COSTS-TOKEN-USAGE-002.1
  - AC-COSTS-TOKEN-USAGE-002.2
  - AC-COSTS-TOKEN-USAGE-002.3
  - AC-COSTS-TOKEN-USAGE-002.4
  - AC-COSTS-TOKEN-USAGE-002.5
  - AC-COSTS-TOKEN-USAGE-002.6
  - AC-COSTS-TOKEN-USAGE-002.7
system_design:
  - ../../../../kandev/docs/specs/costs/system-design/token-usage.md
---

# Task 02: Optional background collection

## Summary

Add a single lifecycle-owned collector with operator settings. It collects eligible sessions and retries final usage without a browser tab.

## In scope

- Add validated collection settings and persist pending final work and source revisions.
- Use lifecycle events with bounded paginated reconciliation for restart gaps.
- Skip idle scans, coalesce work, back off failures and cancel the subprocess tree on shutdown.
- Add manifest settings tests and update README settings documentation.

## Out of scope

- Historical import controls and cost-detail markup.

## Acceptance

- Collection defaults off, rejects invalid intervals and performs no scheduled command while disabled or idle.
- Sessions beyond the first page and sessions ending between polls receive collection attempts.
- Restart and delayed final writes preserve pending work without duplicate totals or concurrent processes.

## ASCII UI preview

### UI-P02: Collection settings and historical import

This task implements only the collection settings region. Task 04 adds import.

```text
Collect token statistics          [Off / On]
Collection interval (minutes)     [5]
[Save]
```

The phone uses the same single-column form with 44px touch controls.
Covers AC-COSTS-TOKEN-USAGE-002.1 and .2.

See the [combined preview](plan.md#ascii-ui-preview). The relevant view labels match the package manifest.

## Verification

The package validation block was run from the plugin repository root; the executed results are recorded below.

```bash
make test vet build package-host
go test -race ./server/...
```

Rendered settings evidence belongs to Task 04 and its installed-plugin settings scenarios.

## Files likely touched

- `manifest.yaml`
- `server/plugin.go`
- `server/collector.go (new)`
- `server/collector_test.go (new)`
- `server/report.go`
- `README.md`

## Dependencies

01-usage-adapter.

## Risks

Events can be missed. Reconciliation must remain SQL-filtered and bounded. Settings saves restart the process.

## Parallelism

`sequential`

## Inputs

- [Requirements](../../../../kandev/docs/specs/costs/requirements/token-usage.md) and frontmatter acceptance IDs.
- [System design](../../../../kandev/docs/specs/costs/system-design/token-usage.md).
- [Parent work package](../../../../kandev/docs/plans/token-usage/plan.md).
- Existing source and test seams in [Technical approach](plan.md#technical-approach).

## Results

Implemented validated opt-in collection settings, one lifecycle-owned worker, paginated reconciliation, idle skipping, final transcript retries, process cancellation, restart-safe checkpoints and bounded shared report execution.

Validation: `make test vet build package-host` passed and `go test -race ./server/...` passed. No live tokscale benchmark was run.
