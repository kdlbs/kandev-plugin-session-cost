import fs from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { fileURLToPath, pathToFileURL } from "node:url";
import { createTokscaleFixture, installSessionCostPackage } from "./host-action-smoke-helper.mjs";
import {
  inspectRetainedUsagePluginState,
  SESSION_COST_DEFAULT_DATABASE,
} from "./retained-usage-state-inspection.mjs";

const hostRoot = process.env.KANDEV_HOST_ROOT || fileURLToPath(new URL("../../kandev", import.meta.url));
const pluginId = "kandev-session-cost";
const retainedTaskTitle = "Retained Session Cost native fixture";
const retainedPrompt = "/slow 1s /with-usage";

// This flow installs the actual Session Cost archive. Only the external
// tokscale command is a fixture; admission, projection, reads and lifecycle
// transitions use the real disposable Host. The core fixture reports the
// separate price through the public SDK; native mock-agent events supply tokens.
export async function runSessionCostRetentionSmoke({ testPage, apiClient, seedData, backend, expect, touch, testInfo }) {
  const packagePath = process.env.SESSION_COST_PACKAGE_PATH;
  if (!packagePath || process.env.SESSION_COST_EXPECT_ACTION !== "1") {
    throw new Error("Retained smoke requires the rebuilt archive and SESSION_COST_EXPECT_ACTION=1 on the integrated Host.");
  }
  const usage = await import(pathToFileURL(path.join(hostRoot, "apps/web/e2e/helpers/usage-history.ts")).href);
  const fixturePlugin = await import(pathToFileURL(path.join(hostRoot, "apps/web/e2e/helpers/plugin-fixture.ts")).href);
  const { SessionPage } = await import(pathToFileURL(path.join(hostRoot, "apps/web/e2e/pages/session-page.ts")).href);
  const fixture = createTokscaleFixture();
  fixture.release("release-first");
  fixture.writeReport([]);
  let chat;
  let deliveryTaskId;
  let installed = false;
  let fixtureInstalled = false;
  const configure = async (collect, interval = 60) => {
    const response = await apiClient.rawRequest("PATCH", `/api/plugins/${pluginId}`, {
      config: { command: `/bin/sh ${fixture.scriptPath}`, collect_statistics: collect, collection_interval_minutes: interval },
    });
    expect(response.status, await response.clone().text()).toBe(200);
  };
  const sessionUsage = async (refresh = false) => {
    const response = await apiClient.rawRequest("POST", `/api/plugins/${pluginId}/actions/session-usage`, {
      taskId: chat.task_id, sessionId: chat.session_id, body: { refresh },
    });
    expect(response.status, await response.clone().text()).toBe(200);
    return response.json();
  };
  const actionState = (saved) => {
    const model = saved.models?.length === 1 ? saved.models[0] : null;
    return {
      found: saved.found,
      saved: saved.saved,
      input: saved.token_amounts?.input ?? null,
      output: saved.token_amounts?.output ?? null,
      total: saved.token_amounts?.total ?? null,
      turns: saved.token_amounts?.turns ?? null,
      costKnown: saved.cost_known,
      cost: saved.cost,
      exact: model?.cost_subcents_exact ?? null,
      currency: saved.currency ?? "",
      costBasis: saved.cost_basis ?? "",
      costPerTurn: saved.cost_per_turn,
      modelCount: saved.models?.length ?? 0,
      projectionStatus: saved.projection_status ?? "",
      projectionPending: Boolean(saved.projection_pending),
      generation: Boolean(saved.projection_generation),
    };
  };
  const waitForAction = async (expected, message = "Session Cost must read the completed Host selection") => {
    let saved;
    await expect.poll(async () => {
      saved = await sessionUsage();
      return actionState(saved);
    }, { timeout: 30_000, message }).toMatchObject({
      found: true, saved: true, projectionStatus: "ready", projectionPending: false, generation: true,
      ...expected,
    });
    return saved;
  };
  const exactReport = async () => {
    let report;
    await expect.poll(async () => {
      report = await usage.readUsage(testPage, seedData.workspaceId, { group: "session" });
      return {
        status: report.projection_status,
        pending: report.projection_pending,
        generation: Boolean(report.projection_generation),
        tokens: report.totals.amounts_decimal?.total_tokens,
        cost: report.totals.amounts_decimal?.cost_subcents,
        tokenSource: report.totals.token_source?.source,
        costSource: report.totals.cost_source?.source,
        basis: report.totals.cost_basis,
      };
    }, { timeout: 30_000, message: "The real Host must publish native tokens and the separate reported price" }).toEqual({
      status: "ready", pending: false, generation: true, tokens: "150", cost: "1200",
      tokenSource: "native", costSource: `plugin:${fixturePlugin.PLUGIN_ID}`, basis: "reported",
    });
    expect(report.rows).toHaveLength(1);
    return report;
  };
  const metricExpectation = (options = {}) => {
    const { total = "150", input = "120", output = "30", costKnown = true, cost = 0.12, exact = "1200", currency = "USD" } = options;
    const expected = { input, output, total, turns: null, costKnown, cost, exact, currency, costPerTurn: 0, modelCount: 1 };
    if (costKnown || options.costBasis !== undefined) expected.costBasis = options.costBasis ?? "reported";
    return expected;
  };
  const reportFixture = (observation) => usage.usageFixture(testPage, seedData.workspaceId, {
    operation: "report", ...observation,
  });
  const reportAccepted = async (observation) => {
    const result = await reportFixture(observation);
    expect(result.Results).toHaveLength(1);
    expect(result.Results[0].Status, result.Results[0].Error).toBe("accepted");
    return result;
  };
  const retireFixture = async (subjectBinding, itemId, acceptedRevision = 1) => {
    const result = await usage.usageFixture(testPage, seedData.workspaceId, {
      operation: "tombstone", subject_binding: subjectBinding, item_id: itemId, revision: acceptedRevision + 1,
    });
    expect(result.Results).toHaveLength(1);
    expect(result.Results[0].Status, result.Results[0].Error).toBe("accepted");
  };
  const readCanonicalRow = async (sourceRecordId) => {
    const page = await usage.usageFixture(testPage, seedData.workspaceId, {
      operation: "canonical", filter: { TaskIDs: [chat.task_id], SessionIDs: [chat.session_id] }, page: { Limit: 200 },
    });
    return page.rows.find((row) => row.SourceRecordID === sourceRecordId);
  };
  await apiClient.rawRequest("DELETE", `/api/plugins/${pluginId}`).catch(() => undefined);
  try {
    installed = true;
    await installSessionCostPackage(testPage, packagePath, expect);
    await configure(true);

    const task = await apiClient.createTaskWithAgent(seedData.workspaceId, retainedTaskTitle, seedData.agentProfileId, {
      workflow_id: seedData.workflowId, workflow_step_id: seedData.startStepId,
      repository_ids: [seedData.repositoryId], executor_profile_id: seedData.worktreeExecutorProfileId,
      start_agent: false, prepare_session: true,
    });
    chat = { task_id: task.id, session_id: "" };
    const sessions = await apiClient.listTaskSessions(task.id);
    const primary = sessions.sessions.find((item) => item.is_primary);
    if (!primary) throw new Error("Native Session Cost fixture did not prepare a primary session");
    chat.session_id = primary.id;
    await testPage.goto(`/t/${chat.task_id}`);
    const sessionPage = new SessionPage(testPage);
    await sessionPage.waitForLoad();
    await sessionPage.sendMessageViaButton(retainedPrompt);
    await expect(sessionPage.chat.getByText("Slow response complete after 1s.", { exact: false })).toBeVisible({ timeout: 60_000 });
    const nativeUsage = await usage.waitForUsage(testPage, seedData.workspaceId, "150", { group: "session" });
    expect(nativeUsage.totals.token_source?.source).toBe("native");

    fixtureInstalled = true;
    await fixturePlugin.installFixturePlugin(testPage);
    const read = (operation) => usage.usageFixture(testPage, seedData.workspaceId, {
      operation, filter: { TaskIDs: [chat.task_id], SessionIDs: [chat.session_id] }, page: { Limit: 200 },
    });
    const nativeRows = await read("canonical");
    expect(nativeRows.rows).toHaveLength(1);
    const native = nativeRows.rows[0];
    expect(native.TotalTokens).toBe(150);
    expect(native.CostMetricAbsent).toBe(true);
    expect(nativeRows.page_info.ProjectionGeneration).toBe(nativeUsage.projection_generation);
    const forbiddenPrivateValues = [native.Model, native.Provider, retainedTaskTitle, retainedPrompt]
      .filter((value) => typeof value === "string" && value.trim().length > 0);
    const inspectInstalledPluginState = () => {
      const inspection = inspectRetainedUsagePluginState({
        tmpDir: backend.tmpDir,
        databaseFilename: SESSION_COST_DEFAULT_DATABASE,
        readOnly: true,
        forbiddenValues: forbiddenPrivateValues,
      });
      expect(inspection.rowCount).toBeGreaterThan(0);
      expect(inspection.schemaCounts["privacy-migration-checkpoint"]).toBeGreaterThan(0);
      return inspection;
    };

    const target = await usage.usageTarget(testPage, seedData.workspaceId, chat.task_id, chat.session_id);
    const initialSharedCostRevision = 2;
    let sharedCostRevision = initialSharedCostRevision;
    const sharedCost = {
      subject_binding: target.SubjectBinding, item_id: "reported-native-cost", revision: sharedCostRevision,
      model_id: native.Model, ...(native.Provider && { provider: native.Provider }),
      work_identity: { Scheme: "provider_event", ID: native.SourceRecordID },
      coverage: { OccurrenceAt: native.ObservedAt },
      cost: { amount: "1200", currency: "USD", basis: "reported" },
      quality: { TokenCoverageComplete: false, CostCoverageComplete: true },
    };
    await reportAccepted(sharedCost);
    const published = await exactReport();
    const splitRows = await read("canonical");
    expect(splitRows.rows).toHaveLength(2);
    const nativeTokenRow = splitRows.rows.find((row) => row.SourceRecordID === native.SourceRecordID);
    const separateCostRow = splitRows.rows.find((row) => row.SourceRecordID === "reported-native-cost");
    expect(nativeTokenRow?.TotalTokens).toBe(150);
    expect(nativeTokenRow?.CostMetricAbsent).toBe(true);
    expect(separateCostRow?.TokenMetricAbsent).toBe(true);
    expect(separateCostRow?.CostSubcents).toBe(1200);
    expect(splitRows.page_info.ProjectionGeneration).toBe(published.projection_generation);

    const refreshed = await sessionUsage(true);
    expect(actionState(refreshed)).toMatchObject({
      projectionStatus: "ready", projectionPending: false, generation: true, ...metricExpectation(),
    });
    expect(refreshed.projection_generation).toBe(published.projection_generation);
    const saved = await waitForAction(metricExpectation(), "The installed Session Cost action must read native tokens and the independent reported price after refresh");
    const runCount = fs.readFileSync(fixture.marker("run-count"), "utf8");
    expect(Number(runCount.trim())).toBeGreaterThan(0);
    expect(saved.models[0].cost_subcents_exact).toBe("1200");
    expect(saved.models[0].provider ?? "").toBe(native.Provider ?? "");
    inspectInstalledPluginState();

    const stale = await reportFixture({ ...sharedCost, revision: sharedCostRevision - 1, cost: { amount: "900", currency: "USD", basis: "reported" } });
    expect(stale.Results[0].Status).toBe("stale");
    await waitForAction(metricExpectation(), "A stale Host write must leave the accepted canonical amount selected");
    const conflict = await reportFixture({ ...sharedCost, cost: { amount: "900", currency: "USD", basis: "reported" } });
    expect(conflict.Results[0].Status).toBe("conflict");
    await waitForAction(metricExpectation(), "A conflicting Host write must leave the accepted canonical amount selected");

    await reportAccepted({ ...sharedCost, revision: ++sharedCostRevision, cost: { amount: "0", currency: "USD", basis: "reported" } });
    await waitForAction(metricExpectation({ cost: 0, exact: "0" }), "A measured zero price must stay known as zero");
    await reportAccepted({ ...sharedCost, revision: ++sharedCostRevision });
    await waitForAction(metricExpectation(), "The accepted correction must replace the measured zero");

    const nilCost = {
      ...sharedCost, item_id: "applicable-nil-cost", revision: 1,
      work_identity: { Scheme: "provider_event", ID: "fixture-applicable-nil-cost" },
      cost: { amount: null, currency: "USD", basis: "unknown" },
    };
    await reportAccepted(nilCost);
    const nilCostRow = await readCanonicalRow(nilCost.item_id);
    expect(nilCostRow?.CostMetricAbsent).toBe(false);
    expect(nilCostRow?.CostSubcents).toBeNull();
    await waitForAction(metricExpectation({ costKnown: false, cost: 0, exact: null }), "An applicable nil price must make complete spend unknown while retaining native tokens");
    await retireFixture(target.SubjectBinding, nilCost.item_id);
    await waitForAction(metricExpectation(), "Retiring the nil price must restore the known reported total");

    const nonapplicable = {
      ...sharedCost, item_id: "nonapplicable-cost", revision: 1,
      work_identity: { Scheme: "provider_event", ID: "fixture-nonapplicable-cost" },
      tokens: { input_tokens: "0", output_tokens: "0", total_tokens: "0" },
      cost: undefined,
      quality: { TokenCoverageComplete: true, CostCoverageComplete: false },
    };
    await reportAccepted(nonapplicable);
    const nonapplicableRow = await readCanonicalRow(nonapplicable.item_id);
    expect(nonapplicableRow?.CostMetricAbsent).toBe(true);
    expect(nonapplicableRow?.TokenMetricAbsent).toBe(false);
    expect(nonapplicableRow?.TotalTokens).toBe(0);
    await waitForAction(metricExpectation(), "A nonapplicable cost metric must not taint the independent reported price");
    await retireFixture(target.SubjectBinding, nonapplicable.item_id);
    await waitForAction(metricExpectation());

    const unknownTokens = {
      ...sharedCost, item_id: "applicable-unknown-tokens", revision: 1,
      work_identity: { Scheme: "provider_event", ID: "fixture-applicable-unknown-tokens" },
      tokens: {}, cost: undefined,
      quality: { TokenCoverageComplete: false, CostCoverageComplete: false },
    };
    await reportAccepted(unknownTokens);
    const unknownTokenRow = await readCanonicalRow(unknownTokens.item_id);
    expect(unknownTokenRow?.TokenMetricAbsent).toBe(false);
    expect(unknownTokenRow?.TotalTokens).toBeNull();
    expect(unknownTokenRow?.CostMetricAbsent).toBe(true);
    await waitForAction(metricExpectation({ total: null, input: null, output: null }), "Unknown applicable tokens must not hide known independent cost");
    await retireFixture(target.SubjectBinding, unknownTokens.item_id);
    await waitForAction(metricExpectation());

    const partialCost = {
      ...sharedCost, item_id: "partial-cost", revision: 1,
      work_identity: { Scheme: "provider_event", ID: "fixture-partial-cost" },
      cost: { amount: "20", currency: "USD", basis: "reported" },
      quality: { TokenCoverageComplete: false, CostCoverageComplete: false },
    };
    await reportAccepted(partialCost);
    const partialRow = await readCanonicalRow(partialCost.item_id);
    expect(partialRow?.CostMetricAbsent).toBe(false);
    expect(partialRow?.CostSubcents).toBe(20);
    expect(partialRow?.CostCoverage).toBe("partial");
    await waitForAction(metricExpectation({ costKnown: false, cost: 0 }), "Partial cost must remain unavailable as complete spend");
    await retireFixture(target.SubjectBinding, partialCost.item_id);
    await waitForAction(metricExpectation());

    await reportAccepted({ ...sharedCost, revision: ++sharedCostRevision, cost: { amount: "1200", currency: "EUR", basis: "reported" } });
    await waitForAction(metricExpectation({ costKnown: false, cost: 0, currency: "EUR", costBasis: "reported" }), "Foreign currency must remain unavailable to USD formatting");
    await reportAccepted({ ...sharedCost, revision: ++sharedCostRevision });
    await waitForAction(metricExpectation());
    const foreignCost = {
      ...sharedCost, item_id: "mixed-currency-cost", revision: 1,
      work_identity: { Scheme: "provider_event", ID: "fixture-mixed-currency-cost" },
      cost: { amount: "20", currency: "EUR", basis: "reported" },
    };
    await reportAccepted(foreignCost);
    await waitForAction(metricExpectation({ costKnown: false, cost: 0, exact: null, currency: "mixed" }), "Mixed currencies must not acquire an invented common exact amount");
    await retireFixture(target.SubjectBinding, foreignCost.item_id);
    await waitForAction(metricExpectation());

    for (const operation of ["report", "tombstone"]) {
      const rejected = await usage.usageFixture(testPage, seedData.workspaceId, {
        operation, subject_binding: "foreign-workspace-binding", item_id: `foreign-${operation}`, revision: 1,
        ...(operation === "report" && {
          cost: { amount: "99", currency: "USD", basis: "reported" },
          quality: { TokenCoverageComplete: false, CostCoverageComplete: true },
        }),
      });
      expect(rejected.Results).toHaveLength(1);
      expect(rejected.Results[0].Status).toBe("rejected");
      expect(rejected.Results[0].Error).toBe("usage target not found");
    }
    await waitForAction(metricExpectation(), "Rejected foreign writes and retirements must not change the authorized selection");
    expect(fs.readFileSync(fixture.marker("run-count"), "utf8")).toBe(runCount);

    const forged = await apiClient.rawRequest("POST", `/api/plugins/${pluginId}/actions/session-usage`, {
      workspaceId: "mismatched-workspace", taskId: chat.task_id, sessionId: chat.session_id, body: { refresh: true },
    });
    expect(forged.status, await forged.text()).toBe(400);
    const crossSession = await apiClient.rawRequest("POST", `/api/plugins/${pluginId}/actions/session-usage`, {
      taskId: chat.task_id, sessionId: "foreign-session-id", body: { refresh: true },
    });
    expect(crossSession.status, await crossSession.text()).toBe(404);

    await testPage.goto(`/t/${chat.task_id}`);
    const action = testPage.getByRole("button", { name: "Session cost", exact: true });
    await expect(action).toBeVisible();
    if (touch) await action.tap();
    else { await action.focus(); await action.press("Enter"); }
    await expect(action).toContainText("$0.12");
    await expect(testPage.locator('[data-slot="tooltip-content"]:visible')).toContainText(native.Model);
    expect(fs.readFileSync(fixture.marker("run-count"), "utf8")).toBe(runCount);
    if (touch) {
      const box = await action.boundingBox();
      expect(box.width).toBeGreaterThanOrEqual(44);
      expect(box.height).toBeGreaterThanOrEqual(44);
    }
    expect(await testPage.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);

    const disabled = await apiClient.rawRequest("POST", `/api/plugins/${pluginId}/disable`);
    expect(disabled.status, await disabled.text()).toBe(200);
    await exactReport();
    const enabled = await apiClient.rawRequest("POST", `/api/plugins/${pluginId}/enable`);
    expect(enabled.status, await enabled.text()).toBe(200);
    await waitForAction(metricExpectation());
    await configure(false);
    await backend.restart();
    await waitForAction(metricExpectation());
    await exactReport();
    inspectInstalledPluginState();

    const deliveryRetention = await import(
      pathToFileURL(path.join(hostRoot, "apps/web/e2e/helpers/delivery-retention.ts")).href
    );
    const prunedDelivery = await deliveryRetention.pruneSavedDelivery(
      testPage,
      apiClient,
      seedData,
      backend,
      {
        prompt: "/slow 1s /with-usage",
        beforePrune: async (taskId, sessionId) => {
          await usage.waitForUsage(testPage, seedData.workspaceId, "150", {
            group: "session",
            task_id: taskId,
            session_id: sessionId,
          });
          const target = await usage.usageTarget(testPage, seedData.workspaceId, taskId, sessionId);
          const canonical = await usage.usageFixture(testPage, seedData.workspaceId, {
            operation: "canonical",
            filter: { TaskIDs: [taskId], SessionIDs: [sessionId] },
            page: { Limit: 200 },
          });
          const native = canonical.rows.find((row) => !row.TokenMetricAbsent);
          expect(native?.TotalTokens).toBe(150);
          expect(target.SubjectBinding).toBeTruthy();
          if (!native) throw new Error("Pruned delivery fixture has no native usage row");
          await usage.reportFixtureUsage(testPage, seedData.workspaceId, {
            subject_binding: target.SubjectBinding,
            item_id: `pruned-delivery-cost-${randomUUID()}`,
            revision: 2,
            model_id: native.Model,
            ...(native.Provider && { provider: native.Provider }),
            work_identity: { Scheme: "provider_event", ID: native.SourceRecordID },
            coverage: { OccurrenceAt: native.ObservedAt },
            cost: { amount: "1200", currency: "USD", basis: "reported" },
            quality: { TokenCoverageComplete: false, CostCoverageComplete: true },
          });
        },
      },
    );
    deliveryTaskId = prunedDelivery.task.id;
    const prunedAction = async () => {
      const response = await apiClient.rawRequest("POST", `/api/plugins/${pluginId}/actions/session-usage`, {
        taskId: prunedDelivery.task.id,
        sessionId: prunedDelivery.sessionId,
        body: { refresh: false },
      });
      expect(response.status, await response.clone().text()).toBe(200);
      return response.json();
    };
    let prunedSaved;
    await expect.poll(async () => {
      prunedSaved = await prunedAction();
      return actionState(prunedSaved);
    }, {
      timeout: 30_000,
      message: "Installed Session Cost must retain native tokens and reported cost after backend delivery payload pruning",
    }).toMatchObject({
      found: true,
      saved: true,
      input: 120,
      output: 30,
      total: 150,
      costKnown: true,
      cost: 0.12,
      exact: "1200",
      currency: "USD",
      costBasis: "reported",
    });
    expect(prunedSaved.models).toHaveLength(1);
    await apiClient.deleteSession(prunedDelivery.sessionId);
    const prunedSessionHistory = await usage.readUsage(testPage, seedData.workspaceId, {
      group: "session",
      task_id: prunedDelivery.task.id,
      session_id: prunedDelivery.sessionId,
    });
    expect(prunedSessionHistory.totals.amounts_decimal?.total_tokens).toBe("150");
    expect(prunedSessionHistory.totals.amounts_decimal?.cost_subcents).toBe("1200");
    expect(prunedSessionHistory.rows[0].session_available).toBe(false);

    await apiClient.archiveTask(chat.task_id);
    const archived = await exactReport();
    expect(archived.rows[0].task_available).toBe(true);
    expect(archived.rows[0].task_archived).toBe(true);
    await apiClient.deleteSession(chat.session_id);
    const deletedSession = await exactReport();
    expect(deletedSession.rows[0].session_available).toBe(false);
    await apiClient.deleteTask(chat.task_id);
    const deletedTask = await exactReport();
    expect(deletedTask.rows[0].task_available).toBe(false);
    expect((await read("canonical")).rows.find((row) => !row.TokenMetricAbsent)?.TotalTokens).toBe(150);
    const denied = await apiClient.rawRequest("POST", `/api/plugins/${pluginId}/actions/session-usage`, {
      taskId: chat.task_id, sessionId: chat.session_id, body: { refresh: true },
    });
    expect(denied.status, await denied.text()).toBe(404);

    await configure(true, 1);
    const imported = await apiClient.rawRequest("POST", `/api/plugins/${pluginId}/actions/historical-import-start`, {
      workspaceId: seedData.workspaceId, body: {},
    });
    expect(imported.status, await imported.clone().text()).toBe(202);
    let importState;
    await expect.poll(async () => {
      const response = await apiClient.rawRequest("POST", `/api/plugins/${pluginId}/actions/historical-import-status`, {
        workspaceId: seedData.workspaceId, body: {},
      });
      expect(response.status, await response.clone().text()).toBe(200);
      importState = await response.json();
      return importState.status;
    }, { timeout: 90_000, message: "Retained targets must remain importable after live task/session deletion, with the configured one-minute import budget" }).toBe("completed");
    expect(importState.processed).toBeGreaterThanOrEqual(1);
    expect(importState.missing).toBe(0);
    expect(importState.diagnostics_reliable).toBe(true);
    expect(importState).not.toHaveProperty("snapshot_id");
    expect(importState).not.toHaveProperty("page_targets");
    await exactReport();
    inspectInstalledPluginState();

    const uninstalled = await apiClient.rawRequest("DELETE", `/api/plugins/${pluginId}`);
    expect(uninstalled.status, await uninstalled.text()).toBe(200);
    installed = false;
    const retained = await exactReport();
    expect(retained.rows[0].task_available).toBe(false);
    expect(retained.rows[0].session_available).toBe(false);
    expect((await read("canonical")).rows.find((row) => !row.TokenMetricAbsent)?.TotalTokens).toBe(150);
    expect((await read("source")).rows).toEqual([]);
    await testPage.goto(`/stats/token-usage?workspaceId=${seedData.workspaceId}&range=all`);
    const breakdown = await usage.selectUsageView(testPage, "Sessions", touch);
    const row = breakdown.getByTestId(touch ? "token-usage-mobile-row" : "token-usage-row");
    await expect(row).toContainText("Unavailable session");
    await expect(row.locator("a")).toHaveCount(0);
    await expect(testPage.getByTestId("token-usage-summary")).toContainText("150");
    if (testInfo) {
      await testInfo.attach("session-cost-retained-package.json", { body: JSON.stringify({ saved, retained, importState }, null, 2), contentType: "application/json" });
      await testInfo.attach(touch ? "retained-phone.png" : "retained-desktop.png", { body: await testPage.screenshot(), contentType: "image/png" });
    }
  } finally {
    try {
      fixture.release("release-first");
      fixture.release("release-refresh");
      if (installed) await apiClient.rawRequest("DELETE", `/api/plugins/${pluginId}`).catch(() => undefined);
      if (fixtureInstalled) await fixturePlugin.uninstallFixturePlugin(apiClient);
      if (deliveryTaskId) await apiClient.deleteTask(deliveryTaskId, { discardWorktreeChanges: true }).catch(() => undefined);
      if (chat) await apiClient.deleteTask(chat.task_id).catch(() => undefined);
    } finally {
      fixture.cleanup();
      await backend.restart();
    }
  }
}
