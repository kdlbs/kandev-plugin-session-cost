import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const hostRoot = process.env.KANDEV_HOST_ROOT || fileURLToPath(new URL("../../kandev", import.meta.url));

export async function loadSessionCostHostFixtures() {
  const hostFixtures = await import(
    pathToFileURL(path.join(hostRoot, "apps/web/e2e/fixtures/test-base.ts")).href
  );
  const { test: hostTest, expect } = hostFixtures;

  if (process.env.SESSION_COST_EXPECT_UPGRADE_REQUIRED !== "1") {
    return { test: hostTest, expect };
  }

  const test = hostTest.extend({
    apiClient: [
      async ({ apiClient }, use) => {
        const updateWorkspace = apiClient.updateWorkspace.bind(apiClient);
        apiClient.updateWorkspace = (workspaceId, updates) => {
          const isOptionalDefaultProfileReset =
            updates &&
            Object.keys(updates).length === 1 &&
            updates.default_agent_profile_id === "";
          if (isOptionalDefaultProfileReset) return Promise.resolve();
          return updateWorkspace(workspaceId, updates);
        };
        await use(apiClient);
      },
      { scope: "worker" },
    ],
  });

  return { test, expect };
}
