import { expect, test } from "@playwright/test";
import { fileByName, openFolder, openView } from "./helpers";

// The only tier that runs the real readers against the real embedded bundle.
//
// pdf.js's worker, its standard fonts and its wasm decoders are all served out
// of internal/api/dist by the daemon; a vitest run with a stubbed fetch cannot
// tell you whether any of them are actually there. Two of the three failure
// modes this covers -- a worker that 404s, an asset the copy plugin stopped
// copying -- are invisible everywhere else.

/** Opens one file in the lightbox from the Files view.
 *
 *  Documents open in Files, not the gallery -- App picks the gallery only for
 *  folders whose name looks photographic -- so this is the path an owner
 *  actually takes to a PDF. */
async function openLightbox(page: import("@playwright/test").Page, filename: string) {
  await openFolder(page, "Documents");
  await openView(page, "files");

  const file = await fileByName(page, "Documents", filename);
  await page.locator(`[data-file-id="${file.id}"] [data-preview-open]`).click();
  await expect(page.locator("[data-preview-kind]")).toBeVisible();
}

test.beforeEach(async ({ page }) => {
  await page.goto("/");
});

test("renders a PDF in the browser instead of downloading it", async ({ page }) => {
  await openLightbox(page, "2026-Q1-Report.pdf");

  await expect(page.locator("[data-preview-kind]")).toHaveAttribute("data-preview-kind", "pdf");
  // The canvas is what pdf.js paints into, so its presence is the evidence
  // that the worker loaded and a page actually rasterised.
  await expect(page.locator("[data-pdf-reader] canvas")).toBeVisible({ timeout: 20_000 });
});

test("pages through a PDF that has more than one page", async ({ page }) => {
  // 2026-Q2-Report.pdf is the two-page fixture: a blank cover sheet and then a
  // page with content on it.
  await openLightbox(page, "2026-Q2-Report.pdf");

  const indicator = page.locator("[data-pdf-page-indicator]");
  await expect(indicator).toContainText("Page 1 of 2", { timeout: 20_000 });

  await page.locator("[data-pdf-next]").click();
  await expect(indicator).toContainText("Page 2 of 2");
});

test("inverts a PDF for dark reading with I", async ({ page }) => {
  await openLightbox(page, "2026-Q1-Report.pdf");
  await expect(page.locator("[data-pdf-reader] canvas")).toBeVisible({ timeout: 20_000 });

  await page.keyboard.press("Shift+I");

  await expect(page.locator("[data-pdf-invert]")).toHaveAttribute("aria-pressed", "true");
  await expect(page.locator(".react-pdf__Page")).toHaveClass(/invert/);
});

test("keeps the mobile lightbox summary and actions at a readable width", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await openFolder(page, "CameraRoll");
  await page.locator("[data-file-id]").first().click();

  const lightbox = page.locator("[data-lightbox]");
  await expect(lightbox).toBeVisible();
  const summary = await page.locator("[data-lightbox-summary]").boundingBox();
  const actions = await page.locator("[data-lightbox-actions]").boundingBox();
  expect(summary?.width).toBeGreaterThan(330);
  expect(actions?.x).toBeGreaterThanOrEqual(0);
  expect((actions?.x ?? 0) + (actions?.width ?? 0)).toBeLessThanOrEqual(390);
});

test("reads an EPUB without downloading the archive to the browser", async ({ page }) => {
  await openLightbox(page, "Pocket-Guide.epub");

  await expect(page.locator("[data-preview-kind]")).toHaveAttribute("data-preview-kind", "epub");
  const frame = page.locator("[data-epub-frame]");
  await expect(frame).toBeVisible({ timeout: 20_000 });
  // Sandboxed to an opaque origin: the markup inside arrived over Syncthing.
  await expect(frame).toHaveAttribute("sandbox", "");
  await expect(page.frameLocator("[data-epub-frame]").locator("h1")).toBeVisible();
});

test("shows a text file inline, including one with no extension", async ({ page }) => {
  // "bookmarks" has no extension at all, so mime.TypeByExtension says nothing
  // about it. Only the scanner's content sniff can classify it.
  await openLightbox(page, "bookmarks");

  await expect(page.locator("[data-preview-kind]")).toHaveAttribute("data-preview-kind", "text");
  await expect(page.locator("[data-text-body]")).toContainText("seed-demo", { timeout: 20_000 });
});

test("serves the preview route inline rather than as an attachment", async ({ page }) => {
  const file = await fileByName(page, "Documents", "2026-Q1-Report.pdf");

  const res = await page.request.get(`/api/v1/files/${file.id}/preview`);

  expect(res.status()).toBe(200);
  expect(res.headers()["content-type"]).toContain("application/pdf");
  expect(res.headers()["content-disposition"]).toContain("inline");
  expect(res.headers()["x-content-type-options"]).toBe("nosniff");
});

test("serves pdf.js's runtime assets from the binary, not a CDN", async ({ page }) => {
  // The daemon runs on a LAN with no egress. A CDN fetch here is not a slow
  // reader, it is a blank one.
  for (const [path, type] of [
    ["/pdfjs/wasm/openjpeg.wasm", "application/wasm"],
    ["/pdfjs/standard_fonts/LiberationSans-Regular.ttf", ""],
  ] as const) {
    const res = await page.request.get(path);
    expect(res.status(), path).toBe(200);
    if (type) {
      expect(res.headers()["content-type"], path).toContain(type);
    }
    // The SPA fallback answers unknown paths with index.html, so a missing
    // asset presents as a 200 full of markup rather than a 404.
    expect((await res.body()).length, path).toBeGreaterThan(1024);
  }
});

test("never asks the network for a pdf.js asset it did not ship", async ({ page }) => {
  const external: string[] = [];
  page.on("request", (req) => {
    const url = new URL(req.url());
    if (url.host !== new URL(page.url() || "http://127.0.0.1").host) {
      external.push(req.url());
    }
  });

  await openLightbox(page, "2026-Q1-Report.pdf");
  await expect(page.locator("[data-pdf-reader] canvas")).toBeVisible({ timeout: 20_000 });

  expect(external, "the reader reached off-box; it must be entirely self-hosted").toEqual([]);
});

test("streams an archived document for preview without restoring it", async ({ page }) => {
  // The owner's explicit call: browsing the archive must not un-archive it.
  const file = await fileByName(page, "OldPhone", "OLD_202301_000.jpg");
  expect(file.status).toBe("archived");

  const res = await page.request.get(`/api/v1/files/${file.id}/preview`);
  expect(res.status()).toBe(200);

  const after = await fileByName(page, "OldPhone", "OLD_202301_000.jpg");
  expect(after.status).toBe("archived");
});
