import fs from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { createRequire } from "node:module";
import { fileURLToPath, pathToFileURL } from "node:url";
import { createTokscaleFixture, installSessionCostPackage } from "./host-action-smoke-helper.mjs";
import {
  inspectRetainedUsagePluginState,
  SESSION_COST_AUTH_DATABASE,
} from "./retained-usage-state-inspection.mjs";

const hostRoot =
  process.env.KANDEV_HOST_ROOT || fileURLToPath(new URL("../../kandev", import.meta.url));
const requireFromHost = createRequire(path.join(hostRoot, "apps/web/package.json"));
const { expect } = requireFromHost("@playwright/test");
const pluginId = "kandev-session-cost";
const retainedPrivateValues = new Set([
  "Private retained task",
  "retained-private-model",
  "private-provider",
].filter((value) => typeof value === "string" && value.trim().length > 0));

async function hostModule(relativePath) {
  return import(pathToFileURL(path.join(hostRoot, relativePath)).href);
}

async function createNativeUsage(page, workspaceId, usage, SessionPage) {
  const agentsResponse = await page.request.get("/api/v1/agents");
  expect(agentsResponse.status(), await agentsResponse.text()).toBe(200);
  const { agents } = await agentsResponse.json();
  const agentProfileId = agents
    .filter((agent) => agent.id !== "dynamic")
    .flatMap((agent) => agent.profiles ?? [])
    .find((profile) => profile.enabled !== false)?.id;
  if (!agentProfileId) throw new Error("Authenticated native usage fixture has no enabled agent profile");

  const response = await page.request.post(`/api/v1/workspaces/${workspaceId}/quick-chat`, {
    data: { title: "Private native usage", agent_profile_id: agentProfileId },
  });
  expect(response.status(), await response.text()).toBe(200);
  const chat = await response.json();
  const settingsResponse = await page.request.patch("/api/v1/user/settings", {
    data: { workspace_id: workspaceId },
  });
  expect(settingsResponse.status(), await settingsResponse.text()).toBe(200);

  await page.goto(`/t/${chat.task_id}`);
  const sessionPage = new SessionPage(page);
  await sessionPage.waitForLoad();
  await sessionPage.sendMessageViaButton("/slow 1s /with-usage");
  await expect(
    sessionPage.chat.getByText("Slow response complete after 1s.", { exact: false }),
  ).toBeVisible({ timeout: 60_000 });

  const nativeUsage = await usage.waitForUsage(page, workspaceId, "150", {
    group: "session",
    task_id: chat.task_id,
    session_id: chat.session_id,
  });
  expect(nativeUsage.totals.token_source?.source).toBe("native");

  const canonical = await usage.usageFixture(page, workspaceId, {
    operation: "canonical",
    filter: { TaskIDs: [chat.task_id], SessionIDs: [chat.session_id] },
    page: { Limit: 200 },
  });
  expect(canonical.rows).toHaveLength(1);
  const native = canonical.rows[0];
  expect(native.TotalTokens).toBe(150);
  expect(native.SourceRecordID).toBeTruthy();
  expect(native.Model).toBeTruthy();
  expect(typeof native.Provider).toBe("string");
  expect(native.ObservedAt).toBeTruthy();
  for (const value of [native.Model, native.Provider]) {
    if (typeof value === "string" && value.trim().length > 0) retainedPrivateValues.add(value);
  }

  const target = await usage.usageTarget(page, workspaceId, chat.task_id, chat.session_id);
  return { ...chat, target, native };
}

