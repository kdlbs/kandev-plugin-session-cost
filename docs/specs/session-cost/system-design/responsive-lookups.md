---
status: draft
system: session-cost
requirements:
  - REQ-SESSION-COST-LOOKUP-001
  - REQ-SESSION-COST-RECOVERY-002
---

# Responsive Session Cost Lookup Design

## Purpose and boundaries

Decouple report execution from the request lifetime inside the plugin.
Retain the current endpoint, session attribution, totals, thresholds, manifest identity, API version, and immutable SDK pin.

## Requirement mapping

| Requirement | Design sections |
| --- | --- |
| REQ-SESSION-COST-LOOKUP-001 | Report coordinator; Request flow; Failure and lifecycle |
| REQ-SESSION-COST-RECOVERY-002 | Browser flow; Presentation; Compatibility |

## Evidence and repair rationale

Installed version 0.4.0 invokes a complete tokscale report synchronously before filtering rows to the requested transcript.
Its report timeout is 120 seconds. The observed host uses the default 30-second HTTP write timeout.
On 2026-10-05, backend logs recorded a webhook RPC 503 at 16:55:31 Lisbon and a killed npx process at 16:56:01.
A later public probe returned valid session JSON after 90.39 seconds. The host contained approximately 31 GB of Codex transcripts.
The UI calls `response.json()` without checking status or content type.
The screenshot confirms HTML reached that parser. The original HTML emitter and status remain unconfirmed.

A shorter synchronous timeout can suppress the parser symptom but cannot produce costs when scans routinely exceed that timeout.
Increasing a host timeout requires operator changes and continues to occupy long requests.
A shared background snapshot preserves the current tokscale source while allowing short HTTP requests.
An in-memory snapshot avoids a database, migrations, and overlap with persistent collection in open PR #10.
These local design choices and their rationale are contained here; no additional repository-wide ADR is required.

## Components and responsibilities

- `server/plugin.go`: bounded Host reads, session attribution, and snapshot projection.
- New report coordinator: a mutex, one active calculation, one successful snapshot, and one last failure.
- `server/tokscale.go` and platform runner helpers: CLI execution and bounded cancellation.
- `server/main.go`: process-lifetime cancellation for background work.
- `ui/bundle.js`: response validation, progress polling, stale values, and localized errors.

## Report coordinator

Keep one process-local snapshot of usage statistics, indexed by transcript ID. Do not retain transcript contents or prompts.
Preserve model rows and the existing exact-or-Codex-suffix matching rules.
Resolve the configured command before scheduling work. Config changes restart the plugin and clear its snapshot.
Use a 30-second freshness interval and a 10-second failure cooldown.
An explicit refresh bypasses freshness and cooldown, but joins an active calculation.
A completed failure remains visible until retry. Do not retry continuously while details stay open.

Schedule the calculation once under the coordinator lock, then release the lock before CLI execution.
Use a context owned by the plugin, with a 120-second deadline, independently of the initiating HTTP request.
Replace the snapshot atomically only after successful output parsing and aggregation.
Keep the prior snapshot when a calculation fails. Never cache a failure as a successful zero-cost report.
Bound process termination and output-pipe draining. Cancel the launched process tree using supported platform mechanisms.
Tests must verify that a spawned descendant cannot keep the worker blocked after cancellation.

## Data and contracts

Continue serving `GET /api/plugins/kandev-session-cost/webhooks/session-cost?task_id=...&active=...`.
Add `refresh=1` for explicit recalculation. Status polling omits this parameter.

Extend JSON responses with optional fields:

| Field | Meaning |
| --- | --- |
| `report_state` | `ready`, `pending`, or `failed` |
| `report_error` | Optional stable code: `timeout`, `unavailable`, or `failed` |
| `stale` | Existing numeric data comes from an earlier report |

Keep the existing cost fields and `found` semantics. Pending does not mean no usage.
`generated_at` identifies the successful snapshot's completion time. Before a snapshot exists, omit or leave it empty.
Only a definite executable launch failure means `unavailable`. A timeout or invalid CLI output means report failure.
Do not run an installation probe under an expired report context.
Return successful status envelopes with HTTP 200 and `application/json`; reserve non-2xx for malformed requests or Host failures.
Old responses without `report_state` are treated as ready by the UI.
The HTTP route remains GET and requires no new Host API, action, capability, or SDK upgrade.

## Request flow

1. Bound the combined Host config and session reads to three seconds using the request context.
2. Resolve the requested active session through the existing Host reader.
3. If the session or transcript is absent, return the applicable empty response without invoking tokscale.
4. Read the snapshot and coordinator state. Schedule work only for a cold or expired snapshot, or explicit refresh.
5. Return the session projection immediately, with progress or failure metadata when applicable.

Do not await the report in an HTTP handler. A blocked CLI must not prevent another session's metadata response.
Short request deadlines cover Host calls, not shared calculation lifetime.

## Browser flow

Check HTTP status and content type before parsing JSON. Accept JSON media types including `+json` variants.
Convert transport failures, non-JSON responses, malformed JSON, and invalid envelopes into localized error categories.
Do not expose raw HTML, parser exception messages, or backend exception text in the composer.
Validate the report fields consumed by the renderer; reject malformed values rather than displaying zero by coercion.

Keep at most one request and one polling timer per mounted action.
Poll pending status every two seconds while details remain open, without sending `refresh=1` again.
After details close, clear the local loaded marker so a later open makes a normal request and the coordinator's freshness interval can take effect.
Use a ten-second browser request deadline and stop automatic polling after 130 seconds with retry available.
Clear timers and abort obsolete requests on close, session change, unmount, and disable.
Use a request-generation guard so late responses cannot overwrite a newer session or refresh.
A cold response shows calculating. A pending refresh retains costs and shows updating.
A failed refresh retains costs with a stale label and an error. Explicit Refresh starts a new generation.

## Presentation and mobile parity

Retain the existing composer action and details composition. This change adds state and copy rather than a navigation redesign.
Desktop uses hover or focus and click-to-pin. Phone users tap the existing action to pin and use visible Refresh.
The nearest exemplar is this plugin's shipped tap-to-pin action and packaged phone test.
Do not add hover-only recovery. Wrap error text within the existing surface and preserve touch targets of at least 44 pixels.
The details surface owns any overflow; prevent document horizontal overflow.
A replacement phone drawer is outside this repair. The tests must exercise tap activation, not infer touch support from hover.
All new copy uses registered plugin translations, with English fallback and the existing Portuguese catalog.
Follow any applicable repository localization requirements without changing unrelated catalog content.

## Failure and lifecycle

Log bounded report failure categories and duration once per calculation. Do not log reports, transcripts, command output, or secrets.
On plugin termination, cancel the owned worker and subprocess tree. Use the SDK's supported process lifecycle, not an invented lifecycle RPC.
Tests register cleanup for any background worker they create.
Report memory, freshness, cooldown, and failure state reset on restart or config changes.
No durable state, new database, metric labels, or recurring background scans are added.

## Compatibility and risks

Source compatibility follows `.kandev-sdk-ref` at `570600439036e81f8e9e1c63f15c4abce8a6c846`.
The adjacent Kandev task worktree is mutable and must not be used as the verification SDK without checking the pin.
Optional response metadata permits new UI to consume old ready responses; matched server and UI ship in one archive.
The snapshot can lag activity by one freshness interval plus calculation time. Label stale data during refresh or failure.
Linux subprocess behavior needs real helper-process tests. Cross-compilation alone does not prove Windows or macOS process cleanup.
Open PR #10 changes collection, manifest, UI, and dependencies. Resolve overlap against live heads before delivery; do not import its storage model.
