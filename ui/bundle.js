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

var TRANSLATIONS = {
  en: {
    actionLabel: "Session cost",
    calculatingCost: "Calculating cost…",
    loadCostError: "Couldn't load cost: {{error}}",
    openToLoadCost: "Open to load session cost",
    tokscaleUnavailable: "tokscale isn't available — set its command in Settings → Plugins → Session Cost.",
    noAgentTranscript: "No agent transcript for this session yet — run the agent first.",
    noRecordedUsage: "No recorded usage for this session yet.",
    turnCount_one: "{{count}} turn",
    turnCount_other: "{{count}} turns",
    costPerTurn: "{{amount}} / turn",
    input: "Input",
    output: "Output",
    cacheRead: "Cache read",
    modelInput: "In {{count}}",
    modelOutput: "Out {{count}}",
    modelCacheRead: "Cache {{count}}",
    refreshSessionCost: "Refresh session cost",
    refreshingCost: "Refreshing…",
    refresh: "Refresh",
  },
  "pt-pt": {
    actionLabel: "Custo da sessão",
    calculatingCost: "A calcular o custo…",
    loadCostError: "Não foi possível carregar o custo: {{error}}",
    openToLoadCost: "Abra para carregar o custo da sessão",
    tokscaleUnavailable: "tokscale não está disponível — defina o comando em Definições → Plugins → Session Cost.",
    noAgentTranscript: "Ainda não existe uma transcrição do agente para esta sessão — execute o agente primeiro.",
    noRecordedUsage: "Ainda não há utilização registada para esta sessão.",
    turnCount_one: "{{count}} turno",
    turnCount_other: "{{count}} turnos",
    costPerTurn: "{{amount}} / turno",
    input: "Entrada",
    output: "Saída",
    cacheRead: "Leitura da cache",
    modelInput: "Entrada {{count}}",
    modelOutput: "Saída {{count}}",
    modelCacheRead: "Cache {{count}}",
    refreshSessionCost: "Atualizar o custo da sessão",
    refreshingCost: "A atualizar…",
    refresh: "Atualizar",
  },
};

function englishMessage(key, options) {
  var count = options && options.count;
  var pluralKey = count === undefined ? key : key + (count === 1 ? "_one" : "_other");
  return TRANSLATIONS.en[pluralKey] || TRANSLATIONS.en[key] || key;
}

function interpolate(message, options) {
  var values = Object.assign({}, (options && options.values) || {});
  if (options && options.count !== undefined) values.count = options.count;
  return String(message).replace(/\{\{([a-zA-Z0-9_]+)\}\}/g, function (_match, name) {
    return values[name] === undefined ? "" : String(values[name]);
  });
}

