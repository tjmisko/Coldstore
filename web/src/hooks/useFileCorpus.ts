import { useCallback, useEffect, useState } from "react";
import { api, errorMessage, isAbortError, type FileFilters } from "../lib/api";
import type { FileRecord } from "../types/api";

/** The server caps GET /files?limit= at 500 (internal/api/handlers_files.go:27),
 *  so the corpus is four requests, not one. */
const PAGE = 500;

/** How many files the finder will ever hold in the browser.
 *
 *  A folder with 100k files cannot ship its filename list to a phone, and
 *  pretending otherwise is how a finder becomes a thirty-second spinner. Above
 *  this the finder says so in its footer AND leans on the server's substring
 *  filter, so a file outside the window is still findable by an exact
 *  substring. */
export const CORPUS_MAX = 2000;

export interface FileCorpus {
  files: FileRecord[];
  loading: boolean;
  /** True when the folder holds more files than the window. */
  truncated: boolean;
  error: string | null;
}

interface Options {
  filters: FileFilters;
  /** The query, used only for the server-side top-up when truncated. */
  query: string;
  enabled: boolean;
}

/** useFileCorpus loads a bounded window of the current folder for the finder to
 *  rank locally, and tops it up from the server when the window is not the
 *  whole folder.
 *
 *  Server-side `q` is `filename LIKE '%q%'` -- filename only, no ranking -- so
 *  it can find a file the window missed but cannot order the results. Merging
 *  its hits into the corpus and ranking the union locally gets both. */
export function useFileCorpus({ filters, query, enabled }: Options): FileCorpus {
  const [corpus, setCorpus] = useState<{ key: string; files: FileRecord[]; truncated: boolean }>({
    key: "",
    files: [],
    truncated: false,
  });
  const [extra, setExtra] = useState<{ key: string; files: FileRecord[] }>({ key: "", files: [] });
  const [error, setError] = useState<string | null>(null);

  const key = JSON.stringify(filters);
  const fresh = enabled && corpus.key === key;

  const load = useCallback(
    async (signal: AbortSignal) => {
      try {
        const collected: FileRecord[] = [];
        let truncated = false;
        for (let offset = 0; offset < CORPUS_MAX; offset += PAGE) {
          const page = await api.files(filters, { offset, limit: PAGE, sort: "name" }, { signal });
          collected.push(...page);
          if (page.length < PAGE) break;
          if (collected.length >= CORPUS_MAX) {
            truncated = true;
            break;
          }
        }
        if (signal.aborted) return;
        setCorpus({ key, files: collected.slice(0, CORPUS_MAX), truncated });
        setError(null);
      } catch (err) {
        if (signal.aborted || isAbortError(err)) return;
        setError(errorMessage(err));
      }
    },
    [filters, key],
  );

  useEffect(() => {
    if (!enabled) return;
    const controller = new AbortController();
    // The await is not decorative: react-hooks/set-state-in-effect inlines a
    // function called straight from an effect body and sees the setState inside
    // it as synchronous.
    void (async () => {
      await load(controller.signal);
    })();
    return () => controller.abort();
  }, [load, enabled]);

  const topUpKey = `${key}|${query}`;
  const topUp = useCallback(
    async (signal: AbortSignal) => {
      try {
        const page = await api.files({ ...filters, search: query }, { offset: 0, limit: 100, sort: "name" }, { signal });
        if (signal.aborted) return;
        setExtra({ key: topUpKey, files: page });
      } catch (err) {
        if (signal.aborted || isAbortError(err)) return;
        setError(errorMessage(err));
      }
    },
    [filters, query, topUpKey],
  );

  useEffect(() => {
    if (!enabled || !corpus.truncated || query.length < 2) return;
    const controller = new AbortController();
    // Debounced: one request per pause, not one per keystroke, because the box
    // on the other end serialises every query behind a single SQLite
    // connection shared with the quota tick.
    const timer = setTimeout(() => {
      void (async () => {
        await topUp(controller.signal);
      })();
    }, 180);
    return () => {
      clearTimeout(timer);
      controller.abort();
    };
  }, [topUp, enabled, corpus.truncated, query]);

  const merged =
    extra.key === topUpKey && extra.files.length > 0
      ? mergeByID(corpus.files, extra.files)
      : corpus.files;

  return {
    files: fresh ? merged : [],
    loading: enabled && !fresh,
    truncated: fresh && corpus.truncated,
    error,
  };
}

function mergeByID(base: FileRecord[], extra: FileRecord[]): FileRecord[] {
  const seen = new Set(base.map((f) => f.id));
  const out = [...base];
  for (const file of extra) {
    if (!seen.has(file.id)) out.push(file);
  }
  return out;
}
