import os from "node:os";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const hostRoot = process.env.KANDEV_HOST_ROOT || fileURLToPath(new URL("../../kandev", import.meta.url));
const hostWeb = path.join(hostRoot, "apps/web");
const hostConfig = await import(pathToFileURL(path.join(hostWeb, "e2e/playwright.config.ts")).href);

export default {
  ...hostConfig.default,
  rootDir: hostWeb,
  testDir: path.dirname(fileURLToPath(import.meta.url)),
  testMatch: ["host-action-smoke.spec.mjs", "mobile-session-cost-action-smoke.spec.mjs"],
  projects: [
    {
      name: "chromium",
      testMatch: "host-action-smoke.spec.mjs",
      use: { browserName: "chromium", viewport: { width: 1280, height: 800 } },
    },
    {
      name: "mobile-chrome",
      testMatch: "mobile-session-cost-action-smoke.spec.mjs",
      use: {
        browserName: "chromium",
        viewport: { width: 393, height: 851 },
        deviceScaleFactor: 3,
        isMobile: true,
        hasTouch: true,
      },
    },
  ],
  globalSetup: path.join(hostWeb, "e2e/global-setup.ts"),
  outputDir: path.join(os.tmpdir(), "session-cost-host-action-smoke"),
};
