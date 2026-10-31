import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

function bundleSource() {
  return readFileSync(new URL("../ui/bundle.js", import.meta.url), "utf8");
}

function element(type, props, ...children) {
  const node = { type, props: props || {}, children: children.flat(Infinity) };
  if (node.props.ref && typeof node.props.ref === "object") {
    node.props.ref.current = {
      contains(target) {
        return target === node.dom || Boolean(target && target.insideTrigger);
      },
    };
  }
  node.dom = new FakeElement();
  return node;
}

class FakeNode {}

class FakeElement extends FakeNode {
  constructor(closestResult = null) {
    super();
    this.closestResult = closestResult;
  }

  closest(selector) {
    return selector === '[data-slot="tooltip-content"]' ? this.closestResult : null;
  }
}

function createDocument() {
  const listeners = new Map();
  return {
    addEventListener(type, listener) {
      if (!listeners.has(type)) listeners.set(type, new Set());
      listeners.get(type).add(listener);
    },
    removeEventListener(type, listener) {
      listeners.get(type)?.delete(listener);
    },
    dispatchEvent(event) {
      for (const listener of listeners.get(event.type) || []) listener(event);
    },
  };
}

function findElement(node, predicate) {
  if (Array.isArray(node)) {
    for (const child of node) {
      const match = findElement(child, predicate);
      if (match) return match;
    }
    return null;
  }
  if (!node || typeof node !== "object") return null;
  if (predicate(node)) return node;
  return findElement(node.children, predicate);
}

function flushPromises() {
  return new Promise((resolve) => setImmediate(resolve));
}

function createFakeTimers() {
  let now = 0;
  let nextId = 1;
  const timers = new Map();
  return {
    setTimeout(callback, delay = 0) {
      const id = nextId++;
      timers.set(id, { callback, due: now + Number(delay || 0) });
      return id;
    },
    clearTimeout(id) {
      timers.delete(id);
    },
    now() {
      return now;
    },
    advance(duration) {
      const end = now + duration;
      while (true) {
        const next = [...timers.entries()]
          .filter(([, timer]) => timer.due <= end)
          .sort((a, b) => a[1].due - b[1].due)[0];
        if (!next) break;
        const [id, timer] = next;
        timers.delete(id);
        now = timer.due;
        timer.callback();
      }
      now = end;
    },
    size() {
      return timers.size;
    },
  };
}

function costResponse(overrides = {}) {
  return {
    found: true,
    cost: 1,
    turns: 1,
    cost_per_turn: 1,
    input: 10,
    output: 5,
    cache_read: 0,
    models: [],
    tokscale: { installed: true },
    acp_session_id: "acp-1",
    ...overrides,
  };
}

function savedCanonicalMetricResponse() {
  const amounts = { input: "100", output: "0", cache_read: "0", cache_write: "0", reasoning: "0", total: "100", turns: "2" };
  return costResponse({
    saved: true, report_state: "ready", projection_status: "ready", projection_generation: "canonical-metric-generation",
    cost: 0.002, cost_known: true, cost_basis: "estimated", currency: "USD", cost_per_turn: 0.001,
    input: 100, output: 0, cache_read: 0, cache_write: 0, reasoning: 0, total: 100, turns: 2,
    token_amounts: { ...amounts },
    models: [{
      model: "exact/model", provider: "provider", input: 100, output: 0, cache_read: 0, cache_write: 0, reasoning: 0,
      token_amounts: { ...amounts }, cost: 0.002, cost_known: true, cost_basis: "estimated", currency: "USD", cost_subcents_exact: "20",
    }],
  });
}

function renderedText(node) {
  if (Array.isArray(node)) return node.map(renderedText).join("");
  if (node == null || typeof node === "boolean") return "";
  if (typeof node !== "object") return String(node);
  return renderedText(node.children);
}

