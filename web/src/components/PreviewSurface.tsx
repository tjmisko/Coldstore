import type { PreviewDecision } from "../lib/preview";
import type { FileRecord } from "../types/api";
import { DocumentReader } from "./readers";

interface PreviewSurfaceProps {
  file: FileRecord;
  preview: PreviewDecision;
}

/** Renders one file with the element that can actually draw it.
 *
 *  The lightbox used to pick its source as "has a thumbnail and is not
 *  missing, therefore renderable as an image". But `internal/thumbnail` also
 *  thumbnails .heif/.heic and .mp4/.mov/.avi/.mkv/.webm, so opening a video
 *  handed the browser the full original as an `<img src>`: it downloaded every
 *  byte over the LAN and then drew a broken-image icon. HEIC did the same
 *  everywhere except Safari, and a PDF thumbnail would have done it to 20 MB
 *  of PDF.
 *
 *  Every branch below is a real decision about what this browser can draw. */
export function PreviewSurface({ file, preview }: PreviewSurfaceProps) {
  switch (preview.kind) {
    case "image":
      return (
        <figure className="flex flex-col items-center gap-2 max-w-full">
          <img
            src={preview.src}
            alt={file.filename}
            data-preview-image
            className="max-w-full max-h-[76vh] object-contain"
          />
          {preview.degraded && (
            <figcaption
              data-preview-degraded
              className="rounded border border-line bg-surface/90 px-2 py-1 text-meta text-ink-muted"
            >
              Showing the thumbnail -- this browser cannot display the original
              format. Download it to see the full image.
            </figcaption>
          )}
        </figure>
      );

    case "video":
      return (
        // preload="metadata", not "auto": the lightbox is a browsing surface
        // and arrowing past ten videos must not start ten full downloads.
        <video
          src={preview.src}
          poster={file.thumbnail_path ? `/api/v1/thumbnails/${file.thumbnail_path}` : undefined}
          controls
          preload="metadata"
          playsInline
          data-preview-video
          className="max-w-full max-h-[76vh]"
        >
          Your browser cannot play this video.
        </video>
      );

    case "pdf":
    case "epub":
    case "text":
      // Behind the app's one React.lazy boundary, reached only now -- when a
      // document is actually opened -- so the gallery grid never loads pdf.js.
      return <DocumentReader file={file} preview={preview} />;

    case "none":
    default:
      return <PreviewUnavailable filename={file.filename} reason={preview.reason} />;
  }
}

function PreviewUnavailable({ filename, reason }: { filename: string; reason: string }) {
  return (
    <div
      data-preview-unavailable
      className="flex flex-col items-center gap-2 text-center px-6"
    >
      <svg
        className="w-16 h-16 text-ink-faint"
        fill="none"
        stroke="currentColor"
        viewBox="0 0 24 24"
        aria-hidden="true"
      >
        <path
          strokeLinecap="round"
          strokeLinejoin="round"
          strokeWidth={1.5}
          d="M9 12h6m-6 4h6m2 5H7a2 2 0 01-2-2V5a2 2 0 012-2h5.586a1 1 0 01.707.293l5.414 5.414a1 1 0 01.293.707V19a2 2 0 01-2 2z"
        />
      </svg>
      <p className="text-ink text-base break-all">{filename}</p>
      {reason && <p className="text-ink-faint text-base max-w-md">{reason}</p>}
    </div>
  );
}
