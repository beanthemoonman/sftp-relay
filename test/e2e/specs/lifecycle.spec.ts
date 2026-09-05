import { expect, test } from "@playwright/test";
import {
  api,
  BASE,
  compose,
  DEST_ROOT,
  getJob,
  nas,
  nasSum,
  queue,
  serverID,
  SOURCE_DIR,
  throttle,
  unthrottle,
  waitFor,
  waitForDone,
  type Job,
} from "../helpers";
import { chooseDest, freshDest, jobCard, openBrowse, queueSelected } from "../ui";
import { fixtureSums } from "../helpers";

test.afterEach(() => unthrottle());

const jobs = () => api<{ jobs: Job[] }>("/api/jobs?limit=200").then((r) => r.jobs);
const stale = () =>
  nas(`ls -d ${DEST_ROOT}/.tmp/job-* /tmp/sftp-relay-job-* 2>/dev/null || true`);
// The bracket keeps pgrep from matching the `sh -c` docker exec runs it in,
// whose own command line contains the pattern.
const lftps = () => nas("pgrep -f '[l]ftp.real' || true");

const waitHealthy = () =>
  waitFor(
    "the relay to answer /api/health",
    async () => (await fetch(BASE + "/api/health")).status,
    (s) => s === 200,
    120_000,
  );

test("concurrency: only the configured number of jobs run at once", async ({ page }) => {
  const dest = freshDest("concurrency");
  // A rate this low makes five 4 KB files take seconds each, which is what turns
  // "two running, three queued" into something observable rather than a race.
  throttle("512");

  await openBrowse(page);
  await page.getByTestId("source-pane").getByRole("button", { name: "📁 tree folder" }).click();
  await chooseDest(page, "concurrency");
  const queued = await queueSelected(page, [
    "file1.bin",
    "file2.bin",
    "file3.bin",
    "file4.bin",
    "file5.bin",
  ]);
  const mine = new Set(queued.map((j) => j.id));

  const settings = await api<Record<string, string>>("/api/settings");
  const limit = Number(settings.concurrency);
  expect(limit).toBe(2);

  const snapshot = await waitFor(
    `exactly ${limit} of the five jobs to be running`,
    async () => (await jobs()).filter((j) => mine.has(j.id)),
    (js) => js.filter((j) => j.status === "running").length === limit,
    120_000,
    200,
  );
  expect(snapshot.filter((j) => j.status === "queued")).toHaveLength(5 - limit);

  // The waiting jobs show their place in the queue, not just "queued".
  await page.goto("/#queue");
  await expect(page.locator('[data-testid="job"][data-status="queued"]').first()).toContainText(
    /queued #\d+/,
  );

  unthrottle();
  for (const j of queued) await waitForDone(j.id, 300_000);
  expect(nasSum(`${dest}/file1.bin`)).not.toBe("");
});

test("cancel: mid-transfer, the job stops and nothing is left behind", async ({ page }) => {
  freshDest("cancel");
  throttle("20M");

  await openBrowse(page);
  await chooseDest(page, "cancel");
  const [job] = await queueSelected(page, ["big.bin"]);

  await waitFor(
    "the transfer to actually start moving bytes",
    () => getJob(job.id),
    (j) => j.status === "running" && j.transferred_bytes > 0,
    120_000,
    100,
  );

  await jobCard(page, job.id).getByRole("button", { name: "Cancel" }).click();
  // Cancelled is terminal, so the row leaves the queue for the history screen.
  await expect(jobCard(page, job.id)).toHaveCount(0);
  await page.goto("/#history");
  await expect(jobCard(page, job.id)).toHaveAttribute("data-status", "cancelled");

  // Closing the SSH session does not reliably kill lftp; the pidfile does.
  await waitFor("no lftp process left on the NAS", lftps, (out) => out === "", 30_000);
  await waitFor("the temp workspace to be gone", stale, (out) => out === "", 30_000);
});

