// Session Cost — chat-toolbar plugin. Registers a coins icon into the
// "chat-input-actions" slot; hovering it fetches the current session's cost
// from the plugin backend (which resolves the ACP transcript id server-side
// via the Host data API, runs tokscale, and computes cost-per-turn). The whole
// payload — total spend, cost/turn, per-model split, and the amber/red colour
// thresholds — is produced backend-side; this bundle only renders it.
//
// No build step, no bundled React: everything comes from the shared host.

// ---- colour palette (readable on the popover in both light & dark) --------
var COLOR = {
  green: "#10b981",
  amber: "#f59e0b",
  red: "#ef4444",
  accent: "#6366f1",
};
// Per-model dot palette, cycled by a stable hash of the model name.
var MODEL_DOTS = ["#6366f1", "#10b981", "#f59e0b", "#ec4899", "#06b6d4", "#8b5cf6", "#f43f5e"];

// tierColor maps a session cost to a colour using the backend-supplied
// thresholds: green below warn, amber at/above warn, red at/above high.
function tierColor(cost, warn, high) {
  var w = typeof warn === "number" ? warn : 1;
  var h = typeof high === "number" ? high : 10;
  if (cost >= h) return COLOR.red;
  if (cost >= w) return COLOR.amber;
  return COLOR.green;
}

function dotColor(model) {
  var s = String(model || "");
  var hash = 0;
  for (var i = 0; i < s.length; i++) hash = (hash * 31 + s.charCodeAt(i)) >>> 0;
  return MODEL_DOTS[hash % MODEL_DOTS.length];
}

// ---- formatting -----------------------------------------------------------
function fmtUSD(n, maxFrac) {
  var v = typeof n === "number" && isFinite(n) ? n : 0;
  return "$" + v.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: maxFrac || 2 });
}

// fmtCompact renders large token counts as 1.2K / 3.4M / 5.6B.
function fmtCompact(n) {
  var v = typeof n === "number" && isFinite(n) ? n : 0;
  var abs = Math.abs(v);
  if (abs >= 1e9) return (v / 1e9).toFixed(1).replace(/\.0$/, "") + "B";
  if (abs >= 1e6) return (v / 1e6).toFixed(1).replace(/\.0$/, "") + "M";
  if (abs >= 1e3) return (v / 1e3).toFixed(1).replace(/\.0$/, "") + "K";
  return String(v);
}

// ---- icon -----------------------------------------------------------------
function coinsIcon(h, size, color) {
  var s = size || 16;
  return h(
    "svg",
    {
      xmlns: "http://www.w3.org/2000/svg",
      width: s,
      height: s,
      viewBox: "0 0 24 24",
      fill: "none",
      stroke: color || "currentColor",
      strokeWidth: 2,
      strokeLinecap: "round",
      strokeLinejoin: "round",
      "aria-hidden": "true",
    },
    h("circle", { cx: 8, cy: 8, r: 6 }),
    h("path", { d: "M18.09 10.37A6 6 0 1 1 10.34 18" }),
    h("path", { d: "M7 6h1v4" }),
    h("path", { d: "M16.71 13.88l.7.71-2.82 2.82" }),
  );
}

// ---- popover pieces -------------------------------------------------------
function headerRow(h) {
  return h(
    "div",
    {
      style: {
        display: "flex",
        alignItems: "center",
        gap: "6px",
        opacity: 0.7,
        fontSize: "10px",
        fontWeight: 600,
        letterSpacing: "0.06em",
        textTransform: "uppercase",
      },
    },
    coinsIcon(h, 13, COLOR.accent),
    h("span", null, "Session cost"),
  );
}

function statRow(h, label, value, valueColor) {
  return h(
    "div",
    { style: { display: "flex", justifyContent: "space-between", gap: "16px", fontSize: "11px" } },
    h("span", { style: { opacity: 0.65 } }, label),
    h(
      "span",
      { style: { fontVariantNumeric: "tabular-nums", color: valueColor || undefined, fontWeight: valueColor ? 600 : 400 } },
      value,
    ),
  );
}

function divider(h) {
  return h("div", { style: { height: "1px", background: "currentColor", opacity: 0.12, margin: "2px 0" } });
}

// stateShell wraps a compact status message (loading / empty / error) under the
// same header the populated card uses, so the popover never "jumps".
function stateShell(h, header, body) {
  return h(
    "div",
    { style: { display: "flex", flexDirection: "column", gap: "6px", minWidth: "170px" } },
    header,
    h("div", { style: { fontSize: "12px", opacity: 0.75, lineHeight: 1.35 } }, body),
  );
}

