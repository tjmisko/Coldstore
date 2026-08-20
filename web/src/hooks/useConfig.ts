import { useState, useEffect, useCallback } from "react";
import type { FolderConfig, PersistentPath, StatusResponse } from "../types/api";

// Each hook below loads once on mount and exposes `refetch` for callers.
//
// The mount effects deliberately `await refetch()` inside an async IIFE instead of
// calling `refetch()` directly. The state update inside refetch always happens after
// `await fetch(...)`, so it was never actually synchronous, but calling an async
// function straight from an effect body trips react-hooks/set-state-in-effect. The IIFE
// makes the asynchrony explicit to the linter. Runtime behavior is unchanged: one fetch
// per mount, state set when it resolves.

export function useStatus() {
  const [status, setStatus] = useState<StatusResponse | null>(null);

  const refetch = useCallback(async () => {
    try {
      const res = await fetch("/api/v1/status");
      setStatus(await res.json());
    } catch (err) {
      console.error("Failed to fetch status:", err);
    }
  }, []);

  useEffect(() => {
    void (async () => {
      await refetch();
    })();
  }, [refetch]);

  const triggerScan = useCallback(async () => {
    try {
      await fetch("/api/v1/scan", { method: "POST" });
    } catch (err) {
      console.error("Failed to trigger scan:", err);
    }
  }, []);

  return { status, refetch, triggerScan };
}

export function useFolders() {
  const [folders, setFolders] = useState<FolderConfig[]>([]);

  const refetch = useCallback(async () => {
    try {
      const res = await fetch("/api/v1/folders");
      setFolders(await res.json());
    } catch (err) {
      console.error("Failed to fetch folders:", err);
    }
  }, []);

  useEffect(() => {
    void (async () => {
      await refetch();
    })();
  }, [refetch]);

  const updateFolder = useCallback(
    async (label: string, config: Partial<FolderConfig>) => {
      await fetch(`/api/v1/folders/${label}`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(config),
      });
      refetch();
    },
    [refetch],
  );

  return { folders, updateFolder, refetch };
}

export function usePersistentPaths(folderLabel: string) {
  const [paths, setPaths] = useState<PersistentPath[]>([]);

  const refetch = useCallback(async () => {
    if (!folderLabel) return;
    try {
      const res = await fetch(`/api/v1/folders/${folderLabel}/persistent`);
      setPaths(await res.json());
    } catch (err) {
      console.error("Failed to fetch persistent paths:", err);
    }
  }, [folderLabel]);

  useEffect(() => {
    void (async () => {
      await refetch();
    })();
  }, [refetch]);

  const addPath = useCallback(
    async (pattern: string) => {
      await fetch(`/api/v1/folders/${folderLabel}/persistent`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ pattern }),
      });
      refetch();
    },
    [folderLabel, refetch],
  );

  const deletePath = useCallback(
    async (id: number) => {
      await fetch(`/api/v1/folders/${folderLabel}/persistent/${id}`, {
        method: "DELETE",
      });
      refetch();
    },
    [folderLabel, refetch],
  );

  return { paths, addPath, deletePath, refetch };
}
