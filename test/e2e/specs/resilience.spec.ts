import { expect, test } from "@playwright/test";
import {
  compose,
  queue,
  serverID,
  SOURCE_DIR,
  throttle,
  unthrottle,
  waitFor,
  waitForDone,
} from "../helpers";
import { chooseDest, destPane, freshDest, jobCard, openBrowse, queueSelected, sourcePane } from "../ui";

test.afterEach(() => unthrottle());

test("SSE: the client shows the disconnection, reconnects, and resyncs", async ({ page }) => {
  const dest = freshDest("sse");
  // A job slow enough that its state genuinely changes while the client is blind.
  throttle("5M");
  const id = await serverID("source-key");
  const [job] = await queue(id, dest, [{ path: `${SOURCE_DIR}/big.bin`, is_dir: false }]);

  await page.goto("/#queue");
  const dot = page.getByTestId("sse-status");
  await expect(dot).toHaveAttribute("data-connected", "true");
  await expect(jobCard(page, job.id)).toBeVisible();

  // Restarting the relay is the bluntest honest way to kill the stream: the
  // connection really goes away, exactly as it would if the Pi rebooted.
  compose("restart", "relay");
  await expect(dot).toHaveAttribute("data-connected", "false", { timeout: 30_000 });

  // Reconnect is on a backoff (1s doubling to 30s), so this is given room.
  await expect(dot).toHaveAttribute("data-connected", "true", { timeout: 60_000 });

  // Nothing is replayed: the server sends a full snapshot instead, and that is
  // what has to put the client back in step with a job whose state moved on.
  unthrottle();
  await waitForDone(job.id, 300_000);
  await expect(jobCard(page, job.id)).toHaveCount(0);
  await page.goto("/#history");
  await expect(jobCard(page, job.id)).toHaveAttribute("data-status", "done");
  await expect(jobCard(page, job.id)).toHaveAttribute("data-percent", "100");
});

test("multi-client: two browsers see the same live progress", async ({ browser }) => {
  const dest = freshDest("multi");
  throttle("20M");
  const id = await serverID("source-key");
  const [job] = await queue(id, dest, [{ path: `${SOURCE_DIR}/big.bin`, is_dir: false }]);

  const pages = await Promise.all(
    [0, 1].map(async () => {
      const ctx = await browser.newContext({
        baseURL: `http://localhost:${process.env.E2E_PORT ?? "8088"}`,
        httpCredentials: { username: "e2e", password: "e2e-pass" },
      });
      const p = await ctx.newPage();
      await p.goto("/#queue");
      return p;
    }),
  );

  const percent = (i: number) =>
    pages[i].locator(`[data-job-id="${job.id}"]`).getAttribute("data-percent");

  // Both must show the bar moving, not just the final state.
  for (const i of [0, 1]) {
    await waitFor(
      `client ${i} to show progress`,
      () => percent(i),
      (v) => Number(v) > 0 && Number(v) < 100,
      120_000,
      100,
    );
  }

  unthrottle();
  await waitForDone(job.id, 300_000);
  for (const p of pages) {
    await expect(p.locator(`[data-job-id="${job.id}"]`)).toHaveCount(0); // moved to history
    await p.goto("/#history");
    await expect(p.locator(`[data-job-id="${job.id}"]`)).toHaveAttribute("data-percent", "100");
    await expect(p.locator(`[data-job-id="${job.id}"]`)).toHaveAttribute("data-status", "done");
    await p.context().close();
  }
});

test.describe("mobile", () => {
  test.use({ viewport: { width: 375, height: 812 } });

  test("the browse-to-queue flow completes at 375px without horizontal scroll", async ({
    page,
  }) => {
    freshDest("mobile");
    await openBrowse(page);

    // One pane at a time on a phone: the toggle is the whole point of the layout.
    await page.getByRole("button", { name: "Destination", exact: true }).click();
    await expect(destPane(page)).toBeVisible();
    await chooseDest(page, "mobile");
    await page.getByRole("button", { name: "Source", exact: true }).click();
    await expect(sourcePane(page)).toBeVisible();

    const [job] = await queueSelected(page, ["small.txt"]);
    await waitForDone(job.id, 120_000);

    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow, "the page must not scroll sideways on a phone").toBeLessThanOrEqual(0);

    // The queue's bottom nav must not sit on top of the job card's controls.
    await page.goto("/#queue");
    await page.goto("/#history");
    const nav = await page.locator("nav.fixed").boundingBox();
    const card = await jobCard(page, job.id).boundingBox();
    expect(nav).not.toBeNull();
    expect(card).not.toBeNull();
    expect(card!.y + card!.height, "the last card is not hidden behind the tab bar").toBeLessThan(
      nav!.y + nav!.height,
    );
  });
});
