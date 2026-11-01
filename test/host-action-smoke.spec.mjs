import { runSessionCostHostSmoke } from "./host-action-smoke-helper.mjs";
import { runSessionCostRetentionSmoke } from "./retained-usage-smoke-helper.mjs";
import { loadSessionCostHostFixtures } from "./host-test-fixtures.mjs";

const { test, expect } = await loadSessionCostHostFixtures();
const expectAction = process.env.SESSION_COST_EXPECT_ACTION === "1";
const expectUpgradeRequired = process.env.SESSION_COST_EXPECT_UPGRADE_REQUIRED === "1";

test.describe("Session Cost packaged action on a desktop host", () => {
  test.afterEach(async ({ apiClient }) => {
    await apiClient.rawRequest("DELETE", "/api/plugins/kandev-session-cost").catch(() => undefined);
  });

  test(
    expectUpgradeRequired
      ? "returns upgrade_required before source access on an older Host"
      : "keeps the session scope and disclosure behavior with the selected host action",
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
        touch: false,
        expect,
      });
    },
  );

  test("retains real Host admitted usage through restart, archive, deletion and uninstall", async ({ testPage, apiClient, seedData, backend }, testInfo) => {
    test.skip(expectUpgradeRequired, "The selected older Host is expected to require retained-usage support.");
    test.setTimeout(360_000);
    await runSessionCostRetentionSmoke({ testPage, apiClient, seedData, backend, testInfo, touch: false, expect });
  });
});
