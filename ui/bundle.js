// Session Cost — chat-toolbar plugin. Registers a coins icon into the
// "chat-input-actions" slot; opening it fetches the current session's cost
// once, and its pinned detail offers an explicit refresh. The plugin backend
// resolves the ACP transcript id server-side via the Host data API, runs
// tokscale, and computes cost-per-turn. The whole
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

function costText(data, value, maxFrac) {
  return data && data.cost_known === false ? "Unavailable" : fmtUSD(value, maxFrac);
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
  var costKnown = d.cost_known !== false;
  var color = costKnown ? tierColor(d.cost, d.warn_threshold, d.high_threshold) : undefined;
  var rows = [
    headerRow(h),
    // Headline amount, coloured by spend tier.
    h(
      "div",
      { style: { fontSize: "22px", fontWeight: 700, lineHeight: 1.1, color: color, fontVariantNumeric: "tabular-nums" } },
      costText(d, d.cost),
    ),
  ];

  // Cost / turn — the headline secondary metric, computed server-side.
  if (costKnown && d.turns > 0) {
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
      statRow(h, "Cache write", fmtCompact(d.cache_write)),
      statRow(h, "Reasoning", fmtCompact(d.reasoning)),
      statRow(h, "Total", fmtCompact(d.total)),
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
            { style: { display: "flex", flexDirection: "column", gap: "2px", minWidth: 0 } },
            h(
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
              h("span", { style: { fontVariantNumeric: "tabular-nums" } }, costText(d, m.cost)),
            ),
            h(
              "div",
              {
                style: {
                  display: "grid",
                  gridTemplateColumns: "repeat(3, minmax(0, 1fr))",
                  columnGap: "8px",
                  paddingLeft: "13px",
                  opacity: 0.6,
                  fontSize: "10px",
                  fontVariantNumeric: "tabular-nums",
                  whiteSpace: "nowrap",
                  overflow: "hidden",
                  textOverflow: "ellipsis",
                },
              },
              h(
                "span",
                { style: { minWidth: 0, overflow: "hidden", textOverflow: "ellipsis", textAlign: "left" } },
                "In " + fmtCompact(m.input),
              ),
              h(
                "span",
                { style: { minWidth: 0, overflow: "hidden", textOverflow: "ellipsis", textAlign: "center" } },
                "Out " + fmtCompact(m.output),
              ),
              h(
                "span",
                { style: { minWidth: 0, overflow: "hidden", textOverflow: "ellipsis", textAlign: "right" } },
                "Cache " + fmtCompact(m.cache_read),
              ),
            ),
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
  if (state.loading && state.data) {
    return h(
      "div",
      { style: { display: "flex", flexDirection: "column", gap: "6px" } },
      costCard(h, state.data),
      h("div", { style: { fontSize: "11px", opacity: 0.7 } }, "Refreshing…"),
    );
  }
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
  if (state.error && state.data) {
    return h(
      "div",
      { style: { display: "flex", flexDirection: "column", gap: "6px" } },
      costCard(h, state.data),
      h(
        "div",
        { style: { fontSize: "11px", color: COLOR.red } },
        (state.data.found ? "Refresh failed: " : "Couldn't load cost: ") + state.error,
      ),
    );
  }
  if (state.error) return stateShell(h, header, "Couldn't load cost: " + state.error);
  var d = state.data;
  if (!d) return stateShell(h, header, "Open to load session cost");
  if (d.tokscale && d.tokscale.installed === false) {
    return stateShell(h, header, "tokscale isn't available — set its command in Settings → Plugins → Session Cost.");
  }
  if (!d.acp_session_id) return stateShell(h, header, "No agent transcript for this session yet — run the agent first.");
  if (!d.found) return stateShell(h, header, "No recorded usage for this session yet.");
  return h(
    "div",
    { style: { display: "flex", flexDirection: "column", gap: "6px" } },
    costCard(h, d),
    d.last_refresh
      ? h("div", { style: { fontSize: "10px", opacity: 0.65 } }, "Saved " + d.last_refresh)
      : null,
    d.error ? h("div", { style: { fontSize: "11px", color: COLOR.red } }, "Refresh failed: " + d.error) : null,
  );
}

// inlineCost is the small coloured amount shown next to the icon once loaded,
// so the chat bar "says the cost" without needing to open the popover.
function inlineCost(h, d) {
  if (!d || !d.found || d.cost_known === false || (d.tokscale && d.tokscale.installed === false)) return null;
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
    var activeSession = ctx.activeSessionId || null;
    var openHook = React.useState(false);
    var open = openHook[0];
    var setOpen = openHook[1];
    var pinnedHook = React.useState(false);
    var pinned = pinnedHook[0];
    var setPinned = pinnedHook[1];
    var stateHook = React.useState({ sessionId: activeSession, loading: false, data: null, error: null });
    var state = stateHook[0];
    var setState = stateHook[1];
    var pinnedRef = React.useRef(false);
    var triggerRef = React.useRef(null);
    var loadedForRef = React.useRef(null);
    var inFlightForRef = React.useRef(null);
    var resetSessionRef = React.useRef(activeSession);

    React.useEffect(
      function () {
        if (resetSessionRef.current === activeSession) return;
        resetSessionRef.current = activeSession;
        pinnedRef.current = false;
        loadedForRef.current = null;
        if (inFlightForRef.current && inFlightForRef.current.sessionId !== activeSession) {
          inFlightForRef.current = null;
        }
        setPinned(false);
        setOpen(false);
        setState({ sessionId: activeSession, loading: false, data: null, error: null });
      },
      [activeSession],
    );

    React.useEffect(function () {
      function closePinnedDetails() {
        pinnedRef.current = false;
        setPinned(false);
        setOpen(false);
      }

      function closeOnOutsidePointer(event) {
        if (!pinnedRef.current || !(event.target instanceof Node)) return;
        if (
          (triggerRef.current && triggerRef.current.contains(event.target)) ||
          (event.target instanceof Element && event.target.closest('[data-slot="tooltip-content"]'))
        ) {
          return;
        }
        closePinnedDetails();
      }

      function closeOnEscape(event) {
        if (event.key === "Escape" && pinnedRef.current) closePinnedDetails();
      }

      document.addEventListener("pointerdown", closeOnOutsidePointer);
      document.addEventListener("keydown", closeOnEscape);
      return function () {
        document.removeEventListener("pointerdown", closeOnOutsidePointer);
        document.removeEventListener("keydown", closeOnEscape);
      };
    }, []);

    var stateMatchesActive = state.sessionId === activeSession;
    var visibleState = stateMatchesActive
      ? state
      : { sessionId: activeSession, loading: false, data: null, error: null };
    var visibleOpen = stateMatchesActive ? open : false;
    var visiblePinned = stateMatchesActive ? pinned : false;

    function load(force) {
      var active = activeSession;
      if (!active) return;
      if (inFlightForRef.current && inFlightForRef.current.sessionId === active) return;
      if (!force && loadedForRef.current === active && (visibleState.data || visibleState.loading)) return;
      var request = { sessionId: active };
      loadedForRef.current = active;
      inFlightForRef.current = request;
      setState(function (current) {
        return { sessionId: active, loading: true, data: current.sessionId === active ? current.data : null, error: null };
      });
      var requestPromise;
      if (host.api.invokeAction) {
        requestPromise = host.api.invokeAction("session-usage", {
          taskId: ctx.taskId || undefined,
          sessionId: active,
          body: { refresh: Boolean(force) },
        });
      } else {
        var qs =
          "webhooks/session-cost?task_id=" +
          encodeURIComponent(ctx.taskId || "") +
          "&active=" +
          encodeURIComponent(active);
        requestPromise = host.api.fetch(qs).then(function (r) {
          return r.json();
        });
      }
      Promise.resolve(requestPromise)
        .then(function (r) {
          var data = r;
          if (inFlightForRef.current !== request) return;
          inFlightForRef.current = null;
          setState(function (current) {
            if (current.sessionId !== active) return current;
            return { sessionId: active, loading: false, data: data, error: null };
          });
        })
        .catch(function (err) {
          if (inFlightForRef.current !== request) return;
          inFlightForRef.current = null;
          setState(function (current) {
            if (current.sessionId !== active) return current;
            return {
              sessionId: active,
              loading: false,
              data: current.sessionId === active ? current.data : null,
              error: String(err && err.message ? err.message : err),
            };
          });
        });
    }

    var loaded = visibleState.data || null;
    var iconColor = loaded && loaded.found ? tierColor(loaded.cost, loaded.warn_threshold, loaded.high_threshold) : undefined;

    return h(
      Tooltip,
      {
        open: visibleOpen,
        onOpenChange: function (nextOpen) {
          if (!nextOpen && stateMatchesActive && pinnedRef.current) return;
          setOpen(nextOpen);
        },
      },
      h(
        TooltipTrigger,
        { asChild: true },
        h(
          Button,
          {
            ref: triggerRef,
            id: "session-cost-action",
            type: "button",
            variant: "ghost",
            size: loaded && loaded.found ? "sm" : "icon",
            className:
              (loaded && loaded.found ? "h-7 px-1.5 " : "h-7 w-7 ") +
              "cursor-pointer text-muted-foreground hover:text-foreground hover:bg-primary/10",
            "aria-label": "Session cost",
            "aria-expanded": visibleOpen,
            onMouseEnter: function () {
              load(false);
            },
            onFocus: function () {
              load(false);
            },
            onClick: function () {
              pinnedRef.current = stateMatchesActive ? !pinnedRef.current : true;
              setPinned(pinnedRef.current);
              setOpen(pinnedRef.current);
              if (pinnedRef.current) load(false);
            },
          },
          coinsIcon(h, 16, iconColor),
          inlineCost(h, loaded),
        ),
      ),
      h(
        TooltipContent,
        { side: "top", align: "end", className: "pointer-events-auto px-3 py-2.5" },
        h(
          "div",
          {
            "aria-busy": visibleState.loading,
            style: { display: "flex", flexDirection: "column", gap: "8px" },
          },
          tooltipBody(h, ui, visibleState),
          visiblePinned
            ? h(
                Button,
                {
                  type: "button",
                  variant: "ghost",
                  size: "sm",
                  className: "min-h-11 w-full cursor-pointer",
                  "aria-label": "Refresh session cost",
                  disabled: visibleState.loading,
                  onClick: function () {
                    load(true);
                  },
                },
                visibleState.loading ? "Refreshing…" : "Refresh",
              )
            : null,
        ),
      ),
    );
  };
}