function createReactHarness() {
  const hooks = [];
  let pendingEffects = [];
  let component;
  let props;
  let cursor = 0;
  let tree;
  let treeBeforeEffects;
  let renderDepth = 0;

  const React = {
    useState(initial) {
      const index = cursor++;
      if (!(index in hooks)) hooks[index] = typeof initial === "function" ? initial() : initial;
      return [
        hooks[index],
        (next) => {
          hooks[index] = typeof next === "function" ? next(hooks[index]) : next;
          render();
        },
      ];
    },
    useRef(initial) {
      const index = cursor++;
      if (!(index in hooks)) hooks[index] = { current: initial };
      return hooks[index];
    },
    useEffect(effect, dependencies) {
      const index = cursor++;
      const previous = hooks[index];
      const changed =
        !previous ||
        !dependencies ||
        !previous.dependencies ||
        dependencies.some((value, dependencyIndex) => value !== previous.dependencies[dependencyIndex]);
      if (!changed) return;
      pendingEffects.push(() => {
        if (previous && previous.cleanup) previous.cleanup();
        hooks[index] = { dependencies, cleanup: null };
        hooks[index].cleanup = effect();
      });
    },
  };

  function render(nextProps) {
    if (nextProps) props = nextProps;
    const outerRender = renderDepth === 0;
    renderDepth += 1;
    cursor = 0;
    tree = component(props);
    if (outerRender) treeBeforeEffects = tree;
    const effects = pendingEffects;
    pendingEffects = [];
    effects.forEach((effect) => effect());
    renderDepth -= 1;
    return tree;
  }

  return {
    React,
    mount(nextComponent, nextProps) {
      component = nextComponent;
      props = nextProps;
      return render();
    },
    render,
    tree() {
      return tree;
    },
    treeBeforeEffects() {
      return treeBeforeEffects;
    },
  };
}

function createActionHarness(options = {}) {
  let definition;
  let Action;
  let translations = {};
  let Settings;
  const requests = [];
  const react = createReactHarness();
  const document = createDocument();
  const timers = createFakeTimers();
  const ui = Object.fromEntries(
    ["Button", "Spinner", "Tooltip", "TooltipTrigger", "TooltipContent"].map((name) => [name, name]),
  );
  if (options.action) ui.Action = function Action() {};
  const sandbox = {
    window: {
      registerKandevPlugin(_id, nextDefinition) {
        definition = nextDefinition;
      },
    },
    encodeURIComponent,
    performance: {now: timers.now},
    AbortController,
    setTimeout: timers.setTimeout,
    clearTimeout: timers.clearTimeout,
    document,
    Element: FakeElement,
    isFinite,
    Math,
    Node: FakeNode,
    String,
  };
  vm.runInNewContext(bundleSource(), sandbox);

  const host = {
    React: react.React,
    jsx: element,
    ui,
    ...(options.locale
      ? {
          i18n: {
            useTranslation() {
              return {
                t(key, translationOptions = {}) {
                  const count = translationOptions.count;
                  const pluralKey = count === undefined ? key : `${key}_${count === 1 ? "one" : "other"}`;
                  const message =
                    translations[options.locale]?.[pluralKey] ??
                    translations[options.locale]?.[key] ??
                    translations.en?.[pluralKey] ??
                    translationOptions.defaultValue ??
                    key;
                  const values = { ...(translationOptions.values || {}), ...(count === undefined ? {} : { count }) };
                  return message.replace(/\{\{([a-zA-Z0-9_]+)\}\}/g, (_match, name) => String(values[name] ?? ""));
                },
              };
            },
          },
        }
      : {}),
    ...(options.context ? { context: options.context } : {}),
    api: {
      ...(options.invokeAction ? { invokeAction: options.invokeAction } : {}),
      fetch(url) {
        let resolve;
        let reject;
        const promise = new Promise((nextResolve, nextReject) => {
          resolve = (data) => nextResolve({ ok: true, headers: {get: () => "application/json"}, json: () => Promise.resolve(data) });
          reject = nextReject;
        });
        requests.push({ url, resolve, reject });
        return promise;
      },
    },
  };
  definition.initialize(
    {
      registerComponent(slot, component) {
        if (slot === "chat-input-actions") Action = component;
        if (slot === "plugin-settings") Settings = component;
      },
      registerTranslations(nextTranslations) {
        translations = nextTranslations;
      },
    },
    host,
  );

  react.mount(options.settings ? Settings : Action, {
    slotProps: {
      taskId: "task-1",
      activeSessionId: "session-1",
      sessionIds: ["session-1"],
      presentation: options.presentation,
    },
  });

  return {
    requests,
    document,
    advanceTime(duration) {timers.advance(duration);},
    translations() {
      return translations;
    },
    rerender(slotProps) {
      react.render({ slotProps });
    },
    text() {
      return renderedText(react.tree());
    },
    textBeforeEffects() {
      return renderedText(react.treeBeforeEffects());
    },
    tooltipBeforeEffects() {
      return findElement(react.treeBeforeEffects(), (node) => node.type === "Tooltip");
    },
    tree: react.tree,
    trigger() {
      return findElement(
        react.tree(),
        (node) => (node.type === "Button" || node.type === ui.Action) && node.props.id === "session-cost-action",
      );
    },
    tooltip() {
      return findElement(react.tree(), (node) => node.type === "Tooltip");
    },
    tooltipContent() {
      return findElement(react.tree(), (node) => node.type === "TooltipContent");
    },
    settings() {
      return Settings;
    },
    refresh() {
      return findElement(
        react.tree(),
        (node) =>
          node.type === "Button" &&
          ["Refresh session cost", "Atualizar o custo da sessão"].includes(node.props["aria-label"]),
      );
    },
    busyRegion() {
      return findElement(react.tree(), (node) => node.props && "aria-busy" in node.props);
    },
  };
}

