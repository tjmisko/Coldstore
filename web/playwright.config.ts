import { defineConfig, devices } from "@playwright/test";

const ADDR = process.env.COLDSTORE_E2E_ADDR ?? "127.0.0.1:8091";

export default defineConfig({
  testDir: "./e2e",
  // One worker, in file order. These tests drive one daemon over one SQLite
  // catalog and several of them move real files; run in parallel they would be
  // asserting against each other's restores.
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  timeout: 30_000,
  expect: { timeout: 10_000 },
  reporter: process.env.CI ? [["github"], ["list"]] : [["list"]],
  use: {
    baseURL: `http://${ADDR}`,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: "../scripts/playwright-server.sh",
    url: `http://${ADDR}/api/v1/status`,
    // Never reuse: the suite's assertions are written against a freshly seeded
    // catalog, and a daemon left running from a previous run has already had
    // its conflicts resolved and its files restored.
    reuseExistingServer: false,
    timeout: 180_000,
    stdout: "pipe",
    stderr: "pipe",
  },
});
