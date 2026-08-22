import { formatSize } from "../lib/format";
import { resolvePreview } from "../lib/preview";
import type { FileRecord } from "../types/api";
import { Overlay } from "./Overlay";
import { PreviewSurface } from "./PreviewSurface";

interface FilePreviewDialogProps {
  file: FileRecord;
  onClose(): void;
}

/** A finder-owned preview that does not depend on the currently loaded grid. */
export function FilePreviewDialog({ file, onClose }: FilePreviewDialogProps) {
  const preview = resolvePreview(file);

  return (
    <Overlay label={`Preview ${file.filename}`} onClose={onClose} className="max-w-6xl">
      <section
        data-finder-open-preview={file.id}
        data-preview-kind={preview.kind}
        className="overflow-hidden rounded-xl border border-line bg-surface shadow-2xl"
      >
        <header className="flex items-center gap-3 border-b border-line px-4 py-3">
          <div className="min-w-0 flex-1">
            <h2 className="truncate text-base font-medium text-ink">{file.filename}</h2>
            <p className="truncate text-meta text-ink-faint">
              {file.folder_label}/{file.relative_path} · {formatSize(file.size_bytes)}
            </p>
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close preview"
            className="rounded-md px-3 py-1.5 text-base text-ink-muted hover:bg-raised hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
          >
            Close
          </button>
        </header>
        <div className="flex min-h-64 max-h-[80vh] items-center justify-center overflow-auto bg-ground p-4">
          <PreviewSurface file={file} preview={preview} />
        </div>
      </section>
    </Overlay>
  );
}