test("new hosts render one localized Action without plugin shell styles", async () => {
  const view = createActionHarness({ action: true, locale: "pt-pt" });
  const trigger = view.trigger();

  assert.equal(trigger.type.name, "Action");
  assert.equal(trigger.props.id, "session-cost-action");
  assert.equal(trigger.props.label, "Custo da sessão");
  assert.equal(trigger.props.tooltip, "");
  assert.ok(trigger.props.icon);
  assert.equal(trigger.props.text, undefined);
  assert.equal(typeof trigger.props.ref.current.contains, "function");
  for (const shellProp of ["className", "style", "size", "variant", "asChild"]) {
    assert.equal(shellProp in trigger.props, false);
  }
  assert.equal(view.translations().en.actionLabel, "Session cost");
  assert.equal(view.translations()["pt-pt"].actionLabel, "Custo da sessão");

  trigger.props.onMouseEnter();
  trigger.props.onFocus();
  trigger.props.onClick();
  assert.equal(view.requests.length, 1);
  assert.equal(view.tooltip().props.open, true);
  assert.equal(view.trigger().props.pressed, true);

  view.requests[0].resolve(costResponse({ cost: 1.25, cost_per_turn: 1.25 }));
  await flushPromises();

  assert.equal(view.trigger().props.label, "Custo da sessão");
  assert.equal(view.trigger().props.text, "$1.25");
  assert.equal(view.trigger().props.tone, "warning");
  assert.match(view.text(), /1 turno/);
  assert.match(view.text(), /\$1\.25 \/ turno/);
  assert.match(view.text(), /Entrada10/);
  assert.equal(view.refresh().props["aria-label"], "Atualizar o custo da sessão");
});

test("older-host mobile fallback keeps a touch target and lets loaded cost content set its width", async () => {
  const view = createActionHarness({ presentation: "mobile" });
  const trigger = view.trigger();

  assert.equal(trigger.type, "Button");
  assert.equal(trigger.props.variant, "ghost");
  assert.match(trigger.props.className, /min-h-11/);
  assert.match(trigger.props.className, /min-w-11/);
  assert.match(trigger.props.className, /\[@media\(pointer:coarse\)\]:h-11/);
  assert.match(trigger.props.className, /\[@media\(pointer:coarse\)\]:w-11/);

  trigger.props.onFocus();
  view.requests[0].resolve(costResponse({ cost: 123456789.12, cost_per_turn: 1.25 }));
  await flushPromises();

  const loadedTrigger = view.trigger();
  assert.equal(loadedTrigger.props.size, "sm");
  assert.match(loadedTrigger.props.className, /px-1\.5/);
  assert.doesNotMatch(loadedTrigger.props.className, /\[@media\(pointer:coarse\)\]:w-11/);
  assert.match(view.text(), /123,456,789\.12/);
});

test("new Action forwards the existing disclosure handlers and closes outside or on Escape", () => {
  const view = createActionHarness({ action: true });
  const trigger = view.trigger();

  trigger.props.onClick();
  assert.equal(view.tooltip().props.open, true);
  view.document.dispatchEvent({ type: "pointerdown", target: new FakeElement() });
  assert.equal(view.tooltip().props.open, false);

  view.trigger().props.onClick();
  view.document.dispatchEvent({ type: "keydown", key: "Escape" });
  assert.equal(view.tooltip().props.open, false);
  assert.equal(view.requests.length, 2);
});

