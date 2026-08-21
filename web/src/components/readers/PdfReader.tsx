import { useCallback, useEffect, useRef, useState } from "react";
import { Document, Page, pdfjs } from "react-pdf";
import workerSrc from "pdfjs-dist/build/pdf.worker.min.mjs?url";
import "react-pdf/dist/Page/AnnotationLayer.css";
import "react-pdf/dist/Page/TextLayer.css";

// The worker comes through Vite's ?url import, so it is emitted as a hashed
// asset that the binary's embedded filesystem serves. pdf.js's default is a
// CDN URL, and the daemon runs on a LAN with no egress -- often behind
// Tailscale with nothing else reachable -- so a CDN worker is not a slow
// reader, it is a blank one.
pdfjs.GlobalWorkerOptions.workerSrc = workerSrc;

// Mirrors PDFJS_ASSET_BASE in vite.config.ts, where the plugin that copies
// these directories out of node_modules is defined.
const ASSET_BASE = "/pdfjs";

// standard_fonts: the standard-14 font programs, needed by older PDFs that use
// Helvetica and friends without embedding them.
// wasm: the JPEG 2000 and colour-management decoders, needed by scans.
// cmaps is deliberately absent -- 1.7 MB of CJK encodings this install has no
// use for.
const DOCUMENT_OPTIONS = {
  standardFontDataUrl: `${ASSET_BASE}/standard_fonts/`,
  wasmUrl: `${ASSET_BASE}/wasm/`,
} as const;

interface PdfReaderProps {
  /** The inline preview URL. NOT the download URL: that one sets
   *  Content-Disposition: attachment, which a fetch would honour. */
  src: string;
  filename: string;
}

const READER_BUTTON =
  "px-3 py-1 rounded-md bg-raised text-ink ring-1 ring-line-strong hover:bg-raised-hover disabled:opacity-40 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor";

/** A pdf.js reader, rendered in the browser rather than on the daemon.
 *
 *  Rasterising a page server-side would put that work on a 1.5 GHz 2011 core
 *  inside the request path, competing with Syncthing and the thumbnail pass.
 *  Here the daemon's whole job is ServeFile with range requests, which it
 *  already does -- including for archived files, which stream out of cold
 *  storage without being restored. */
export default function PdfReader({ src, filename }: PdfReaderProps) {
  const [pageCount, setPageCount] = useState(0);
  const [page, setPage] = useState(1);
  const [error, setError] = useState("");
  const [width, setWidth] = useState(0);
  const frameRef = useRef<HTMLDivElement | null>(null);

  // react-pdf renders at a fixed pixel width, so it needs a measured one. A
  // percentage would render at the intrinsic page size and overflow the
  // lightbox on a phone.
  useEffect(() => {
    const el = frameRef.current;
    if (!el) return;
    const measure = () => setWidth(el.clientWidth);
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  // There is deliberately no reset effect for page/pageCount: DocumentReader
  // keys this component by src, so a different PDF is a different instance
  // with fresh state. Resetting in an effect instead would leave page 8 of the
  // previous document requested against a two-page one for a paint.

  const onLoad = useCallback(({ numPages }: { numPages: number }) => {
    setPageCount(numPages);
    setError("");
  }, []);

  const onError = useCallback((err: Error) => {
    setError(err.message || "This PDF could not be read.");
  }, []);

  if (error) {
    return (
      <div data-pdf-error className="text-center px-6">
        <p className="text-ink">{filename}</p>
        <p className="text-alarm text-base mt-2">{error}</p>
      </div>
    );
  }

  return (
    <div data-pdf-reader className="flex flex-col items-center gap-3 w-full">
      <div ref={frameRef} className="w-full max-w-3xl overflow-auto max-h-[68vh] flex justify-center">
        <Document
          file={src}
          options={DOCUMENT_OPTIONS}
          onLoadSuccess={onLoad}
          onLoadError={onError}
          onSourceError={onError}
          loading={<ReaderMessage>Loading {filename}...</ReaderMessage>}
          error={<ReaderMessage>This PDF could not be read.</ReaderMessage>}
        >
          {width > 0 && (
            <Page
              pageNumber={page}
              width={Math.min(width, 900)}
              renderAnnotationLayer={false}
              loading={<ReaderMessage>Rendering page {page}...</ReaderMessage>}
            />
          )}
        </Document>
      </div>

      {pageCount > 1 && (
        <div className="flex items-center gap-3 text-base text-ink-muted">
          <button
            type="button"
            data-pdf-prev
            onClick={() => setPage((p) => Math.max(1, p - 1))}
            disabled={page <= 1}
            className={READER_BUTTON}
          >
            Previous
          </button>
          <span data-pdf-page-indicator>
            Page {page} of {pageCount}
          </span>
          <button
            type="button"
            data-pdf-next
            onClick={() => setPage((p) => Math.min(pageCount, p + 1))}
            disabled={page >= pageCount}
            className={READER_BUTTON}
          >
            Next
          </button>
        </div>
      )}
    </div>
  );
}

function ReaderMessage({ children }: { children: React.ReactNode }) {
  return <p className="text-ink-muted text-base py-12">{children}</p>;
}
