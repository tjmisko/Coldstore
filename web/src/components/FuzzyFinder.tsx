import { useCallback, useMemo, useState } from "react";
import { rank, type FileFields } from "../keymap/fuzzy";
import { CORPUS_MAX, useFileCorpus } from "../hooks/useFileCorpus";
import { api, type FileFilters } from "../lib/api";
import { formatCount, formatSize } from "../lib/format";
import type { FileRecord } from "../types/api";
import { Overlay } from "./Overlay";

interface FuzzyFinderProps {
  initialQuery: string;
  filters: FileFilters;
  onClose(): void;
  /** Switch to the file's folder and put the cursor on it. */
  onReveal(file: FileRecord): void;
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
export function FuzzyFinder({ initialQuery, filters, onClose, onReveal }: FuzzyFinderProps) {
  const [query, setQuery] = useState(initialQuery);
  const [highlight, setHighlight] = useState(0);
  const corpus = useFileCorpus({ filters, query, enabled: true });

  const results = useMemo(
    () => rank(query, corpus.files, fieldsOf).slice(0, 200),
    [query, corpus.files],
  );
  const index = Math.min(highlight, Math.max(0, results.length - 1));
  const active = results[index]?.item;

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
        if (active) {
          onReveal(active);
          onClose();
        }
        return;
      }
      if (event.ctrlKey && event.key.toLowerCase() === "y") {
        event.preventDefault();
        if (active) window.open(api.downloadUrl(active.id), "_blank", "noopener");
        return;
      }
      const down = event.key === "ArrowDown" || (event.ctrlKey && event.key.toLowerCase() === "n");
      const up = event.key === "ArrowUp" || (event.ctrlKey && event.key.toLowerCase() === "p");
      if (down || up) {
        event.preventDefault();
        move(down ? 1 : -1);
      }
    },
    [active, move, onClose, onReveal],
  );

  return (
    <Overlay label="Find a file" onClose={onClose} align="top" className="max-w-3xl" testAttribute="finder">
      <div className="rounded-xl border border-neutral-700 bg-neutral-900 shadow-2xl overflow-hidden">
        <input
          autoFocus
          data-fuzzy-finder
          aria-label="Find a file"
          placeholder="filename, path, folder, extension or status"
          spellCheck={false}
          autoComplete="off"
          value={query}
          onChange={(event) => {
            setQuery(event.target.value);
            setHighlight(0);
          }}
          onKeyDown={onKeyDown}
          className="w-full bg-transparent px-4 py-3 text-sm outline-none border-b border-neutral-800"
        />

        {corpus.error && (
          <p className="px-4 py-2 text-xs text-red-400" role="alert">
            {corpus.error}
          </p>
        )}

        <ul className="max-h-[55vh] overflow-y-auto" role="listbox" aria-label="Matches">
          {results.map((result, at) => (
            <li key={result.item.id}>
              <button
                type="button"
                role="option"
                aria-selected={at === index}
                data-finder-result={result.item.id}
                onMouseDown={(event) => {
                  event.preventDefault();
                  onReveal(result.item);
                  onClose();
                }}
                className={`flex w-full items-baseline gap-3 px-4 py-1.5 text-left ${
                  at === index ? "bg-neutral-800" : "hover:bg-neutral-800/60"
                }`}
              >
                <span className="text-sm text-neutral-200 truncate max-w-[45%]">
                  {result.item.filename}
                </span>
                <span className="text-xs text-neutral-500 truncate flex-1">
                  {result.item.folder_label}/{result.item.relative_path}
                </span>
                <span className="text-xs text-neutral-500 whitespace-nowrap">
                  {formatSize(result.item.size_bytes)}
                </span>
                <span
                  className={`text-xs whitespace-nowrap ${
                    result.item.status === "archived" ? "text-amber-400" : "text-neutral-500"
                  }`}
                >
                  {result.item.status}
                </span>
              </button>
            </li>
          ))}
        </ul>

        {!corpus.loading && results.length === 0 && (
          <p className="px-4 py-6 text-center text-sm text-neutral-500">No file matches.</p>
        )}

        <p
          className="border-t border-neutral-800 px-4 py-2 text-xs text-neutral-500"
          data-finder-footer
        >
          {corpus.loading
            ? "Loading..."
            : corpus.truncated
              ? `Searching the first ${formatCount(CORPUS_MAX)} files in this folder, plus anything the server matches on filename. Narrow the filter to search the rest.`
              : `${formatCount(results.length)} of ${formatCount(corpus.files.length)} files. Enter reveals, Ctrl-y downloads.`}
        </p>
      </div>
    </Overlay>
  );
}
