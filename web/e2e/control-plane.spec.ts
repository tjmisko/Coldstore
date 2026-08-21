import { expect, test } from "@playwright/test";
import { applyFilter, fileByName, openFolder, openView } from "./helpers";

test.beforeEach(async ({ page }) => {
  await page.goto("/");
  // The shell renders as soon as GET /status answers; every test starts from
  // a folder it chose itself rather than from the auto-selected first one.
  await expect(page.locator('[data-folder-nav="CameraRoll"]')).toBeVisible();
});

test("serves the embedded React control plane from the daemon itself", async ({ page }) => {
  await expect(page.getByRole("heading", { name: "Coldstore", level: 1 })).toBeVisible();
  for (const label of ["CameraRoll", "Backups", "Documents", "OldPhone"]) {
    await expect(page.locator(`[data-folder-nav="${label}"]`)).toBeVisible();
  }
});

test("marks the disabled folder and the one no longer declared in coldstore.toml", async ({ page }) => {
  // Both are flagged rather than hidden: a folder that has stopped moving must
  // say why, not disappear.
  const documents = page.locator('[data-folder-nav="Documents"]');
  await expect(documents).toContainText("disabled");
  const orphan = page.locator('[data-folder-nav="OldPhone"]');
  await expect(orphan).toContainText("not in coldstore.toml");
});

test("draws the pinned segment inside the total, never beyond it", async ({ page }) => {
  const meters = page.getByRole("meter");
  const count = await meters.count();
  expect(count).toBeGreaterThan(0);

  for (let i = 0; i < count; i++) {
    const meter = meters.nth(i);
    const total = Number(await meter.getAttribute("data-total-pct"));
    const pinned = Number(await meter.getAttribute("data-pinned-pct"));
    expect(pinned).toBeLessThanOrEqual(total + 0.001);
    expect(total).toBeLessThanOrEqual(100);
  }
});

test("shows the wedge banner with both figures and lands its link on the auto-pinned filter", async ({ page }) => {
  const banner = page.locator('[data-wedge-banner="Backups"]');
  await expect(banner).toBeVisible();
  await expect(banner).toContainText("48 kB pinned exceeds 20 kB budget");
  await expect(banner).toContainText("nothing left to offload");

  await banner.getByRole("button", { name: /Review auto-pinned files/ }).click();

  // One click, on the folder that is wedged, with the filter already applied:
  // a remedy the owner has to assemble themselves is not a remedy.
  await expect(page.getByRole("heading", { name: "Backups", level: 2 })).toBeVisible();
  await expect(page.locator('[data-filter="pinned-auto"]')).toHaveAttribute("aria-selected", "true");
  await expect(page.locator("tr[data-file-id]").first()).toBeVisible();
});

test("orders the auto-pinned filter oldest pin first", async ({ page }) => {
  await openFolder(page, "Backups");
  await applyFilter(page, "pinned-auto");
  await expect(page.locator("tr[data-file-id]").first()).toBeVisible();

  const ids = await page.locator("tr[data-file-id]").evaluateAll((rows) =>
    rows.map((row) => Number((row as HTMLElement).dataset.fileId)),
  );
  const res = await page.request.get("/api/v1/files?folder=Backups&pinned=1&pin_source=restore&sort=pinned_at&limit=500");
  const files = (await res.json()) as { id: number; pinned_at: number }[];
  const pinnedAt = new Map(files.map((f) => [f.id, f.pinned_at]));

  // The oldest auto-pins are the ones the owner has most certainly forgotten,
  // and this filter exists to put them first.
  const rendered = ids.map((id) => pinnedAt.get(id)!);
  expect(rendered).toEqual([...rendered].sort((a, b) => a - b));
});

test("selects a contiguous range with shift-click and reports the count", async ({ page }) => {
  await openFolder(page, "CameraRoll");
  const tiles = page.locator("[data-file-id]");
  await expect(tiles.first()).toBeVisible();

  await tiles.nth(0).click({ modifiers: ["ControlOrMeta"] });
  await tiles.nth(3).click({ modifiers: ["Shift"] });

  await expect(page.locator("[data-selection-count]")).toHaveText("4 selected");
  for (const index of [0, 1, 2, 3]) {
    await expect(tiles.nth(index)).toHaveAttribute("data-selected", "true");
  }
  await expect(tiles.nth(4)).toHaveAttribute("data-selected", "false");
});