test("registers an owner-scoped settings component for historical import", () => {
  const view = createActionHarness();

  assert.equal(typeof view.settings(), "function");
});

test("first tap pins details open and starts one initial request", () => {
  const view = createActionHarness();

  view.trigger().props.onClick();

  assert.equal(view.tooltip().props.open, true);
  assert.equal(view.requests.length, 1);
});

test("focus then click shares one request and stays open through its result", async () => {
  const view = createActionHarness();
  const trigger = view.trigger();

  trigger.props.onFocus();
  trigger.props.onClick();

  assert.equal(view.requests.length, 1);
  assert.equal(view.tooltip().props.open, true);

  view.requests[0].resolve(costResponse({ cost: 1.25, input: 100, output: 50 }));
  await flushPromises();

  assert.equal(view.tooltip().props.open, true);
});

test("per-model rows use compact token labels", async () => {
  const view = createActionHarness();

  view.trigger().props.onClick();
  view.requests[0].resolve(
    costResponse({
      models: [{ model: "gpt-5.6-sol", cost: 15.01, input: 2000000, output: 168000, cache_read: 75600000 }],
    }),
  );
  await flushPromises();

  assert.match(view.text(), /gpt-5\.6-sol/);
  assert.match(view.text(), /In 2MOut 168KCache 75\.6M/);
});

test("per-model token columns align left, center, and right", async () => {
  const view = createActionHarness();

  view.trigger().props.onClick();
  view.requests[0].resolve(
    costResponse({
      models: [
        { model: "gpt-5.6-sol", cost: 15.01, input: 2000000, output: 168000, cache_read: 75600000 },
        { model: "gpt-5.6-luna", cost: 0.83, input: 200000, output: 11700, cache_read: 300000 },
      ],
    }),
  );
  await flushPromises();

  const tokenGrid = findElement(
    view.tree(),
    (node) => node.type === "div" && node.props.style && node.props.style.gridTemplateColumns === "repeat(3, minmax(0, 1fr))",
  );
  assert.ok(tokenGrid);
  assert.equal(tokenGrid.children.length, 3);
  assert.equal(tokenGrid.children[0].props.style.textAlign, "left");
  assert.equal(tokenGrid.children[1].props.style.textAlign, "center");
  assert.equal(tokenGrid.children[2].props.style.textAlign, "right");
  assert.match(view.text(), /In 2MOut 168KCache 75\.6M/);
  assert.match(view.text(), /In 200KOut 11\.7KCache 300K/);
});

test("second tap closes without loading and reopening rechecks saved usage", async () => {
  const view = createActionHarness();

  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ found: false, cost: 0, turns: 0, input: 0, output: 0 }));
  await flushPromises();

  view.trigger().props.onClick();
  assert.equal(view.tooltip().props.open, false);
  assert.equal(view.requests.length, 1);

  view.trigger().props.onClick();
  assert.equal(view.tooltip().props.open, true);
  assert.equal(view.requests.length, 2);
});

test("pinned Refresh forces one request and stays open while disabled", async () => {
  const view = createActionHarness();

  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse());
  await flushPromises();

  assert.ok(view.refresh());
  assert.equal(view.refresh().props.type, "button");
  assert.match(view.refresh().props.className, /min-h-11/);

  view.refresh().props.onClick();

  assert.equal(view.requests.length, 2);
  assert.equal(view.tooltip().props.open, true);
  assert.equal(view.refresh().props.disabled, true);
  assert.equal(view.busyRegion().props["aria-busy"], true);

  view.refresh().props.onClick();
  assert.equal(view.requests.length, 2);

  view.requests[1].resolve(costResponse({ cost: 2, input: 20, output: 10 }));
  await flushPromises();

  assert.equal(view.tooltip().props.open, true);
  assert.equal(view.refresh().props.disabled, false);
});

test("inside interaction stays open while outside pointer and Escape dismiss", () => {
  const view = createActionHarness();

  view.trigger().props.onClick();
  view.document.dispatchEvent({
    type: "pointerdown",
    target: new FakeElement({ dataset: { slot: "tooltip-content" } }),
  });
  assert.equal(view.tooltip().props.open, true);

  view.document.dispatchEvent({ type: "pointerdown", target: new FakeElement() });
  assert.equal(view.tooltip().props.open, false);
  assert.equal(view.requests.length, 1);

  view.trigger().props.onClick();
  view.document.dispatchEvent({ type: "keydown", key: "Escape" });
  assert.equal(view.tooltip().props.open, false);
  assert.equal(view.requests.length, 2);
});

