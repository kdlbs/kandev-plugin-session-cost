---
status: done
requirements:
  - REQ-SESSION-COST-LOOKUP-001
system_design:
  - ../../specs/session-cost/system-design/responsive-lookups.md
---

# Background Report and Prompt Responses

## Outcome and dependency

A blocked CLI does not block the webhook. This is the first work order.
Design: [responsive lookups](../../specs/session-cost/system-design/responsive-lookups.md).
Covers AC-SESSION-COST-LOOKUP-001.1 through .10.

## Owned files

`server/plugin.go`, `server/tokscale.go`, `server/main.go`, new report coordinator and runner helpers, and colocated Go tests.
Keep the manifest, SDK pin, permissions, module replacement, and release identity unchanged.

## TDD and implementation

1. Add `TestHandleWebhook_SlowReportReturnsPending` with an injected runner that blocks until released or canceled.
2. Run the new test before the fix. It must fail because the handler waits for the blocked runner.
3. Add a mutex-protected snapshot and one shared calculation, using the design's request, freshness, and failure limits.
4. Add tests for concurrent sessions, snapshot projection, failed refresh retention, cold failure, no transcript, freshness, explicit refresh, and cooldown.
5. Add tests for caller cancellation, report timeout, malformed output, plugin termination, and a subprocess descendant holding stdout open.
6. Preserve the existing aggregation and exact-or-suffix matching assertions. Use deterministic channels and injected time; avoid long sleeps.

## Acceptance

- Blocked calculation returns pending promptly and a ready response appears after release.
- Concurrent lookups invoke one runner; failure preserves a prior snapshot and permits explicit retry.
- Worker timeout and plugin shutdown release owned process resources without preventing later work.

## Exact verification

Run from the plugin root after the pinned-SDK setup in task 03:

```sh
go test ./server/... -run 'TestHandleWebhook_SlowReportReturnsPending' -count=1
go test -race ./server/... -count=1
make check-format
make vet
```

## Risks and results

OS process-tree cancellation requires platform helpers and runtime tests. Cross-build results alone do not prove cleanup.
Results: The regression failed before the implementation because the webhook waited for the blocked runner. After implementation, the slow-report test, full server tests, race tests, format check, and vet passed. The cancellation test confirmed a child process could not keep output draining blocked on Linux. Windows amd64 and macOS arm64 test binaries cross-compiled; those builds do not prove platform process cleanup at runtime.
