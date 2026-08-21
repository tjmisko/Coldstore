import { describe, expect, it } from "vitest";
import { aFile } from "../test/fixtures";
import { needsReader, resolvePreview } from "./preview";

// BUG-3. The lightbox used to pick its source as "has a thumbnail and is not
// missing, therefore renderable as an image". internal/thumbnail also
// thumbnails .heif/.heic and .mp4/.mov/.avi/.mkv/.webm, so opening a video
// handed the browser the full original as an <img src>: every byte over the
// LAN, then a broken-image icon. These tests are the per-type decision that
// replaced it.

describe("resolvePreview", () => {
  it("should render an image when the extension is one the browser decodes", () => {
    for (const extension of [".jpg", ".jpeg", ".png", ".webp", ".gif"]) {
      const decision = resolvePreview(aFile({ id: 7, extension }));
      expect(decision.kind, extension).toBe("image");
      expect(decision.src, extension).toBe("/api/v1/files/7/download");
      expect(decision.degraded, extension).toBe(false);
    }
  });

  it("should render a video when the file is a video, not an image", () => {
    for (const extension of [".mp4", ".mov", ".webm", ".mkv", ".avi"]) {
      const decision = resolvePreview(
        aFile({ id: 3, extension, thumbnail_path: "CameraRoll/clip.thumb.jpg" }),
      );
      expect(decision.kind, extension).toBe("video");
    }
  });

  it("should render the thumbnail when the image format is one the browser cannot decode", () => {
    // The whole point: do NOT hand the browser 5 MB of HEIC to draw a broken
    // icon with. The thumbnail is already a JPEG.
    const decision = resolvePreview(
      aFile({ id: 4, extension: ".heic", thumbnail_path: "CameraRoll/a.heic.thumb.jpg" }),
    );

    expect(decision.kind).toBe("image");
    expect(decision.src).toBe("/api/v1/thumbnails/CameraRoll/a.heic.thumb.jpg");
    expect(decision.degraded).toBe(true);
  });

  it("should refuse to preview when the format is undecodable and there is no thumbnail", () => {
    const decision = resolvePreview(aFile({ id: 5, extension: ".heic", thumbnail_path: undefined }));

    expect(decision.kind).toBe("none");
    expect(decision.src).toBe("");
    expect(decision.reason).not.toBe("");
  });

  it("should use the inline preview route when the file is a document", () => {
    // The download route sets Content-Disposition: attachment, which turns a
    // reader into a download.
    for (const [extension, kind] of [
      [".pdf", "pdf"],
      [".epub", "epub"],
      [".txt", "text"],
      [".md", "text"],
      [".csv", "text"],
    ] as const) {
      const decision = resolvePreview(aFile({ id: 9, extension }));
      expect(decision.kind, extension).toBe(kind);
      expect(decision.src, extension).toBe("/api/v1/files/9/preview");
    }
  });

  it("should classify by the sniffed type when the file has no extension", () => {
    // A bookmark export called "reading" has an empty extension and an empty
    // mime_type. detected_type is the only thing that can classify it.
    const decision = resolvePreview(
      aFile({ id: 11, filename: "bookmarks", extension: "", mime_type: "", detected_type: "text/plain" }),
    );

    expect(decision.kind).toBe("text");
    expect(decision.src).toBe("/api/v1/files/11/preview");
  });

  it("should classify by the sniffed type when the extension-less file is a PDF", () => {
    const decision = resolvePreview(
      aFile({ id: 12, filename: "mystery", extension: "", detected_type: "application/pdf" }),
    );

    expect(decision.kind).toBe("pdf");
  });

  it("should prefer the extension over the sniffed type when both are known", () => {
    // The extension is what the owner named the file, and it is right almost
    // always. The sniff is the way in for files that have no extension.
    const decision = resolvePreview(
      aFile({ id: 13, extension: ".jpg", detected_type: "application/pdf" }),
    );

    expect(decision.kind).toBe("image");
  });

  it("should refuse to preview when the file is missing", () => {
    const decision = resolvePreview(
      aFile({ id: 14, status: "missing", thumbnail_path: "CameraRoll/gone.thumb.jpg" }),
    );

    expect(decision.kind).toBe("none");
    expect(decision.src).toBe("");
  });

  it("should preview an archived file when it is still in cold storage", () => {
    // Archived files stream out of cold storage and stay archived, so the
    // lightbox must not treat "archived" as "unavailable".
    const decision = resolvePreview(aFile({ id: 15, status: "archived", extension: ".jpg" }));

    expect(decision.kind).toBe("image");
    expect(decision.src).toBe("/api/v1/files/15/download");
  });

  it("should fall back to the thumbnail when the type is unrecognised but one exists", () => {
    const decision = resolvePreview(
      aFile({ id: 16, extension: ".xyz", thumbnail_path: "CameraRoll/x.thumb.jpg" }),
    );

    expect(decision.kind).toBe("image");
    expect(decision.degraded).toBe(true);
  });

  it("should say why there is nothing to show when the type is unrecognised", () => {
    const decision = resolvePreview(
      aFile({ id: 17, extension: ".xyz", thumbnail_path: undefined }),
    );

    expect(decision.kind).toBe("none");
    expect(decision.reason).not.toBe("");
  });

  it("should match the extension when the case differs", () => {
    const decision = resolvePreview(aFile({ id: 18, extension: ".JPG" }));

    expect(decision.kind).toBe("image");
  });
});

describe("needsReader", () => {
  it("should require a reader when the file is a document format", () => {
    expect(needsReader("pdf")).toBe(true);
    expect(needsReader("epub")).toBe(true);
  });

  it("should not require a reader when the file renders with a native element", () => {
    expect(needsReader("image")).toBe(false);
    expect(needsReader("video")).toBe(false);
    expect(needsReader("text")).toBe(false);
    expect(needsReader("none")).toBe(false);
  });
});