function translate(t, key, options) {
  var settings = options || {};
  var defaultValue = englishMessage(key, settings);
  if (typeof t === "function") {
    return t(key, {
      defaultValue: defaultValue,
      count: settings.count,
      values: settings.values,
    });
  }
  return interpolate(defaultValue, settings);
}

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
function headerRow(h, t) {
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
    h("span", null, translate(t, "actionLabel")),
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

function costCard(h, d, t) {
  var color = tierColor(d.cost, d.warn_threshold, d.high_threshold);
  var rows = [
    headerRow(h, t),
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
        h("span", { style: { opacity: 0.65 } }, translate(t, "turnCount", { count: d.turns })),
        h(
          "span",
          { style: { color: COLOR.accent, fontWeight: 600, fontVariantNumeric: "tabular-nums" } },
          translate(t, "costPerTurn", { values: { amount: fmtUSD(d.cost_per_turn, 4) } }),
        ),
      ),
    );
  }

  rows.push(divider(h));
  rows.push(
    h(
      "div",
      { style: { display: "flex", flexDirection: "column", gap: "2px" } },
      statRow(h, translate(t, "input"), fmtCompact(d.input)),
      statRow(h, translate(t, "output"), fmtCompact(d.output)),
      statRow(h, translate(t, "cacheRead"), fmtCompact(d.cache_read)),
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
              h("span", { style: { fontVariantNumeric: "tabular-nums" } }, fmtUSD(m.cost)),
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
                translate(t, "modelInput", { values: { count: fmtCompact(m.input) } }),
              ),
              h(
                "span",
                { style: { minWidth: 0, overflow: "hidden", textOverflow: "ellipsis", textAlign: "center" } },
                translate(t, "modelOutput", { values: { count: fmtCompact(m.output) } }),
              ),
              h(
                "span",
                { style: { minWidth: 0, overflow: "hidden", textOverflow: "ellipsis", textAlign: "right" } },
                translate(t, "modelCacheRead", { values: { count: fmtCompact(m.cache_read) } }),
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
function tooltipBody(h, ui, state, t) {
  var header = headerRow(h, t);
  if (state.loading) {
    return stateShell(
      h,
      header,
      h(
        "span",
        { style: { display: "inline-flex", alignItems: "center", gap: "6px" } },
        ui.Spinner ? h(ui.Spinner, { style: { width: "13px", height: "13px" } }) : null,
        translate(t, "calculatingCost"),
      ),
    );
  }
  if (state.error) return stateShell(h, header, translate(t, "loadCostError", { values: { error: state.error } }));
  var d = state.data;
  if (!d) return stateShell(h, header, translate(t, "openToLoadCost"));
  if (d.tokscale && d.tokscale.installed === false) {
    return stateShell(h, header, translate(t, "tokscaleUnavailable"));
  }
  if (!d.acp_session_id) return stateShell(h, header, translate(t, "noAgentTranscript"));
  if (!d.found) return stateShell(h, header, translate(t, "noRecordedUsage"));
  return costCard(h, d, t);
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
  var useTranslation = host.i18n && typeof host.i18n.useTranslation === "function"
    ? host.i18n.useTranslation
    : null;

  return function SessionCostAction(props) {
    var translation = useTranslation ? useTranslation() : null;
    var t = translation && translation.t;
    var actionLabel = translate(t, "actionLabel");
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
      setState({ sessionId: active, loading: true, data: null, error: null });
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
              data: null,
              error: String(err && err.message ? err.message : err),
            };
          });
        });
    }

    var loaded = !visibleState.loading && !visibleState.error ? visibleState.data : null;
    var iconColor = loaded && loaded.found ? tierColor(loaded.cost, loaded.warn_threshold, loaded.high_threshold) : undefined;
    var hasInlineCost = loaded && loaded.found && !(loaded.tokscale && loaded.tokscale.installed === false);

    function onTriggerClick() {
      pinnedRef.current = stateMatchesActive ? !pinnedRef.current : true;
      setPinned(pinnedRef.current);
      setOpen(pinnedRef.current);
      if (pinnedRef.current) load(false);
    }

    var actionTone =
      iconColor === COLOR.red ? "danger" : iconColor === COLOR.amber ? "warning" : iconColor ? "success" : "neutral";
    var trigger =
      typeof ui.Action === "function"
        ? h(ui.Action, {
            ref: triggerRef,
            id: "session-cost-action",
            label: actionLabel,
            icon: coinsIcon(h, 16, iconColor),
            text: loaded && loaded.found ? fmtUSD(loaded.cost) : undefined,
            tone: actionTone,
            pressed: visiblePinned,
            tooltip: "",
            "aria-expanded": visibleOpen,
            onMouseEnter: function () {
              load(false);
            },
            onFocus: function () {
              load(false);
            },
            onClick: onTriggerClick,
          })
        : h(
            Button,
            {
              ref: triggerRef,
              id: "session-cost-action",
              type: "button",
              variant: "ghost",
              size: loaded && loaded.found ? "sm" : "icon",
              className:
                (loaded && loaded.found ? "h-7 px-1.5 " : "h-7 w-7 ") +
                (ctx.presentation === "mobile" ? "min-h-11 min-w-11 " : "") +
                "[@media(pointer:coarse)]:h-11 " +
                (hasInlineCost ? "" : "[@media(pointer:coarse)]:w-11 ") +
                "cursor-pointer text-muted-foreground hover:text-foreground hover:bg-primary/10",
              "aria-label": actionLabel,
              "aria-expanded": visibleOpen,
              onMouseEnter: function () {
                load(false);
              },
              onFocus: function () {
                load(false);
              },
              onClick: onTriggerClick,
            },
            coinsIcon(h, 16, iconColor),
            inlineCost(h, loaded),
          );

    return h(
      Tooltip,
      {
        open: visibleOpen,
        onOpenChange: function (nextOpen) {
          if (!nextOpen && stateMatchesActive && pinnedRef.current) return;
          setOpen(nextOpen);
        },
      },
      h(TooltipTrigger, { asChild: true }, trigger),
      h(
        TooltipContent,
        { side: "top", align: "end", className: "pointer-events-auto px-3 py-2.5" },
        h(
          "div",
          {
            "aria-busy": visibleState.loading,
            style: { display: "flex", flexDirection: "column", gap: "8px" },
          },
          tooltipBody(h, ui, visibleState, t),
          visiblePinned
            ? h(
                Button,
                {
                  type: "button",
                  variant: "ghost",
                  size: "sm",
                  className: "min-h-11 w-full cursor-pointer",
                  "aria-label": translate(t, "refreshSessionCost"),
                  disabled: visibleState.loading,
                  onClick: function () {
                    load(true);
                  },
                },
                visibleState.loading ? translate(t, "refreshingCost") : translate(t, "refresh"),
              )
            : null,
        ),
      ),
    );
  };
}

window.registerKandevPlugin("kandev-session-cost", {
  initialize: function (registry, host) {
    if (typeof registry.registerTranslations === "function") {
      registry.registerTranslations(TRANSLATIONS);
    }
    registry.registerComponent("chat-input-actions", makeSessionCostAction(host));
  },
});