test("session changes close details and ignore the prior session response", async () => {
  const view = createActionHarness();

  view.trigger().props.onClick();
  view.rerender({ taskId: "task-1", activeSessionId: "session-2", sessionIds: ["session-1", "session-2"] });

  assert.equal(view.tooltip().props.open, false);

  view.trigger().props.onClick();
  assert.equal(view.requests.length, 2);
  view.requests[1].resolve(costResponse({ cost: 2, input: 20, output: 10, acp_session_id: "acp-2" }));
  await flushPromises();
  assert.match(view.text(), /\$2\.00/);

  view.requests[0].resolve(costResponse({ cost: 9, input: 90, output: 45 }));
  await flushPromises();

  assert.match(view.text(), /\$2\.00/);
  assert.doesNotMatch(view.text(), /\$9\.00/);
});

test("session change render never exposes the prior cached details", async () => {
  const view = createActionHarness();

  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ cost: 9, input: 90, output: 45 }));
  await flushPromises();
  assert.match(view.text(), /\$9\.00/);

  view.rerender({ taskId: "task-1", activeSessionId: "session-2", sessionIds: ["session-1", "session-2"] });

  assert.equal(view.tooltipBeforeEffects().props.open, false);
  assert.doesNotMatch(view.textBeforeEffects(), /\$9\.00/);
});

test("hover and focus stay ephemeral, accessible, and cache-aware", async () => {
  const view = createActionHarness();
  const trigger = view.trigger();

  view.tooltip().props.onOpenChange(true);
  trigger.props.onMouseEnter();
  trigger.props.onFocus();

  assert.equal(view.requests.length, 1);
  assert.equal(view.trigger().props["aria-label"], "Session cost");
  assert.equal(view.trigger().props["aria-expanded"], true);
  assert.equal(view.busyRegion().props["aria-busy"], true);
  assert.equal(view.refresh(), null);
  assert.match(view.tooltipContent().props.className, /pointer-events-auto/);
  assert.match(view.text(), /Calculating cost/);

  view.requests[0].resolve(costResponse({ cost: 3, input: 30, output: 15 }));
  await flushPromises();

  assert.equal(view.busyRegion().props["aria-busy"], false);
  assert.match(view.text(), /\$3\.00/);
  assert.equal(view.refresh(), null);

  view.trigger().props.onMouseEnter();
  view.trigger().props.onFocus();
  assert.equal(view.requests.length, 1);

  view.tooltip().props.onOpenChange(false);
  assert.equal(view.tooltip().props.open, false);
});

test("Refresh error renders in place and re-enables the native button", async () => {
  const view = createActionHarness();

  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ found: false, cost: 0, turns: 0, input: 0, output: 0 }));
  await flushPromises();

  view.refresh().props.onClick();
  view.requests[1].reject(new Error("network down"));
  await flushPromises();

  assert.equal(view.tooltip().props.open, true);
  assert.match(view.text(), /Couldn't load or refresh cost. Try again./);
  assert.equal(view.refresh().props.type, "button");
  assert.equal(view.refresh().props.disabled, false);
});

test("localized Action keeps unpriced saved usage unavailable without a success tone", async () => {
  const view = createActionHarness({ action: true, locale: "pt-pt" });
  assert.equal(view.translations().en.importTitle, "Import historical usage");
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ cost: 0, cost_known: false, cache_write: 12, reasoning: 4, total: 36 }));
  await flushPromises();
  assert.equal(view.trigger().props.text, undefined);
  assert.equal(view.trigger().props.tone, "neutral");
  assert.match(view.text(), /Indisponível/);
  assert.match(view.text(), /Escrita da cache12/);
  assert.match(view.text(), /Raciocínio4/);
  assert.doesNotMatch(view.text(), /\$0\.00/);
});


