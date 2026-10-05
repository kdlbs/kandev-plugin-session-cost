import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const hostRoot = process.env.KANDEV_HOST_ROOT || fileURLToPath(new URL("../../kandev", import.meta.url));

function createTokscaleFixture() {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "session-cost-tokscale-"));
  const scriptPath = path.join(directory, "tokscale-fixture");
  const script = `#!/bin/sh
set -eu
directory=\${0%/*}
count=0
if [ -f "$directory/run-count" ]; then count=$(cat "$directory/run-count"); fi
count=$((count + 1))
printf '%s' "$count" > "$directory/run-count"
wait_for() {
  while [ ! -e "$directory/$1" ]; do sleep 0.02; done
}
if [ "$count" -eq 1 ]; then
  : > "$directory/first-started"
  wait_for release-first
fi
if [ -e "$directory/block-next" ]; then
  rm -f "$directory/block-next"
  : > "$directory/refresh-started"
  wait_for release-refresh
fi
if [ -e "$directory/fail-next" ]; then
  rm -f "$directory/fail-next"
  printf '%s' 'intentional smoke-test failure' >&2
  exit 3
fi
cat "$directory/report.json"
`;
  fs.writeFileSync(scriptPath, script, { mode: 0o755 });
  return {
    directory,
    marker: (name) => path.join(directory, name),
    writeReport(entries) {
      fs.writeFileSync(path.join(directory, "report.json"), JSON.stringify({ entries }));
    },
    release(name) {
      fs.writeFileSync(path.join(directory, name), "");
    },
    cleanup() {
      fs.rmSync(directory, { recursive: true, force: true });
    },
    scriptPath,
  };
}

async function getACPTranscriptId(apiClient, sessionId, expect) {
  let transcriptId = "";
  await expect
    .poll(
      async () => {
        const response = await apiClient.rawRequest("GET", `/api/v1/task-sessions/${sessionId}`);
        if (!response.ok) return "";
        const payload = await response.json();
        transcriptId = payload.session?.metadata?.acp?.session_id || "";
        return transcriptId;
      },
      { timeout: 30_000, message: `wait for ACP transcript for task session ${sessionId}` },
    )
    .toBeTruthy();
  return transcriptId;
}

function reportEntry(sessionId, cost) {
  return {
    sessionId,
    model: "fixture-model",
    input: 1200,
    output: 340,
    cacheRead: 50,
    messageCount: 2,
    cost,
  };
}

async function openQuickChatDialog(testPage, touch, expectAction, expect) {
  await testPage.goto("/");
  if (touch && expectAction) {
    await testPage.getByTestId("app-nav-trigger").tap();
    await testPage.getByTestId("mobile-quick-chat-button").tap();
  } else {
    await testPage.keyboard.press(`${process.platform === "darwin" ? "Meta" : "Control"}+Shift+q`);
  }

  const dialog = testPage.getByRole("dialog", { name: "Quick Chat" });
  await expect(dialog).toBeVisible({ timeout: 15_000 });
  return dialog;
}

async function openNewQuickChatSetup(dialog, testPage, expect) {
  const setup = dialog.getByTestId("quick-chat-setup");
  if (!(await setup.isVisible({ timeout: 1_000 }).catch(() => false))) {
    await dialog.getByTestId("quick-chat-add-menu-trigger").click();
    await testPage.getByTestId("quick-chat-new-agent").click();
  }
  await expect(setup).toBeVisible({ timeout: 5_000 });
  return setup;
}

async function waitForQuickChatAction(dialog, expect) {
  await expect(dialog.getByRole("button", { name: "Session cost", exact: true })).toBeVisible({
    timeout: 30_000,
  });
}

async function waitForTouchTarget(action, expect) {
  await expect
    .poll(async () => {
      const bounds = await action.boundingBox();
      return Boolean(bounds && bounds.width >= 44 && bounds.height >= 44);
    }, { timeout: 5_000 })
    .toBe(true);
  return action.boundingBox();
}

