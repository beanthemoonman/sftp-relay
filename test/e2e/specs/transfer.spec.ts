import { expect, test } from "@playwright/test";
import {
  fixtureSums,
  getJob,
  nas,
  nasSum,
  sh,
  throttle,
  unthrottle,
  waitFor,
  waitForDone,
} from "../helpers";
import { chooseDest, enter, freshDest, jobCard, openBrowse, queueSelected } from "../ui";

const sums = fixtureSums();

test.afterEach(() => unthrottle());

test("single file: progress advances, completes, and the bytes match on the NAS", async ({
  page,
}) => {
  const dest = freshDest("single");
  // ~10 s of transfer, so "the bar moved" is an observation and not a race.
  throttle("20M");
  await openBrowse(page);
  await chooseDest(page, "single");
  const [job] = await queueSelected(page, ["big.bin"]);

  // Queueing hands off to the queue screen; the card is the UI's own evidence.
  await expect(jobCard(page, job.id)).toHaveAttribute("data-status", "running", {
    timeout: 60_000,
  });

  // Progress has to be seen moving, not just seen finished: a bar that only ever
  // shows 0 then 100 would pass a naive assertion.
  const midway = await waitFor(
    "big.bin to be partly transferred",
    () => getJob(job.id),
    (j) => j.transferred_bytes > 0 && j.transferred_bytes < j.total_bytes,
    120_000,
    100,
  );
  expect(midway.percent).toBeGreaterThan(0);
  expect(midway.percent).toBeLessThan(100);

  const done = await waitForDone(job.id, 300_000);
  expect(done.percent).toBe(100);
  expect(done.total_bytes).toBe(209715200);
  expect(nasSum(`${dest}/big.bin`)).toBe(sums.get("big.bin"));
  // xfer:use-temp-file leaves .in.<name> behind if lftp never finished cleanly.
  expect(nas(`ls -A ${sh(dest)}`)).toBe("big.bin");
});

test("directory: the mirror arrives with matching checksums and correct nesting", async ({
  page,
}) => {
  const dest = freshDest("dir");
  await openBrowse(page);
  await chooseDest(page, "dir");
  const [job] = await queueSelected(page, ["tree"]);

  await waitForDone(job.id, 300_000);

  for (const [rel, sum] of sums) {
    if (!rel.startsWith("tree/")) continue;
    expect(nasSum(`${dest}/${rel}`), rel).toBe(sum);
  }
  // Nesting, not a flattened dump: the three-level file must be three levels down.
  expect(nas(`test -f ${sh(dest + "/tree/a/b/c/deep.txt")} && echo yes`)).toBe("yes");
});

test("multi-select: five items queued in one action all complete", async ({ page }) => {
  const dest = freshDest("batch");
  await openBrowse(page);
  await enter(page, "tree");
  await chooseDest(page, "batch");

  const names = ["file1.bin", "file2.bin", "file3.bin", "file4.bin", "file5.bin"];
  const jobs = await queueSelected(page, names);
  expect(jobs).toHaveLength(5);

  for (const j of jobs) await waitForDone(j.id, 300_000);
  for (const n of names) expect(nasSum(`${dest}/${n}`), n).toBe(sums.get(`tree/${n}`));
});

test("edge-case names: a zero-byte file and one with spaces and unicode", async ({ page }) => {
  const dest = freshDest("edges");
  await openBrowse(page);
  await chooseDest(page, "edges");

  const names = ["empty.bin", "name with spaces ü.txt"];
  const jobs = await queueSelected(page, names);
  for (const j of jobs) await waitForDone(j.id, 120_000);
  for (const n of names) expect(nasSum(`${dest}/${n}`), n).toBe(sums.get(n));
});
