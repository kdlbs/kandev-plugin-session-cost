import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

function bundleSource() {
  return readFileSync(process.env.SESSION_COST_TEST_BUNDLE_PATH || new URL("../ui/bundle.js", import.meta.url), "utf8");
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
    cost_per_turn: 1,
    turns: 1,
    input: 10,
    output: 5,
    cache_read: 0,
    warn_threshold: 1,
    high_threshold: 10,
    models: [],
    tokscale: { installed: true },
    acp_session_id: "acp-1",
    ...overrides,
  };
}

function response(data, options = {}) {
  const status = options.status ?? 200;
  const contentType = options.contentType ?? "application/json";
  return {
    ok: options.ok ?? (status >= 200 && status < 300),
    status,
    headers: {
      get(name) {
        return name.toLowerCase() === "content-type" ? contentType : null;
      },
    },
    json() {
      return options.jsonError ? Promise.reject(options.jsonError) : Promise.resolve(data);
    },
  };
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
    unmount() {
      for (const hook of hooks) {
        if (hook && typeof hook.cleanup === "function") {
          const cleanup = hook.cleanup;
          hook.cleanup = null;
          cleanup();
        }
      }
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
    document,
    Element: FakeElement,
    isFinite,
    Math,
    Node: FakeNode,
    String,
    AbortController,
    performance: { now: timers.now },
    setTimeout: timers.setTimeout,
    clearTimeout: timers.clearTimeout,
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
    api: {
      fetch(url, init) {
        let resolve;
        let reject;
        let resolveResponse;
        const promise = new Promise((nextResolve, nextReject) => {
          resolve = (data, responseOptions = {}) => nextResolve(response(data, responseOptions));
          resolveResponse = (value) => nextResolve(value);
          reject = nextReject;
        });
        requests.push({ url, signal: init && init.signal, resolve, resolveResponse: (value) => resolveResponse(value), reject });
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

  react.mount(Action, {
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
    advanceTimers(duration) {
      timers.advance(duration);
    },
    pendingTimers() {
      return timers.size();
    },
    unmount() {
      react.unmount();
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
  assert.equal(view.requests.length, 2, "closing aborts the obsolete request before a new open");
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

test("second tap closes and reopening checks the cached report without forcing refresh", async () => {
  const view = createActionHarness();

  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ cost: 1.25, cost_per_turn: 1.25 }));
  await flushPromises();

  view.trigger().props.onClick();
  assert.equal(view.tooltip().props.open, false);
  assert.equal(view.requests.length, 1);

  view.trigger().props.onClick();
  assert.equal(view.tooltip().props.open, true);
  assert.equal(view.requests.length, 2);
  assert.doesNotMatch(view.requests[1].url, /refresh=1/);
  assert.match(view.text(), /\$1\.25/);

  view.requests[1].resolve(costResponse({ cost: 2, cost_per_turn: 2 }));
  await flushPromises();

  assert.match(view.text(), /\$2\.00/);
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
  assert.match(view.text(), /Couldn't load or refresh cost/);
  assert.doesNotMatch(view.text(), /network down/);
  assert.equal(view.refresh().props.type, "button");
  assert.equal(view.refresh().props.disabled, false);
  view.trigger().props.onFocus();
  assert.equal(view.requests.length, 2, "a failed request waits for an explicit retry");
});

test("HTML gateway failure shows a localized retry without exposing its body", async () => {
  const view = createActionHarness({ locale: "pt-pt" });

  view.trigger().props.onClick();
  view.requests[0].resolveResponse(
    response("<!DOCTYPE html><title>Gateway failure</title>", {
      status: 502,
      contentType: "text/html; charset=utf-8",
      jsonError: new SyntaxError("Unexpected token '<' in JSON"),
    }),
  );
  await flushPromises();

  assert.match(view.text(), /Não foi possível carregar ou atualizar o custo/);
  assert.doesNotMatch(view.text(), /DOCTYPE|Gateway failure|Unexpected token/);
  assert.equal(view.refresh().props.disabled, false);
});

test("JSON errors, malformed JSON, and invalid report values never render as zero cost", async () => {
  const invalidResponses = [
    response({ error: "backend secret" }, { status: 503 }),
    response(null, { jsonError: new SyntaxError("Unexpected end of JSON input") }),
    response(costResponse({ cost: "not-a-number" })),
    response(costResponse(), { contentType: "" }),
  ];

  for (const badResponse of invalidResponses) {
    const view = createActionHarness();
    view.trigger().props.onClick();
    view.requests[0].resolveResponse(badResponse);
    await flushPromises();
    assert.match(view.text(), /Couldn't load or refresh cost/);
    assert.doesNotMatch(view.text(), /\$0\.00|backend secret|Unexpected end/);
    assert.equal(view.refresh().props.disabled, false);
  }
});

test("pending reports poll without refresh, then explicit Refresh runs once and retains stale cost", async () => {
  const view = createActionHarness();
  const pendingCold = costResponse({
    found: false,
    cost: 0,
    cost_per_turn: 0,
    turns: 0,
    input: 0,
    output: 0,
    acp_session_id: "acp-1",
    report_state: "pending",
    stale: false,
  });

  view.trigger().props.onClick();
  view.requests[0].resolve(pendingCold);
  await flushPromises();
  assert.match(view.text(), /Calculating cost/);
  assert.equal(view.refresh().props.disabled, true);
  assert.equal(view.busyRegion().props["aria-busy"], true);

  view.advanceTimers(1999);
  assert.equal(view.requests.length, 1);
  view.advanceTimers(1);
  assert.equal(view.requests.length, 2);
  assert.doesNotMatch(view.requests[1].url, /refresh=1/);
  view.requests[1].resolve(costResponse({ cost: 0.58, cost_per_turn: 0.29, turns: 2 }));
  await flushPromises();
  assert.match(view.text(), /\$0\.58/);
  assert.equal(view.refresh().props.disabled, false);

  view.refresh().props.onClick();
  assert.match(view.requests[2].url, /refresh=1/);
  assert.equal(view.requests.length, 3);
  const pendingRefresh = costResponse({ report_state: "pending", stale: true, cost: 0.58, cost_per_turn: 0.29, turns: 2 });
  view.requests[2].resolve(pendingRefresh);
  await flushPromises();
  assert.match(view.text(), /Updating cost\. Showing the previous result/);
  assert.match(view.text(), /\$0\.58/);
  assert.equal(view.refresh().props.disabled, true);

  view.advanceTimers(2000);
  assert.equal(view.requests.length, 4);
  assert.doesNotMatch(view.requests[3].url, /refresh=1/);
  view.requests[3].resolve(costResponse({ cost: 0.75, cost_per_turn: 0.25, turns: 3 }));
  await flushPromises();
  assert.match(view.text(), /\$0\.75/);
  assert.doesNotMatch(view.text(), /previous result/);
});

test("server refresh failure keeps the last cost and offers a retry", async () => {
  const view = createActionHarness();
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ cost: 0.58, cost_per_turn: 0.29, turns: 2 }));
  await flushPromises();

  view.refresh().props.onClick();
  view.requests[1].resolve(
    costResponse({
      report_state: "failed",
      report_error: "timeout",
      stale: true,
      cost: 0.58,
      cost_per_turn: 0.29,
      turns: 2,
    }),
  );
  await flushPromises();

  assert.match(view.text(), /Couldn't refresh cost\. Showing the previous result/);
  assert.match(view.text(), /\$0\.58/);
  assert.equal(view.refresh().props.disabled, false);
  assert.doesNotMatch(view.text(), /timeout/);
});

test("legacy ready payloads without progress fields still render", async () => {
  const view = createActionHarness();
  const legacy = costResponse({ cost: 1.25, cost_per_turn: 1.25 });
  delete legacy.report_state;
  delete legacy.report_error;
  delete legacy.stale;

  view.trigger().props.onClick();
  view.requests[0].resolve(legacy);
  await flushPromises();

  assert.match(view.text(), /\$1\.25/);
  assert.equal(view.busyRegion().props["aria-busy"], false);
});

test("missing transcript keeps its empty state when no report command ran", async () => {
  const view = createActionHarness();
  view.trigger().props.onClick();
  view.requests[0].resolve(
    costResponse({
      found: false,
      cost: 0,
      cost_per_turn: 0,
      turns: 0,
      acp_session_id: "",
      tokscale: { installed: false },
      report_state: "ready",
    }),
  );
  await flushPromises();

  assert.match(view.text(), /No agent transcript for this session yet/);
  assert.doesNotMatch(view.text(), /tokscale isn't available/);
});

test("JSON media types with a structured suffix remain valid", async () => {
  const view = createActionHarness();
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ cost: 1.5 }), { contentType: "application/problem+json; charset=utf-8" });
  await flushPromises();

  assert.match(view.text(), /\$1\.50/);
});

test("closing or unmounting pending details cancels polling and obsolete requests", async () => {
  const view = createActionHarness();
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ found: false, cost: 0, turns: 0, report_state: "pending" }));
  await flushPromises();
  assert.equal(view.pendingTimers(), 2);

  view.advanceTimers(2000);
  assert.equal(view.requests.length, 2);
  const pollRequest = view.requests[1];
  view.trigger().props.onClick();
  assert.equal(view.tooltip().props.open, false);
  assert.equal(pollRequest.signal.aborted, true);
  view.advanceTimers(4000);
  assert.equal(view.requests.length, 2);
  pollRequest.resolve(costResponse({ cost: 99 }));
  await flushPromises();
  assert.doesNotMatch(view.text(), /\$99\.00/);

  view.trigger().props.onClick();
  assert.equal(view.requests.length, 3);
  view.requests[2].resolve(costResponse({ found: false, cost: 0, turns: 0, report_state: "pending" }));
  await flushPromises();
  assert.equal(view.pendingTimers(), 2);
  view.advanceTimers(2000);
  assert.equal(view.requests.length, 4);
  view.unmount();
  assert.equal(view.pendingTimers(), 0);
  view.advanceTimers(4000);
  assert.equal(view.requests.length, 4);
});

