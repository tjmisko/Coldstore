import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { aFile } from "../test/fixtures";
import { resolvePreview } from "../lib/preview";
import { PreviewSurface } from "./PreviewSurface";
import type { FileRecord } from "../types/api";

function renderFor(file: FileRecord) {
  return render(<PreviewSurface file={file} preview={resolvePreview(file)} />);
}

describe("PreviewSurface", () => {
  it("should use an img element when the file is a browser-decodable image", () => {
    renderFor(aFile({ id: 1, filename: "one.jpg", extension: ".jpg" }));

    const img = document.querySelector("[data-preview-image]") as HTMLImageElement;
    expect(img).not.toBeNull();
    expect(img.tagName).toBe("IMG");
    expect(img.getAttribute("src")).toBe("/api/v1/files/1/download");
    expect(document.querySelector("[data-preview-video]")).toBeNull();
  });

  it("should use a video element when the file is a video", () => {
    // The bug, exactly: this file has a thumbnail, and the old code read that
    // as "is an image" and put 400 MB of MP4 inside an <img src>.
    renderFor(
      aFile({
        id: 2,
        filename: "clip.mp4",
        extension: ".mp4",
        thumbnail_path: "CameraRoll/clip.mp4.thumb.jpg",
      }),
    );

    const video = document.querySelector("[data-preview-video]") as HTMLVideoElement;
    expect(video).not.toBeNull();
    expect(video.tagName).toBe("VIDEO");
    expect(document.querySelector("[data-preview-image]")).toBeNull();
  });

  it("should not download the original when the video is only being browsed past", () => {
    renderFor(aFile({ id: 3, filename: "clip.mp4", extension: ".mp4" }));

    const video = document.querySelector("[data-preview-video]") as HTMLVideoElement;
    expect(video.getAttribute("preload")).toBe("metadata");
  });

  it("should show the thumbnail when the browser cannot decode the original format", () => {
    renderFor(
      aFile({
        id: 4,
        filename: "IMG_0001.heic",
        extension: ".heic",
        thumbnail_path: "CameraRoll/IMG_0001.heic.thumb.jpg",
      }),
    );

    const img = document.querySelector("[data-preview-image]") as HTMLImageElement;
    expect(img.getAttribute("src")).toBe("/api/v1/thumbnails/CameraRoll/IMG_0001.heic.thumb.jpg");
  });

  it("should say the image is degraded when it is showing the thumbnail", () => {
    // Otherwise the owner reads a 400px stand-in as the file itself.
    renderFor(
      aFile({
        id: 5,
        filename: "IMG_0001.heic",
        extension: ".heic",
        thumbnail_path: "CameraRoll/IMG_0001.heic.thumb.jpg",
      }),
    );

    expect(document.querySelector("[data-preview-degraded]")).not.toBeNull();
  });

  it("should not claim degradation when it is showing the original", () => {
    renderFor(aFile({ id: 6, filename: "one.jpg", extension: ".jpg" }));

    expect(document.querySelector("[data-preview-degraded]")).toBeNull();
  });

  it("should say why nothing is shown when the file is missing", () => {
    renderFor(
      aFile({
        id: 7,
        filename: "gone.jpg",
        extension: ".jpg",
        status: "missing",
        thumbnail_path: "CameraRoll/gone.jpg.thumb.jpg",
      }),
    );

    expect(document.querySelector("[data-preview-unavailable]")).not.toBeNull();
    expect(screen.getByText("gone.jpg")).toBeInTheDocument();
    expect(document.querySelector("[data-preview-image]")).toBeNull();
  });

  it("should name the file when there is no preview for its type", () => {
    renderFor(aFile({ id: 8, filename: "firmware.bin", extension: ".bin", thumbnail_path: undefined }));

    expect(document.querySelector("[data-preview-unavailable]")).not.toBeNull();
    expect(screen.getByText("firmware.bin")).toBeInTheDocument();
  });
});
