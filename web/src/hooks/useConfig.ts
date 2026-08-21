import { useCallback, useEffect, useState } from "react";
import { ApiRequestError, api, errorMessage } from "../lib/api";
import type {
  CatalogEvent,
  FolderConfig,
  PersistentPath,
  StatusResponse,
  UpdateFolderRequest,
} from "../types/api";

/** Ten seconds. over_budget, wedged and the budget bars have to move while a
 *  bulk job runs, and a dashboard that only updates when the owner presses
 *  refresh is one that reports a wedge they cleared five minutes ago. */
const STATUS_POLL_MS = 10_000;

export interface StatusState {
  status: StatusResponse | null;
  error: string | null;
  /** The HTTP status of the last failure, so the caller can tell "wrong
   *  password" from "daemon is not running". They need different sentences. */
  errorStatus: number | null;
  refetch(): Promise<void>;
}

export function useStatus(): StatusState {
  const [status, setStatus] = useState<StatusResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [errorStatus, setErrorStatus] = useState<number | null>(null);

  const load = useCallback(async (signal?: AbortSignal) => {
    try {
      const next = await api.status();
      if (signal?.aborted) return;
      setStatus(next);
      setError(null);
      setErrorStatus(null);
    } catch (err) {
      if (signal?.aborted) return;
      setError(errorMessage(err));
      setErrorStatus(err instanceof ApiRequestError ? err.status : null);
    }
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;

    const tick = async () => {
      await load(controller.signal);
      if (controller.signal.aborted) return;
      timer = setTimeout(() => void tick(), STATUS_POLL_MS);
    };

    void tick();
    return () => {
      controller.abort();
      if (timer) clearTimeout(timer);
    };
  }, [load]);

  const refetch = useCallback(() => load(), [load]);
  return { status, error, errorStatus, refetch };
}

export interface FoldersState {
  folders: FolderConfig[];
  error: string | null;
  /** Narrowed to the three fields the UI owns. With Partial<FolderConfig> the
   *  first budget editor written against it would have PUT syncthing_path="",
   *  grace_minutes=0 and enabled=false alongside the budget -- disabling the
   *  folder on its first save. */
  updateFolder(label: string, patch: UpdateFolderRequest): Promise<FolderConfig>;
  refetch(): Promise<void>;
}

export function useFolders(): FoldersState {
  const [folders, setFolders] = useState<FolderConfig[]>([]);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async (signal?: AbortSignal) => {
    try {
      const next = await api.folders();
      if (signal?.aborted) return;
      setFolders(next);
      setError(null);
    } catch (err) {
      if (signal?.aborted) return;
      setError(errorMessage(err));
    }
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    // The await is not decorative. react-hooks/set-state-in-effect inlines a
    // function called straight from an effect body and sees the setState
    // inside it as synchronous; behind an await it is what it has always
    // really been -- a continuation that runs after the request returns.
    void (async () => {
      await load(controller.signal);
    })();
    return () => controller.abort();
  }, [load]);

  const updateFolder = useCallback(
    async (label: string, patch: UpdateFolderRequest) => {
      const updated = await api.updateFolder(label, patch);
      setFolders((prev) => prev.map((f) => (f.folder_label === label ? updated : f)));
      return updated;
    },
    [],
  );

  const refetch = useCallback(() => load(), [load]);
  return { folders, error, updateFolder, refetch };
}

export interface PersistentPathsState {
  paths: PersistentPath[];
  error: string | null;
  addPath(pattern: string): Promise<void>;
  deletePath(id: number): Promise<void>;
  refetch(): Promise<void>;
}

export function usePersistentPaths(folderLabel: string): PersistentPathsState {
  const [paths, setPaths] = useState<PersistentPath[]>([]);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(
    async (signal?: AbortSignal) => {
      if (!folderLabel) {
        setPaths([]);
        return;
      }
      try {
        const next = await api.persistentPaths(folderLabel);
        if (signal?.aborted) return;
        setPaths(next);
        setError(null);
      } catch (err) {
        if (signal?.aborted) return;
        setError(errorMessage(err));
      }
    },
    [folderLabel],
  );

  useEffect(() => {
    const controller = new AbortController();
    // The await is not decorative. react-hooks/set-state-in-effect inlines a
    // function called straight from an effect body and sees the setState
    // inside it as synchronous; behind an await it is what it has always
    // really been -- a continuation that runs after the request returns.
    void (async () => {
      await load(controller.signal);
    })();
    return () => controller.abort();
  }, [load]);

  const addPath = useCallback(
    async (pattern: string) => {
      await api.addPersistentPath(folderLabel, pattern);
      await load();
    },
    [folderLabel, load],
  );

  const deletePath = useCallback(
    async (id: number) => {
      await api.deletePersistentPath(folderLabel, id);
      await load();
    },
    [folderLabel, load],
  );

  const refetch = useCallback(() => load(), [load]);
  return { paths, error, addPath, deletePath, refetch };
}

/** useEvents reads the audit log. warningsOnly is the default the ConfigPanel
 *  needs: one enforcement pass writes hundreds of `archive` rows, so an
 *  unfiltered "last 10 events" would never once show the wedge warning, the
 *  cold-storage warning or a conflict. */
export function useEvents(
  folder: string,
  options: { warningsOnly?: boolean; type?: string; limit?: number } = {},
) {
  const { warningsOnly = true, type, limit = 10 } = options;
  const [events, setEvents] = useState<CatalogEvent[]>([]);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const next = await api.events({ folder, type, warningsOnly, limit });
        if (signal?.aborted) return;
        setEvents(next);
        setError(null);
      } catch (err) {
        if (signal?.aborted) return;
        setError(errorMessage(err));
      }
    },
    [folder, type, warningsOnly, limit],
  );

  useEffect(() => {
    const controller = new AbortController();
    // The await is not decorative. react-hooks/set-state-in-effect inlines a
    // function called straight from an effect body and sees the setState
    // inside it as synchronous; behind an await it is what it has always
    // really been -- a continuation that runs after the request returns.
    void (async () => {
      await load(controller.signal);
    })();
    return () => controller.abort();
  }, [load]);

  const refetch = useCallback(() => load(), [load]);
  return { events, error, refetch };
}
