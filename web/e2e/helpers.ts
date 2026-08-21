import { expect, type Page } from "@playwright/test";

/** Open a folder from the sidebar and wait for its heading. */
export async function openFolder(page: Page, label: string) {
  await page.locator(`[data-folder-nav="${label}"]`).click();
  await expect(page.getByRole("heading", { name: label, level: 2 })).toBeVisible();
}

export async function openView(page: Page, view: "gallery" | "files" | "config") {
  await page.locator(`[data-view-nav="${view}"]`).click();
}

export async function applyFilter(page: Page, filter: string) {
  await page.locator(`[data-filter="${filter}"]`).click();
}

/** The ids the API returns for a filter, so a test can act on exactly the rows
 *  it means rather than on whatever happens to be first on screen. */
export async function fileIDs(page: Page, query: string): Promise<number[]> {
  const res = await page.request.get(`/api/v1/files/ids?${query}`);
  expect(res.ok()).toBeTruthy();
  const body = (await res.json()) as { ids: number[] };
  return body.ids;
}

export async function fileByName(page: Page, folder: string, filename: string) {
  const res = await page.request.get(
    `/api/v1/files?folder=${encodeURIComponent(folder)}&q=${encodeURIComponent(filename)}&limit=5`,
  );
  expect(res.ok()).toBeTruthy();
  const files = (await res.json()) as { id: number; filename: string; status: string; pinned: boolean }[];
  const match = files.find((f) => f.filename === filename);
  expect(match, `no file named ${filename} in ${folder}`).toBeTruthy();
  return match!;
}

/** Ticks a row's selection box.
 *
 *  The native input is sr-only -- the visible box is a styled span, so that it
 *  can be drawn at a size a thumb can hit rather than at the browser default's
 *  13px. Playwright will not click a visually-hidden element, and neither can a
 *  person: both go for the label. Everything else about the input is unchanged,
 *  so getByRole, toBeChecked and the tab order all still work on it. */
export async function selectRow(page: Page, fileID: number) {
  await page.locator(`tr[data-file-id="${fileID}"] label`).first().click();
  await expect(
    page.locator(`tr[data-file-id="${fileID}"] input[type=checkbox]`),
  ).toBeChecked();
}
