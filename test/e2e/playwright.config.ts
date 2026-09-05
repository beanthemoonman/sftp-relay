import { defineConfig } from "@playwright/test";

// retries: 0 is deliberate. A flake here is a real race in internal/jobs
// (definition_of_done.md §2), and a retry would hide it.
export default defineConfig({
  testDir: "./specs",
  globalSetup: "./global-setup.ts",
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 180_000,
  expect: { timeout: 15_000 },
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: {
    baseURL: `http://localhost:${process.env.E2E_PORT ?? "8088"}`,
    httpCredentials: { username: "e2e", password: "e2e-pass" },
    trace: "retain-on-failure",
    video: "retain-on-failure",
  },
  projects: [
    { name: "main", testIgnore: "**/cleanup.spec.ts" },
    { name: "cleanup", testMatch: "**/cleanup.spec.ts", dependencies: ["main"] },
  ],
});