test("restores a selection as a job and repaints the rows as synced and pinned", async ({ page }) => {
  await openFolder(page, "CameraRoll");
  await openView(page, "files");
  await applyFilter(page, "archived");

  // Two named files, chosen so this test never touches the conflicted row --
  // restoring that one resolves its conflict, and another test is about to
  // assert the conflict is still outstanding.
  const first = await fileByName(page, "CameraRoll", "IMG_20260112_091015.jpg");
  const second = await fileByName(page, "CameraRoll", "IMG_20260205_170833.jpg");
  expect(first.status).toBe("archived");

  for (const file of [first, second]) {
    await page.locator(`tr[data-file-id="${file.id}"] input[type=checkbox]`).click();
  }
  await expect(page.locator("[data-selection-count]")).toHaveText("2 selected");

  // Scoped to the bulk bar: every archived ROW has a Restore button too, and
  // this test is about the job, not the single-file path.
  await page.locator("[data-selection-bar]").getByRole("button", { name: "Restore", exact: true }).click();

  await expect(page.locator("[data-job-summary]")).toContainText("Restored 2 of 2");
  for (const file of [first, second]) {
    const row = page.locator(`tr[data-file-id="${file.id}"]`);
    await expect(row).toContainText("synced");
    // D2: a restore pins what it brings back, and the badge has to say so --
    // an invisible pin is what makes the eventual wedge inexplicable.
    await expect(row.locator(`[data-pin-toggle="${file.id}"]`)).toHaveAttribute("data-pinned", "true");
    await expect(row.locator(`[data-pin-toggle="${file.id}"]`)).toHaveAttribute("data-pin-source", "restore");
  }
});

test("refuses a restore that would blow the budget and says what it would cost", async ({ page }) => {
  await openFolder(page, "Backups");
  await applyFilter(page, "archived");
  const rows = page.locator("tr[data-file-id]");
  await expect(rows.first()).toBeVisible();

  await rows.first().locator("input[type=checkbox]").click();
  await page.locator("[data-selection-bar]").getByRole("button", { name: "Restore", exact: true }).click();

  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("Backups");
  // Backups is wedged: every unpinned file is already in cold storage, so
  // there is nothing left for coldstore to evict and the dialog must say so
  // rather than naming a file that cannot move.
  await expect(dialog).toContainText(/every other file is pinned|other files/);

  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(dialog).toBeHidden();
  // Cancel means cancel: the row is still archived.
  await expect(rows.first()).toContainText("archived");
});

test("pins a file from the gallery and keeps the pin across a reload", async ({ page }) => {
  await openFolder(page, "CameraRoll");
  const target = await fileByName(page, "CameraRoll", "concert_night.jpg");
  expect(target.pinned).toBe(false);

  const tile = page.locator(`[data-file-id="${target.id}"]`);
  await tile.locator(`[data-pin-toggle="${target.id}"]`).click();

  await expect(tile.locator('[data-badge="pinned"]')).toBeVisible();
  // Pinning must not open the lightbox: the pin and the checkbox are nested
  // controls, and this is the click that used to do both.
  await expect(page.getByRole("button", { name: "Download" })).toBeHidden();

  await page.reload();
  await openFolder(page, "CameraRoll");
  await expect(page.locator(`[data-pin-toggle="${target.id}"]`)).toHaveAttribute("data-pinned", "true");
});

test("resolves a conflict and takes the file out of the Conflicts tab", async ({ page }) => {
  await openFolder(page, "CameraRoll");
  await openView(page, "files");
  await applyFilter(page, "conflicts");

  const row = page.locator("tr[data-file-id]").first();
  await expect(row).toBeVisible();
  await expect(row).toContainText("IMG_20260105_143022.jpg");

  await row.getByRole("button", { name: "Keep archived copy" }).click();

  // The row leaves the tab because a resolution is logged as its own event
  // type; matching on "has a conflict event" alone would keep it here forever.
  await expect(page.getByText("No files match this filter.")).toBeVisible();
});

