export async function runSessionCostHostSmoke({
  testPage,
  apiClient,
  seedData,
  expectAction,
  touch,
  expect,
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

    quickChat = await apiClient.startQuickChat(
      seedData.workspaceId,
      seedData.agentProfileId,
      "Session Cost action smoke",
    );

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

    const openQuickChat = async () => {
      await testPage.goto("/");
      await testPage.keyboard.press(`${process.platform === "darwin" ? "Meta" : "Control"}+Shift+q`);
      const dialog = testPage.getByRole("dialog", { name: "Quick Chat" });
      await expect(dialog).toBeVisible({ timeout: 15_000 });
      await expect(dialog.getByTestId("quick-chat-messages")).toBeVisible();
      await expect(dialog.locator(".tiptap.ProseMirror")).toBeVisible({ timeout: 30_000 });
      return dialog;
    };

    const expectLegacyCoarseLayout = async (button) => {
      const layout = await button.evaluate((buttonElement) => {
        const toolbar = buttonElement.closest('[data-testid="mobile-chat-input-toolbar"]');
        const icon = buttonElement.querySelector("svg");
        const amount = Array.from(buttonElement.querySelectorAll("span")).find((element) =>
          element.textContent.includes("123"),
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
          buttonInsideToolbar: Boolean(toolbarBounds && contains(toolbarBounds, buttonBounds)),
          buttonWidth: buttonBounds.right - buttonBounds.left,
        };
      });

      expect(layout.toolbarFound).toBe(true);
      expect(layout.iconInsideButton).toBe(true);
      expect(layout.amountInsideButton).toBe(true);
      expect(layout.buttonInsideToolbar).toBe(true);
      expect(layout.buttonWidth).toBeGreaterThanOrEqual(44);
    };

    const dialog = await openQuickChat();
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
      const bounds = await action.boundingBox();
      expect(bounds).not.toBeNull();
      expect(bounds.height).toBeGreaterThanOrEqual(44);
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
    if (touch && !expectAction) await expectLegacyCoarseLayout(action);

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
      const taskBounds = await taskChatAction.boundingBox();
      expect(taskBounds).not.toBeNull();
      expect(taskBounds.height).toBeGreaterThanOrEqual(44);
      expect(taskBounds.width).toBeGreaterThanOrEqual(44);
      await taskChatAction.tap();
    } else {
      await taskChatAction.focus();
    }
    await expect.poll(() => requests.length, { timeout: 10_000 }).toBe(3);
    expect(requests[2].taskId).toBe(quickChat.task_id);
    expect(requests[2].active).toBe(quickChat.session_id);
    await expect(taskChatAction).toContainText(/123.*456.*789/);
    if (touch && !expectAction) await expectLegacyCoarseLayout(taskChatAction);
  } finally {
    if (installed) await apiClient.rawRequest("DELETE", "/api/plugins/kandev-session-cost").catch(() => undefined);
    if (quickChat?.task_id) await apiClient.deleteTask(quickChat.task_id).catch(() => undefined);
  }
}