var IMPORT_TRANSLATIONS = {
  en: {
    importTitle: "Import historical usage",
    importDescription: "Read existing local tokscale sessions into the Token Usage page.",
    importWorkspace: "Workspace",
    importNoWorkspace: "No workspace is available.",
    importNotStarted: "History has not been imported.",
    importRunning: "Import is running.",
    importCompleted: "Import completed.",
    importFailed: "Import failed.",
    importCancelled: "Import was cancelled.",
    importDisabled: "Enable statistics collection before importing history.",
    importProcessed: "Processed {count} sessions",
    importMissing: "{count} sessions have no matching tokscale usage.",
    importUndated: "Some lifetime usage has no source date and stays outside dated charts.",
    importLastSuccessful: "Last successful pass: {value}",
    importStart: "Import history",
    importAgain: "Import again",
    importCancel: "Cancel import",
    importWorking: "Working...",
    importError: "Could not read import status: {message}",
  },
};

function importStatusKey(status) {
  switch (status) {
    case "running":
      return "importRunning";
    case "completed":
      return "importCompleted";
    case "failed":
      return "importFailed";
    case "cancelled":
      return "importCancelled";
    case "disabled":
      return "importDisabled";
    default:
      return "importNotStarted";
  }
}

function importTranslation(t, key, values) {
  return t(key, values ? { values: values } : undefined);
}

