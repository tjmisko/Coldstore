import { useCallback, useState } from "react";
import { api, errorMessage, type FileFilters, type NearFilter } from "../lib/api";
import { useToast } from "../lib/toast";
import { useFiles, type UseFiles } from "./useFiles";
import { useSelection, type Selection } from "./useSelection";
import type { FileRecord, FileSort, TagUndo } from "../types/api";

/** The file-review filters, including the capture-date queue from tagging design 3.2.
 *
 *  The two pin filters are not a nicety. A restore pins silently and
 *  permanently and nothing ever releases that pin, so a folder's long-run
 *  destination under ordinary use is wedged. Nine months in, "Pinned" is four
 *  thousand identical badges of which perhaps one gigabyte was intentional,
 *  and the fastest-looking remedy -- select all, Unpin -- is the one that
 *  releases the favourites the owner deliberately protected. pin_source is
 *  what keeps those two piles apart. */
export type FilterKey =
  | "all"
  | "synced"
  | "archived"
  | "pinned-user"
  | "pinned-auto"
  | "untagged"
  | "undated"
  | "conflicts";

export interface FilterPreset {
  key: FilterKey;
  label: string;
  hint?: string;
  filters: FileFilters;
  sort?: FileSort;
}

export const FILTER_PRESETS: FilterPreset[] = [
  { key: "all", label: "All", filters: {} },
  { key: "synced", label: "Synced", filters: { status: "synced" } },
  { key: "archived", label: "Archived", filters: { status: "archived" } },
  {
    key: "untagged",
    label: "Untagged",
    hint: "Files with no tags yet — the starting queue for a tagging pass.",
    filters: { untagged: true },
  },
  {
    key: "pinned-user",
    label: "Pinned by me",
    hint: "Files you pinned on purpose. Coldstore never archives these.",
    filters: { pinned: true, pinSource: "user" },
  },
  {
    key: "pinned-auto",
    label: "Auto-pinned on restore",
    hint: "Every restore pins the file it brings back, and nothing releases that pin. Oldest first.",
    filters: { pinned: true, pinSource: "restore" },
    sort: "pinned_at",
  },
  {
    key: "undated",
    label: "No capture date",
    hint: "Files whose metadata was scanned but did not contain a capture date.",
    filters: { undated: true },
  },
  {
    key: "conflicts",
    label: "Conflicts",
    hint: "A file came back at the path of a record coldstore had archived. Choose which copy to keep.",
    filters: { status: "archived", conflict: true },
  },
];

export function filterPreset(key: FilterKey): FilterPreset {
  return FILTER_PRESETS.find((p) => p.key === key) ?? FILTER_PRESETS[0];
}

export interface FileActions {
  setPinned(file: FileRecord, pinned: boolean): Promise<void>;
  restore(file: FileRecord): Promise<void>;
  archive(file: FileRecord): Promise<void>;
  resolve(file: FileRecord, keep: "device" | "archive"): Promise<void>;
  download(file: FileRecord): void;
  setDescription(file: FileRecord, description: string): Promise<boolean>;
  setCaptureDate(file: FileRecord, capturedLocal: string): Promise<boolean>;
  setLocation(file: FileRecord, lat: number, lon: number): Promise<boolean>;
  tagFiles(ids: number[], add: string[], remove: string[]): Promise<boolean>;
}

export interface FileWorkspace {
  preset: FilterPreset;
  files: UseFiles;
  selection: Selection;
  actions: FileActions;
  /** The filters currently in force, for "select all in folder". */
  filters: FileFilters;
  /** Held for the keyboard undo slice. This UI deliberately does not bind U. */
  lastTagUndo: TagUndo | null;
}

interface Options {
  folder: string;
  filterKey: FilterKey;
  sort?: FileSort;
  limit?: number;
  near?: NearFilter;
  /** Called after any action that can move bytes or change a pin, so the
   *  budget bars and the wedge banner redraw without waiting for the poll. */
  onFolderChanged?: () => void;
}

/** useFileWorkspace is everything Gallery and FileBrowser share: the same
 *  filters, the same selection semantics, the same per-file actions, and the
 *  same repaint rules afterwards. Two implementations of "restore this row"
 *  is how one of them ends up without the res.ok check. */
