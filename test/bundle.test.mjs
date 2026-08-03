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

function costResponse(overrides = {}) {
  return {
    found: true,
    cost: 1,
    turns: 1,
    input: 10,
    output: 5,
    cache_read: 0,
    models: [],
    tokscale: { installed: true },
    acp_session_id: "acp-1",
    ...overrides,
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
  };
}

function createActionHarness() {
  let definition;
  let Action;
  const requests = [];
  const react = createReactHarness();
  const document = createDocument();
  const ui = Object.fromEntries(
    ["Button", "Spinner", "Tooltip", "TooltipTrigger", "TooltipContent"].map((name) => [name, name]),
  );
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
  };
  vm.runInNewContext(bundleSource(), sandbox);

  const host = {
    React: react.React,
    jsx: element,
    ui,
    api: {
      fetch(url) {
        let resolve;
        let reject;
        const promise = new Promise((nextResolve, nextReject) => {
          resolve = (data) => nextResolve({ json: () => Promise.resolve(data) });
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
      },
    },
    host,
  );

  react.mount(Action, {
    slotProps: { taskId: "task-1", activeSessionId: "session-1", sessionIds: ["session-1"] },
  });

  return {
    requests,
    document,
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
      return findElement(react.tree(), (node) => node.type === "Button" && node.props.id === "session-cost-action");
    },
    tooltip() {
      return findElement(react.tree(), (node) => node.type === "Tooltip");
    },
    tooltipContent() {
      return findElement(react.tree(), (node) => node.type === "TooltipContent");
    },
    refresh() {
      return findElement(
        react.tree(),
        (node) => node.type === "Button" && node.props["aria-label"] === "Refresh session cost",
      );
    },
    busyRegion() {
      return findElement(react.tree(), (node) => node.props && "aria-busy" in node.props);
    },
  };
}

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

test("per-model rows show compact token details", async () => {
  const view = createActionHarness();

  view.trigger().props.onClick();
  view.requests[0].resolve(
    costResponse({
      models: [{ model: "gpt-5.6-sol", cost: 15.01, input: 2000000, output: 168000, cache_read: 75600000 }],
    }),
  );
  await flushPromises();

  assert.match(view.text(), /gpt-5\.6-sol/);
  assert.match(view.text(), /Input 2M · Output 168K · Cache read 75\.6M/);
});

test("second tap closes without loading and cached reopen stays request-free", async () => {
  const view = createActionHarness();

  view.trigger().props.onClick();
  view.requests[0].resolve(costResponse({ found: false, cost: 0, turns: 0, input: 0, output: 0 }));
  await flushPromises();

  view.trigger().props.onClick();
  assert.equal(view.tooltip().props.open, false);
  assert.equal(view.requests.length, 1);

  view.trigger().props.onClick();
  assert.equal(view.tooltip().props.open, true);
  assert.equal(view.requests.length, 1);
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
  assert.equal(view.requests.length, 1);
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
  assert.match(view.text(), /Couldn't load cost: network down/);
  assert.equal(view.refresh().props.type, "button");
  assert.equal(view.refresh().props.disabled, false);
});
