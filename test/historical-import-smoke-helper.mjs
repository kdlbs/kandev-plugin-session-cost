import fs from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { createTokscaleFixture, getACPTranscriptId, installSessionCostPackage } from "./host-action-smoke-helper.mjs";

const hostRoot = process.env.KANDEV_HOST_ROOT || fileURLToPath(new URL("../../kandev", import.meta.url));
const pluginId = "kandev-session-cost";
const secondWorkspaceName = "Historical import queue fixture";

async function createSession({ apiClient, testPage, workspaceId, workflowId, startStepId, repositoryId, seedData, title, expect }) {
  const task = await apiClient.createTaskWithAgent(workspaceId, title, seedData.agentProfileId, {
    workflow_id: workflowId,
    workflow_step_id: startStepId,
    repository_ids: [repositoryId],
    executor_profile_id: seedData.worktreeExecutorProfileId,
    start_agent: false,
    prepare_session: true,
  });
  const sessions = await apiClient.listTaskSessions(task.id);
  const primary = sessions.sessions.find((session) => session.is_primary);
  if (!primary) throw new Error(`Historical import fixture did not prepare a session for ${title}`);

  await testPage.goto(`/t/${task.id}`);
  const { SessionPage } = await import(pathToFileURL(path.join(hostRoot, "apps/web/e2e/pages/session-page.ts")).href);
  const sessionPage = new SessionPage(testPage);
  await sessionPage.waitForLoad();
  await sessionPage.sendMessageViaButton("/slow 1s");
  await expect(sessionPage.chat.getByText("Slow response complete after 1s.", { exact: false })).toBeVisible({ timeout: 60_000 });

  return {
    task,
    sessionId: primary.id,
    transcriptId: await getACPTranscriptId(apiClient, primary.id, expect),
  };
}

async function configure(apiClient, scriptPath, collect, interval, expect) {
  const response = await apiClient.rawRequest("PATCH", `/api/plugins/${pluginId}`, {
    config: {
      command: `/bin/sh ${scriptPath}`,
      collect_statistics: collect,
      collection_interval_minutes: interval,
    },
  });
  expect(response.status, await response.clone().text()).toBe(200);
}

async function readImportStatus(apiClient, workspaceId) {
  const response = await apiClient.rawRequest("POST", `/api/plugins/${pluginId}/actions/historical-import-status`, {
    workspaceId,
    body: {},
  });
  if (!response.ok) return undefined;
  return response.json();
}

async function waitForImportStatus(apiClient, workspaceId, status, expect, timeout = 120_000) {
  let state;
  await expect.poll(async () => {
    state = await readImportStatus(apiClient, workspaceId);
    return state?.status;
  }, { timeout, intervals: [250, 500, 1000], message: `Wait for ${workspaceId} import to become ${status}` }).toBe(status);
  return state;
}

function importedEntry(sessionId, sourceRecordId) {
  return {
    sessionId,
    model: "fixture-import-model",
    provider: "anthropic",
    input: 1200,
    output: 340,
    cacheRead: 50,
    cacheWrite: 10,
    reasoning: 20,
    total: 1620,
    messageCount: 2,
    cost: 0.42,
    sourceRecordId,
  };
}

