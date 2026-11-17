import { expect, test, type Page } from "@playwright/test";
import { fileByName, openFolder, openView } from "./helpers";

/** Type a vim sequence the way the binding table spells one. */
async function type(page: Page, written: string) {
  for (const key of written.split("")) {
    await page.keyboard.press(key === " " ? "Space" : key);
  }
}

test.beforeEach(async ({ page }) => {
  await page.goto("/");
  await expect(page.locator('[data-folder-nav="CameraRoll"]')).toBeVisible();
  await openFolder(page, "CameraRoll");
  await expect(page.locator("[data-file-id]").first()).toBeVisible();
});

test("generates the cheatsheet from the bindings the engine actually dispatches on", async ({ page }) => {
  await page.keyboard.press("?");

  const sheet = page.getByRole("dialog", { name: "Keyboard shortcuts" });
  await expect(sheet).toBeVisible();
  await expect(sheet.locator('[data-cheatsheet-binding="op.archive"]')).toContainText("Archive");
  await expect(sheet.locator('[data-cheatsheet-binding="motion.first"]')).toContainText("gg");

  await page.keyboard.press("Escape");
  await expect(sheet).toBeHidden();
});

test("moves a visible cursor with j, gg and G, and keeps DOM focus on it", async ({ page }) => {
  const first = page.locator("[data-file-id]").first();
  await expect(first).toHaveAttribute("data-cursor", "true");

  await type(page, "j");
  await expect(page.locator('[data-cursor="true"]')).toHaveCount(1);
  await expect(first).toHaveAttribute("data-cursor", "false");

  await type(page, "G");
  const onCursor = page.locator('[data-cursor="true"]');
  await expect(onCursor).toHaveCount(1);
  // The drawn cursor and the DOM focus are written from one source of truth,
  // so they cannot disagree.
  await expect(onCursor).toBeFocused();

  await type(page, "gg");
  await expect(first).toHaveAttribute("data-cursor", "true");
});

test("selects a visual range and keeps it after Escape", async ({ page }) => {
  await type(page, "v");
  await type(page, "2j");

  await expect(page.locator("[data-selection-count]")).toBeVisible();
  await page.keyboard.press("Escape");

  // In vim nothing stays selected after Escape. Here the point of a visual
  // range is to end up with a selection.
  await expect(page.locator("[data-selection-count]")).toBeVisible();
  await expect(page.locator('[data-selected="true"]').first()).toBeVisible();
});

test("toggles selection directly with Space", async ({ page }) => {
  await page.keyboard.press("Space");
  await expect(page.locator("[data-selection-count]")).toHaveText("1 selected");

  await page.keyboard.press("Space");

  await expect(page.locator("[data-selection-bar]")).toBeHidden();
});

test("asks before archiving a range wider than a handful, and cancel means cancel", async ({ page }) => {
  await type(page, "d");
  await type(page, "G");

  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText(/Archive \d+ files\?/);
  // Every archive is a Syncthing deletion on every device the owner has, and
  // dG names no number.
  await expect(dialog).toContainText(/Syncthing will delete them/);

  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(dialog).toBeHidden();
  await expect(page.locator("[data-job-summary]")).toBeHidden();
});

test("switches filter from the command palette", async ({ page }) => {
  await page.keyboard.press(":");
  const input = page.locator("[data-command-palette]");
  await expect(input).toBeFocused();

  await input.fill(":filter archived");
  await page.keyboard.press("Enter");

  await expect(page.locator('[data-filter="archived"]')).toHaveAttribute("aria-selected", "true");
  await expect(input).toBeHidden();
});

test("refuses a command that is not one instead of closing silently", async ({ page }) => {
  await page.keyboard.press(":");
  const input = page.locator("[data-command-palette]");
  await input.fill(":wq");
  await page.keyboard.press("Enter");

  await expect(page.locator("[data-palette-error]")).toContainText("not a command: :wq");
  await expect(input).toBeVisible();
});

test("finds a file by name and opens its preview", async ({ page }) => {
  await page.keyboard.press("F10");
  const input = page.locator("[data-fuzzy-finder]");
  await expect(input).toBeFocused();

  await input.fill("concert");
  const firstResult = page.locator("[data-finder-result]").first();
  await expect(firstResult).toContainText("concert_night.jpg");

  await page.keyboard.press("Enter");
  await expect(input).toBeHidden();
  await expect(page.locator("[data-finder-open-preview]")).toBeVisible();
  await expect(page.getByRole("dialog", { name: "Preview concert_night.jpg" })).toBeVisible();
});

