import { runSessionCostHostSmoke } from "./host-action-smoke-helper.mjs";
import { runSessionCostRetentionSmoke } from "./retained-usage-smoke-helper.mjs";
import { loadSessionCostHostFixtures } from "./host-test-fixtures.mjs";

const { test, expect } = await loadSessionCostHostFixtures();
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
