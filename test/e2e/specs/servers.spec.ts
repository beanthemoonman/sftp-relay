import { expect, test } from "@playwright/test";
import { api, SOURCE_DIR } from "../helpers";

// The two servers global-setup created are left alone; CRUD gets its own row so
// spec order stays irrelevant.
const NAME = "crud-temp";
const RENAMED = "crud-temp-renamed";
const row = (name: string) => `[data-testid="server"][data-name="${name}"]`;

test("server CRUD: add, test the connection, edit, delete", async ({ page }) => {
  await page.goto("/#servers");
  await page.getByRole("button", { name: "Add server" }).click();

  await page.getByLabel("Name", { exact: true }).fill(NAME);
  await page.getByLabel("Host", { exact: true }).fill("sftp-source");
  await page.getByLabel("Port", { exact: true }).fill("22");
  await page.getByLabel("Username", { exact: true }).fill("passuser");
  await page.getByLabel("Authentication").selectOption("password");
  await page.getByLabel("Password", { exact: true }).fill("passpw");
  await page.getByLabel("Default remote path").fill(SOURCE_DIR);
  await page.getByRole("button", { name: "Save", exact: true }).click();

  const card = page.locator(row(NAME));
  await expect(card).toContainText("passuser@sftp-source:22");
  await expect(card).toContainText("password");

  // Testing the connection is also what pins the host key on first connect.
  await card.getByRole("button", { name: "Test connection" }).click();
  await expect(card.getByRole("status")).toContainText(/OK — \d+ entries/);
  await expect(card.getByRole("status")).toContainText("host key pinned now");

  await card.getByRole("button", { name: "Edit" }).click();
  await page.getByLabel("Name", { exact: true }).fill(RENAMED);
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.locator(row(RENAMED))).toBeVisible();

  await page.locator(row(RENAMED)).getByRole("button", { name: "Delete" }).click();
  await expect(page.locator(row(RENAMED))).toHaveCount(0);

  const left = await api<{ name: string }[]>("/api/servers");
  expect(left.map((s) => s.name)).not.toContain(RENAMED);
});
