import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { runSessionCostAuthenticatedRetentionSmoke } from "./retained-usage-auth-smoke-helper.mjs";

const hostRoot =
  process.env.KANDEV_HOST_ROOT || fileURLToPath(new URL("../../kandev", import.meta.url));
const { backendFixture: test } = await import(
  pathToFileURL(path.join(hostRoot, "apps/web/e2e/fixtures/backend.ts")).href
);

test.describe.serial("Session Cost authenticated retained usage", () => {
  test("isolates real bindings, preserves captured attribution, and erases retained history", async ({
    backend,
    browser,
  }) => {
    test.setTimeout(300_000);
    await runSessionCostAuthenticatedRetentionSmoke({ backend, browser });
  });
});
