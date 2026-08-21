import { useCallback, useMemo, useState } from "react";
import { rank, type FileFields } from "../keymap/fuzzy";
import { CORPUS_MAX, useFileCorpus } from "../hooks/useFileCorpus";
import { api, type FileFilters } from "../lib/api";
import { formatCount, formatSize } from "../lib/format";
import { resolvePreview } from "../lib/preview";
import type { FileRecord } from "../types/api";
import { Overlay } from "./Overlay";
import { PreviewSurface } from "./PreviewSurface";

interface FuzzyFinderProps {
  initialQuery: string;
  filters: FileFilters;
  onClose(): void;
  onOpen(file: FileRecord): void;
  /** Switch to the Files view and put the cursor on this complete record. */
  onShowInFiles(file: FileRecord): void;
}

function fieldsOf(file: FileRecord): FileFields {
  return {
    filename: file.filename,
    path: file.relative_path,
    folder: file.folder_label,
    extension: file.extension,
    status: file.status,
  };
}

/** F10 and <leader>fe.
 *
 *  Searches filenames, paths, folder, extension and status. Ranking is entirely
 *  client-side because the server's `q` is `filename LIKE '%q%'` and has no
 *  ordering at all; the corpus is bounded because a folder with 100k files
 *  cannot ship its filename list to a phone. */