export function useFileWorkspace(options: Options): FileWorkspace {
  const { folder, filterKey, sort, limit, near, onFolderChanged } = options;
  const preset = filterPreset(filterKey);
  const toast = useToast();
  const [lastTagUndo, setLastTagUndo] = useState<TagUndo | null>(null);

  const files = useFiles({
    folder,
    ...preset.filters,
    near,
    sort: preset.sort ?? sort,
    limit,
  });
  const selection = useSelection(files.files.map((f) => f.id));

  const { patchFile, refetch, refetchIds } = files;

  const setPinned = useCallback(
    async (file: FileRecord, pinned: boolean) => {
      try {
        const res = await api.pin(file.id, pinned);
        patchFile(file.id, res.file);
        if (res.warning) toast.show("info", res.warning);
        onFolderChanged?.();
      } catch (err) {
        toast.show("error", `${file.filename}: ${errorMessage(err)}`);
      }
    },
    [patchFile, toast, onFolderChanged],
  );

  const restore = useCallback(
    async (file: FileRecord) => {
      try {
        // Through apiFetch, and the returned record is written back. The old
        // handler awaited fetch() with no res.ok check and no repaint, so a
        // failed restore looked identical to a successful one and a
        // successful one left the row reading "archived" with its Restore
        // button still showing.
        const updated = await api.restore(file.id);
        patchFile(file.id, updated);
        toast.show("success", `Restored ${file.filename}`);
        onFolderChanged?.();
      } catch (err) {
        toast.show("error", `${file.filename}: ${errorMessage(err)}`);
      }
    },
    [patchFile, toast, onFolderChanged],
  );

  const archive = useCallback(
    async (file: FileRecord) => {
      try {
        const updated = await api.archive(file.id);
        patchFile(file.id, updated);
        toast.show("success", `Archived ${file.filename}`);
        onFolderChanged?.();
      } catch (err) {
        toast.show("error", `${file.filename}: ${errorMessage(err)}`);
      }
    },
    [patchFile, toast, onFolderChanged],
  );

  const resolve = useCallback(
    async (file: FileRecord, keep: "device" | "archive") => {
      try {
        await api.resolve(file.id, keep);
        toast.show(
          "success",
          keep === "device"
            ? `Kept the copy on your device; the cold-storage copy of ${file.filename} was deleted`
            : `Kept the archived copy; the device copy of ${file.filename} was renamed aside`,
        );
        // A resolved row leaves the Conflicts filter, so this is a reload
        // rather than a patch: the list it belonged to no longer contains it.
        await refetch();
        onFolderChanged?.();
      } catch (err) {
        toast.show("error", `${file.filename}: ${errorMessage(err)}`);
      }
    },
    [refetch, toast, onFolderChanged],
  );

  const download = useCallback((file: FileRecord) => {
    window.open(api.downloadUrl(file.id), "_blank", "noopener");
  }, []);

  const setDescription = useCallback(
    async (file: FileRecord, description: string): Promise<boolean> => {
      try {
        const updated = await api.updateFile(file.id, { description });
        patchFile(file.id, updated);
        return true;
      } catch (err) {
        toast.show("error", `${file.filename}: ${errorMessage(err)}`);
        return false;
      }
    },
    [patchFile, toast],
  );

  const setCaptureDate = useCallback(
    async (file: FileRecord, capturedLocal: string): Promise<boolean> => {
      try {
        const updated = await api.updateFile(file.id, { captured_local: capturedLocal });
        patchFile(file.id, updated);
        return true;
      } catch (err) {
        toast.show("error", `${file.filename}: ${errorMessage(err)}`);
        return false;
      }
    },
    [patchFile, toast],
  );

  const setLocation = useCallback(
    async (file: FileRecord, lat: number, lon: number): Promise<boolean> => {
      try {
        const updated = await api.updateFile(file.id, { gps_lat: lat, gps_lon: lon });
        // A coordinate edit can move the row into or out of the exact circle.
        // Re-run that filter instead of leaving a stale row in the result.
        if (near) await refetch();
        else patchFile(file.id, updated);
        return true;
      } catch (err) {
        toast.show("error", `${file.filename}: ${errorMessage(err)}`);
        return false;
      }
    },
    [near, patchFile, refetch, toast],
  );

  const tagFiles = useCallback(
    async (ids: number[], add: string[], remove: string[]): Promise<boolean> => {
      try {
        const result = await api.bulkTags({ ids, add, remove });
        setLastTagUndo(result.undo);
        await refetchIds(ids);
        const verb = add.length > 0 ? "Tagged" : "Removed tags from";
        const created = result.created.length > 0
          ? ` · created ${result.created.join(", ")}`
          : "";
        toast.show(
          "success",
          `${verb} ${result.updated.toLocaleString()} file${result.updated === 1 ? "" : "s"}${created}`,
        );
        return true;
      } catch (err) {
        toast.show("error", `Tags: ${errorMessage(err)}`);
        return false;
      }
    },
    [refetchIds, toast],
  );

  return {
    preset,
    files,
    selection,
    actions: {
      setPinned, restore, archive, resolve, download, setDescription, setCaptureDate, setLocation,
      tagFiles,
    },
    filters: { folder, ...preset.filters, near },
    lastTagUndo,
  };
}