async function attachBackendDiagnostics(backend, testInfo, testPage) {
  if (testInfo && testPage) {
    await testInfo.attach("quick-chat-start-failure.png", {
      body: await testPage.screenshot(),
      contentType: "image/png",
    }).catch(() => undefined);
  }
  if (!backend || !testInfo) return;

  const logPaths = [backend.logPath, path.join(backend.tmpDir, "backend-process.log")];
  for (const [index, logPath] of logPaths.entries()) {
    try {
      if (!fs.statSync(logPath).isFile()) continue;
      const contents = fs.readFileSync(logPath, "utf8");
      await testInfo.attach(`backend-log-${index + 1}.txt`, {
        body: contents.slice(-100_000),
        contentType: "text/plain",
      });
    } catch {
      // The backend may not have created this optional diagnostic file yet.
    }
  }
}

async function captureTaskComposerScreenshot(testPage, testInfo, touch) {
  const filename = touch ? "v0.97.0-phone-task-composer.png" : "v0.97.0-desktop-task-composer.png";
  const screenshot = await testPage.screenshot();
  await testInfo.attach(filename, { body: screenshot, contentType: "image/png" });
  const artifactDir = process.env.SESSION_COST_SMOKE_ARTIFACT_DIR;
  if (artifactDir) {
    fs.mkdirSync(artifactDir, { recursive: true });
    fs.writeFileSync(path.join(artifactDir, filename), screenshot);
  }
}

