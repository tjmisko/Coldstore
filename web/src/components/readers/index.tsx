import { lazy, Suspense } from "react";
import type { PreviewDecision } from "../../lib/preview";
import type { FileRecord } from "../../types/api";

/** The ONE code-split boundary in the app.
 *
 *  pdf.js is ~1.5 MB. Splitting it does NOT shrink the binary -- `//go:embed
 *  all:dist` embeds every file whether it is split or not -- so what this buys
 *  is first paint: the gallery grid, which is what the app opens on and what
 *  it is used for, never touches the reader chunk.
 *
 *  Imported on LIGHTBOX OPEN for a previewable file, not on app mount. A
 *  dynamic import at module scope would defeat the whole thing. */
const PdfReader = lazy(() => import("./PdfReader"));
const EpubReader = lazy(() => import("./EpubReader"));
const TextReader = lazy(() => import("./TextReader"));

interface DocumentReaderProps {
  file: FileRecord;
  preview: PreviewDecision;
}

/** Each reader is keyed by what it is reading, so arrowing from one document
 *  to the next remounts rather than reusing state. Without it page 8 of the
 *  previous PDF is requested against a two-page one, and the previous book's
 *  section index shows for a paint. */
export function DocumentReader({ file, preview }: DocumentReaderProps) {
  return (
    <Suspense fallback={<ReaderFallback filename={file.filename} />}>
      {preview.kind === "pdf" && (
        <PdfReader key={preview.src} src={preview.src} filename={file.filename} />
      )}
      {preview.kind === "epub" && (
        <EpubReader key={file.id} fileId={file.id} filename={file.filename} />
      )}
      {preview.kind === "text" && (
        <TextReader key={preview.src} src={preview.src} filename={file.filename} />
      )}
    </Suspense>
  );
}

function ReaderFallback({ filename }: { filename: string }) {
  return (
    <p data-reader-loading className="text-ink-muted text-base py-12">
      Opening {filename}...
    </p>
  );
}