test("reopening after the initial request is canceled starts a fresh request", () => {
  const view = createActionHarness();

  view.trigger().props.onClick();
  const canceledRequest = view.requests[0];
  view.trigger().props.onClick();
  assert.equal(canceledRequest.signal.aborted, true);

  view.trigger().props.onClick();
  assert.equal(view.tooltip().props.open, true);
  assert.equal(view.requests.length, 2);
  assert.equal(view.requests[1].signal.aborted, false);
  view.unmount();
});

test("session changes abort old requests and ignore their late response", async () => {
  const view = createActionHarness();
  view.trigger().props.onClick();
  const oldRequest = view.requests[0];
  view.rerender({ taskId: "task-1", activeSessionId: "session-2", sessionIds: ["session-1", "session-2"] });
  assert.equal(oldRequest.signal.aborted, true);
  oldRequest.resolve(costResponse({ cost: 9 }));
  await flushPromises();

  view.trigger().props.onClick();
  view.requests[1].resolve(costResponse({ cost: 2, acp_session_id: "acp-2" }));
  await flushPromises();
  assert.match(view.text(), /\$2\.00/);
  assert.doesNotMatch(view.text(), /\$9\.00/);
});

test("request timeout aborts the fetch and preserves a previous result", async () => {
  const view = createActionHarness();
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ cost: 0.58, cost_per_turn: 0.29, turns: 2 }));
  await flushPromises();

  view.refresh().props.onClick();
  const refreshRequest = view.requests[1];
  view.advanceTimers(10000);

  assert.equal(refreshRequest.signal.aborted, true);
  assert.match(view.text(), /Couldn't refresh cost\. Showing the previous result/);
  assert.match(view.text(), /\$0\.58/);
  assert.equal(view.refresh().props.disabled, false);
});

test("automatic polling stops at 130 elapsed seconds and aborts an in-flight poll", async () => {
  const view = createActionHarness();
  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ found: false, cost: 0, turns: 0, report_state: "pending" }));
  await flushPromises();

  for (let poll = 0; poll < 24; poll += 1) {
    view.advanceTimers(2000);
    const request = view.requests[view.requests.length - 1];
    assert.equal(view.requests.length, poll + 2);
    view.advanceTimers(3000);
    request.resolve(costResponse({ found: false, cost: 0, turns: 0, report_state: "pending" }));
    await flushPromises();
  }

  view.advanceTimers(2000);
  assert.equal(view.requests.length, 26);
  const inFlightPoll = view.requests[25];
  view.advanceTimers(8000);

  assert.equal(inFlightPoll.signal.aborted, true);
  assert.match(view.text(), /Cost calculation is taking too long/);
  assert.equal(view.refresh().props.disabled, false);
  assert.equal(view.pendingTimers(), 0);
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