export async function runSessionCostAuthenticatedRetentionSmoke({ backend, browser }) {
  const packagePath = process.env.SESSION_COST_PACKAGE_PATH;
  if (!packagePath) throw new Error("Retained auth smoke requires SESSION_COST_PACKAGE_PATH.");

  const [auth, fixturePlugin, usage, authUsage, sessionPageModule] = await Promise.all([
    hostModule("apps/web/e2e/helpers/auth.ts"),
    hostModule("apps/web/e2e/helpers/plugin-fixture.ts"),
    hostModule("apps/web/e2e/helpers/usage-history.ts"),
    hostModule("apps/web/e2e/helpers/usage-history-auth.ts"),
    hostModule("apps/web/e2e/pages/session-page.ts"),
  ]);
  const { SessionPage } = sessionPageModule;

  await backend.restart({
    KANDEV_FEATURES_AUTH: "true",
    KANDEV_FEATURES_MULTI_TENANCY: "true",
    KANDEV_DATABASE_PATH: path.join(backend.tmpDir, authUsage.USAGE_AUTH_DATABASE),
  });

  let operator;
  let anonymousContext;
  const fixture = createTokscaleFixture();
  let operatorPage;
  let alpha;
  let beta;
  let sessionCostInstalled = false;
  let fixturePluginInstalled = false;
  const suffix = randomUUID().slice(0, 8);
  const operatorCredentials = {
    email: `usage-operator-${suffix}@e2e.dev`,
    password: randomUUID(),
    displayName: "Usage operator",
  };

  const installCostPlugin = async () => {
    await installSessionCostPackage(operatorPage, packagePath, expect);
    sessionCostInstalled = true;
    await fixturePlugin.installFixturePlugin(operatorPage);
    fixturePluginInstalled = true;
    fixture.release("release-first");
    fixture.release("release-refresh");
    fixture.writeReport([]);
    const configured = await operator.request.patch(`/api/plugins/${pluginId}`, {
      data: {
        config: {
          command: `/bin/sh ${fixture.scriptPath}`,
          collect_statistics: true,
          collection_interval_minutes: 60,
        },
      },
    });
    expect(configured.status(), await configured.text()).toBe(200);
  };

  const inspectInstalledPluginState = () => {
    const inspection = inspectRetainedUsagePluginState({
      tmpDir: backend.tmpDir,
      databaseFilename: SESSION_COST_AUTH_DATABASE,
      readOnly: true,
      forbiddenValues: [...retainedPrivateValues],
    });
    expect(inspection.rowCount).toBeGreaterThan(0);
    expect(inspection.schemaCounts["privacy-migration-checkpoint"]).toBeGreaterThan(0);
    return inspection;
  };

  const sessionCostAction = async (page, workspaceId, taskId, sessionId) => {
    const response = await page.request.post(`/api/plugins/${pluginId}/actions/session-usage`, {
      data: { workspaceId, taskId, sessionId, body: { refresh: false } },
    });
    return response;
  };

  const fixtureRunCount = () => {
    const marker = fixture.marker("run-count");
    return fs.existsSync(marker) ? fs.readFileSync(marker, "utf8") : null;
  };

  const reportCost = async (page, workspaceId, target, native, itemId) => {
    await usage.reportFixtureUsage(page, workspaceId, {
      subject_binding: target.SubjectBinding,
      item_id: itemId,
      revision: 2,
      model_id: native.Model,
      ...(native.Provider && { provider: native.Provider }),
      work_identity: { Scheme: "provider_event", ID: native.SourceRecordID },
      coverage: { OccurrenceAt: native.ObservedAt },
      cost: { amount: "1200", currency: "USD", basis: "reported" },
      quality: { TokenCoverageComplete: false, CostCoverageComplete: true },
    });
  };

  try {
    operator = await browser.newContext({ baseURL: backend.frontendUrl });
    await auth.setupAdmin(operator, backend.baseUrl, operatorCredentials);
    await auth.login(operator, backend.baseUrl, operatorCredentials);
    operatorPage = await operator.newPage();
    await installCostPlugin();

    alpha = await authUsage.createUsageTenant(
      browser,
      backend,
      operator,
      `usage-alpha-${suffix}`,
      "31",
    );
    beta = await authUsage.createUsageTenant(
      browser,
      backend,
      operator,
      `usage-beta-${suffix}`,
      "47",
    );

    await usage.waitForUsage(alpha.page, alpha.workspace, "31");
    await usage.waitForUsage(beta.page, beta.workspace, "47");
    const alphaNative = await createNativeUsage(alpha.page, alpha.workspace, usage, SessionPage);
    const expectedAlphaTokens = "181";
    await usage.waitForUsage(alpha.page, alpha.workspace, expectedAlphaTokens);
    expect(
      (await usage.readUsage(alpha.page, alpha.workspace)).totals.amounts_decimal?.total_tokens,
    ).toBe(expectedAlphaTokens);
    expect(
      (await usage.readUsage(beta.page, beta.workspace)).totals.amounts_decimal?.total_tokens,
    ).toBe("47");

    const alphaTarget = alphaNative.target;
    await reportCost(alpha.page, alpha.workspace, alphaTarget, alphaNative.native, `auth-cost-${suffix}`);

    let alphaNativeUsage;
    await expect.poll(async () => {
      alphaNativeUsage = await usage.readUsage(alpha.page, alpha.workspace, {
        task_id: alphaNative.task_id,
        session_id: alphaTarget.SessionID,
      });
      return alphaNativeUsage.totals.amounts_decimal?.cost_subcents;
    }, {
      timeout: 30_000,
      message: "The authenticated task stats API must expose the exact native event's accepted Host reported cost",
    }).toBe("1200");
    expect(alphaNativeUsage.totals.amounts_decimal?.total_tokens).toBe("150");
    expect(alphaNativeUsage.totals.token_source?.source).toBe("native");
    const alphaAction = await sessionCostAction(
      alpha.page,
      alpha.workspace,
      alphaNative.task_id,
      alphaTarget.SessionID,
    );
    expect(alphaAction.status(), await alphaAction.text()).toBe(200);
    const alphaSaved = await alphaAction.json();
    expect(alphaSaved).toMatchObject({
      found: true,
      saved: true,
      cost_known: true,
      cost: 0.12,
      currency: "USD",
    });
    expect(alphaSaved.models).toHaveLength(1);
    expect(alphaSaved.models[0].cost_subcents_exact).toBe("1200");

    const betaForeignWorkspace = await beta.page.request.get(
      `/api/v1/workspaces/${alpha.workspace}/stats/token-usage?range=all&org_id=${alpha.org.id}`,
    );
    expect(betaForeignWorkspace.status(), await betaForeignWorkspace.text()).toBe(404);
    const alphaForeignWorkspace = await alpha.page.request.get(
      `/api/v1/workspaces/${beta.workspace}/stats/token-usage?range=all&org_id=${beta.org.id}`,
    );
    expect(alphaForeignWorkspace.status(), await alphaForeignWorkspace.text()).toBe(404);
    const betaForeignAction = await sessionCostAction(
      beta.page,
      alpha.workspace,
      alphaNative.task_id,
      alphaTarget.SessionID,
    );
    expect(betaForeignAction.status(), await betaForeignAction.text()).toBe(404);

    const alphaWorkspaceUsage = await usage.readUsage(alpha.page, alpha.workspace);
    const alphaWorkspaceBeforeLegacy = {
      total_tokens: alphaWorkspaceUsage.totals.amounts_decimal?.total_tokens,
      cost_subcents: alphaWorkspaceUsage.totals.amounts_decimal?.cost_subcents,
    };
    expect(alphaWorkspaceBeforeLegacy).toEqual({
      total_tokens: expectedAlphaTokens,
      cost_subcents: null,
    });
    const alphaCanonicalBeforeLegacy = {
      total_tokens: alphaNativeUsage.totals.amounts_decimal?.total_tokens,
      cost_subcents: alphaNativeUsage.totals.amounts_decimal?.cost_subcents,
    };
    expect(alphaCanonicalBeforeLegacy).toEqual({
      total_tokens: "150",
      cost_subcents: "1200",
    });
    const alphaActionSnapshot = (payload) => ({
      found: payload.found,
      saved: payload.saved,
      cost_known: payload.cost_known,
      cost: payload.cost,
      currency: payload.currency,
      models: payload.models.map((model) => ({
        cost_subcents_exact: model.cost_subcents_exact,
      })),
    });
    const alphaActionBeforeLegacy = alphaActionSnapshot(alphaSaved);

    const legacyQuery = new URLSearchParams({
      task_id: alphaNative.task_id,
      active: alphaTarget.SessionID,
    });
    const legacyWebhookPath = `/api/plugins/${pluginId}/webhooks/session-cost?${legacyQuery}`;
    anonymousContext = await browser.newContext({ baseURL: backend.frontendUrl });
    expect(await anonymousContext.cookies()).toEqual([]);
    const sourceRunsBeforeLegacy = fixtureRunCount();
    const anonymousLegacy = await anonymousContext.request.get(legacyWebhookPath);
    expect(anonymousLegacy.status(), "anonymous legacy webhook request must require authentication").toBe(401);
    const sameOriginLegacyRequest = async (page) => {
      await page.goto("/");
      return page.evaluate(async (requestPath) => {
        const response = await fetch(requestPath, { credentials: "same-origin" });
        return { status: response.status, body: await response.text() };
      }, legacyWebhookPath);
    };
    const betaLegacy = await sameOriginLegacyRequest(beta.page);
    expect(betaLegacy.status, "authenticated foreign legacy request must return fixed guidance").toBe(200);
    const alphaLegacy = await sameOriginLegacyRequest(alpha.page);
    expect(alphaLegacy.status, "authenticated owner legacy request must return upgrade guidance").toBe(200);

    const betaLegacyPayload = JSON.parse(betaLegacy.body);
    const alphaLegacyPayload = JSON.parse(alphaLegacy.body);
    const expectedLegacyPayload = {
      compatibility: "upgrade_required",
      error: "Use the authenticated session-usage action to view or refresh usage.",
    };
    const legacyUsageFields = [
      "found",
      "saved",
      "cost_known",
      "amounts",
      "token_amounts",
      "cost",
      "models",
      "task_id",
      "kandev_session_id",
      "acp_session_id",
    ];
    for (const payload of [betaLegacyPayload, alphaLegacyPayload]) {
      expect(payload.compatibility).toBe("upgrade_required");
      expect(
        legacyUsageFields.filter((field) => Object.prototype.hasOwnProperty.call(payload, field)),
        "legacy webhook guidance must not expose usage values or target identifiers",
      ).toEqual([]);
      expect(payload).toEqual(expectedLegacyPayload);
    }
    expect(alphaLegacyPayload).toEqual(betaLegacyPayload);
    expect(fixtureRunCount()).toBe(sourceRunsBeforeLegacy);

    const alphaUsageAfterLegacy = await usage.readUsage(alpha.page, alpha.workspace);
    expect({
      total_tokens: alphaUsageAfterLegacy.totals.amounts_decimal?.total_tokens,
      cost_subcents: alphaUsageAfterLegacy.totals.amounts_decimal?.cost_subcents,
    }).toEqual(alphaWorkspaceBeforeLegacy);
    const alphaNativeUsageAfterLegacy = await usage.readUsage(alpha.page, alpha.workspace, {
      task_id: alphaNative.task_id,
      session_id: alphaTarget.SessionID,
    });
    expect({
      total_tokens: alphaNativeUsageAfterLegacy.totals.amounts_decimal?.total_tokens,
      cost_subcents: alphaNativeUsageAfterLegacy.totals.amounts_decimal?.cost_subcents,
    }).toEqual(alphaCanonicalBeforeLegacy);
    const alphaActionAfterLegacyResponse = await sessionCostAction(
      alpha.page,
      alpha.workspace,
      alphaNative.task_id,
      alphaTarget.SessionID,
    );
    expect(alphaActionAfterLegacyResponse.status()).toBe(200);
    const alphaActionAfterLegacy = await alphaActionAfterLegacyResponse.json();
    expect(alphaActionSnapshot(alphaActionAfterLegacy)).toEqual(alphaActionBeforeLegacy);
    const betaUsageAfterLegacy = await usage.readUsage(beta.page, beta.workspace);
    expect(betaUsageAfterLegacy.totals.amounts_decimal?.total_tokens).toBe("47");

    const forgedBinding = await usage.usageFixture(beta.page, beta.workspace, {
      operation: "report",
      subject_binding: alphaTarget.SubjectBinding,
      item_id: `issued-foreign-binding-${suffix}`,
      revision: 2,
      model_id: "forged-private-model",
      tokens: { total_tokens: "9999" },
      quality: { TokenCoverageComplete: true, CostCoverageComplete: false },
    });
    expect(forgedBinding.Results).toHaveLength(1);
    expect(forgedBinding.Results[0].Status).toBe("rejected");

    const transferred = await authUsage.seedAuthenticatedUsage(alpha.page, "53");
    const transferredNative = await createNativeUsage(
      alpha.page,
      transferred.workspace,
      usage,
      SessionPage,
    );
    const transferredTarget = transferredNative.target;
    expect(transferredTarget.SessionID).toBeTruthy();
    await reportCost(
      alpha.page,
      transferred.workspace,
      transferredTarget,
      transferredNative.native,
      `transferred-cost-${suffix}`,
    );
    let beforeTransferAction;
    await expect.poll(async () => {
      const response = await sessionCostAction(
        alpha.page,
        transferred.workspace,
        transferredNative.task_id,
        transferredTarget.SessionID,
      );
      expect(response.status()).toBe(200);
      beforeTransferAction = await response.json();
      return beforeTransferAction.models?.[0]?.cost_subcents_exact;
    }, {
      timeout: 30_000,
      message: "The installed session-usage action must expose the accepted Host reported cost before workspace transfer",
    }).toBe("1200");
    expect(beforeTransferAction).toMatchObject({
      found: true,
      saved: true,
      cost_known: true,
      cost: 0.12,
      currency: "USD",
    });
    expect(beforeTransferAction.models).toHaveLength(1);
    expect(beforeTransferAction.models[0].cost_subcents_exact).toBe("1200");

    const capturedAlphaScopes = authUsage.retainedOrganizationScopes(backend, alpha.org.id);
    expect(capturedAlphaScopes.length).toBeGreaterThan(0);
    const capturedAlphaRows = authUsage.retainedOrganizationRows(backend, alpha.org.id).count;
    expect(capturedAlphaRows).toBeGreaterThan(0);
    authUsage.transferUsageWorkspace(backend, transferred.workspace, beta);
    expect(authUsage.retainedOrganizationScopes(backend, alpha.org.id)).toEqual(capturedAlphaScopes);
    expect(authUsage.retainedOrganizationRows(backend, alpha.org.id).count).toBe(capturedAlphaRows);

    const formerOwner = await alpha.page.request.get(
      `/api/v1/workspaces/${transferred.workspace}/stats/token-usage?range=all`,
    );
    expect(formerOwner.status(), await formerOwner.text()).toBe(404);
    const newOwnerReport = await usage.readUsage(beta.page, transferred.workspace);
    expect(newOwnerReport.rows).toEqual([]);
    expect(newOwnerReport.has_history).toBe(false);
    expect(newOwnerReport.totals.total_tokens).toBeNull();
    const oldBindingAfterTransfer = await usage.usageFixture(beta.page, transferred.workspace, {
      operation: "report",
      subject_binding: transferredTarget.SubjectBinding,
      item_id: `transferred-old-binding-${suffix}`,
      revision: 2,
      tokens: { total_tokens: "8888" },
      quality: { TokenCoverageComplete: true, CostCoverageComplete: false },
    });
    expect(oldBindingAfterTransfer.Results).toHaveLength(1);
    expect(oldBindingAfterTransfer.Results[0].Status).toBe("rejected");
    const newOwnerAction = await sessionCostAction(
      beta.page,
      transferred.workspace,
      transferredNative.task_id,
      transferredTarget.SessionID,
    );
    expect([200, 404]).toContain(newOwnerAction.status());
    if (newOwnerAction.status() === 200) {
      const noTransferredUsage = await newOwnerAction.json();
      expect(noTransferredUsage.found).toBe(false);
      expect(noTransferredUsage.cost_known).toBe(false);
      expect(noTransferredUsage.models).toEqual([]);
    }
    inspectInstalledPluginState();

    const alphaScopes = authUsage.retainedOrganizationScopes(backend, alpha.org.id);
    const beforeErasure = authUsage.retainedScopeFootprint(backend, alphaScopes);
    expect(beforeErasure.usage_canonical_facts).toBeGreaterThan(0);
    const erased = await operator.request.delete(`/api/v1/instance/orgs/${alpha.org.id}`, {
      data: { slug: alpha.org.slug },
    });
    expect(erased.status(), await erased.text()).toBe(200);
    expect(authUsage.retainedOrganizationRows(backend, alpha.org.id).count).toBe(0);
    for (const count of Object.values(authUsage.retainedScopeFootprint(backend, alphaScopes))) {
      expect(count).toBe(0);
    }
    await usage.waitForUsage(beta.page, beta.workspace, "47");
    expect((await usage.readUsage(beta.page, beta.workspace)).totals.amounts_decimal?.total_tokens).toBe("47");
    inspectInstalledPluginState();
  } finally {
    try {
      if (sessionCostInstalled)
        await operator.request.delete(`/api/plugins/${pluginId}`).catch(() => undefined);
      if (fixturePluginInstalled)
        await operator.request.delete(`/api/plugins/${fixturePlugin.PLUGIN_ID}`).catch(() => undefined);
      await anonymousContext?.close();
      await alpha?.context.close();
      await beta?.context.close();
      await operator?.close();
    } finally {
      try {
        await backend.restart();
      } finally {
        fixture.cleanup();
      }
    }
  }
}
