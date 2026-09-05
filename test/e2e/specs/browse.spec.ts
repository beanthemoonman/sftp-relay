import { expect, test } from "@playwright/test";
import { DEST_ROOT, api, nas } from "../helpers";

const source = (page: import("@playwright/test").Page) => page.getByTestId("source-pane");
const dest = (page: import("@playwright/test").Page) => page.getByTestId("dest-pane");

test("browses the remote tree three levels deep and back via the breadcrumb", async ({ page }) => {
  await page.goto("/#browse");
  const pane = source(page);
  await expect(pane.getByText("small.txt")).toBeVisible();

  // Directories sort first, whatever the alphabet says: "tree" before "big.bin".
  const rows = pane.locator("li");
  await expect(rows.first()).toContainText("tree");
  const names = await rows.allInnerTexts();
  expect(names.findIndex((n) => n.includes("tree"))).toBeLessThan(
    names.findIndex((n) => n.includes("small.txt")),
  );

  // Sizes and modified dates render, and a folder says so rather than "0 B".
  await expect(pane.locator("li").filter({ hasText: "small.txt" })).toContainText("17 B");
  await expect(pane.locator("li").filter({ hasText: "small.txt" })).toContainText(
    /\d{1,4}[/-]\d{1,2}/,
  );
  await expect(rows.first()).toContainText("folder");

  // The entry button's accessible name folds in its size line, so "folder"
  // comes for free and keeps "a" from also matching "abc".
  for (const dir of ["tree", "a", "b", "c"]) {
    await pane.getByRole("button", { name: `📁 ${dir} folder` }).click();
    await expect(pane.getByRole("button", { name: dir, exact: true })).toBeVisible();
  }
  await expect(pane.getByText("deep.txt")).toBeVisible();

  // Back up in one click via the breadcrumb, not four.
  await pane.getByRole("button", { name: "upload", exact: true }).click();
  await expect(pane.getByText("small.txt")).toBeVisible();
  await expect(pane.getByText("deep.txt")).toHaveCount(0);
});

test("browses the NAS destination tree, creates a folder, and refuses an escape", async ({
  page,
}) => {
  const folder = "browse-made-me";
  nas(`rm -rf ${DEST_ROOT}/${folder}`);

  await page.goto("/#browse");
  const pane = dest(page);

  // The picker starts at the allowed roots themselves, not at "/".
  await expect(pane.getByText("Allowed destination roots")).toBeVisible();
  await pane.getByRole("button", { name: `📁 ${DEST_ROOT}`, exact: true }).click();

  await pane.getByLabel("New folder here").fill(folder);
  await pane.getByRole("button", { name: "Create" }).click();
  await expect(pane.getByRole("button", { name: `📁 ${folder}`, exact: true })).toBeVisible();
  expect(nas(`test -d ${DEST_ROOT}/${folder} && echo yes`)).toBe("yes");

  // A name that climbs out of the root must be refused, visibly.
  await pane.getByLabel("New folder here").fill("../../etc/evil");
  await pane.getByRole("button", { name: "Create" }).click();
  await expect(pane.getByText(/outside the allowed roots/)).toBeVisible();
  expect(nas("test -e /etc/evil && echo yes || echo no")).toBe("no");
});

test("the API refuses a destination outside the allowed roots", async () => {
  await expect(
    api(`/api/nas/browse?path=${encodeURIComponent("/etc")}`),
  ).rejects.toThrow(/outside the allowed roots/);
  await expect(
    api(`/api/nas/browse?path=${encodeURIComponent(DEST_ROOT + "foo")}`),
  ).rejects.toThrow(/outside the allowed roots/);
  const listing = await api<{ path: string }>(
    `/api/nas/browse?path=${encodeURIComponent(DEST_ROOT)}`,
  );
  expect(listing.path).toBe(DEST_ROOT);
});
