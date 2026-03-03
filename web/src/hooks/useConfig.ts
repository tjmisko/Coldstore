import { useState, useEffect, useCallback } from "react";
import type { FolderConfig, PersistentPath, StatusResponse } from "../types/api";

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
    refetch();
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
    refetch();
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
    refetch();
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