export function FuzzyFinder({
  initialQuery,
  filters,
  onClose,
  onOpen,
  onShowInFiles,
}: FuzzyFinderProps) {
  const [query, setQuery] = useState(initialQuery);
  const [highlight, setHighlight] = useState(0);
  const [selected, setSelected] = useState<ReadonlyMap<number, FileRecord>>(() => new Map());
  const corpus = useFileCorpus({ filters, query, enabled: true });

  const results = useMemo(
    () => rank(query, corpus.files, fieldsOf).slice(0, 200),
    [query, corpus.files],
  );
  const index = Math.min(highlight, Math.max(0, results.length - 1));
  const active = results[index]?.item;
  const preview = active ? resolvePreview(active) : null;

  const toggleSelected = useCallback((file: FileRecord) => {
    setSelected((current) => {
      const next = new Map(current);
      if (next.has(file.id)) next.delete(file.id);
      else next.set(file.id, file);
      return next;
    });
  }, []);

  const download = useCallback(() => {
    const targets = selected.size > 0 ? [...selected.values()] : active ? [active] : [];
    for (const file of targets) window.open(api.downloadUrl(file.id), "_blank", "noopener");
  }, [active, selected]);

  const move = useCallback(
    (delta: number) => {
      setHighlight((current) => {
        const size = Math.max(1, results.length);
        return (current + delta + size) % size;
      });
    },
    [results.length],
  );

  const onKeyDown = useCallback(
    (event: React.KeyboardEvent<HTMLInputElement>) => {
      if (event.key === "Escape") {
        event.preventDefault();
        onClose();
        return;
      }
      if (event.key === "Enter") {
        event.preventDefault();
        if (active) onOpen(active);
        return;
      }
      if (event.ctrlKey && event.key.toLowerCase() === "x") {
        event.preventDefault();
        if (active) toggleSelected(active);
        return;
      }
      if (event.ctrlKey && event.key.toLowerCase() === "y") {
        event.preventDefault();
        download();
        return;
      }
      if (event.ctrlKey && event.key.toLowerCase() === "g") {
        event.preventDefault();
        if (active) onShowInFiles(active);
        return;
      }
      const down = event.key === "ArrowDown" || (event.ctrlKey && event.key.toLowerCase() === "n");
      const up = event.key === "ArrowUp" || (event.ctrlKey && event.key.toLowerCase() === "p");
      if (down || up) {
        event.preventDefault();
        move(down ? 1 : -1);
      }
    },
    [active, download, move, onClose, onOpen, onShowInFiles, toggleSelected],
  );

  return (
    <Overlay label="Find a file" onClose={onClose} align="top" className="max-w-6xl" testAttribute="finder">
      <div className="overflow-hidden rounded-xl border border-line bg-surface shadow-2xl">
        <input
          autoFocus
          data-fuzzy-finder
          aria-label="Find a file"
          placeholder="filename, path, folder, extension or status"
          spellCheck={false}
          autoComplete="off"
          value={query}
          aria-activedescendant={active ? `finder-result-${active.id}` : undefined}
          onChange={(event) => {
            setQuery(event.target.value);
            setHighlight(0);
          }}
          onKeyDown={onKeyDown}
          className="w-full border-b border-line bg-transparent px-4 py-3 text-base text-ink outline-none"
        />

        {corpus.error && (
          <p className="px-4 py-2 text-meta text-alarm" role="alert">
            {corpus.error}
          </p>
        )}

        <div className="grid min-h-64 md:grid-cols-[minmax(0,3fr)_minmax(18rem,2fr)]">
          <ul
            className="max-h-[55vh] overflow-y-auto border-b border-line md:border-b-0 md:border-r"
            role="listbox"
            aria-label="Matches"
            aria-multiselectable="true"
          >
            {results.map((result, at) => {
              const isSelected = selected.has(result.item.id);
              return (
                <li key={result.item.id}>
                  <button
                    id={`finder-result-${result.item.id}`}
                    type="button"
                    role="option"
                    aria-selected={isSelected}
                    data-finder-result={result.item.id}
                    data-finder-active={at === index}
                    data-finder-selected={isSelected}
                    onMouseEnter={() => setHighlight(at)}
                    onMouseDown={(event) => {
                      event.preventDefault();
                      if (event.ctrlKey || event.metaKey) toggleSelected(result.item);
                      else onOpen(result.item);
                    }}
                    className={`flex w-full items-baseline gap-3 px-4 py-1.5 text-left ${
                      at === index ? "bg-raised-hover" : "hover:bg-raised"
                    } ${isSelected ? "border-l-2 border-cursor pl-3.5" : ""}`}
                  >
                    <span className="max-w-[45%] truncate text-base text-ink">
                      {result.item.filename}
                    </span>
                    <span className="flex-1 truncate text-meta text-ink-faint">
                      {result.item.folder_label}/{result.item.relative_path}
                    </span>
                    <span className="whitespace-nowrap text-meta text-ink-faint">
                      {formatSize(result.item.size_bytes)}
                    </span>
                    <span
                      className={`whitespace-nowrap text-meta ${
                        result.item.status === "archived" ? "text-cold" : "text-ink-faint"
                      }`}
                    >
                      {result.item.status}
                    </span>
                  </button>
                </li>
              );
            })}
          </ul>

          <div
            data-finder-preview={active?.id ?? "empty"}
            data-preview-kind={preview?.kind ?? "none"}
            className="flex max-h-[55vh] min-h-64 items-center justify-center overflow-auto bg-ground p-4"
          >
            {active && preview ? (
              <PreviewSurface file={active} preview={preview} />
            ) : (
              <p className="text-base text-ink-faint">No file selected.</p>
            )}
          </div>
        </div>

        {!corpus.loading && results.length === 0 && (
          <p className="px-4 py-6 text-center text-base text-ink-faint">No file matches.</p>
        )}

        <p
          className="border-t border-line px-4 py-2 text-meta text-ink-faint"
          data-finder-footer
        >
          {corpus.loading
            ? "Loading..."
            : corpus.truncated
              ? `Searching the first ${formatCount(CORPUS_MAX)} files in this folder, plus anything the server matches on filename. Narrow the filter to search the rest.`
              : `${formatCount(results.length)} of ${formatCount(corpus.files.length)} files. Enter previews, Ctrl-x selects, Ctrl-y downloads selection, Ctrl-g shows in Files view.`}
        </p>
      </div>
    </Overlay>
  );
}