test("saves a budget from the settings panel and shows it after a reload", async ({ page }) => {
  await openFolder(page, "Documents");
  await openView(page, "config");

  const budget = page.locator('[data-budget-input="Documents"]');
  await expect(budget).toHaveValue("100 kB");
  await budget.fill("500 kB");
  await budget.blur();

  await expect(page.locator('[data-folder-card="Documents"]')).not.toContainText("over budget");

  await page.reload();
  await openView(page, "config");
  // The budget lives in the catalog now, not in coldstore.toml, and it
  // survives without a restart.
  await expect(page.locator('[data-budget-input="Documents"]')).toHaveValue("500 kB");
});

test("reports how many files a persistent pattern really protects", async ({ page }) => {
  await openFolder(page, "CameraRoll");
  await openView(page, "config");
  await page.locator('[data-manage-folder="CameraRoll"]').click();

  // Seeded from coldstore.toml: Favorites/ protects the whole subtree.
  const seeded = page.locator('[data-pattern="Favorites/"]');
  await expect(seeded).toContainText("protects 3 files in this folder");

  await page.locator("[data-new-pattern]").fill("Nope/*");
  await page.getByRole("button", { name: "Add" }).click();

  // A pattern that protects nothing looks exactly like one that protects
  // everything until it says so -- and the owner has just trusted their
  // favourites to it.
  const empty = page.locator('[data-pattern="Nope/*"]');
  await expect(empty).toContainText("protects 0 files in this folder -- check the pattern");
});

test("shows warning-class activity for the selected folder", async ({ page }) => {
  await openFolder(page, "Backups");
  await openView(page, "config");
  await page.locator('[data-manage-folder="Backups"]').click();

  const activity = page.getByRole("heading", { name: "Activity: Backups" }).locator("..");
  await expect(activity).toContainText("folder_wedged");
  // One enforcement pass writes hundreds of archive rows; without the class
  // filter the wedge warning would never appear in the last ten.
  await expect(activity).not.toContainText("archive\n");
});

test.describe("on a phone-sized viewport", () => {
  test.use({ viewport: { width: 390, height: 844 } });

  test("does not park the sidebar toggle on top of the folder name", async ({ page }) => {
    // The toggle is `fixed top-3 left-3 md:hidden` and the heading had no left
    // inset below that breakpoint, so "CameraRoll" rendered as "meraRoll".
    await page.locator('[data-folder-nav="CameraRoll"]').click();

    const toggle = page.getByRole("button", { name: "Toggle sidebar" });
    const heading = page.getByRole("heading", { name: "CameraRoll", level: 2 });
    await expect(heading).toBeVisible();

    const toggleBox = await toggle.boundingBox();
    const headingBox = await heading.boundingBox();
    expect(toggleBox).not.toBeNull();
    expect(headingBox).not.toBeNull();
    expect(headingBox!.x).toBeGreaterThanOrEqual(toggleBox!.x + toggleBox!.width);
  });

  test("should let a touch device select a photo when nothing is selected yet", async ({ page }) => {
    // The tile checkbox used to be `opacity-0 group-hover:opacity-100`. A phone
    // has no hover, so it never appeared -- and the `anySelected` escape hatch
    // that also reveals it could not be reached, because reaching it required
    // selecting something first. The gallery's entire multi-select feature was
    // dead on the device the photos are browsed from.
    await page.locator('[data-folder-nav="CameraRoll"]').click();
    const tile = page.locator("[data-file-id]").first();
    await expect(tile).toBeVisible();

    const box = tile.locator('input[type="checkbox"]');
    const opacity = await box.evaluate((el) => {
      const label = el.closest("label")!;
      return window.getComputedStyle(label).opacity;
    });
    expect(Number(opacity)).toBeGreaterThan(0.25);

    // And it actually selects, with no hover anywhere in this test. The tap
    // lands on the label, which is what a thumb hits -- the input itself is
    // sr-only so that the box can be drawn at a size a thumb can find.
    await tile.locator("label").first().click();
    await expect(box).toBeChecked();
    await expect(page.locator("[data-selection-count]")).toContainText("1 selected");
  });

  test("should give the tile checkbox a touch-sized hit target", async ({ page }) => {
    await page.locator('[data-folder-nav="CameraRoll"]').click();
    const label = page.locator("[data-file-id]").first().locator("label").first();

    // The native 13px box was unhittable with a thumb. 36px is the padded label.
    const box = await label.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.width).toBeGreaterThanOrEqual(32);
    expect(box!.height).toBeGreaterThanOrEqual(32);
  });
});