function makeHistoricalImportSettings(host) {
  var React = host.React;
  var h = host.jsx;
  var ui = host.ui || {};
  var Card = ui.Card || "div";
  var CardHeader = ui.CardHeader || "div";
  var CardTitle = ui.CardTitle || "div";
  var CardContent = ui.CardContent || "div";
  var Button = ui.Button || "button";
  var Progress = ui.Progress || null;
  var Spinner = ui.Spinner || null;
  var Select = ui.Select || null;
  var SelectContent = ui.SelectContent || null;
  var SelectItem = ui.SelectItem || null;
  var SelectTrigger = ui.SelectTrigger || null;
  var SelectValue = ui.SelectValue || null;

  return function HistoricalImportSettings() {
    var translation = host.i18n && host.i18n.useTranslation
      ? host.i18n.useTranslation()
      : { t: function (key) { return key; } };
    var t = translation.t;
    var context = host.context || {};
    var initialWorkspaceIds = context.getWorkspaceIds ? context.getWorkspaceIds() : [];
    var workspaceIdsState = React.useState(Array.prototype.slice.call(initialWorkspaceIds || []));
    var workspaceIds = workspaceIdsState[0];
    var setWorkspaceIds = workspaceIdsState[1];
    var selectedState = React.useState(function () {
      var active = context.getActiveWorkspaceId ? context.getActiveWorkspaceId() : undefined;
      return active || (workspaceIds.length ? workspaceIds[0] : "");
    });
    var workspaceId = selectedState[0];
    var setWorkspaceId = selectedState[1];
    var statusState = React.useState(null);
    var status = statusState[0];
    var setStatus = statusState[1];
    var loadingState = React.useState(false);
    var loading = loadingState[0];
    var setLoading = loadingState[1];
    var errorState = React.useState("");
    var error = errorState[0];
    var setError = errorState[1];
    var busyState = React.useState(false);
    var busy = busyState[0];
    var setBusy = busyState[1];
    var pollState = React.useState(0);
    var poll = pollState[0];
    var setPoll = pollState[1];

    React.useEffect(function () {
      if (!context.subscribeWorkspaces) return undefined;
      function update(ids) {
        var next = Array.prototype.slice.call(ids || []);
        setWorkspaceIds(next);
        setWorkspaceId(function (current) {
          if (current && next.indexOf(current) >= 0) return current;
          var active = context.getActiveWorkspaceId ? context.getActiveWorkspaceId() : undefined;
          return active || (next.length ? next[0] : "");
        });
      }
      var unsubscribe = context.subscribeWorkspaces(update);
      update(context.getWorkspaceIds ? context.getWorkspaceIds() : workspaceIds);
      return unsubscribe;
    }, []);

    React.useEffect(function () {
      var cancelled = false;
      var timer = null;
      if (!workspaceId || !host.api || !host.api.invokeAction) {
        setStatus(null);
        setLoading(false);
        return undefined;
      }
      function readStatus() {
        setLoading(true);
        host.api.invokeAction("historical-import-status", { workspaceId: workspaceId })
          .then(function (next) {
            if (cancelled) return;
            setStatus(next || null);
            setLoading(false);
            if (next && next.status === "running") {
              timer = setTimeout(function () { setPoll(function (value) { return value + 1; }); }, 2000);
            }
          })
          .catch(function (reason) {
            if (cancelled) return;
            setLoading(false);
            setError(String(reason && reason.message ? reason.message : reason));
          });
      }
      setError("");
      readStatus();
      return function () {
        cancelled = true;
        if (timer !== null) clearTimeout(timer);
      };
    }, [workspaceId, poll]);

    function runAction(action) {
      if (!workspaceId || busy || !host.api || !host.api.invokeAction) return;
      setBusy(true);
      setError("");
      host.api.invokeAction(action, { workspaceId: workspaceId })
        .then(function (next) {
          setStatus(next || null);
          setPoll(function (value) { return value + 1; });
        })
        .catch(function (reason) {
          setError(String(reason && reason.message ? reason.message : reason));
        })
        .then(function () { setBusy(false); });
    }

    var running = Boolean(status && status.status === "running");
    var statusMessage = status
      ? importTranslation(t, importStatusKey(status.status))
      : importTranslation(t, "importNotStarted");
    var processed = status && typeof status.processed === "number" ? status.processed : 0;
    var missing = status && typeof status.missing === "number" ? status.missing : 0;
    var children = [
      h(CardHeader, { key: "header" },
        h(CardTitle, null, importTranslation(t, "importTitle")),
        h("p", { style: { fontSize: "12px", opacity: 0.7, margin: 0 } }, importTranslation(t, "importDescription"))),
      h(CardContent, { key: "content", style: { display: "flex", flexDirection: "column", gap: "12px" } },
        workspaceIds.length > 1 && Select && SelectTrigger && SelectContent && SelectItem
          ? h("label", { style: { display: "flex", flexDirection: "column", gap: "5px", fontSize: "12px" } },
              h("span", null, importTranslation(t, "importWorkspace")),
              h(Select, { value: workspaceId, onValueChange: setWorkspaceId },
                h(SelectTrigger, { "aria-label": importTranslation(t, "importWorkspace") },
                  SelectValue ? h(SelectValue, { placeholder: importTranslation(t, "importWorkspace") }) : workspaceId),
                h(SelectContent, null, workspaceIds.map(function (id) {
                  return h(SelectItem, { key: id, value: id }, id);
                }))))
          : workspaceId
            ? h("div", { style: { fontSize: "12px", opacity: 0.7 } }, importTranslation(t, "importWorkspace") + ": " + workspaceId)
            : h("div", { style: { fontSize: "12px", opacity: 0.7 } }, importTranslation(t, "importNoWorkspace")),
        loading
          ? h("div", { style: { display: "flex", alignItems: "center", gap: "7px", fontSize: "12px", opacity: 0.7 } },
              Spinner ? h(Spinner, { style: { width: "14px", height: "14px" } }) : null,
              importTranslation(t, "importWorking"))
          : h("div", { style: { fontSize: "13px" } }, statusMessage),
        running && Progress ? h(Progress, { "aria-label": statusMessage }) : null,
        status && status.status !== "not_started"
          ? h("div", { style: { display: "flex", flexDirection: "column", gap: "4px", fontSize: "12px", opacity: 0.75 } },
              h("span", null, importTranslation(t, "importProcessed", { count: processed })),
              missing > 0 ? h("span", null, importTranslation(t, "importMissing", { count: missing })) : null,
              status.last_successful_at
                ? h("span", null, importTranslation(t, "importLastSuccessful", { value: status.last_successful_at }))
                : null,
              status.undated ? h("span", null, importTranslation(t, "importUndated")) : null)
          : null,
        status && status.last_error
          ? h("div", { style: { color: COLOR.red, fontSize: "12px" } }, status.last_error)
          : null,
        error
          ? h("div", { style: { color: COLOR.red, fontSize: "12px" } }, importTranslation(t, "importError", { message: error }))
          : null,
        h("div", { style: { display: "flex", flexWrap: "wrap", gap: "8px" } },
          running
            ? h(Button, { type: "button", variant: "outline", size: "sm", className: "min-h-11 cursor-pointer", disabled: busy, onClick: function () { runAction("historical-import-cancel"); } }, importTranslation(t, "importCancel"))
            : h(Button, { type: "button", variant: "outline", size: "sm", className: "min-h-11 cursor-pointer", disabled: busy || !workspaceId, onClick: function () { runAction("historical-import-start"); } }, importTranslation(t, status && status.status === "completed" ? "importAgain" : "importStart")))
      ),
    ];
    return h(Card, { "data-plugin": "kandev-session-cost", "data-testid": "historical-import-settings" }, children);
  };
}

window.registerKandevPlugin("kandev-session-cost", {
  initialize: function (registry, host) {
    if (registry.registerTranslations) registry.registerTranslations(IMPORT_TRANSLATIONS);
    registry.registerComponent("chat-input-actions", makeSessionCostAction(host));
    registry.registerComponent("plugin-settings", makeHistoricalImportSettings(host));
  },
});
