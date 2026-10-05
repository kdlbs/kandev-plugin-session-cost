---
status: draft
system: session-cost
created: 2026-10-05
owners:
  - kdlbs
---

# Responsive Session Cost Lookups

## Overview

Users must receive cost details or a clear progress state when a transcript scan takes longer than an HTTP request.
Session Cost owns this outcome because it calculates and presents the cost.

## Terminology

- **Snapshot:** The last successful usage report and its calculation time.
- **Pending:** A report calculation is active.
- **Stale:** A displayed value comes from an earlier successful report.

## Requirements

### REQ-SESSION-COST-LOOKUP-001: Responsive lookup and recovery

**Intent:** Keep the composer usable during slow calculations and preserve accurate session attribution.

#### Acceptance criteria

- **AC-SESSION-COST-LOOKUP-001.1:** When report calculation is blocked, a lookup shall return a JSON progress response within five seconds.
- **AC-SESSION-COST-LOOKUP-001.2:** Concurrent lookups shall share at most one active report calculation per plugin process.
- **AC-SESSION-COST-LOOKUP-001.3:** Once calculation succeeds, subsequent lookups shall return the requested session's totals, models, turns, and cost per turn.
- **AC-SESSION-COST-LOOKUP-001.4:** While recalculation is pending or fails, the plugin shall retain the last successful value and identify it as stale.
- **AC-SESSION-COST-LOOKUP-001.5:** Pending lookup shall show calculating or updating. It shall not show zero cost, missing usage, or dependency setup guidance.
- **AC-SESSION-COST-LOOKUP-001.6:** When calculation times out, returns invalid output, or cannot start, details shall show the applicable failure and permit explicit retry.
- **AC-SESSION-COST-LOOKUP-001.7:** Cancellation of one browser request shall not cancel the calculation that other requests share.
- **AC-SESSION-COST-LOOKUP-001.8:** When no active session or transcript exists, the plugin shall avoid report calculation and show the existing applicable empty state.
- **AC-SESSION-COST-LOOKUP-001.9:** When the active session changes, the previous session's response shall not change the new session's cost or details.
- **AC-SESSION-COST-LOOKUP-001.10:** When the plugin stops, it shall cancel its report calculation and release owned subprocesses and timers.

### REQ-SESSION-COST-RECOVERY-002: Clear transport failures

**Intent:** Keep request errors understandable and recoverable on desktop and phones.

#### Acceptance criteria

- **AC-SESSION-COST-RECOVERY-002.1:** An HTML response, including a gateway error, shall show a localized request failure instead of a JSON parser exception or HTML content.
- **AC-SESSION-COST-RECOVERY-002.2:** A JSON error response or invalid report payload shall show a localized failure rather than an empty cost result.
- **AC-SESSION-COST-RECOVERY-002.3:** Desktop hover, keyboard focus, and phone tap shall support progress, failure, and retry for the active session.
- **AC-SESSION-COST-RECOVERY-002.4:** Pending status polling shall stop when details close, the session changes, or the plugin unmounts or is disabled.
- **AC-SESSION-COST-RECOVERY-002.5:** Existing successful responses without progress metadata shall continue to render correctly.
- **AC-SESSION-COST-RECOVERY-002.6:** Error and progress text shall use plugin translations and remain contained within the details surface.
- **AC-SESSION-COST-RECOVERY-002.7:** Reopening details after a successful lookup shall make a normal lookup so the report freshness interval can take effect, while retaining the previous value during the request.

## Out of scope

Host timeout changes, persistent usage storage, historical imports, new permissions, tokscale upgrades, release publication, and marketplace changes.
