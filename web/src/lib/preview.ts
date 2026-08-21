import { api } from "./api";
import type { FileRecord } from "../types/api";

/** How the lightbox should render one file.
 *
 *  This exists because "has a thumbnail" was being used as a proxy for "is an
 *  image". It is not: the daemon also thumbnails .heic/.heif and
 *  .mp4/.mov/.avi/.mkv/.webm. So opening a video handed the browser the full
 *  original as an `<img src>` -- every byte of it over the LAN -- and then
 *  rendered a broken-image icon. HEIC did the same everywhere except Safari.
 *  Once documents get thumbnails the same line would do it to a 20 MB PDF. */
export type PreviewKind =
  | "image"
  | "video"
  | "pdf"
  | "epub"
  | "text"
  | "none";

export interface PreviewDecision {
  kind: PreviewKind;
  /** The URL to hand the rendering element. Empty when kind is "none". */
  src: string;
  /** True when src is the 400px thumbnail standing in for an original this
   *  browser cannot decode, so the UI can say so rather than pretending the
   *  blurry version is the file. */
  degraded: boolean;
  /** Why there is nothing to show. Empty unless kind is "none". */
  reason: string;
}

/** Extensions the browser decodes in an `<img>` on every platform we care
 *  about. GIF is here because the daemon thumbnails it; AVIF is not, because
 *  support is still uneven enough that the thumbnail is the safer source. */
const BROWSER_IMAGE_EXTENSIONS = new Set([".jpg", ".jpeg", ".png", ".webp", ".gif"]);
const BROWSER_IMAGE_TYPES = new Set(["image/jpeg", "image/png", "image/webp", "image/gif"]);

/** Formats the daemon can thumbnail but a browser generally cannot decode.
 *  Safari renders HEIC; nothing else does, and finding out costs a full
 *  download of a phone-camera original. The thumbnail is already a JPEG. */
const OPAQUE_IMAGE_EXTENSIONS = new Set([".heic", ".heif", ".avif"]);
const OPAQUE_IMAGE_TYPES = new Set(["image/heic", "image/heif", "image/avif"]);

const VIDEO_EXTENSIONS = new Set([".mp4", ".mov", ".webm", ".mkv", ".avi"]);
const VIDEO_TYPES = new Set([
  "video/mp4",
  "video/quicktime",
  "video/webm",
  "video/x-matroska",
  "video/x-msvideo",
]);

const PDF_EXTENSIONS = new Set([".pdf"]);
const EPUB_EXTENSIONS = new Set([".epub"]);
const TEXT_EXTENSIONS = new Set([
  ".txt",
  ".md",
  ".csv",
  ".log",
  ".json",
  ".xml",
  ".yaml",
  ".yml",
  ".ini",
  ".conf",
  ".toml",
]);

function extensionOf(file: FileRecord): string {
  return (file.extension || "").toLowerCase();
}

/** The sniffed type wins only where the extension says nothing. A file the
 *  owner named .jpg is treated as a JPEG; a file called "bookmarks" has no
 *  extension at all, and detected_type is the only thing that can classify it. */
function detectedOf(file: FileRecord): string {
  return (file.detected_type || "").toLowerCase();
}

export function resolvePreview(file: FileRecord): PreviewDecision {
  const none = (reason: string): PreviewDecision => ({
    kind: "none",
    src: "",
    degraded: false,
    reason,
  });

  if (file.status === "missing") {
    return none("This file is no longer on the device or in cold storage.");
  }

  const ext = extensionOf(file);
  const detected = detectedOf(file);
  const original = api.downloadUrl(file.id);
  const thumbnail = file.thumbnail_path ? api.thumbnailUrl(file.thumbnail_path) : "";

  if (BROWSER_IMAGE_EXTENSIONS.has(ext) || BROWSER_IMAGE_TYPES.has(detected)) {
    return { kind: "image", src: original, degraded: false, reason: "" };
  }

  if (OPAQUE_IMAGE_EXTENSIONS.has(ext) || OPAQUE_IMAGE_TYPES.has(detected)) {
    // Deliberately the thumbnail, not the original. Handing the browser the
    // original is the bug: it downloads a 5 MB HEIC to draw a broken icon.
    if (thumbnail) {
      return { kind: "image", src: thumbnail, degraded: true, reason: "" };
    }
    return none("This browser cannot display this image format. Download it to view it.");
  }

  if (VIDEO_EXTENSIONS.has(ext) || VIDEO_TYPES.has(detected)) {
    return { kind: "video", src: original, degraded: false, reason: "" };
  }

  if (PDF_EXTENSIONS.has(ext) || detected === "application/pdf") {
    return { kind: "pdf", src: api.previewUrl(file.id), degraded: false, reason: "" };
  }

  if (EPUB_EXTENSIONS.has(ext) || detected === "application/epub+zip") {
    return { kind: "epub", src: api.previewUrl(file.id), degraded: false, reason: "" };
  }

  if (TEXT_EXTENSIONS.has(ext) || detected === "text/plain") {
    return { kind: "text", src: api.previewUrl(file.id), degraded: false, reason: "" };
  }

  if (thumbnail) {
    return { kind: "image", src: thumbnail, degraded: true, reason: "" };
  }
  return none("No preview is available for this file type.");
}

/** True when the lightbox has to load a code-split reader for this file, so
 *  the caller can prefetch it rather than waiting for the user to open one. */
export function needsReader(kind: PreviewKind): boolean {
  return kind === "pdf" || kind === "epub";
}