test("opens a context menu on right-click and scopes it to the selection", async ({ page }) => {
  await type(page, "v");
  await type(page, "2j");
  await page.keyboard.press("Escape");

  await page.locator('[data-selected="true"]').first().click({ button: "right" });

  const menu = page.getByRole("menu");
  await expect(menu).toBeVisible();
  // Right-clicking INSIDE a selection acts on the whole selection.
  await expect(menu).toContainText(/\d+ files selected/);

  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
});

test("reaches the context menu from the keyboard alone", async ({ page }) => {
  await page.keyboard.press("Shift+F10");

  await expect(page.getByRole("menu")).toBeVisible();
});

test("leaves the budget editor alone: a typed size is not a motion", async ({ page }) => {
  await openFolder(page, "Documents");
  await openView(page, "config");

  const budget = page.locator('[data-budget-input="Documents"]');
  await budget.click();
  await budget.fill("");
  await budget.type("20 GB");

  // "20 GB" is 2, 0, G, B to the machine, and G is a motion to the end of the
  // folder. The guard runs before normalisation, so none of it is looked at.
  await expect(budget).toHaveValue("20 GB");
  await expect(page.getByRole("dialog")).toHaveCount(0);
});

test("toggles the folder sidebar and steps folders with the requested keys", async ({ page }) => {
  const sidebar = page.locator("[data-folder-sidebar]");
  await expect(sidebar).toBeVisible();

  await page.keyboard.press("F1");
  await expect(sidebar).toBeHidden();
  await expect(page.getByRole("button", { name: "Toggle sidebar" })).toHaveAttribute("aria-expanded", "false");

  await page.keyboard.press("F1");
  await expect(sidebar).toBeVisible();

  // The keys step through the sidebar in the order it is drawn, so the
  // neighbour is read from the sidebar rather than named here. This used to
  // say "Backups", which was true only while the sidebar listed folders in
  // config order; it now lists them by label and the next one is "Documents".
  const labels = (await sidebar.locator("[data-folder-nav]").evaluateAll((nodes) =>
    nodes.map((node) => node.getAttribute("data-folder-nav")),
  )).filter((label): label is string => label !== null && label !== "all");
  const at = labels.indexOf("CameraRoll");
  expect(at, "CameraRoll is not in the sidebar").toBeGreaterThanOrEqual(0);
  expect(labels.length).toBeGreaterThan(1);
  const below = labels[(at + 1) % labels.length];

  await page.keyboard.press("Control+ArrowDown");
  await expect(page.getByRole("heading", { name: below, level: 2 })).toBeVisible();
  await page.keyboard.press("Control+ArrowUp");
  await expect(page.getByRole("heading", { name: "CameraRoll", level: 2 })).toBeVisible();
});

test("keeps the file-row cursor and checkbox inset from the scroll edge", async ({ page }) => {
  await openView(page, "files");
  const table = page.getByRole("table");
  const viewport = table.locator("..");
  const row = page.locator('[data-cursor-scope="browser"][data-cursor="true"]');
  const box = row.locator("td").first().locator("span").first();
  const [viewportBounds, rowBounds, boxBounds] = await Promise.all([
    viewport.boundingBox(),
    row.boundingBox(),
    box.boundingBox(),
  ]);

  expect(viewportBounds).not.toBeNull();
  expect(rowBounds).not.toBeNull();
  expect(boxBounds).not.toBeNull();
  expect(rowBounds!.x - viewportBounds!.x).toBeGreaterThanOrEqual(3);
  expect(boxBounds!.x - rowBounds!.x).toBeGreaterThanOrEqual(11);
});

test("renames a file from its context menu", async ({ page }) => {
  await openFolder(page, "Documents");
  await openView(page, "files");
  const file = await fileByName(page, "Documents", "README.txt");
  const row = page.locator(`[data-file-id="${file.id}"]`);
  await expect(row).toBeVisible();

  await row.click({ button: "right" });
  const menu = page.getByRole("menu");
  await expect(menu.getByRole("menuitem", { name: "Pin" })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Unpin" })).toHaveCount(0);
  await expect(menu.locator("[data-menu-icon]")).toHaveCount(await menu.getByRole("menuitem").count());
  await menu.getByRole("menuitem", { name: "Rename" }).click();

  const dialog = page.getByRole("dialog", { name: "Rename README.txt" });
  const input = dialog.getByRole("textbox", { name: "New name for README.txt" });
  await input.fill("README-renamed.txt");
  await dialog.getByRole("button", { name: "Rename" }).click();

  await expect(dialog).toBeHidden();
  await expect(row).toContainText("README-renamed.txt");
  expect((await fileByName(page, "Documents", "README-renamed.txt")).id).toBe(file.id);
});
