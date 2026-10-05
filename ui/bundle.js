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
var POLL_INTERVAL_MS = 2000;
var POLL_TIMEOUT_MS = 130000;
// Per-model dot palette, cycled by a stable hash of the model name.
var MODEL_DOTS = ["#6366f1", "#10b981", "#f59e0b", "#ec4899", "#06b6d4", "#8b5cf6", "#f43f5e"];

var TRANSLATIONS = {
  en: {
    actionLabel: "Session cost",
    calculatingCost: "Calculating cost…",
    loadCostError: "Couldn't load cost: {{error}}",
    requestFailed: "Couldn't load or refresh cost. Try again.",
    updateFailed: "Couldn't refresh cost. Showing the previous result. Try again.",
    reportFailed: "Couldn't update cost. Try again.",
    reportTimeout: "The cost report took too long. Try again.",
    reportUnavailable: "Couldn't start tokscale. Check its command in Settings → Plugins → Session Cost.",
    updatingCost: "Updating cost. Showing the previous result.",
    previousResult: "Previous result",
    pollingTimeout: "Cost calculation is taking too long. Refresh to try again.",
    pollingTimeoutPrevious: "Cost calculation is taking too long. Showing the previous result. Refresh to try again.",
    requestTimeout: "The session cost request took too long. Try again.",
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
    requestFailed: "Não foi possível carregar ou atualizar o custo. Tente novamente.",
    updateFailed: "Não foi possível atualizar o custo. A mostrar o resultado anterior. Tente novamente.",
    reportFailed: "Não foi possível atualizar o custo. Tente novamente.",
    reportTimeout: "O relatório de custos demorou demasiado tempo. Tente novamente.",
    reportUnavailable: "Não foi possível iniciar o tokscale. Verifique o comando em Definições → Plugins → Session Cost.",
    updatingCost: "A atualizar o custo. A mostrar o resultado anterior.",
    previousResult: "Resultado anterior",
    pollingTimeout: "O cálculo do custo está a demorar demasiado. Atualize para tentar novamente.",
    pollingTimeoutPrevious: "O cálculo do custo está a demorar demasiado. A mostrar o resultado anterior. Atualize para tentar novamente.",
    requestTimeout: "O pedido do custo da sessão demorou demasiado. Tente novamente.",
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
    {
      style: {
        display: "flex",
        flexDirection: "column",
        gap: "6px",
        minWidth: 0,
        maxWidth: "min(90vw, 360px)",
        overflowWrap: "anywhere",
      },
    },
    header,
    h("div", { style: { fontSize: "12px", opacity: 0.75, lineHeight: 1.35 } }, body),
  );
}

function costCard(h, d, t, status) {
  var color = tierColor(d.cost, d.warn_threshold, d.high_threshold);
  var rows = [
    headerRow(h, t),
  ];
  if (status) {
    rows.push(
      h(
        "div",
        {
          role: "status",
          style: { fontSize: "11px", opacity: 0.75, lineHeight: 1.35, overflowWrap: "anywhere" },
        },
        status,
      ),
    );
  }
  rows.push(
    // Headline amount, coloured by spend tier.
    h(
      "div",
      { style: { fontSize: "22px", fontWeight: 700, lineHeight: 1.1, color: color, fontVariantNumeric: "tabular-nums" } },
      fmtUSD(d.cost),
    ),
  );

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
    {
      style: {
        display: "flex",
        flexDirection: "column",
        gap: "6px",
        minWidth: "min(190px, 80vw)",
        maxWidth: "min(90vw, 360px)",
        overflowWrap: "anywhere",
      },
    },
    rows,
  );
}