test("restart: an in-flight job is interrupted and resumes rather than restarting", async ({
  page,
}) => {
  const dest = freshDest("resume");
  // ~40 s at this rate: long enough to survive a container restart mid-flight.
  throttle("5M");

  await openBrowse(page);
  await chooseDest(page, "resume");
  const [job] = await queueSelected(page, ["big.bin"]);

  const partial = await waitFor(
    "at least 10 MB of big.bin to be on the NAS",
    () => getJob(job.id),
    (j) => j.transferred_bytes > 10 * 1024 * 1024,
    120_000,
    100,
  );
  const before = Number(nas(`stat -c %s ${dest}/.in.big.bin`));
  expect(before).toBeGreaterThan(0);

  compose("restart", "relay");
  await waitHealthy();

  // interrupted is a visible, runnable state: the row stays in the queue and the
  // scheduler picks it back up.
  await waitFor(
    "the job to be running again after the restart",
    () => getJob(job.id),
    (j) => j.status === "running" || j.status === "done",
    120_000,
    100,
  );
  const after = Number(nas(`stat -c %s ${dest}/.in.big.bin ${dest}/big.bin 2>/dev/null | head -1`));
  expect(after, "pget -c must resume, not truncate and start over").toBeGreaterThanOrEqual(
    before,
  );

  unthrottle();
  await waitForDone(job.id, 300_000);
  expect(nasSum(`${dest}/big.bin`)).toBe(fixtureSums().get("big.bin"));
});

test("failure surfacing: a nonexistent remote path fails readably and offers retry", async ({
  page,
}) => {
  freshDest("missing");
  const id = await serverID("source-key");
  const [job] = await queue(id, `${DEST_ROOT}/missing`, [
    { path: `${SOURCE_DIR}/definitely-not-here.bin`, is_dir: false },
  ]);

  const failed = await waitFor(
    "the job to fail",
    () => getJob(job.id),
    (j) => ["failed", "done", "cancelled"].includes(j.status),
    120_000,
  );
  expect(failed.status).toBe("failed");
  expect(failed.error).not.toBe("");

  await page.goto("/#history");
  const card = jobCard(page, job.id);
  await expect(card).toContainText(failed.error.slice(0, 30));
  await expect(card.getByRole("button", { name: "Retry" })).toBeVisible();

  // Retry clones the row rather than reviving it, so history stays honest.
  const [res] = await Promise.all([
    page.waitForResponse((r) => r.url().includes(`/api/jobs/${job.id}/retry`)),
    card.getByRole("button", { name: "Retry" }).click(),
  ]);
  const clone = (await res.json()) as Job;
  expect(clone.id).not.toBe(job.id);
  expect((await getJob(job.id)).status).toBe("failed");
  await waitFor(
    "the retry to fail the same way",
    () => getJob(clone.id),
    (j) => j.status === "failed",
    120_000,
  );
});

test("unreachable NAS: a job errors instead of hanging, and the app recovers", async () => {
  const dest = freshDest("nasdown");
  throttle("5M");
  const id = await serverID("source-key");
  const [job] = await queue(id, dest, [{ path: `${SOURCE_DIR}/big.bin`, is_dir: false }]);
  await waitFor(
    "the transfer to start",
    () => getJob(job.id),
    (j) => j.status === "running" && j.transferred_bytes > 0,
    120_000,
    100,
  );

  compose("stop", "fake-nas");
  try {
    const ended = await waitFor(
      "the job to end rather than hang",
      () => getJob(job.id),
      (j) => ["failed", "cancelled", "done"].includes(j.status),
      120_000,
    );
    expect(ended.status).not.toBe("done");
    expect(ended.error).not.toBe("");

    // The UI must say so too, not just sit on a stale progress bar.
    await expect(
      api(`/api/nas/browse?path=${encodeURIComponent(DEST_ROOT)}`),
    ).rejects.toThrow();
  } finally {
    compose("start", "fake-nas");
  }

  await waitFor(
    "the NAS to be browsable again",
    () => api(`/api/nas/browse?path=${encodeURIComponent(DEST_ROOT)}`).then(() => true, () => false),
    (ok) => ok === true,
    120_000,
  );

  // A docker stop is a SIGKILL for lftp, which outruns the cleanup trap. The
  // documented recovery is the boot-time sweep, so prove it actually sweeps.
  unthrottle();
  compose("restart", "relay");
  await waitHealthy();
  await waitFor("the boot sweep to clear the stale workspace", stale, (out) => out === "", 60_000);

  // And the app still works afterwards.
  const [again] = await queue(id, dest, [{ path: `${SOURCE_DIR}/small.txt`, is_dir: false }]);
  await waitForDone(again.id, 120_000);
});
