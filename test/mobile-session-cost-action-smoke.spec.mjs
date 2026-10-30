import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { runSessionCostHostSmoke } from "./host-action-smoke-helper.mjs";
import { runSessionCostRetentionSmoke } from "./retained-usage-smoke-helper.mjs";

const hostRoot = process.env.KANDEV_HOST_ROOT || fileURLToPath(new URL("../../kandev", import.meta.url));
const { test, expect } = await import(
  pathToFileURL(path.join(hostRoot, "apps/web/e2e/fixtures/test-base.ts")).href
);
const expectAction = process.env.SESSION_COST_EXPECT_ACTION === "1";
const expectUpgradeRequired = process.env.SESSION_COST_EXPECT_UPGRADE_REQUIRED === "1";

test.describe("Session Cost packaged action on a phone host", () => {
  test.afterEach(async ({ apiClient }) => {
    await apiClient.rawRequest("DELETE", "/api/plugins/kandev-session-cost").catch(() => undefined);
  });

  test(
    expectUpgradeRequired
      ? "returns upgrade_required before source access on an older phone Host"
      : "keeps the composer action touch-sized and opens its details",
    async ({
      testPage,
      apiClient,
      seedData,
      backend,
    }, testInfo) => {
      test.setTimeout(240_000);
      await runSessionCostHostSmoke({
        testPage,
        apiClient,
        seedData,
        backend,
        testInfo,
        expectAction,
        expectUpgradeRequired,
        touch: true,
        expect,
      });
    },
  );

  test("retains real Host admitted usage through restart, archive, deletion and uninstall on a phone", async ({ testPage, apiClient, seedData, backend }, testInfo) => {
    test.skip(expectUpgradeRequired, "The selected older Host is expected to require retained-usage support.");
    test.setTimeout(360_000);
    await runSessionCostRetentionSmoke({ testPage, apiClient, seedData, backend, testInfo, touch: true, expect });
  });
});
