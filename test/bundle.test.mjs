import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

function bundleSource() {
  return readFileSync(new URL("../ui/bundle.js", import.meta.url), "utf8");
}

function element(type, props, ...children) {
  return { type, props: props || {}, children: children.flat(Infinity) };
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

function createReactHarness() {
  const hooks = [];
  let component;
  let props;
  let cursor = 0;
  let tree;

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
    useEffect() {
      cursor++;
    },
  };

  function render(nextProps) {
    if (nextProps) props = nextProps;
    cursor = 0;
    tree = component(props);
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
  };
}

function createActionHarness() {
  let definition;
  let Action;
  const requests = [];
  const react = createReactHarness();
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
    isFinite,
    Math,
    String,
  };
  vm.runInNewContext(bundleSource(), sandbox);

  const host = {
    React: react.React,
    jsx: element,
    ui,
    api: {
      fetch(url) {
        requests.push(url);
        return new Promise(() => {});
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
    tree: react.tree,
    trigger() {
      return findElement(react.tree(), (node) => node.type === "Button" && node.props.id === "session-cost-action");
    },
    tooltip() {
      return findElement(react.tree(), (node) => node.type === "Tooltip");
    },
  };
}

test("first tap pins details open and starts one initial request", () => {
  const view = createActionHarness();

  view.trigger().props.onClick();

  assert.equal(view.tooltip().props.open, true);
  assert.equal(view.requests.length, 1);
});
