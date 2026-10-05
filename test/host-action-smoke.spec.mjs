import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { runSessionCostHostSmoke } from "./host-action-smoke-helper.mjs";

const hostRoot = process.env.KANDEV_HOST_ROOT || fileURLToPath(new URL("../../kandev", import.meta.url));
const { test, expect } = await import(
  pathToFileURL(path.join(hostRoot, "apps/web/e2e/fixtures/test-base.ts")).href
);
const expectAction = process.env.SESSION_COST_EXPECT_ACTION === "1";

test.describe("Session Cost packaged action on a desktop host", () => {
  test.afterEach(async ({ apiClient }) => {
    await apiClient.rawRequest("DELETE", "/api/plugins/kandev-session-cost").catch(() => undefined);
  });

  test("keeps the session scope and disclosure behavior with the selected host action", async ({
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
      touch: false,
      expect,
    });
  });
});