function costCard(h, d) {
  var color = tierColor(d.cost, d.warn_threshold, d.high_threshold);
  var rows = [
    headerRow(h),
    // Headline amount, coloured by spend tier.
    h(
      "div",
      { style: { fontSize: "22px", fontWeight: 700, lineHeight: 1.1, color: color, fontVariantNumeric: "tabular-nums" } },
      fmtUSD(d.cost),
    ),
  ];

  // Cost / turn — the headline secondary metric, computed server-side.
  if (d.turns > 0) {
    rows.push(
      h(
        "div",
        {
          style: {
            display: "flex",
            alignItems: "baseline",
            justifyContent: "space-between",
            gap: "12px",
            fontSize: "11px",
          },
        },
        h("span", { style: { opacity: 0.65 } }, d.turns + (d.turns === 1 ? " turn" : " turns")),
        h(
          "span",
          { style: { color: COLOR.accent, fontWeight: 600, fontVariantNumeric: "tabular-nums" } },
          fmtUSD(d.cost_per_turn, 4) + " / turn",
        ),
      ),
    );
  }

  rows.push(divider(h));
  rows.push(
    h(
      "div",
      { style: { display: "flex", flexDirection: "column", gap: "2px" } },
      statRow(h, "Input", fmtCompact(d.input)),
      statRow(h, "Output", fmtCompact(d.output)),
      statRow(h, "Cache read", fmtCompact(d.cache_read)),
    ),
  );

  var models = d.models || [];
  if (models.length) {
    rows.push(divider(h));
    rows.push(
      h(
        "div",
        { style: { display: "flex", flexDirection: "column", gap: "3px" } },
        models.map(function (m) {
          return h(
            "div",
            { style: { display: "flex", justifyContent: "space-between", alignItems: "center", gap: "16px", fontSize: "11px" } },
            h(
              "span",
              { style: { display: "inline-flex", alignItems: "center", gap: "6px", minWidth: 0 } },
              h("span", {
                style: {
                  width: "7px",
                  height: "7px",
                  borderRadius: "9999px",
                  background: dotColor(m.model),
                  flex: "0 0 auto",
                },
              }),
              h(
                "span",
                { style: { opacity: 0.75, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" } },
                m.model,
              ),
            ),
            h("span", { style: { fontVariantNumeric: "tabular-nums" } }, fmtUSD(m.cost)),
          );
        }),
      ),
    );
  }

  return h(
    "div",
    { style: { display: "flex", flexDirection: "column", gap: "6px", minWidth: "190px" } },
    rows,
  );
}

// tooltipBody renders the popover contents for the current fetch state.
function tooltipBody(h, ui, state) {
  var header = headerRow(h);
  if (state.loading) {
    return stateShell(
      h,
      header,
      h(
        "span",
        { style: { display: "inline-flex", alignItems: "center", gap: "6px" } },
        ui.Spinner ? h(ui.Spinner, { style: { width: "13px", height: "13px" } }) : null,
        "Calculating cost…",
      ),
    );
  }
  if (state.error) return stateShell(h, header, "Couldn't load cost: " + state.error);
  var d = state.data;
  if (!d) return stateShell(h, header, "Hover to load session cost");
  if (d.tokscale && d.tokscale.installed === false) {
    return stateShell(h, header, "tokscale isn't available — set its command in Settings → Plugins → Session Cost.");
  }
  if (!d.acp_session_id) return stateShell(h, header, "No agent transcript for this session yet — run the agent first.");
  if (!d.found) return stateShell(h, header, "No recorded usage for this session yet.");
  return costCard(h, d);
}

// inlineCost is the small coloured amount shown next to the icon once loaded,
// so the chat bar "says the cost" without needing to open the popover.
function inlineCost(h, d) {
  if (!d || !d.found || (d.tokscale && d.tokscale.installed === false)) return null;
  return h(
    "span",
    {
      style: {
        marginLeft: "3px",
        fontSize: "11px",
        fontWeight: 600,
        fontVariantNumeric: "tabular-nums",
        color: tierColor(d.cost, d.warn_threshold, d.high_threshold),
      },
    },
    fmtUSD(d.cost),
  );
}

function makeSessionCostAction(host) {
  var React = host.React;
  var h = host.jsx;
  var ui = host.ui;
  var Button = ui.Button;
  var Tooltip = ui.Tooltip;
  var TooltipTrigger = ui.TooltipTrigger;
  var TooltipContent = ui.TooltipContent;

  return function SessionCostAction(props) {
    var ctx = (props && props.slotProps) || {};
    var stateHook = React.useState({ loading: false, data: null, error: null });
    var state = stateHook[0];
    var setState = stateHook[1];
    var loadedForRef = React.useRef(null);

    function load(force) {
      var active = ctx.activeSessionId;
      if (!active) return;
      if (!force && loadedForRef.current === active && (state.data || state.loading)) return;
      loadedForRef.current = active;
      setState({ loading: true, data: null, error: null });
      var qs =
        "webhooks/session-cost?task_id=" +
        encodeURIComponent(ctx.taskId || "") +
        "&active=" +
        encodeURIComponent(active);
      host.api
        .fetch(qs)
        .then(function (r) {
          return r.json();
        })
        .then(function (data) {
          setState({ loading: false, data: data, error: null });
        })
        .catch(function (err) {
          setState({
            loading: false,
            data: null,
            error: String(err && err.message ? err.message : err),
          });
        });
    }

    var loaded = !state.loading && !state.error ? state.data : null;
    var iconColor = loaded && loaded.found ? tierColor(loaded.cost, loaded.warn_threshold, loaded.high_threshold) : undefined;

    return h(
      Tooltip,
      null,
      h(
        TooltipTrigger,
        { asChild: true },
        h(
          Button,
          {
            id: "session-cost-action",
            type: "button",
            variant: "ghost",
            size: loaded && loaded.found ? "sm" : "icon",
            className:
              (loaded && loaded.found ? "h-7 px-1.5 " : "h-7 w-7 ") +
              "cursor-pointer text-muted-foreground hover:text-foreground hover:bg-primary/10",
            "aria-label": "Session cost",
            onMouseEnter: function () {
              load(false);
            },
            onFocus: function () {
              load(false);
            },
            onClick: function () {
              load(true);
            },
          },
          coinsIcon(h, 16, iconColor),
          inlineCost(h, loaded),
        ),
      ),
      h(TooltipContent, { side: "top", align: "end", className: "px-3 py-2.5" }, tooltipBody(h, ui, state)),
    );
  };
}

window.registerKandevPlugin("kandev-session-cost", {
  initialize: function (registry, host) {
    registry.registerComponent("chat-input-actions", makeSessionCostAction(host));
  },
});
