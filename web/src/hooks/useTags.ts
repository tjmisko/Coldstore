import { useCallback, useEffect, useRef, useState } from "react";
import { api, errorMessage, isAbortError } from "../lib/api";
import type { Tag, TagDeleteResponse, TagRenameResponse } from "../types/api";

/** Debounced, abortable tag completion. The generation check is deliberate:
 * some fetch shims resolve after abort, and an old vocabulary is worse than
 * showing no suggestions while the current request catches up. */
export function useTagCompletions(folder: string, prefix: string, enabled: boolean) {
  const [tags, setTags] = useState<Tag[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [resolvedKey, setResolvedKey] = useState("");
  const generation = useRef(0);
  const key = `${folder}\u0000${prefix.trim()}`;

  useEffect(() => {
    if (!enabled || !folder) {
      return;
    }
    const mine = ++generation.current;
    const controller = new AbortController();
    const timer = window.setTimeout(() => {
      setLoading(true);
      void api.tags({ folder, prefix: prefix.trim(), limit: 50 }, { signal: controller.signal })
        .then((next) => {
          if (mine !== generation.current) return;
          setTags(next);
          setError(null);
          setResolvedKey(key);
        })
        .catch((err: unknown) => {
          if (mine !== generation.current || isAbortError(err)) return;
          setTags([]);
          setError(errorMessage(err));
          setResolvedKey(key);
        })
        .finally(() => {
          if (mine === generation.current) setLoading(false);
        });
    }, 120);

    return () => {
      window.clearTimeout(timer);
      controller.abort();
    };
  }, [enabled, folder, key, prefix]);

  if (!enabled || !folder) return { tags: [], loading: false, error: null };
  const fresh = resolvedKey === key;
  return { tags: fresh ? tags : [], loading: loading || !fresh, error: fresh ? error : null };
}

export function useTagManager(folder: string) {
  const [tags, setTags] = useState<Tag[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const generation = useRef(0);

  const refresh = useCallback(async () => {
    if (!folder) {
      setTags([]);
      return;
    }
    const mine = ++generation.current;
    setLoading(true);
    try {
      // Lifecycle operations are catalog-wide. Folder-scoped counts would
      // understate a merge or delete that also changes files in other folders.
      const next = await api.tags({ limit: 500 });
      if (mine !== generation.current) return;
      setTags(next);
      setError(null);
    } catch (err) {
      if (mine === generation.current) setError(errorMessage(err));
    } finally {
      if (mine === generation.current) setLoading(false);
    }
  }, [folder]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const rename = useCallback(
    async (name: string, nextName: string): Promise<TagRenameResponse> => {
      const result = await api.renameTag(name, nextName);
      await refresh();
      return result;
    },
    [refresh],
  );

  const remove = useCallback(
    async (name: string, cascade: boolean): Promise<TagDeleteResponse> => {
      const result = await api.deleteTag(name, cascade);
      await refresh();
      return result;
    },
    [refresh],
  );

  return { tags, loading, error, refresh, rename, remove };
}

/** The remove picker must not offer a tag merely because it exists somewhere
 * in the folder. Resolve the exact covered ids in bounded GET chunks and count
 * only tags actually present on those files. */
export function useTagsForFiles(ids: number[], enabled: boolean) {
  const [result, setResult] = useState<{ key: string; tags: Tag[]; error: string | null }>({
    key: "", tags: [], error: null,
  });
  const generation = useRef(0);
  const key = [...new Set(ids.filter((id) => id > 0))].sort((a, b) => a - b).join(",");

  useEffect(() => {
    if (!enabled || !key) return;
    const requestIDs = key.split(",").map(Number);
    const mine = ++generation.current;
    const controller = new AbortController();
    const chunks: number[][] = [];
    for (let index = 0; index < requestIDs.length; index += 500) {
      chunks.push(requestIDs.slice(index, index + 500));
    }
    void Promise.all(chunks.map((chunk) => api.filesByIDs(chunk, { signal: controller.signal })))
      .then((pages) => {
        if (mine !== generation.current) return;
        const counts = new Map<string, { name: string; count: number }>();
        for (const file of pages.flat()) {
          for (const name of file.tags) {
            const fold = name.toLocaleLowerCase();
            const current = counts.get(fold);
            counts.set(fold, { name: current?.name ?? name, count: (current?.count ?? 0) + 1 });
          }
        }
        const tags = [...counts.values()]
          .sort((a, b) => b.count - a.count || a.name.localeCompare(b.name))
          .map((tag, index) => ({
            id: -(index + 1), name: tag.name, count: tag.count, descendant_count: tag.count,
          }));
        setResult({ key, tags, error: null });
      })
      .catch((err: unknown) => {
        if (mine !== generation.current || isAbortError(err)) return;
        setResult({ key, tags: [], error: errorMessage(err) });
      });
    return () => {
      controller.abort();
    };
  }, [enabled, key]);

  if (!enabled) return { tags: [], loading: false, error: null };
  return result.key === key
    ? { tags: result.tags, loading: false, error: result.error }
    : { tags: [], loading: key !== "", error: null };
}