export async function runSessionCostHostSmoke({
  testPage,
  apiClient,
  seedData,
  expectAction,
  touch,
  expect,
  backend,
  testInfo,
}) {
  const packagePath = process.env.SESSION_COST_PACKAGE_PATH;
  if (!packagePath) throw new Error("Set SESSION_COST_PACKAGE_PATH to a built plugin archive.");

  let installed = false;
  let quickChat;
  let secondQuickChat;
  let fixture;
  await apiClient.rawRequest("DELETE", "/api/plugins/kandev-session-cost").catch(() => undefined);
  try {
    await testPage.goto("/settings/plugins");
    await testPage.getByTestId("install-plugin-trigger").click();
    await expect(testPage.getByTestId("install-plugin-dialog")).toBeVisible();
    await testPage.getByTestId("install-plugin-tab-upload").click();
    await testPage.getByTestId("install-plugin-file-input").setInputFiles(packagePath);
    await testPage.getByTestId("install-plugin-upload-submit").click();
    await expect(testPage.getByTestId("install-plugin-dialog")).toBeHidden({ timeout: 30_000 });
    await expect(testPage.getByTestId("plugin-row-kandev-session-cost")).toBeVisible();
    installed = true;

    fixture = createTokscaleFixture();
    const configResponse = await apiClient.rawRequest("PATCH", "/api/plugins/kandev-session-cost", {
      config: { command: `/bin/sh ${fixture.scriptPath}` },
    });
    if (!configResponse.ok) {
      throw new Error(`Could not configure the packaged plugin fixture (${configResponse.status}): ${await configResponse.text()}`);
    }

    const requests = [];
    testPage.on("request", (request) => {
      const requestUrl = new URL(request.url());
      const action = requestUrl.pathname.endsWith("/actions/session-usage");
      if (!action && !requestUrl.pathname.endsWith("/webhooks/session-cost")) return;
      const payload = action ? request.postDataJSON() : null;
      requests.push({
        taskId: action ? payload?.taskId : requestUrl.searchParams.get("task_id"),
        active: action ? payload?.sessionId : requestUrl.searchParams.get("active"),
        refresh: action ? Boolean(payload?.body?.refresh) : requestUrl.searchParams.get("refresh") === "1",
      });
    });

    let dialog;
    if (expectAction) {
      dialog = await openQuickChatDialog(testPage, touch, expectAction, expect);
      const setup = await openNewQuickChatSetup(dialog, testPage, expect);
      const openingAction = setup.getByRole("button", { name: "Session cost", exact: true });
      await expect(openingAction).toBeVisible();
      await expect(openingAction).toHaveAttribute("id", "session-cost-action");
      await expect(openingAction).toHaveAttribute("data-slot", "surface-action");
      await expect(openingAction).toHaveAttribute("data-surface", "composer");
      await expect(openingAction).toHaveAttribute("aria-label", "Session cost");
      const startButton = setup.getByTestId("quick-chat-send");
      // The host sends disabled=true to plugin actions while this empty
      // opening composer cannot submit. Session Cost remains an independent,
      // read-only action that explains there is no active session to load.
      await expect(startButton).toBeDisabled();
      await expect(openingAction).toBeEnabled();

      if (touch) {
        const bounds = await waitForTouchTarget(openingAction, expect);
        expect(bounds).not.toBeNull();
        await openingAction.tap();
      } else {
        await openingAction.focus();
        await expect(openingAction).toBeFocused();
        await openingAction.press("Enter");
      }
      await expect(openingAction).toHaveAttribute("aria-expanded", "true");
      await expect(
        testPage
          .locator('[data-slot="tooltip-content"]:visible')
          .getByText("Open to load session cost", { exact: true })
          .first(),
      ).toBeVisible();
      await testPage.keyboard.press("Escape");
      await expect(openingAction).toHaveAttribute("aria-expanded", "false");

      const { dwell } = await import(
        pathToFileURL(path.join(hostRoot, "apps/web/e2e/helpers/causal-waits.ts")).href
      );
      await dwell(
        testPage,
        250,
        "negative-assertion",
        "the Quick Chat opening composer has no active session, so the cost action must not fetch usage",
      );
      expect(requests).toHaveLength(0);

      const { selectAgentIfNeeded } = await import(
        pathToFileURL(path.join(hostRoot, "apps/web/e2e/tests/chat/quick-chat-helpers.ts")).href
      );
      await selectAgentIfNeeded(dialog, testPage);
      // A selected profile still cannot submit an empty prompt. Check that the
      // host's composer-disabled state does not disable this read-only action.
      await expect(startButton).toBeDisabled();
      await expect(openingAction).toBeEnabled();
      const startResponsePromise = testPage.waitForResponse(
        (response) =>
          new URL(response.url()).pathname.endsWith("/quick-chat") &&
          response.request().method() === "POST",
        { timeout: 30_000 },
      );
      await setup.getByTestId("task-description-input").fill("Session Cost action smoke");
      await expect(startButton).toBeEnabled({ timeout: 10_000 });
      if (touch) await startButton.tap();
      else await startButton.click();
      const startResponse = await startResponsePromise;
      const startBody = await startResponse.json();
      if (!startResponse.ok() && testInfo) {
        await testInfo.attach("quick-chat-start-response.json", {
          body: JSON.stringify({ status: startResponse.status(), body: startBody }, null, 2),
          contentType: "application/json",
        });
        await attachBackendDiagnostics(backend, testInfo, testPage);
      }
      if (!startBody.task_id || !startBody.session_id) {
        throw new Error(`Quick Chat did not return a task and session identity: ${JSON.stringify(startBody)}`);
      }
      quickChat = { task_id: startBody.task_id, session_id: startBody.session_id };
      await waitForQuickChatAction(dialog, expect);
    } else {
      quickChat = await apiClient.startQuickChat(
        seedData.workspaceId,
        seedData.agentProfileId,
        "Session Cost action smoke",
      );
      dialog = await openQuickChatDialog(testPage, touch, expectAction, expect);
    }

    secondQuickChat = await apiClient.startQuickChat(
      seedData.workspaceId,
      seedData.agentProfileId,
      "Session Cost second-session smoke",
    );
    const primaryTranscriptId = await getACPTranscriptId(apiClient, quickChat.session_id, expect);
    const secondTranscriptId = await getACPTranscriptId(apiClient, secondQuickChat.session_id, expect);
    fixture.writeReport([
      reportEntry(primaryTranscriptId, 123456789.12),
      reportEntry(secondTranscriptId, 2.5),
    ]);

    const openQuickChat = async () => {
      return openQuickChatDialog(testPage, touch, expectAction, expect);
    };

    const expectComposerGeometry = async (button) => {
      if (touch) await waitForTouchTarget(button, expect);
      const layout = await button.evaluate((buttonElement) => {
        const toolbar = buttonElement.closest(
          '[data-testid="mobile-chat-input-toolbar"], [data-testid="chat-input-toolbar"]',
        );
        const icon = buttonElement.querySelector("svg");
        const amount = Array.from(buttonElement.querySelectorAll("*")).find((element) =>
          /\$\d/.test(element.textContent) &&
          !Array.from(element.children).some((child) => /\$\d/.test(child.textContent)),
        );
        const rect = (element) => {
          const bounds = element.getBoundingClientRect();
          return { top: bounds.top, right: bounds.right, bottom: bounds.bottom, left: bounds.left };
        };
        const contains = (parent, child) =>
          child.top >= parent.top - 1 &&
          child.right <= parent.right + 1 &&
          child.bottom <= parent.bottom + 1 &&
          child.left >= parent.left - 1;
        const buttonBounds = rect(buttonElement);
        const iconBounds = icon && rect(icon);
        const amountBounds = amount && rect(amount);
        const toolbarBounds = toolbar && rect(toolbar);

        return {
          toolbarFound: Boolean(toolbar),
          iconInsideButton: Boolean(iconBounds && contains(buttonBounds, iconBounds)),
          amountInsideButton: Boolean(amountBounds && contains(buttonBounds, amountBounds)),
          amountTextFits: Boolean(amount && amount.scrollWidth <= amount.clientWidth + 1),
          buttonInsideToolbar: Boolean(toolbarBounds && contains(toolbarBounds, buttonBounds)),
          buttonWidth: buttonBounds.right - buttonBounds.left,
          buttonHeight: buttonBounds.bottom - buttonBounds.top,
          pointerCoarse: window.matchMedia("(pointer: coarse)").matches,
          touchPoints: navigator.maxTouchPoints,
          documentWidth: document.documentElement.scrollWidth,
          documentClientWidth: document.documentElement.clientWidth,
        };
      });

      expect(layout.toolbarFound).toBe(true);
      expect(layout.iconInsideButton).toBe(true);
      expect(layout.amountInsideButton).toBe(true);
      expect(layout.amountTextFits).toBe(true);
      expect(layout.buttonInsideToolbar).toBe(true);
      expect(layout.documentWidth).toBeLessThanOrEqual(layout.documentClientWidth);
      if (touch) {
        expect(layout.pointerCoarse).toBe(true);
        expect(layout.touchPoints).toBeGreaterThan(0);
        expect(layout.buttonWidth).toBeGreaterThanOrEqual(44);
        expect(layout.buttonHeight).toBeGreaterThanOrEqual(44);
      } else {
        expect(layout.buttonHeight).toBeGreaterThanOrEqual(27);
        expect(layout.buttonHeight).toBeLessThanOrEqual(29);
      }
    };

    const action = dialog.getByRole("button", { name: "Session cost", exact: true });
    await expect(action).toBeVisible();
    await expect(action).toHaveAttribute("id", "session-cost-action");
    if (expectAction) {
      await expect(action).toHaveAttribute("data-slot", "surface-action");
      await expect(action).toHaveAttribute("data-surface", "composer");
    } else {
      await expect(action).toHaveAttribute("data-variant", "ghost");
      await expect(action).not.toHaveAttribute("data-surface");
    }

    if (touch) {
      const bounds = await waitForTouchTarget(action, expect);
      expect(bounds).not.toBeNull();
      await action.tap();
    } else {
      await action.hover();
      await action.focus();
      await expect(action).toBeFocused();
    }

    await expect.poll(() => requests.length, { timeout: 10_000 }).toBe(1);
    expect(requests[0].taskId).toBe(quickChat.task_id);
    expect(requests[0].active).toBe(quickChat.session_id);
    const tooltip = testPage.locator('[data-slot="tooltip-content"]:visible');
    await expect(tooltip.getByText("Calculating cost…", { exact: true }).first()).toBeVisible();
    await expect.poll(() => fs.existsSync(fixture.marker("first-started")), { timeout: 10_000 }).toBe(true);
    const concurrentLookupStartedAt = Date.now();
    const concurrentLookup = await testPage.evaluate(async ({ taskId, sessionId }) => {
      const query = new URLSearchParams({ task_id: taskId, active: sessionId });
      const response = await fetch(`/api/plugins/kandev-session-cost/webhooks/session-cost?${query}`);
      return { status: response.status, payload: await response.json() };
    }, { taskId: secondQuickChat.task_id, sessionId: secondQuickChat.session_id });
    expect(Date.now() - concurrentLookupStartedAt).toBeLessThan(5_000);
    expect(concurrentLookup.status).toBe(200);
    expect(concurrentLookup.payload.report_state).toBe("pending");
    expect(concurrentLookup.payload.kandev_session_id).toBe(secondQuickChat.session_id);
    expect(requests[1].taskId).toBe(secondQuickChat.task_id);
    expect(requests[1].active).toBe(secondQuickChat.session_id);
    expect(fs.readFileSync(fixture.marker("run-count"), "utf8")).toBe("1");
    const beforeInitialPoll = requests.length;
    fixture.release("release-first");
    await expect.poll(() => requests.length, { timeout: 10_000 }).toBeGreaterThan(beforeInitialPoll);
    expect(requests.slice(beforeInitialPoll).every((request) => !request.refresh)).toBe(true);
    await expect(action).toContainText(/123.*456.*789/);
    await expect(action).toHaveAttribute("aria-label", "Session cost");
    await expectComposerGeometry(action);

    if (!touch) await action.press("Enter");
    await expect(action).toHaveAttribute("aria-expanded", "true");
    if (expectAction) await expect(action).toHaveAttribute("aria-pressed", "true");
    const details = tooltip.getByText("fixture-model", { exact: true }).first();
    await expect(details).toBeVisible();

    await testPage.keyboard.press("Escape");
    await expect(action).toHaveAttribute("aria-expanded", "false");
    const { dwell } = await import(
      pathToFileURL(path.join(hostRoot, "apps/web/e2e/helpers/causal-waits.ts")).href
    );
    fixture.writeReport([
      reportEntry(primaryTranscriptId, 0.75),
      reportEntry(secondTranscriptId, 2.5),
    ]);
    fixture.release("block-next");

    if (touch) await action.tap();
    else await action.press("Enter");
    await expect(action).toHaveAttribute("aria-expanded", "true");
    const beforeBlockedRefresh = requests.length;
    await testPage.getByLabel("Refresh session cost").first().click();
    await expect.poll(() => requests.length, { timeout: 10_000 }).toBeGreaterThan(beforeBlockedRefresh);
    expect(requests[beforeBlockedRefresh].refresh).toBe(true);
    await expect(tooltip.getByText("Updating cost. Showing the previous result.", { exact: true }).first()).toBeVisible();
    await expect(action).toContainText(/123.*456.*789/);
    await expect(testPage.getByLabel("Refresh session cost").first()).toBeDisabled();
    await expect.poll(() => fs.existsSync(fixture.marker("refresh-started")), { timeout: 10_000 }).toBe(true);
    if (touch) await action.tap();
    else await testPage.keyboard.press("Escape");
    await expect(action).toHaveAttribute("aria-expanded", "false");
    const closedRequestCount = requests.length;
    await dwell(testPage, 2200, "negative-assertion", "closed Session Cost details stop pending status polling");
    expect(requests).toHaveLength(closedRequestCount);
    fixture.release("release-refresh");
    const beforeRefreshPoll = requests.length;
    if (touch) await action.tap();
    else await action.press("Enter");
    await expect(action).toHaveAttribute("aria-expanded", "true");
    await expect(action).toContainText("$0.75", { timeout: 15_000 });
    await expect.poll(() => requests.length, { timeout: 10_000 }).toBeGreaterThan(beforeRefreshPoll);
    expect(requests.slice(beforeRefreshPoll).every((request) => !request.refresh)).toBe(true);

    fixture.writeReport([
      reportEntry(primaryTranscriptId, 0.5),
      reportEntry(secondTranscriptId, 2.5),
    ]);
    fixture.release("fail-next");
    const failedRefreshRequest = requests.length;
    await testPage.getByLabel("Refresh session cost").first().click();
    await expect.poll(() => requests.length, { timeout: 10_000 }).toBeGreaterThan(failedRefreshRequest);
    expect(requests.at(-1).refresh).toBe(true);
    await expect(
      tooltip.getByText("Couldn't refresh cost. Showing the previous result. Try again.", { exact: true }).first(),
    ).toBeVisible();
    await expect(action).toContainText("$0.75");
    await expect(testPage.getByLabel("Refresh session cost").first()).toBeEnabled();
    const failureBounds = await tooltip.boundingBox();
    const viewportSize = testPage.viewportSize();
    expect(failureBounds).not.toBeNull();
    expect(failureBounds.x).toBeGreaterThanOrEqual(-1);
    expect(failureBounds.x + failureBounds.width).toBeLessThanOrEqual(viewportSize.width + 1);
    const documentWidths = await testPage.evaluate(() => ({
      width: document.documentElement.scrollWidth,
      clientWidth: document.documentElement.clientWidth,
    }));
    expect(documentWidths.width).toBeLessThanOrEqual(documentWidths.clientWidth);

    fixture.writeReport([
      reportEntry(primaryTranscriptId, 0.5),
      reportEntry(secondTranscriptId, 2.5),
    ]);
    const retryRequest = requests.length;
    await testPage.getByLabel("Refresh session cost").first().click();
    await expect.poll(() => requests.length, { timeout: 10_000 }).toBeGreaterThan(retryRequest);
    expect(requests.at(-1).refresh).toBe(true);
    await expect(testPage.getByLabel("Refresh session cost").first()).toBeDisabled();
    await expect(action).toContainText("$0.50", { timeout: 15_000 });

    await testPage.goto("/settings/plugins");
    const row = testPage.getByTestId("plugin-row-kandev-session-cost");
    await row.getByRole("button", { name: "Disable" }).click();
    await expect(row.getByText("Disabled", { exact: true })).toBeVisible();
    const disabledDialog = await openQuickChat();
    await expect(disabledDialog.getByRole("button", { name: "Session cost", exact: true })).toHaveCount(0);

    await testPage.goto("/settings/plugins");
    await row.getByRole("button", { name: "Enable" }).click();
    await expect(row.getByText("Active", { exact: true })).toBeVisible();
    const reenabledDialog = await openQuickChat();
    const reenabledAction = reenabledDialog.getByRole("button", { name: "Session cost", exact: true });
    await expect(reenabledAction).toBeVisible();
    if (expectAction) {
      await expect(reenabledAction).toHaveAttribute("data-slot", "surface-action");
    } else {
      await expect(reenabledAction).toHaveAttribute("data-variant", "ghost");
    }

    await testPage.goto(`/t/${quickChat.task_id}`);
    const taskChatAction = testPage.getByRole("button", { name: "Session cost", exact: true });
    await expect(taskChatAction).toBeVisible({ timeout: 30_000 });
    if (expectAction) {
      await expect(taskChatAction).toHaveAttribute("data-surface", "composer");
    } else {
      await expect(taskChatAction).toHaveAttribute("data-variant", "ghost");
    }
    const beforeTaskChatRequest = requests.length;
    if (touch) {
      const taskBounds = await waitForTouchTarget(taskChatAction, expect);
      expect(taskBounds).not.toBeNull();
      await taskChatAction.tap();
    } else {
      await taskChatAction.focus();
    }
    await expect.poll(() => requests.length, { timeout: 10_000 }).toBeGreaterThan(beforeTaskChatRequest);
    expect(requests.at(-1).taskId).toBe(quickChat.task_id);
    expect(requests.at(-1).active).toBe(quickChat.session_id);
    await expect(taskChatAction).toContainText("$0.50");
    await expectComposerGeometry(taskChatAction);
    if (testInfo) await captureTaskComposerScreenshot(testPage, testInfo, touch);

    await testPage.goto(`/t/${secondQuickChat.task_id}`);
    const secondSessionAction = testPage.getByRole("button", { name: "Session cost", exact: true });
    await expect(secondSessionAction).toBeVisible({ timeout: 30_000 });
    const beforeSecondSessionRequest = requests.length;
    if (touch) await secondSessionAction.tap();
    else await secondSessionAction.focus();
    await expect.poll(() => requests.length, { timeout: 10_000 }).toBeGreaterThan(beforeSecondSessionRequest);
    expect(requests.at(-1).taskId).toBe(secondQuickChat.task_id);
    expect(requests.at(-1).active).toBe(secondQuickChat.session_id);
    await expect(secondSessionAction).toContainText("$2.50");
    await expect(secondSessionAction).not.toContainText(/123.*456.*789|\$0\.50/);
    } finally {
      if (fixture) {
        fixture.release("release-first");
        fixture.release("release-refresh");
      }
      if (installed) await apiClient.rawRequest("DELETE", "/api/plugins/kandev-session-cost").catch(() => undefined);
      if (quickChat?.task_id) await apiClient.deleteTask(quickChat.task_id).catch(() => undefined);
      if (secondQuickChat?.task_id) await apiClient.deleteTask(secondQuickChat.task_id).catch(() => undefined);
      if (fixture) fixture.cleanup();
  }
}
