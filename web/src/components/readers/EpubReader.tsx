import { useCallback, useEffect, useState } from "react";
import { useKeymapLayer } from "../../hooks/useKeymapLayer";
import type { Command } from "../../keymap/types";
import { api } from "../../lib/api";
import type { EpubSpine } from "../../types/api";

interface EpubReaderProps {
  fileId: number;
  filename: string;
}

const READER_BUTTON =
  "px-3 py-1 rounded-md bg-raised text-ink ring-1 ring-line-strong hover:bg-raised-hover disabled:opacity-40 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor";

/** A book reader with no book-reading library in it.
 *
 *  The daemon already had to learn the EPUB container format to extract cover
 *  thumbnails, so the spine and its resources come out of the same parser. The
 *  alternative -- epub.js -- means downloading the whole book as an
 *  arrayBuffer and unzipping 20 MB in a phone browser, for a feature whose job
 *  is "look before downloading".
 *
 *  What it gives up is epub.js's paginated columns and CFI position memory.
 *  It drops in behind this same lazy boundary later if those turn out to
 *  matter. */
export default function EpubReader({ fileId, filename }: EpubReaderProps) {
  const [spine, setSpine] = useState<EpubSpine | null>(null);
  const [section, setSection] = useState(0);
  const [error, setError] = useState("");

  const handleKeymap = useCallback((command: Command): boolean => {
    if (command.kind !== "action") return false;
    const count = Math.max(1, command.count);
    if (command.action === "previous-document-page") {
      setSection((current) => Math.max(0, current - count));
      return true;
    }
    if (command.action === "next-document-page") {
      setSection((current) => Math.min((spine?.sections.length ?? 1) - 1, current + count));
      return true;
    }
    return false;
  }, [spine?.sections.length]);
  useKeymapLayer("epub-reader", handleKeymap);

  // No reset of spine/section here: DocumentReader keys this component by
  // file id, so a different book is a different instance with fresh state.
  // Resetting inside the effect instead would be a cascading render, and it
  // would leave the previous book's section index visible for one paint.
  useEffect(() => {
    const controller = new AbortController();

    (async () => {
      try {
        const body = await api.epubSpine(fileId, { signal: controller.signal });
        setSpine(body);
      } catch (err) {
        if (controller.signal.aborted) return;
        setError(err instanceof Error ? err.message : "This book could not be opened.");
      }
    })();

    return () => controller.abort();
  }, [fileId]);

  if (error) {
    return (
      <div data-epub-error className="text-center px-6">
        <p className="text-ink">{filename}</p>
        <p className="text-alarm text-base mt-2">{error}</p>
      </div>
    );
  }

  if (!spine) {
    return (
      <p data-epub-loading className="text-ink-muted text-base py-12">
        Opening {filename}...
      </p>
    );
  }

  if (spine.sections.length === 0) {
    return (
      <div data-epub-empty className="text-center px-6">
        <p className="text-ink">{spine.title || filename}</p>
        <p className="text-ink-faint text-base mt-2">This book has no readable sections.</p>
      </div>
    );
  }

  const current = spine.sections[Math.min(section, spine.sections.length - 1)];

  return (
    <div data-epub-reader className="flex flex-col items-center gap-3 w-full">
      {/* sandbox with NO allow-scripts and NO allow-same-origin. The markup
          inside is whatever arrived over Syncthing; an opaque origin means it
          cannot read this page, its storage or its cookies. Relative URLs in
          the chapter still resolve, because they resolve against the
          document's URL rather than its origin -- which is why the resource
          route serves entries at their real archive paths and nothing has to
          be rewritten. */}
      <iframe
        key={current.path}
        data-epub-frame
        title={`${spine.title || filename} - section ${section + 1}`}
        src={api.epubResourceUrl(fileId, current.path)}
        sandbox=""
        referrerPolicy="no-referrer"
        className="w-full max-w-3xl h-[68vh] bg-ink rounded-md"
      />

      <div className="flex items-center gap-3 text-base text-ink-muted">
        <button
          type="button"
          data-epub-prev
          onClick={() => setSection((n) => Math.max(0, n - 1))}
          disabled={section <= 0}
          className={READER_BUTTON}
        >
          Previous
        </button>
        <span data-epub-section-indicator>
          {current.label || `Section ${section + 1}`} of {spine.sections.length}
        </span>
        <button
          type="button"
          data-epub-next
          onClick={() => setSection((n) => Math.min(spine.sections.length - 1, n + 1))}
          disabled={section >= spine.sections.length - 1}
          className={READER_BUTTON}
        >
          Next
        </button>
      </div>
    </div>
  );
}
