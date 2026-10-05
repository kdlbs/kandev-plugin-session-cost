---
status: done
requirements:
  - REQ-SESSION-COST-LOOKUP-001
  - REQ-SESSION-COST-RECOVERY-002
system_design:
  - ../../specs/session-cost/system-design/responsive-lookups.md
---

# Progress and Transport Recovery

## Outcome and dependency

Details display pending and failed calculations correctly, and HTML errors never appear as parser exceptions.
Depends on task 01's response envelope.
Design: [responsive lookups](../../specs/session-cost/system-design/responsive-lookups.md).
Covers AC-SESSION-COST-LOOKUP-001.4 through .6 and .9, plus AC-SESSION-COST-RECOVERY-002.1 through .6.

## Owned files

`ui/bundle.js`, `test/bundle.test.mjs`, `test/host-action-smoke-helper.mjs`, and existing desktop and phone smoke specs.
New copy uses registered plugin translations. Keep existing English fallback and Portuguese coverage.

## TDD and implementation

1. Extend the response harness with status, content type, headers, abort support, and controllable timers.
2. Add `HTML gateway failure renders a retryable localized error` before changing production code.
3. Verify that the baseline exposes a parser failure or fails the required localized-error assertion.
4. Validate status, JSON content type, report shape, and progress metadata before rendering.
5. Poll only while pending details remain open. Explicit Refresh sends `refresh=1` once; status polls omit it.
6. Preserve existing costs on pending or failed refresh. Stop timers and abort obsolete requests on session change and unmount.
7. Cover pending-to-ready, pending-to-failed, malformed JSON, JSON 503, missing content type, legacy ready payloads, and stale response races.
8. Add desktop and phone rendered scenarios to the existing packaged smoke tests.

## Acceptance

- HTML, malformed JSON, and JSON errors produce a localized retry state without raw response text.
- Pending reports become ready through bounded polling; stale costs remain visible and labeled during refresh and failure.
- Closing, session switching, disabling, or unmounting stops polling and prevents late responses from changing another session.

## ASCII UI preview

Use UI-02 from [the plan](plan.md#ascii-ui-previews):

```text
SESSION COST
(o) Calculating cost...

SESSION COST
$0.58                    Previous result
Couldn't refresh cost. Try again.
[Refresh]
```

Desktop hover and phone tap use the same status order. Pinning exposes retry on desktop; phone tap pins immediately.
Long error text wraps. Phone action and Refresh remain reachable by touch and stay within the viewport.
No drawer or disclosure redesign belongs to this work order.

## Exact verification

From the plugin root:

```sh
node --test --test-name-pattern='HTML gateway failure' test/bundle.test.mjs
node --test test/bundle.test.mjs
node --check ui/bundle.js
```

Run the packaged desktop and phone checks in task 03 before delivery.

## Risks and results

The current fake responses provide only `json()`. Update them to model real Response metadata rather than weakening response validation.
Results: The HTML gateway regression failed against the old bundle with a JSON parser error that included the HTML body. A no-transcript regression also failed because it showed tokscale setup guidance. The current bundle validates transport and report data, then shows the no-transcript state before command guidance. All 26 bundle tests pass. The elapsed-time test simulates 24 pending polls with three-second responses, then verifies the 130-second deadline aborts an in-flight poll. Close, session change, unmount, and retry coverage still passes. Both syntax checks and the packaged desktop and phone scenarios pass under task 03.

PR fixup: `node --test test/bundle.test.mjs` passed all 26 tests after adding coverage that reopening details makes a normal lookup and retains the previous value until the new result arrives. The system design and recovery requirements now state that reopening must let the report freshness interval take effect.