export async function runHistoricalImportSmoke({ testPage, apiClient, seedData, backend, expect, touch, prCapture }) {
  const packagePath = process.env.SESSION_COST_PACKAGE_PATH;
  if (!packagePath || process.env.SESSION_COST_EXPECT_ACTION !== "1") {
    throw new Error("Historical import smoke requires the rebuilt package and SESSION_COST_EXPECT_ACTION=1.");
  }
  const fixture = createTokscaleFixture();
  fixture.release("release-first");
  fixture.writeReport([]);

  let installed = false;
  let firstTask;
  let skippedTask;
  let secondTask;
  let secondWorkspace;
  let fixtureBlocked = false;

  try {
    await installSessionCostPackage(testPage, packagePath, expect);
    installed = true;
    await configure(apiClient, fixture.scriptPath, false, 60, expect);

    firstTask = await createSession({
      apiClient,
      testPage,
      workspaceId: seedData.workspaceId,
      workflowId: seedData.workflowId,
      startStepId: seedData.startStepId,
      repositoryId: seedData.repositoryId,
      seedData,
      title: "Historical import session",
      expect,
    });
    skippedTask = await createSession({
      apiClient,
      testPage,
      workspaceId: seedData.workspaceId,
      workflowId: seedData.workflowId,
      startStepId: seedData.startStepId,
      repositoryId: seedData.repositoryId,
      seedData,
      title: "Historical import with unsafe attribution",
      expect,
    });

    const legacy = await apiClient.rawRequest("POST", `/api/v1/e2e/usage-history/${seedData.workspaceId}/adopt-legacy-session`, {
      session_id: firstTask.sessionId,
    });
    expect(legacy.status, await legacy.clone().text()).toBe(200);
    const legacyState = await legacy.json();
    expect(legacyState).toMatchObject({
      status: "adopted",
      source_scope_status: "eligible",
      has_transcript_id: true,
    });
    secondWorkspace = await apiClient.createWorkspace(secondWorkspaceName);
    const secondWorkflow = await apiClient.createWorkflow(secondWorkspace.id, "Historical import workflow", "simple");
    const secondSteps = await apiClient.listWorkflowSteps(secondWorkflow.id);
    const secondStartStep = secondSteps.steps.find((step) => step.is_start_step) ?? secondSteps.steps[0];
    const secondRepository = await apiClient.createRepository(
      secondWorkspace.id,
      path.join(backend.tmpDir, "repos", "e2e-repo"),
    );
    secondTask = await createSession({
      apiClient,
      testPage,
      workspaceId: secondWorkspace.id,
      workflowId: secondWorkflow.id,
      startStepId: secondStartStep.id,
      repositoryId: secondRepository.id,
      seedData,
      title: "Queued historical import session",
      expect,
    });
    const secondLegacy = await apiClient.rawRequest("POST", `/api/v1/e2e/usage-history/${secondWorkspace.id}/adopt-legacy-session`, {
      session_id: secondTask.sessionId,
    });
    expect(secondLegacy.status, await secondLegacy.clone().text()).toBe(200);
    const secondLegacyState = await secondLegacy.json();
    expect(secondLegacyState).toMatchObject({
      status: "adopted",
      source_scope_status: "eligible",
      has_transcript_id: true,
    });

    const unsafe = await apiClient.rawRequest("POST", `/api/v1/e2e/usage-history/${seedData.workspaceId}/mark-legacy-session-unsafe`, {
      session_id: skippedTask.sessionId,
    });
    expect(unsafe.status, await unsafe.clone().text()).toBe(200);

    const report = [
      importedEntry(firstTask.transcriptId, "history-import-first"),
      importedEntry(secondTask.transcriptId, "history-import-second"),
    ];
    fixture.writeReport(report);

    const usage = await import(pathToFileURL(path.join(hostRoot, "apps/web/e2e/helpers/usage-history.ts")).href);

    await testPage.goto(`/settings/plugins/${pluginId}`);
    const card = testPage.getByTestId("historical-import-settings");
    await expect(card).toBeVisible({ timeout: 30_000 });
    await expect(card.getByRole("button", { name: "Import history", exact: true })).toBeVisible();

    fixture.blockNext();
    fixtureBlocked = true;
    await configure(apiClient, fixture.scriptPath, true, 1, expect);
    await card.getByRole("button", { name: "Import history", exact: true }).click();
    await expect(card.getByRole("button", { name: "Cancel import", exact: true })).toBeVisible();
    await expect.poll(() => fs.existsSync(fixture.marker("refresh-started")), {
      timeout: 30_000,
      message: "The synthetic tokscale report must be active before the import progress assertion",
    }).toBe(true);
    await expect(card.getByRole("progressbar")).toBeVisible();
    if (prCapture?.capturing) {
      const { waitForFiniteAnimations } = await import(
        pathToFileURL(path.join(hostRoot, "apps/web/e2e/helpers/pr-capture.ts")).href
      );
      await waitForFiniteAnimations(card);
      await prCapture.screenshot(touch ? "phone-import-progress" : "desktop-import-progress", {
        caption: touch ? "Historical import progress on a phone" : "Historical import progress on desktop",
      });
    }

    const queuedStart = await apiClient.rawRequest("POST", `/api/plugins/${pluginId}/actions/historical-import-start`, {
      workspaceId: secondWorkspace.id,
      body: {},
    });
    expect(queuedStart.status, await queuedStart.clone().text()).toBe(202);
    await waitForImportStatus(apiClient, secondWorkspace.id, "queued", expect);

    await backend.restart();
    fixture.release("release-refresh");
    fixtureBlocked = false;

    await testPage.goto(`/settings/plugins/${pluginId}`);
    await expect(card).toBeVisible({ timeout: 30_000 });
    const completed = await waitForImportStatus(apiClient, seedData.workspaceId, "completed", expect);
    expect(completed).toMatchObject({ sessions_found: 2, sessions_imported: 1, sessions_skipped: 1 });
    const queued = await waitForImportStatus(apiClient, secondWorkspace.id, "completed", expect);
    expect(queued).toMatchObject({ sessions_found: 1, sessions_imported: 1, sessions_skipped: 0 });

    const reportReady = await usage.waitForUsage(testPage, seedData.workspaceId, "1620", {
      group: "session",
      task_id: firstTask.task.id,
      session_id: firstTask.sessionId,
    });
    expect(reportReady.totals.amounts_decimal?.cost_subcents).toBe("4200");

    await testPage.goto(`/t/${skippedTask.task.id}`);
    const costAction = testPage.getByRole("button", { name: "Session cost", exact: true });
    await expect(costAction).toBeVisible();
    if (touch) await costAction.tap();
    else {
      await costAction.focus();
      await costAction.press("Enter");
    }
    await expect(testPage.getByRole("tooltip")).toContainText("Cost can't be attributed safely for this session");

    await testPage.goto(`/stats/token-usage?workspaceId=${seedData.workspaceId}&range=all`);
    const summary = testPage.getByTestId("token-usage-summary");
    await expect(testPage.getByRole("tab", { name: "Token Usage", exact: true })).toBeVisible();
    await expect(summary).toContainText("1,620");
  } finally {
    if (fixtureBlocked) fixture.release("release-refresh");
    if (installed) await apiClient.rawRequest("DELETE", `/api/plugins/${pluginId}`).catch(() => undefined);
    if (secondTask?.task?.id) await apiClient.deleteTask(secondTask.task.id, { discardWorktreeChanges: true }).catch(() => undefined);
    if (skippedTask?.task?.id) await apiClient.deleteTask(skippedTask.task.id, { discardWorktreeChanges: true }).catch(() => undefined);
    if (firstTask?.task?.id) await apiClient.deleteTask(firstTask.task.id, { discardWorktreeChanges: true }).catch(() => undefined);
    if (secondWorkspace?.id) await apiClient.deleteWorkspace(secondWorkspace.id, secondWorkspaceName).catch(() => undefined);
    fixture.cleanup();
    await backend.restart();
  }
}