test("historical import actions follow the selected workspace and interpolate status values", async () => {
  const actions = [];
  const view = createActionHarness({ locale: "en", settings: true,
    context: { getWorkspaceIds: () => ["ws-1", "ws-2"], getActiveWorkspaceId: () => "ws-2" },
    invokeAction: (key, input) => {
      actions.push({ key, input });
      return Promise.resolve({ status: "completed", processed: 7, missing: 2, last_successful_at: "2026-10-05" });
    },
  });
  await flushPromises();
  assert.equal(actions[0].key, "historical-import-status");
  assert.equal(actions[0].input.workspaceId, "ws-2");
  assert.match(view.text(), /Processed 7 sessions/);
  assert.match(view.text(), /2 sessions have no matching/);
  assert.match(view.text(), /Last successful pass: 2026-10-05/);
  const start = findElement(view.tree(), (node) => node.type === "Button" && renderedText(node) === "Import again");
  start.props.onClick();
  await flushPromises();
  assert.equal(actions[1].key, "historical-import-start");
  assert.equal(actions[1].input.workspaceId, "ws-2");
});

test("authenticated usage action polls a pending report without repeating refresh", async () => {
  const actions = [];
  const results = [costResponse({ found: false, report_state: "pending" }), costResponse({ report_state: "ready", saved: true })];
  const view = createActionHarness({ invokeAction(key, input, options) {
    actions.push({key, input, options});
    return Promise.resolve(results.shift());
  }});
  view.trigger().props.onClick();
  await flushPromises();
  assert.equal(actions.length, 1);
  assert.equal(actions[0].key, "session-usage");
  assert.equal(actions[0].input.body.refresh, false);
  assert.ok(actions[0].options.signal instanceof AbortSignal);
  assert.match(view.text(), /Calculating cost/);
  assert.equal(view.refresh().props.disabled, true);
  view.advanceTime(2000);
  await flushPromises();
  assert.equal(actions.length, 2);
  assert.equal(actions[1].input.body.refresh, false);
  assert.match(view.text(), /\$1.00/);
  assert.equal(view.refresh().props.disabled, false);
  view.advanceTime(4000);
  assert.equal(actions.length, 2);
});

test("retained categories preserve unknown and exact integers", async () => {
  const view = createActionHarness({ action: true, locale: "en" });
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({
    input: null, output: null, cache_read: 0, turns: null,
    token_amounts: { input: "9223372036854775807", output: null, cache_read: "0", turns: null },
    models: [{ model: "exact/model-id", input: null, output: null, cache_read: 0, cost: 1,
      token_amounts: { input: "9223372036854775807", output: null, cache_read: "0" } }],
  }));
  await flushPromises();
  assert.match(view.text(), /9,223,372,036,854,775,807/);
  assert.match(view.text(), /Unavailable/);
  assert.match(view.text(), /Cache read0/);
  assert.doesNotMatch(view.text(), /9,223,372,036,854,776,000/);
});

test("saved split canonical metrics render known totals and Kandev cost per turn", async () => {
  const view = createActionHarness({ action: true, locale: "en" });
  view.trigger().props.onClick();
  view.requests[0].resolve(savedCanonicalMetricResponse());
  await flushPromises();
  assert.match(view.text(), /Total100/);
  assert.match(view.text(), /Input100/);
  assert.match(view.text(), /Estimated cost/);
  assert.match(view.text(), /\$0\.001 \/ turn/);
  assert.equal((view.text().match(/exact\/model/g) || []).length, 1);
  assert.doesNotMatch(view.text(), /Unavailable/);
});

test("saved applicable unknown cost preserves known tokens without a cost per turn", async () => {
  const view = createActionHarness({ action: true, locale: "en" });
  const payload = savedCanonicalMetricResponse();
  payload.cost_known = false;
  payload.cost_per_turn = 0;
  payload.models[0].cost_known = false;
  payload.models[0].cost_subcents_exact = null;
  view.trigger().props.onClick();
  view.requests[0].resolve(payload);
  await flushPromises();
  assert.match(view.text(), /Total100/);
  assert.match(view.text(), /Input100/);
  assert.match(view.text(), /Unavailable/);
  assert.doesNotMatch(view.text(), /\$|\/ turn/);
});

test("saved applicable unknown token category preserves independent known cost", async () => {
  const view = createActionHarness({ action: true, locale: "en" });
  const payload = savedCanonicalMetricResponse();
  payload.input = null;
  payload.token_amounts.input = null;
  payload.models[0].input = null;
  payload.models[0].token_amounts.input = null;
  view.trigger().props.onClick();
  view.requests[0].resolve(payload);
  await flushPromises();
  assert.match(view.text(), /Total100/);
  assert.match(view.text(), /InputUnavailable/);
  assert.match(view.text(), /\$0\.001 \/ turn/);
  assert.doesNotMatch(view.text(), /Input0/);
});

