import fs from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const hostRoot = process.env.KANDEV_HOST_ROOT || fileURLToPath(new URL("../../kandev", import.meta.url));

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

    const fakeResponse = {
      found: true,
      cost: 123456789.12,
      cost_per_turn: 1.25,
      turns: 2,
      input: 1200,
      output: 340,
      cache_read: 50,
      models: [{ model: "fixture-model", cost: 123456789.12, input: 1200, output: 340, cache_read: 50 }],
      tokscale: { installed: true },
      acp_session_id: "fixture-acp-session",
    };
    const requests = [];
    await testPage.route("**/webhooks/session-cost**", async (route) => {
      const requestUrl = new URL(route.request().url());
      requests.push({ taskId: requestUrl.searchParams.get("task_id"), active: requestUrl.searchParams.get("active") });
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(fakeResponse) });
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
      const startResponsePromise = testPage.waitForResponse(
        (response) =>
          new URL(response.url()).pathname.endsWith("/quick-chat") &&
          response.request().method() === "POST",
        { timeout: 30_000 },
      );
      await setup.getByTestId("task-description-input").fill("Session Cost action smoke");
      const startButton = setup.getByTestId("quick-chat-send");
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
        const amount = Array.from(buttonElement.querySelectorAll("*" )).find((element) =>
          element.textContent.includes("123") &&
          !Array.from(element.children).some((child) => child.textContent.includes("123")),
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
    await expect(action).toContainText(/123.*456.*789/);
    await expect(action).toHaveAttribute("aria-label", "Session cost");
    await expectComposerGeometry(action);

    if (!touch) await action.press("Enter");
    await expect(action).toHaveAttribute("aria-expanded", "true");
    if (expectAction) await expect(action).toHaveAttribute("aria-pressed", "true");
    const details = testPage
      .locator('[data-slot="tooltip-content"]:visible')
      .getByText("fixture-model", { exact: true })
      .first();
    await expect(details).toBeVisible();

    await testPage.keyboard.press("Escape");
    await expect(action).toHaveAttribute("aria-expanded", "false");
    expect(requests).toHaveLength(1);

    await action.press("Enter");
    await expect(action).toHaveAttribute("aria-expanded", "true");
    await testPage.getByLabel("Refresh session cost").first().click();
    await expect.poll(() => requests.length, { timeout: 10_000 }).toBe(2);

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
    if (touch) {
      const taskBounds = await waitForTouchTarget(taskChatAction, expect);
      expect(taskBounds).not.toBeNull();
      await taskChatAction.tap();
    } else {
      await taskChatAction.focus();
    }
    await expect.poll(() => requests.length, { timeout: 10_000 }).toBe(3);
    expect(requests[2].taskId).toBe(quickChat.task_id);
    expect(requests[2].active).toBe(quickChat.session_id);
    await expect(taskChatAction).toContainText(/123.*456.*789/);
    await expectComposerGeometry(taskChatAction);
    if (testInfo) await captureTaskComposerScreenshot(testPage, testInfo, touch);
  } finally {
    if (installed) await apiClient.rawRequest("DELETE", "/api/plugins/kandev-session-cost").catch(() => undefined);
    if (quickChat?.task_id) await apiClient.deleteTask(quickChat.task_id).catch(() => undefined);
  }
}