// tooltipBody renders the popover contents for the current fetch state.
function tooltipBody(h, ui, state, t) {
  var header = headerRow(h, t);
  var d = state.data;
  var hasCost = d && d.found && !(d.tokscale && d.tokscale.installed === false);
  var status = null;
  if (state.error) {
    status = state.error === "poll_timeout"
      ? translate(t, hasCost ? "pollingTimeoutPrevious" : "pollingTimeout")
      : state.error === "request_timeout"
        ? translate(t, hasCost ? "updateFailed" : "requestTimeout")
      : hasCost
        ? translate(t, "updateFailed")
        : state.error === "report_timeout"
          ? translate(t, "reportTimeout")
          : translate(t, "requestFailed");
  } else if (state.pending || (state.loading && hasCost)) {
    status = translate(t, "updatingCost");
  } else if (d && d.report_state === "failed") {
    var reportErrorKey =
      d.report_error === "timeout"
        ? "reportTimeout"
        : d.report_error === "unavailable"
          ? "reportUnavailable"
          : "reportFailed";
    status = hasCost
      ? translate(t, "updateFailed")
      : translate(t, reportErrorKey);
  }
  if ((state.loading || state.pending) && !hasCost && !state.error && (!d || d.report_state !== "failed")) {
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
  if (state.error && !d) return stateShell(h, header, status);
  if (!d) return stateShell(h, header, translate(t, "openToLoadCost"));
  if (status && hasCost) return costCard(h, d, t, status);
  if (status && !d.found) return stateShell(h, header, status);
  if (!d.acp_session_id) return stateShell(h, header, translate(t, "noAgentTranscript"));
  if (d.tokscale && d.tokscale.installed === false) {
    return stateShell(h, header, translate(t, "tokscaleUnavailable"));
  }
  if (!d.found) return stateShell(h, header, translate(t, "noRecordedUsage"));
  return costCard(h, d, t, state.stale || d.stale ? translate(t, "previousResult") : null);
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

function validNumber(value) {
  return typeof value === "number" && isFinite(value) && value >= 0;
}

function validateCostResponse(data) {
  if (!data || typeof data !== "object" || Array.isArray(data)) throw new Error("invalid_response");
  var state = data.report_state === undefined ? "ready" : data.report_state;
  if (!["ready", "pending", "failed"].includes(state)) throw new Error("invalid_response");
  if (typeof data.found !== "boolean" || typeof data.acp_session_id !== "string") throw new Error("invalid_response");
  if (data.stale !== undefined && typeof data.stale !== "boolean") throw new Error("invalid_response");
  if (data.report_error !== undefined && typeof data.report_error !== "string") throw new Error("invalid_response");
  if (data.tokscale !== undefined) {
    if (!data.tokscale || typeof data.tokscale !== "object" || Array.isArray(data.tokscale)) throw new Error("invalid_response");
    if (data.tokscale.installed !== undefined && typeof data.tokscale.installed !== "boolean") throw new Error("invalid_response");
  }
  if (data.models !== undefined && !Array.isArray(data.models)) throw new Error("invalid_response");
  if (data.found) {
    ["cost", "turns", "input", "output", "cache_read"].forEach(function (field) {
      if (!validNumber(data[field])) throw new Error("invalid_response");
    });
    if (data.turns > 0 && !validNumber(data.cost_per_turn)) throw new Error("invalid_response");
    if (!Array.isArray(data.models)) throw new Error("invalid_response");
    ["warn_threshold", "high_threshold"].forEach(function (field) {
      if (data[field] !== undefined && !validNumber(data[field])) throw new Error("invalid_response");
    });
    data.models.forEach(function (model) {
      if (!model || typeof model !== "object" || Array.isArray(model) || typeof model.model !== "string") throw new Error("invalid_response");
      ["cost", "input", "output", "cache_read"].forEach(function (field) {
        if (!validNumber(model[field])) throw new Error("invalid_response");
      });
    });
  }
  data.report_state = state;
  return data;
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
    var stateHook = React.useState({ sessionId: activeSession, loading: false, pending: false, stale: false, data: null, error: null });
    var state = stateHook[0];
    var setState = stateHook[1];
    var pinnedRef = React.useRef(false);
    var triggerRef = React.useRef(null);
    var loadedForRef = React.useRef(null);
    var requestRef = React.useRef(null);
    var generationRef = React.useRef(0);
    var pollTimerRef = React.useRef(null);
    var pollDeadlineRef = React.useRef(null);
    var pollDeadlineTimerRef = React.useRef(null);
    var resetSessionRef = React.useRef(activeSession);
    var activeSessionRef = React.useRef(activeSession);
    activeSessionRef.current = activeSession;

    function clearPollTimer() {
      if (pollTimerRef.current !== null) clearTimeout(pollTimerRef.current);
      pollTimerRef.current = null;
    }

    function clearPollDeadlineTimer() {
      if (pollDeadlineTimerRef.current !== null) clearTimeout(pollDeadlineTimerRef.current);
      pollDeadlineTimerRef.current = null;
    }

    function clearPollDeadline() {
      clearPollDeadlineTimer();
      pollDeadlineRef.current = null;
    }

    function startPollDeadline(sessionId) {
      clearPollDeadline();
      var deadline = { sessionId: sessionId, at: performance.now() + POLL_TIMEOUT_MS };
      pollDeadlineRef.current = deadline;
      return deadline;
    }

    function expirePending(deadline) {
      if (pollDeadlineRef.current !== deadline || activeSessionRef.current !== deadline.sessionId) return;
      clearPollTimer();
      clearPollDeadline();
      var request = requestRef.current;
      if (request && request.sessionId === deadline.sessionId) {
        requestRef.current = null;
        if (request.timeoutTimer !== null) clearTimeout(request.timeoutTimer);
        generationRef.current += 1;
        if (request.controller) request.controller.abort();
      }
      setState(function (current) {
        if (current.sessionId !== deadline.sessionId) return current;
        return Object.assign({}, current, {
          loading: false,
          pending: false,
          stale: Boolean(current.data) || current.stale,
          error: "poll_timeout",
        });
      });
    }

    function cancelRequests() {
      clearPollTimer();
      clearPollDeadlineTimer();
      generationRef.current += 1;
      var request = requestRef.current;
      requestRef.current = null;
      if (!request) return;
      if (request.timeoutTimer !== null) clearTimeout(request.timeoutTimer);
      if (request.force) loadedForRef.current = null;
      if (request.controller) request.controller.abort();
    }

    function closeDetails() {
      cancelRequests();
      pinnedRef.current = false;
      setPinned(false);
      setOpen(false);
      setState(function (current) {
        if (current.sessionId !== activeSessionRef.current) return current;
        return Object.assign({}, current, { loading: false });
      });
    }

    React.useEffect(
      function () {
        if (resetSessionRef.current !== activeSession) {
          resetSessionRef.current = activeSession;
          pinnedRef.current = false;
          loadedForRef.current = null;
          clearPollDeadline();
          setPinned(false);
          setOpen(false);
          setState({ sessionId: activeSession, loading: false, pending: false, stale: false, data: null, error: null });
        }
        return function () {
          cancelRequests();
          clearPollDeadline();
        };
      },
      [activeSession],
    );

    React.useEffect(function () {
      function closeOnOutsidePointer(event) {
        if (!pinnedRef.current || !(event.target instanceof Node)) return;
        if (
          (triggerRef.current && triggerRef.current.contains(event.target)) ||
          (event.target instanceof Element && event.target.closest('[data-slot="tooltip-content"]'))
        ) {
          return;
        }
        closeDetails();
      }

      function closeOnEscape(event) {
        if (event.key === "Escape" && pinnedRef.current) closeDetails();
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
      : { sessionId: activeSession, loading: false, pending: false, stale: false, data: null, error: null };
    var visibleOpen = stateMatchesActive ? open : false;
    var visiblePinned = stateMatchesActive ? pinned : false;

    function load(force, poll) {
      var active = activeSession;
      if (!active) return;
      if (requestRef.current && requestRef.current.sessionId === active) return;
      if (!poll && !force && loadedForRef.current === active) return;
      if (!poll || !pollDeadlineRef.current || pollDeadlineRef.current.sessionId !== active) {
        startPollDeadline(active);
      }
      var controller = typeof AbortController === "function" ? new AbortController() : null;
      var request = {
        sessionId: active,
        generation: generationRef.current,
        controller: controller,
        timeoutTimer: null,
        force: Boolean(force),
        poll: Boolean(poll),
      };
      requestRef.current = request;
      setState(function (current) {
        if (current.sessionId !== active) {
          return { sessionId: active, loading: true, pending: false, stale: false, data: null, error: null };
        }
        return Object.assign({}, current, { loading: true, error: null });
      });
      var qs =
        "webhooks/session-cost?task_id=" +
        encodeURIComponent(ctx.taskId || "") +
        "&active=" +
        encodeURIComponent(active) +
        (force ? "&refresh=1" : "");
      function isCurrent() {
        return requestRef.current === request && request.generation === generationRef.current && activeSession === active;
      }
      function clearRequestTimer() {
        if (request.timeoutTimer !== null) clearTimeout(request.timeoutTimer);
        request.timeoutTimer = null;
      }
      request.timeoutTimer = setTimeout(function () {
        if (!isCurrent()) return;
        clearRequestTimer();
        requestRef.current = null;
        generationRef.current += 1;
        clearPollDeadline();
        loadedForRef.current = active;
        if (controller) controller.abort();
        setState(function (current) {
          if (current.sessionId !== active) return current;
          return Object.assign({}, current, { loading: false, pending: false, error: "request_timeout" });
        });
      }, 10000);

      var fetchOptions = controller ? { signal: controller.signal } : undefined;
      var fetchPromise;
      try {
        fetchPromise = host.api.fetch(qs, fetchOptions);
      } catch {
        fetchPromise = Promise.reject(new Error("request_failed"));
      }
      Promise.resolve(fetchPromise)
        .then(function (r) {
          if (!r || typeof r.json !== "function") throw new Error("invalid_response");
          if (r.ok === false || (typeof r.status === "number" && (r.status < 200 || r.status >= 300))) {
            throw new Error("request_failed");
          }
          var contentType = r.headers && typeof r.headers.get === "function" ? r.headers.get("content-type") : "";
          if (typeof contentType !== "string" || !/^application\/(?:json|[a-z0-9!#$&^_.+-]+\+json)(?:\s*;|$)/i.test(contentType)) {
            throw new Error("invalid_response");
          }
          return Promise.resolve(r.json()).then(validateCostResponse);
        })
        .then(function (data) {
          if (!isCurrent()) return;
          clearRequestTimer();
          requestRef.current = null;
          loadedForRef.current = active;
          if (data.report_state === "pending") {
            if (!pollDeadlineRef.current || pollDeadlineRef.current.sessionId !== active) {
              startPollDeadline(active);
            }
          } else {
            clearPollDeadline();
          }
          setState(function (current) {
            if (current.sessionId !== active) return current;
            return {
              sessionId: active,
              loading: false,
              pending: data.report_state === "pending",
              stale: Boolean(data.stale),
              data: data,
              error: null,
            };
          });
        })
        .catch(function () {
          if (!isCurrent()) return;
          clearRequestTimer();
          requestRef.current = null;
          loadedForRef.current = active;
          clearPollDeadline();
          if (controller) controller.abort();
          setState(function (current) {
            if (current.sessionId !== active) return current;
            return {
              sessionId: active,
              loading: false,
              pending: false,
              stale: Boolean(current.data) || current.stale,
              data: current.data,
              error: "request_failed",
            };
          });
        });
    }

    React.useEffect(
      function () {
        if (!visibleOpen || !visibleState.pending) {
          clearPollTimer();
          clearPollDeadlineTimer();
          return undefined;
        }

        var deadline = pollDeadlineRef.current;
        if (!deadline || deadline.sessionId !== activeSession) deadline = startPollDeadline(activeSession);
        var remaining = deadline.at - performance.now();
        if (remaining <= 0) {
          expirePending(deadline);
          return undefined;
        }
        if (pollDeadlineTimerRef.current === null) {
          pollDeadlineTimerRef.current = setTimeout(function () {
            pollDeadlineTimerRef.current = null;
            expirePending(deadline);
          }, remaining);
        }
        if (visibleState.loading) return undefined;

        var timer = setTimeout(function () {
          if (pollTimerRef.current === timer) pollTimerRef.current = null;
          if (pollDeadlineRef.current !== deadline) return;
          if (performance.now() >= deadline.at) {
            expirePending(deadline);
            return;
          }
          load(false, true);
        }, Math.min(POLL_INTERVAL_MS, remaining));
        pollTimerRef.current = timer;
        return function () {
          if (pollTimerRef.current === timer) {
            clearTimeout(timer);
            pollTimerRef.current = null;
          }
        };
      },
      [visibleOpen, activeSession, visibleState.pending, visibleState.loading],
    );

    var loaded = visibleState.data;
    var iconColor = loaded && loaded.found ? tierColor(loaded.cost, loaded.warn_threshold, loaded.high_threshold) : undefined;
    var hasInlineCost = loaded && loaded.found && !(loaded.tokscale && loaded.tokscale.installed === false);

    function onTriggerClick() {
      pinnedRef.current = stateMatchesActive ? !pinnedRef.current : true;
      setPinned(pinnedRef.current);
      setOpen(pinnedRef.current);
      if (pinnedRef.current) load(false);
      else closeDetails();
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
          if (!nextOpen) {
            if (stateMatchesActive && pinnedRef.current) return;
            closeDetails();
            return;
          }
          setOpen(true);
        },
      },
      h(TooltipTrigger, { asChild: true }, trigger),
      h(
        TooltipContent,
        {
          side: "top",
          align: "end",
          className: "pointer-events-auto px-3 py-2.5",
          style: { maxWidth: "min(90vw, 360px)", overflowWrap: "anywhere" },
        },
        h(
          "div",
          {
            "aria-busy": visibleState.loading || visibleState.pending,
            style: { display: "flex", flexDirection: "column", gap: "8px", minWidth: 0, maxWidth: "min(90vw, 360px)" },
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
                  disabled: visibleState.loading || visibleState.pending,
                  onClick: function () {
                    load(true);
                  },
                },
                visibleState.loading || visibleState.pending ? translate(t, "refreshingCost") : translate(t, "refresh"),
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
