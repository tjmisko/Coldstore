import { formatCaptureOffset } from "../lib/capture";
import type { FileRecord } from "../types/api";

export function CaptureMeta({ file }: { file: FileRecord }) {
  if (file.captured_local === "") {
    return (
      <span
        data-capture-date=""
        data-capture-source={file.captured_source}
        className="text-meta text-ink-faint"
      >
        {file.meta_scanned_at === null ? "Metadata not scanned yet" : "No capture date recorded"}
      </span>
    );
  }

  return (
    <span className="flex flex-wrap items-baseline gap-x-2 text-meta">
      <time
        dateTime={file.captured_local}
        data-capture-date={file.captured_local}
        data-capture-source={file.captured_source}
        className="tabular text-ink-muted"
      >
        {file.captured_local}
      </time>
      <span className="text-ink-faint">{formatCaptureOffset(file.captured_offset_minutes)}</span>
    </span>
  );
}
