import { runHistoricalImportSmoke } from "./historical-import-smoke-helper.mjs";
import { loadSessionCostHostFixtures } from "./host-test-fixtures.mjs";

const { test, expect } = await loadSessionCostHostFixtures();

test.describe.serial("Session Cost historical import on desktop", () => {
  test("adopts and imports existing sessions, queues work, and resumes after restart", async ({
    testPage,
    apiClient,
    seedData,
    backend,
    prCapture,
  }) => {
    test.setTimeout(480_000);
    await runHistoricalImportSmoke({ testPage, apiClient, seedData, backend, expect, touch: false, prCapture });
  });
});
