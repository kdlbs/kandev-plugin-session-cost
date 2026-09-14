---
id: "03-saved-cost-ui"
title: "Saved cost and touch details"
status: done
wave: 3
depends_on: ["01-usage-adapter", "02-background-collection"]
plan: "plan.md"
requirements:
  - REQ-COSTS-TOKEN-USAGE-003
acceptance_criteria:
  - AC-COSTS-TOKEN-USAGE-003.1
  - AC-COSTS-TOKEN-USAGE-003.2
  - AC-COSTS-TOKEN-USAGE-003.3
  - AC-COSTS-TOKEN-USAGE-003.4
system_design:
  - ../../../../kandev/docs/specs/costs/system-design/token-usage.md
---

# Task 03: Saved cost and touch details

## Summary

Show saved core usage immediately and retain it during refresh. Provide the same details and refresh action through desktop popovers and phone drawers.

## In scope

- Separate saved-read loading from refresh state and preserve the last accepted session values.
- Handle no data, upgrade-required, stale values and failed refresh without false zero totals.
- Fence late responses by session and update token/model rows for nullable measurements.
- Add installed-plugin desktop and phone E2E, localization, and updated README behavior.

## Out of scope

- Native statistics, historical import controls and new collection policy.

## Acceptance

- A slow or failed refresh keeps the saved amount visible with a timestamp and status.
- Session switching never exposes the previous session amount or accepts its late response.
- Keyboard and touch users can refresh and dismiss the actual detail surface.

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

- `ui/bundle.js`
- `test/bundle.test.mjs`
- `README.md`
- `../kandev/apps/web/e2e/tests/plugins/session-cost-refresh.spec.ts (new)`
- `../kandev/apps/web/e2e/tests/plugins/mobile-session-cost-refresh.spec.ts (new)`

## Dependencies

01-usage-adapter, 02-background-collection.

## Risks

The local React harness cannot prove touch geometry. Installed-plugin browser tests must exercise the real drawer.

## Parallelism

`sequential`

## Inputs

- [Requirements](../../../../kandev/docs/specs/costs/requirements/token-usage.md) and frontmatter acceptance IDs.
- [System design](../../../../kandev/docs/specs/costs/system-design/token-usage.md).
- [Parent work package](../../../../kandev/docs/plans/token-usage/plan.md).
- Existing source and test seams in [Technical approach](plan.md#technical-approach).

## Results

Implemented saved-first session cost reads, refresh preservation on failure, session response fencing, cache-write and reasoning details, mobile-sized controls and the shared toolbar presentation.

Validation: `make test vet build package-host` passed; the Node bundle suite passed 12/12; and `node --check ui/bundle.js` passed. Installed-plugin desktop and mobile E2E were not run.