test("unsupported retained hosts preserve saved costs and show upgrade guidance", async () => {
  const view = createActionHarness({ action: true, locale: "en" });
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ saved: true, compatibility: "upgrade_required" }));
  await flushPromises();
  assert.match(view.text(), /\$1.00/);
  assert.match(view.text(), /Upgrade Kandev to save retained usage/);
});

test("legacy import diagnostics do not present unreliable counts as exact", async () => {
  const view = createActionHarness({ locale: "en", settings: true,
    context: { getWorkspaceIds: () => ["ws"], getActiveWorkspaceId: () => "ws" },
    invokeAction: () => Promise.resolve({ status: "failed", processed: 200, missing: 300, diagnostics_reliable: false }),
  });
  await flushPromises();
  assert.match(view.text(), /unreliable missing-session diagnostics/);
  assert.doesNotMatch(view.text(), /300 sessions have no matching/);
});

test("canonical projection lag shows the last saved selection and polls for completion", async () => {
  const view = createActionHarness({ action: true, locale: "en" });
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ saved: true, projection_status: "preparing", projection_generation: "opaque-complete", pending_projection_generation: "opaque-next", projection_pending: true }));
  await flushPromises();
  assert.match(view.text(), /\$1.00/);
  assert.match(view.text(), /Saved usage is updating. Showing the last completed selection./);
  assert.equal(view.refresh().props.disabled, true);
  view.advanceTime(2000);
  await flushPromises();
  assert.equal(view.requests.length, 2);
  view.requests[1].resolve(costResponse({ saved: true, cost: 2, projection_status: "ready", projection_generation: "opaque-next", projection_pending: false }));
  await flushPromises();
  assert.match(view.text(), /\$2.00/);
  assert.doesNotMatch(view.text(), /Saved usage is updating/);
  assert.equal(view.refresh().props.disabled, false);
  view.advanceTime(4000);
  assert.equal(view.requests.length, 2);
});

test("empty canonical preparation does not claim accepted usage or zero cost", async () => {
  const view = createActionHarness({ action: true, locale: "en" });
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ found: false, cost: 0, projection_status: "preparing", projection_generation: "opaque", projection_pending: true }));
  await flushPromises();
  assert.match(view.text(), /Saved usage is preparing./);
  assert.doesNotMatch(view.text(), /accepted|\$0\.00|No recorded usage/);
  assert.equal(view.refresh().props.disabled, true);
});

test("invalid saved usage queries show a query error without polling or invented amounts", async () => {
  const view = createActionHarness({ action: true, locale: "en" });
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ found: false, saved: false, report_state: "failed", report_error: "invalid_query", cost_known: false }));
  await flushPromises();
  assert.match(view.text(), /Kandev rejected the saved usage query/);
  assert.doesNotMatch(view.text(), /start tokscale|\$0\.00|Showing the previous result/);
  assert.equal(view.refresh().props.disabled, false);
  view.advanceTime(4000);
  assert.equal(view.requests.length, 1);
});

test("authorization denial clears earlier saved amounts and stops polling", async () => {
  for (const status of [401, 403, 404]) {
    const view = createActionHarness({ action: true, locale: "en" });
    view.trigger().props.onClick();
    view.requests[0].resolve(costResponse({ saved: true }));
    await flushPromises();
    assert.match(view.text(), /\$1.00/);
    view.refresh().props.onClick({ preventDefault() {}, stopPropagation() {} });
    assert.equal(view.requests.length, 2);
    const error = new Error("scope is no longer authorized");
    error.status = status;
    view.requests[1].reject(error);
    await flushPromises();
    assert.match(view.text(), /Saved usage is no longer accessible/);
    assert.doesNotMatch(view.text(), /\$1.00|Showing the previous result|Saved usage is updating/);
    assert.equal(view.trigger().props.text, undefined);
    view.advanceTime(4000);
    assert.equal(view.requests.length, 2);
  }
});

test("unsupported source context shows canonical amounts with explicit scope guidance", async () => {
  const view = createActionHarness({ action: true, locale: "en" });
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ saved: true, report_state: "failed", report_error: "source_context", compatibility: "source_context_unavailable" }));
  await flushPromises();
  assert.match(view.text(), /Source usage is unavailable for this retained scope/);
  assert.match(view.text(), /\$1.00/);
  assert.doesNotMatch(view.text(), /start tokscale|Upgrade Kandev|Calculating/);
  view.advanceTime(4000);
  assert.equal(view.requests.length, 1);
});

