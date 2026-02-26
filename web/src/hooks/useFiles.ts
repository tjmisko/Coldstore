import { useState, useCallback, useEffect, useRef } from "react";
import type { FileRecord, FileStatus } from "../types/api";

interface UseFilesOptions {
  folder?: string;
  status?: FileStatus;
  extension?: string;
  search?: string;
  sort?: string;
  limit?: number;
}

export function useFiles(options: UseFilesOptions = {}) {
  const [files, setFiles] = useState<FileRecord[]>([]);
  const [loading, setLoading] = useState(false);
  const [hasMore, setHasMore] = useState(true);
  const offsetRef = useRef(0);
  const limit = options.limit ?? 50;

  const buildUrl = useCallback(
    (offset: number) => {
      const params = new URLSearchParams();
      if (options.folder) params.set("folder", options.folder);
      if (options.status) params.set("status", options.status);
      if (options.extension) params.set("ext", options.extension);
      if (options.search) params.set("q", options.search);
      if (options.sort) params.set("sort", options.sort);
      params.set("offset", String(offset));
      params.set("limit", String(limit));
      return `/api/v1/files?${params}`;
    },
    [options.folder, options.status, options.extension, options.search, options.sort, limit],
  );

  const fetchPage = useCallback(
    async (reset = false) => {
      if (loading) return;
      setLoading(true);

      const offset = reset ? 0 : offsetRef.current;
      try {
        const res = await fetch(buildUrl(offset));
        const data: FileRecord[] = await res.json();

        if (reset) {
          setFiles(data);
          offsetRef.current = data.length;
        } else {
          setFiles((prev) => [...prev, ...data]);
          offsetRef.current += data.length;
        }
        setHasMore(data.length === limit);
      } catch (err) {
        console.error("Failed to fetch files:", err);
      } finally {
        setLoading(false);
      }
    },
    [buildUrl, limit, loading],
  );

  // Reset when filters change
  useEffect(() => {
    offsetRef.current = 0;
    setFiles([]);
    setHasMore(true);
    fetchPage(true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [options.folder, options.status, options.extension, options.search, options.sort]);

  const loadMore = useCallback(() => {
    if (hasMore && !loading) {
      fetchPage(false);
    }
  }, [hasMore, loading, fetchPage]);

  return { files, loading, hasMore, loadMore, refetch: () => fetchPage(true) };
}
