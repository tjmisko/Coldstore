import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { api, errorMessage, isAbortError, type FileFilters } from "../lib/api";
import type { FileRecord, FileSort } from "../types/api";

/** The server caps GET /files?ids= at 500. refetchIds chunks at that number
 *  rather than inventing a query shape: without a server-side id filter it
 *  would fall back to the unfiltered first page, and a bulk restore of 34
 *  files would patch 50 arbitrary rows with stale-but-plausible data while the
 *  34 restored ones went on rendering as archived. */
const ID_CHUNK = 500;

/** Above this, reconciling by id is more requests than simply reloading. */
const ID_REFETCH_CEILING = 2000;

export interface UseFilesOptions extends FileFilters {
  sort?: FileSort;
  limit?: number;
}

interface Page {
  /** The query these files answer. Rendering compares it with the CURRENT
   *  query, which is what makes "loading" a derived value instead of a state
   *  written from an effect -- and what stops a filter change from showing the
   *  previous folder's photos under the new folder's heading. */
  query: string;
  files: FileRecord[];
  hasMore: boolean;
}

export interface UseFiles {
  files: FileRecord[];
  loading: boolean;
  error: string | null;
  hasMore: boolean;
  loadMore(): void;
  refetch(): Promise<void>;
  refetchIds(ids: number[]): Promise<void>;
  patchFile(id: number, partial: Partial<FileRecord>): void;
}

export function useFiles(options: UseFilesOptions): UseFiles {
  const {
    folder, status, extension, search, pinned, pinSource, conflict,
    capturedFrom, capturedTo, undated, near,
    sort, limit = 60,
  } = options;

  const filters = useMemo<FileFilters>(
    () => ({
      folder, status, extension, search, pinned, pinSource, conflict,
      capturedFrom, capturedTo, undated, near,
    }),
    [
      folder, status, extension, search, pinned, pinSource, conflict,
      capturedFrom, capturedTo, undated, near,
    ],
  );
  const query = useMemo(
    () => JSON.stringify([filters, sort, limit]),
    [filters, sort, limit],
  );

  const [page, setPage] = useState<Page>({ query: "", files: [], hasMore: true });
  const [appending, setAppending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // generation, not a `loading` flag in the dependency list. The old guard
  // returned early while a page was in flight and listed `loading` as a
  // dependency, so a filter reset arriving mid-page was dropped silently and
  // the grid kept showing the previous filter's rows.
  const generation = useRef(0);
  const offset = useRef(0);

  const fresh = page.query === query;
  const files = fresh ? page.files : [];

  useEffect(() => {
    let cancelled = false;
    const controller = new AbortController();
    const mine = ++generation.current;
    offset.current = 0;

    void (async () => {
      try {
        const data = await api.files(filters, { offset: 0, limit, sort },
          { signal: controller.signal });
        if (cancelled || mine !== generation.current) return;
        offset.current = data.length;
        setPage({ query, files: data, hasMore: data.length === limit });
        setError(null);
      } catch (err) {
        if (cancelled || isAbortError(err)) return;
        setError(errorMessage(err));
      }
    })();

    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [query, filters, sort, limit]);

  const loadMore = useCallback(() => {
    if (!fresh || !page.hasMore || appending) return;
    const mine = generation.current;
    setAppending(true);
    void (async () => {
      try {
        const data = await api.files(filters, { offset: offset.current, limit, sort });
        if (mine !== generation.current) return;
        offset.current += data.length;
        setPage((prev) =>
          prev.query === query
            ? { query, files: [...prev.files, ...data], hasMore: data.length === limit }
            : prev,
        );
        setError(null);
      } catch (err) {
        if (mine === generation.current) setError(errorMessage(err));
      } finally {
        if (mine === generation.current) setAppending(false);
      }
    })();
  }, [fresh, page.hasMore, appending, filters, limit, sort, query]);

  const refetch = useCallback(async () => {
    const mine = ++generation.current;
    try {
      const data = await api.files(filters, { offset: 0, limit, sort });
      if (mine !== generation.current) return;
      offset.current = data.length;
      setPage({ query, files: data, hasMore: data.length === limit });
      setError(null);
    } catch (err) {
      if (mine === generation.current) setError(errorMessage(err));
    }
  }, [filters, limit, sort, query]);

  const patchFile = useCallback((id: number, partial: Partial<FileRecord>) => {
    setPage((prev) => ({
      ...prev,
      files: prev.files.map((f) => (f.id === id ? { ...f, ...partial } : f)),
    }));
  }, []);

  /** refetchIds repaints exactly the rows a job touched. Above the ceiling it
   *  reloads instead: past that point the chunked round trips cost more than
   *  the page they are patching. */
  const refetchIds = useCallback(
    async (ids: number[]) => {
      if (ids.length === 0) return;
      if (ids.length > ID_REFETCH_CEILING) {
        await refetch();
        return;
      }
      try {
        const chunks: number[][] = [];
        for (let i = 0; i < ids.length; i += ID_CHUNK) chunks.push(ids.slice(i, i + ID_CHUNK));
        const pages = await Promise.all(chunks.map((chunk) => api.filesByIDs(chunk)));
        const byID = new Map<number, FileRecord>();
        for (const rows of pages) for (const row of rows) byID.set(row.id, row);
        setPage((prev) => ({
          ...prev,
          files: prev.files.map((f) => byID.get(f.id) ?? f),
        }));
      } catch (err) {
        setError(errorMessage(err));
      }
    },
    [refetch],
  );

  return {
    files,
    loading: !fresh || appending,
    error,
    hasMore: fresh ? page.hasMore : true,
    loadMore,
    refetch,
    refetchIds,
    patchFile,
  };
}
