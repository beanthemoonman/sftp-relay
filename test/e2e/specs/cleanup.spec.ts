import { expect, test } from "@playwright/test";
import { DEST_ROOT, nas, waitFor } from "../helpers";

// Runs last, as its own Playwright project: it asserts what the whole suite left
// behind, so anything that runs after it would invalidate the result.
test("nothing is left behind on the NAS after a full run", async () => {
  // Job workspaces: the staging dir inside the share and the runtime dir outside.
  await waitFor(
    "no orphaned job workspaces",
    () => nas(`ls -d ${DEST_ROOT}/.tmp/job-* /tmp/sftp-relay-job-* 2>/dev/null || true`),
    (out) => out === "",
    30_000,
  );

  // No lftp still running. The bracket stops pgrep matching its own sh -c.
  expect(nas("pgrep -f '[l]ftp.real' || true")).toBe("");

  // And no leaked SSH connections. One is expected and correct: internal/nas
  // holds a single keepalive'd connection to the NAS for the life of the
  // process. More than that means sessions are not being closed.
  const sessions = nas("ps -o args= -A | grep -c '[s]shd: nas' || true");
  expect(Number(sessions)).toBeLessThanOrEqual(1);
});