test("unsupported empty source context does not show a zero or cached former-scope cost", async () => {
  const view = createActionHarness({ action: true, locale: "en" });
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ found: false, saved: false, cost_known: false, report_state: "failed", compatibility: "source_context_unavailable" }));
  await flushPromises();
  assert.match(view.text(), /Source usage is unavailable for this retained scope/);
  assert.doesNotMatch(view.text(), /\$0\.00|\$1.00|Calculating/);
  view.advanceTime(4000);
  assert.equal(view.requests.length, 1);
});

test("source eligibility preparation polls without displaying a measured zero", async () => {
  const view = createActionHarness({ action: true, locale: "en" });
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ found: false, saved: false, cost_known: false, report_state: "pending", compatibility: "source_context_preparing" }));
  await flushPromises();
  assert.match(view.text(), /Source eligibility is preparing/);
  assert.doesNotMatch(view.text(), /\$0\.00|\$1\.00|Calculating cost/);
  assert.equal(view.refresh().props.disabled, true);
  view.advanceTime(2000);
  await flushPromises();
  assert.equal(view.requests.length, 2);
  view.requests[1].resolve(costResponse({ found: false, saved: false, cost_known: false, report_state: "failed", compatibility: "source_context_unavailable" }));
  await flushPromises();
  assert.match(view.text(), /Source usage is unavailable/);
  view.advanceTime(4000);
  assert.equal(view.requests.length, 2);
});

test("source eligibility preparation preserves the selected canonical values", async () => {
  const view = createActionHarness({ action: true, locale: "en" });
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ saved: true, projection_generation: "authorized-complete", report_state: "pending", compatibility: "source_context_preparing" }));
  await flushPromises();
  assert.match(view.text(), /Source eligibility is preparing/);
  assert.match(view.text(), /\$1\.00/);
  assert.doesNotMatch(view.text(), /Import completed|Calculating cost/);
});

test("historical discovery preparation remains cancellable and polls without completed counts", async () => {
  const actions = [];
  const view = createActionHarness({ settings: true, locale: "en",
    context: { getWorkspaceIds: () => ["ws"], getActiveWorkspaceId: () => "ws" },
    invokeAction(key) {
      actions.push(key);
      return Promise.resolve({ status: key === "historical-import-cancel" ? "cancelled" : "preparing", processed: 0, missing: 0 });
    },
  });
  await flushPromises();
  assert.match(view.text(), /Preparing historical source metadata/);
  assert.doesNotMatch(view.text(), /Import completed|Processed 0|no matching tokscale/);
  view.advanceTime(2000);
  await flushPromises();
  assert.equal(actions.filter(key => key === "historical-import-status").length, 2);
  const cancel = findElement(view.tree(), node => node.type === "Button" && renderedText(node) === "Cancel import");
  assert.ok(cancel);
  cancel.props.onClick();
  await flushPromises();
  assert.ok(actions.includes("historical-import-cancel"));
});

test("ambiguous model membership preserves only the accepted saved selection", async () => {
  const view = createActionHarness({ action: true, locale: "en" });
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ cost: 3, input: 300, total: 300, saved: true, stale: true, report_state: "failed", report_error: "source_membership", compatibility: "source_membership_ambiguous" }));
  await flushPromises();
  assert.match(view.text(), /Source model groups changed/);
  assert.match(view.text(), /\$3\.00/);
  assert.doesNotMatch(view.text(), /\$9\.00|Calculating cost/);
  view.advanceTime(4000);
  assert.equal(view.requests.length, 1);
  assert.equal(view.refresh().props.disabled, false);
});

test("ambiguous membership without saved rows clears an earlier raw result", async () => {
  const view = createActionHarness({ action: true, locale: "en" });
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ cost: 9, saved: false }));
  await flushPromises();
  view.refresh().props.onClick();
  view.requests[1].resolve(costResponse({ found: false, cost_known: false, saved: false, report_state: "failed", report_error: "source_membership", compatibility: "source_membership_ambiguous" }));
  await flushPromises();
  assert.match(view.text(), /Source model groups changed/);
  assert.doesNotMatch(view.text(), /\$9\.00|\$0\.00/);
  view.advanceTime(4000);
  assert.equal(view.requests.length, 2);
});
