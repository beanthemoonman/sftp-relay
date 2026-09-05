import { expect, test } from "@playwright/test";
import { AUTH, BASE } from "../helpers";

// Plain fetch, not Playwright's request fixture: that one inherits
// use.httpCredentials from the config and would quietly authenticate.
const anon = (path: string, headers: Record<string, string> = {}) =>
  fetch(BASE + path, { headers, redirect: "manual" });

test.describe("auth", () => {
  test("rejects an unauthenticated request and accepts correct credentials", async () => {
    // Health is the one deliberately open route, so the container healthcheck
    // needs no credentials.
    expect((await anon("/api/health")).status).toBe(200);

    for (const path of ["/api/servers", "/api/jobs", "/api/settings", "/api/events", "/"]) {
      const res = await anon(path);
      expect(res.status, `${path} unauthenticated`).toBe(401);
      expect(res.headers.get("www-authenticate")).toContain("Basic");
    }

    const wrongPass = await anon("/api/servers", {
      Authorization: "Basic " + Buffer.from("e2e:nope").toString("base64"),
    });
    expect(wrongPass.status).toBe(401);

    const wrongUser = await anon("/api/servers", {
      Authorization: "Basic " + Buffer.from("nope:e2e-pass").toString("base64"),
    });
    expect(wrongUser.status).toBe(401);

    expect((await anon("/api/servers", { Authorization: AUTH })).status).toBe(200);
  });

  test("serves the UI to an authenticated browser", async ({ page }) => {
    await page.goto("/#browse");
    await expect(page.getByRole("heading", { name: "sftp-relay" })).toBeVisible();
    await expect(page.getByTestId("sse-status")).toHaveAttribute("data-connected", "true");
  });
});
