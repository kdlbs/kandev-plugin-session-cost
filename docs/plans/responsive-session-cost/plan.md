---
created: 2026-10-05
status: in_progress
requirements:
  - REQ-SESSION-COST-LOOKUP-001
  - REQ-SESSION-COST-RECOVERY-002
system_design:
  - ../../specs/session-cost/system-design/responsive-lookups.md
legacy_specs: []
---

# Responsive Session Cost Fix Plan

## Outcome and evidence

Return progress promptly while one shared report runs. Retain stale cost during refresh and handle transport failures clearly.
The confirmed failure mechanism and remaining HTML-source uncertainty are recorded in the [design](../../specs/session-cost/system-design/responsive-lookups.md).
The plugin owns this repair. No Kandev production changes are required.

## Requirements and delivery order

The [requirements](../../specs/session-cost/requirements/responsive-lookups.md) define both backend behavior and visible recovery.
Work runs sequentially in the primary conversation. This plan does not authorize subagents.

| Order | Work order | Status |
| --- | --- | --- |
| 1 | [Background report and prompt responses](task-01-background-report.md) | done |
| 2 | [Progress and transport recovery](task-02-progress-and-recovery.md) | done |
| 3 | [Package verification and PR delivery](task-03-package-and-pr.md) | done |

## Assumption check

The user requested a plugin fix and PR after a read-only diagnosis.
Verified baseline: main and this branch point to `91be5a0046b869fbb87551d8753ea094e0adc349`, release 0.4.0.
The plugin has no existing requirement or system-design documents. Its root README describes current behavior.
The proposed snapshot remains in memory and preserves tokscale as the source. No installation or host timeout changes are assumed.
Freshness and polling intervals are reversible implementation choices. They do not introduce operator settings.
The existing details composition remains intact on desktop and phone.

## Scope and exclusions

Include report coordination, lifecycle cancellation, response metadata, browser state, translation, tests, and operator documentation.
Exclude persistent collection from PR #10, historical imports, manifest version changes, SDK upgrades, releases, and live plugin installation.
The user authorized commit, push, and PR creation. Those actions follow implementation and checks in a later turn.

## ASCII UI previews

UI-01: Existing desktop hover error, from the screenshot:

```text
SESSION COST
Couldn't load cost: Unexpected token '<',
"<!DOCTYPE ..." is not valid JSON
```

UI-02: Proposed cold, pending, and failure content:

```text
SESSION COST
(o) Calculating cost...
[Refresh disabled while pending]

SESSION COST
$0.58                    535 turns
(o) Updating cost. Showing previous result.
[Refresh disabled while pending]

SESSION COST
$0.58                    Previous result
Couldn't refresh cost. Try again.
[Refresh]
```

The amount and turn count are illustrative. The order of status, retained data, and retry is required.
Desktop hover omits the Refresh row until the user pins the details.
Phone tap pins the same state hierarchy. Refresh remains visible and touch accessible; long failure copy wraps within the details surface.
This repair does not introduce a new phone layout or alter the existing disclosure primitive.
UI-02 covers AC-SESSION-COST-LOOKUP-001.4 through .6 and AC-SESSION-COST-RECOVERY-002.1 through .3 and .6.

## Verification and delivery

Each work order defines its exact regression checks. The final order runs the package and desktop/phone checks.
Use a disposable host with mock report commands. Never install into the user's live instance.
Resolve `.kandev-sdk-ref` into a private detached SDK checkout and use an external Go workspace override.
Keep the task's existing Kandev worktree unchanged.
Before commit, re-read main and PR #10 and reconcile changed contracts if either moved.
Open a PR against main after required checks pass. No release, merge, or publication is authorized.

## Risks

A long scan still takes time; progress and stale values must remain accurate throughout that scan.
Cancellation must bound pipe draining and release owned descendants. Platform-specific cleanup requires runtime evidence where available.
Tests must use injected clocks and blocked runners rather than waiting 120 seconds.
PR #10 overlaps this change. Record any remaining overlap in the PR description.

## Results

All three work orders are complete. Source, package, desktop, phone, and documentation checks pass. Commit `98d316bafd8f9e0bfe3bb6342321d3360969bb16` is on `feature/diagnose-session-cos-3b5`, and [PR #14](https://github.com/kdlbs/kandev-plugin-session-cost/pull/14) is open. Required GitHub CI checks were queued at the last check.
