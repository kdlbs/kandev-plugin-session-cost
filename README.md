# kandev-session-cost

A [kandev](https://github.com/kdlbs/kandev) plugin that adds a **cost icon to
the chat bar** showing the spend of the current session — total cost,
**cost-per-turn**, and a per-model breakdown — parsed from your local agent CLI
transcript by the [tokscale](https://github.com/junhoyeo/tokscale) CLI.

The amount is colour-coded by how much you've spent (green → amber → red), and
after the first hover the cost stays pinned next to the icon so the bar always
"says the cost".

## What it does

- A coins button in the chat composer toolbar (`chat-input-actions` slot,
  desktop + mobile). Hover / focus / click loads the current session's cost.
- **Hover popover**: the headline amount (colour-coded by spend tier),
  **cost / turn**, token totals (input / output / cache read), and a per-model
  table with a coloured dot per model.
- **Inline amount**: once loaded, the total is shown next to the icon in the
  tier colour.
- Reads spend from tokscale, keyed on the agent transcript id. The backend
  resolves the composer's kandev session id to its ACP transcript id via the
  Host data API (`api_read: ["sessions"]`) — the UI only ever sends kandev ids.
- **Cost per turn is computed server-side**: tokscale reports the session's
  message (turn) count, and the backend returns `cost / turns`.
- When tokscale is unavailable the popover explains how to fix it instead of
  erroring.

## Settings

Settings → Plugins → Session Cost (generated from the manifest `config_schema`):

- `command` — explicit tokscale invocation (path or full command); empty
  auto-detects (`tokscale` on PATH, else pinned `npx -y tokscale@4.5.3`).
- `warn_threshold` (USD, default 1) — amount turns **amber** at or above this.
- `high_threshold` (USD, default 10) — amount turns **red** at or above this.

## Layout

- `manifest.yaml` — one GET webhook (`session-cost`), the UI bundle, the
  `api_read: ["sessions"]` capability, and the `config_schema`.
- `server/` — Go backend half (`pluginsdk.Plugin`), spawned by kandev over the
  gRPC plugin contract. Maps kandev session id → ACP transcript id, runs
  tokscale grouped by session, and computes cost-per-turn. A failed tokscale
  run degrades to an install-status payload rather than a 500.
- `ui/bundle.js` — hand-written, no-build ES module using the shared host React
  instance and `host.ui` components. It only renders the backend payload.

## Build & install

Requires a kandev monorepo checkout at `../kandev` (see the `replace` directive
in `go.mod`).

```sh
make test           # unit tests (no tokscale needed — the runner is injected)
make package-host   # tarball for this machine only (fast iteration)
make package        # tarball for all 5 supported platforms
```

Install the tarball via Settings → Plugins → Install plugin (upload), or:

```sh
curl -F package=@kandev-session-cost-0.1.0.tar.gz http://localhost:8080/api/plugins/install
```
