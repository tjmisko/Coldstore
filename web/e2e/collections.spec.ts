import { createHash } from "node:crypto";
import { expect, test, type Page } from "@playwright/test";
import { fileByName, openFolder, openView } from "./helpers";

// The Scans collection is seeded by scripts/seed-demo through archive.Open and
// Ingest -- the same code path the daemon runs -- so these tests browse a real
// content-addressed archive over the real embedded bundle.
//
// The tests that change state (tagging, trash) leave the collection as they
// found it, because every spec in this suite shares one seeded daemon.

const COLLECTION = "Scans";
const PHOTO = "Passport-Scan.jpg";

test.beforeEach(async ({ page }) => {
  await page.goto("/");
});

async function openCollection(page: Page) {
  await openFolder(page, COLLECTION);
  await openView(page, "files");
}

test("should list the seeded collection in the sidebar and show its files", async ({ page }) => {
  await expect(page.locator(`[data-folder-nav="${COLLECTION}"]`)).toBeVisible();

  await openCollection(page);

  for (const name of [PHOTO, "Lease-Page-1.jpg", "2024-03-Groceries.jpg", "filing-index.txt"]) {
    const file = await fileByName(page, COLLECTION, name);
    await expect(page.locator(`[data-file-id="${file.id}"]`)).toBeVisible();
  }
  const res = await page.request.get(`/api/v1/files?folder=${COLLECTION}&limit=50`);
  expect(((await res.json()) as unknown[]).length).toBe(6);
});

test("should show thumbnails and open a file in the lightbox", async ({ page }) => {
  await openFolder(page, COLLECTION);
  await openView(page, "gallery");

  const thumbnails = page.locator('img[src*="/api/v1/thumbnails/sha256/"]');
  await expect(thumbnails.first()).toBeVisible();
  expect(await thumbnails.count()).toBeGreaterThanOrEqual(5);
  // A thumbnail the daemon cannot serve renders as a broken image.
  await expect
    .poll(() => thumbnails.first().evaluate((img: HTMLImageElement) => img.naturalWidth))
    .toBeGreaterThan(0);

  await openView(page, "files");
  const file = await fileByName(page, COLLECTION, PHOTO);
  await page.locator(`[data-file-id="${file.id}"] [data-preview-open]`).click();
  await expect(page.locator("[data-preview-kind]")).toBeVisible();
  await expect(page.locator("[data-preview-image]")).toBeVisible();
});

test("should download a collection file", async ({ page }) => {
  await openCollection(page);
  const file = await fileByName(page, COLLECTION, PHOTO);

  const res = await page.request.get(`/api/v1/files/${file.id}/download`);

  expect(res.status()).toBe(200);
  expect(res.headers()["content-disposition"]).toContain("attachment");
  const body = await res.body();
  const digest = createHash("sha256").update(body).digest("hex");
  expect(res.headers()["etag"]).toBe(`"sha256:${digest}"`);
  expect(Number(res.headers()["content-length"])).toBe(body.length);

  // The same download, reached the way a person reaches it.
  const row = page.locator(`[data-file-id="${file.id}"]`);
  await row.click({ button: "right" });
  const [request] = await Promise.all([
    page.context().waitForEvent("request", (req) => req.url().includes(`/api/v1/files/${file.id}/download`)),
    page.getByRole("menu").locator('[data-menu-item="download"]').click(),
  ]);
  expect(request.method()).toBe("GET");
});

test("should tag a collection file and find it by that tag", async ({ page }) => {
  await openCollection(page);
  const file = await fileByName(page, COLLECTION, "Lease-Page-2.jpg");

  await page.locator(`[data-file-id="${file.id}"]`).click({ button: "right" });
  await page.getByRole("menu").locator('[data-menu-item="add-tags"]').click();
  const input = page.locator("#tag-prompt-input");
  await input.fill("housing/lease");
  await page.getByRole("button", { name: "Apply tags" }).click();
  await expect(page.locator("[data-tag-prompt]")).toBeHidden();

  const res = await page.request.get(`/api/v1/files?folder=${COLLECTION}&tag=housing`);
  const found = (await res.json()) as { id: number; tags: string[] }[];
  expect(found.map((f) => f.id)).toEqual([file.id]);
  expect(found[0].tags).toContain("housing/lease");

  // And it survives a reload: the tag is in the journal, not in the page.
  await page.reload();
  const again = await page.request.get(`/api/v1/files?folder=${COLLECTION}&tag=housing/lease`);
  expect(((await again.json()) as { id: number }[]).map((f) => f.id)).toEqual([file.id]);

  // Leave the collection as it was.
  const removed = await page.request.post("/api/v1/files/tags", {
    data: { ids: [file.id], remove: ["housing/lease"] },
  });
  expect(removed.ok()).toBeTruthy();
});

test("should move a collection file to trash and restore it", async ({ page }) => {
  await openCollection(page);
  const file = await fileByName(page, COLLECTION, "filing-index.txt");
  const row = page.locator(`[data-file-id="${file.id}"]`);

  await row.click({ button: "right" });
  await page.getByRole("menu").locator('[data-menu-item="trash"]').click();
  await expect(row).toBeHidden();

  const trashed = await page.request.get(`/api/v1/files?folder=${COLLECTION}&status=trashed`);
  expect(((await trashed.json()) as { id: number; status: string }[]).map((f) => f.id)).toEqual([file.id]);
  const live = await page.request.get(`/api/v1/files?folder=${COLLECTION}&limit=50`);
  expect(((await live.json()) as { id: number }[]).map((f) => f.id)).not.toContain(file.id);

  // Restore it the way the UI does, so the collection is whole again.
  const restored = await page.request.post(`/api/v1/collections/${COLLECTION}/trash`, {
    data: { ids: [file.id], trashed: false },
  });
  expect(restored.ok()).toBeTruthy();
  const after = await page.request.get(`/api/v1/files?folder=${COLLECTION}&limit=50`);
  expect(((await after.json()) as { id: number }[]).map((f) => f.id)).toContain(file.id);
});
