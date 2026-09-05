import type { Page } from "@playwright/test";
import { DEST_ROOT, type Job, nas, serverID, sh } from "./helpers";

/** freshDest wipes and recreates a destination folder under the allowed root. */
export function freshDest(name: string): string {
  const p = `${DEST_ROOT}/${name}`;
  nas(`rm -rf ${sh(p)} && mkdir -p ${sh(p)} && chown nas:nas ${sh(p)}`);
  return p;
}

export const sourcePane = (page: Page) => page.getByTestId("source-pane");
export const destPane = (page: Page) => page.getByTestId("dest-pane");

export async function openBrowse(page: Page, server = "source-key") {
  const id = await serverID(server);
  await page.goto("/#browse");
  await page.getByRole("combobox").selectOption(String(id));
}

/** enter walks the source pane down a list of directory names. */
export async function enter(page: Page, ...dirs: string[]) {
  for (const d of dirs) {
    await sourcePane(page).getByRole("button", { name: `📁 ${d} folder` }).click();
  }
}

/** chooseDest clicks the allowed root and then the named sub-folder in it. */
export async function chooseDest(page: Page, sub: string) {
  const pane = destPane(page);
  await pane.getByRole("button", { name: `📁 ${DEST_ROOT}`, exact: true }).click();
  await pane.getByRole("button", { name: `📁 ${sub}`, exact: true }).click();
}

/**
 * queueSelected ticks each named entry and presses Download, returning the jobs
 * the API actually created — one request, however many items were selected.
 */
export async function queueSelected(page: Page, names: string[]): Promise<Job[]> {
  for (const n of names) {
    await sourcePane(page).getByRole("checkbox", { name: n, exact: true }).check();
  }
  const [res] = await Promise.all([
    page.waitForResponse(
      (r) => r.url().endsWith("/api/jobs") && r.request().method() === "POST",
    ),
    page.getByTestId("selection-bar").getByRole("button", { name: "Download" }).click(),
  ]);
  const body = (await res.json()) as { jobs: Job[] };
  return body.jobs;
}

export const jobCard = (page: Page, id: number) => page.locator(`[data-job-id="${id}"]`);
