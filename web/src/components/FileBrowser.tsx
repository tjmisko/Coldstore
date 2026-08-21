import { useCallback, useEffect, useRef, useState } from "react";
import { formatCount, formatDate, formatSize } from "../lib/format";
import { useBulkActions } from "../hooks/useBulkActions";
import { useFileWorkspace, type FilterKey } from "../hooks/useFileWorkspace";
import { useGridKeymap } from "../hooks/useGridKeymap";
import { useViewport } from "../hooks/useViewport";
import type { FileRecord } from "../types/api";
import { ContextMenu, type ContextMenuTarget } from "./ContextMenu";
import { FilterBar } from "./FilterBar";
import { PinToggle } from "./PinToggle";
import { SelectionBar } from "./SelectionBar";

interface FileBrowserProps {
  folder: string;
  filterKey: FilterKey;
  onFilterChange(key: FilterKey): void;
  onFolderChanged(): void;
  reveal?: { fileID: number; token: number } | null;
}

/** A table row is about this tall, and <C-d> needs to know. Measured rather
 *  than guessed would be better; measured costs a ResizeObserver on a list that
 *  can be five hundred rows long, and being one row out on a half-page scroll
 *  is not a defect anyone will notice. */
const ROW_HEIGHT = 37;

function statusColor(status: string): string {
  switch (status) {
    case "synced":
      return "text-emerald-400";
    case "archived":
      return "text-amber-400";
    case "restoring":
    case "archiving":
      return "text-blue-400";
    case "missing":
      return "text-red-400";
    default:
      return "text-neutral-400";
  }
}

export function FileBrowser({
  folder,
  filterKey,
  onFilterChange,
  onFolderChanged,
  reveal = null,
}: FileBrowserProps) {
  const workspace = useFileWorkspace({ folder, filterKey, sort: "name", onFolderChanged });
  const { files: list, selection, actions, preset, filters } = workspace;
  const { files, loading, error, hasMore, loadMore, refetchIds } = list;

  const observer = useRef<IntersectionObserver | null>(null);
  const headerCheckbox = useRef<HTMLInputElement>(null);
  const [menu, setMenu] = useState<ContextMenuTarget | null>(null);

  const sentinelRef = useCallback(
    (node: HTMLDivElement | null) => {
      observer.current?.disconnect();
      if (!node) return;
      observer.current = new IntersectionObserver(
        (entries) => {
          if (entries[0].isIntersecting && hasMore && !loading) loadMore();
        },
        { rootMargin: "200px" },
      );
      observer.current.observe(node);
    },
    [hasMore, loading, loadMore],
  );

  const loadedSelected = files.filter((f) => selection.isSelected(f.id)).length;
  const allLoadedSelected = files.length > 0 && loadedSelected === files.length;

  // indeterminate has no HTML attribute; it is a DOM property, so a
  // partially-selected header checkbox can only be set through a ref.
  useEffect(() => {
    if (headerCheckbox.current) {
      headerCheckbox.current.indeterminate = loadedSelected > 0 && !allLoadedSelected;
    }
  }, [loadedSelected, allLoadedSelected]);

  const onFinished = useCallback(
    async (ids: number[]) => {
      await refetchIds(ids);
      onFolderChanged();
    },
    [refetchIds, onFolderChanged],
  );

  const bulk = useBulkActions({ selection, filters, onFinished });
  const viewport = useViewport();

  const openMenu = useCallback(
    (index: number, at: { x: number; y: number } | null) => {
      const file = files[index];
      if (!file) return;
      const inSelection = selection.isSelected(file.id) && selection.count > 0;
      setMenu({ file, ids: inSelection ? selection.ids : [file.id], at });
    },
    [files, selection],
  );

  // A table is a grid one column wide, which is the whole reason it gets every
  // motion the gallery has without a second implementation.
  const cursor = useGridKeymap({
    scope: "browser",
    files,
    columns: 1,
    visibleRows: Math.max(1, Math.floor(viewport.height / ROW_HEIGHT)),
    selection,
    actions,
    bulk,
    conflictMode: preset.key === "conflicts",
    onOpen: () => {},
    onContextMenu: (index) => openMenu(index, null),
    active: menu === null,
    reveal,
  });

  if (!folder) {
    return (
      <div className="flex items-center justify-center h-full text-neutral-500">
        Select a folder from the sidebar
      </div>
    );
  }

  return (
    <div className="p-4 md:p-6 pb-24">
      <h2 className="text-xl font-semibold mb-3">{folder}</h2>
      <FilterBar active={filterKey} onChange={onFilterChange} />

      {error && (
        <p className="mb-3 text-sm text-red-400" role="alert">
          {error}
        </p>
      )}

      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="text-left text-neutral-500 border-b border-neutral-800">
              <th className="pb-2 pr-2 w-8">
                <input
                  ref={headerCheckbox}
                  type="checkbox"
                  aria-label="Select all loaded files"
                  data-select-all-loaded
                  checked={allLoadedSelected}
                  onChange={() => {
                    if (allLoadedSelected) selection.clear();
                    else selection.selectMany(files.map((f) => f.id));
                  }}
                  className="accent-blue-500"
                />
              </th>
              <th className="pb-2 pr-2 w-8" title="Pinned files are never archived">
                Pin
              </th>
              <th className="pb-2 pr-4 font-medium">Name</th>
              <th className="pb-2 pr-4 font-medium hidden md:table-cell">Path</th>
              <th className="pb-2 pr-4 font-medium">Size</th>
              <th className="pb-2 pr-4 font-medium hidden sm:table-cell">Modified</th>
              <th className="pb-2 pr-4 font-medium">Status</th>
              <th className="pb-2 font-medium">Actions</th>
            </tr>
          </thead>
          <tbody>
            {files.map((file, index) => (
              <Row
                key={file.id}
                file={file}
                selected={selection.isSelected(file.id)}
                cursor={index === cursor.index}
                conflictMode={preset.key === "conflicts"}
                onFocusRow={() => cursor.setIndex(index)}
                onToggle={() => {
                  cursor.setIndex(index);
                  selection.toggle(file.id);
                }}
                onToggleRange={() => selection.toggleRange(file.id)}
                onPin={(pinned) => void actions.setPinned(file, pinned)}
                onRestore={() => void actions.restore(file)}
                onArchive={() => void actions.archive(file)}
                onResolve={(keep) => void actions.resolve(file, keep)}
                onDownload={() => actions.download(file)}
                onMenu={(at) => {
                  cursor.setIndex(index);
                  openMenu(index, at);
                }}
              />
            ))}
          </tbody>
        </table>
      </div>

      <div ref={sentinelRef} className="h-8" />

      {loading && <div className="text-center text-neutral-500 py-4">Loading...</div>}

      {!loading && files.length === 0 && (
        <div className="text-center text-neutral-500 py-10 text-sm">
          No files match this filter.
        </div>
      )}

      {!hasMore && files.length > 0 && (
        <div className="text-center text-neutral-600 py-4 text-sm">
          {formatCount(files.length)} files
        </div>
      )}

      {menu && (
        <ContextMenu
          target={menu}
          conflictMode={preset.key === "conflicts"}
          actions={actions}
          bulk={bulk}
          onOpen={() => actions.download(menu.file)}
          onClose={() => setMenu(null)}
        />
      )}

      <SelectionBar selection={selection} files={files} bulk={bulk} />
    </div>
  );
}

interface RowProps {
  file: FileRecord;
  selected: boolean;
  cursor: boolean;
  conflictMode: boolean;
  onFocusRow(): void;
  onToggle(): void;
  onToggleRange(): void;
  onPin(pinned: boolean): void;
  onRestore(): void;
  onArchive(): void;
  onResolve(keep: "device" | "archive"): void;
  onDownload(): void;
  onMenu(at: { x: number; y: number }): void;
}

function Row({
  file,
  selected,
  cursor,
  conflictMode,
  onFocusRow,
  onToggle,
  onToggleRange,
  onPin,
  onRestore,
  onArchive,
  onResolve,
  onDownload,
  onMenu,
}: RowProps) {
  return (
    <tr
      // tabIndex -1, not 0: five hundred rows in the tab order is its own
      // accessibility failure. The row is entered with j/k or with a click, and
      // Tab still walks the controls inside it.
      tabIndex={-1}
      data-file-id={file.id}
      data-selected={selected}
      data-cursor={cursor}
      data-cursor-scope="browser"
      onClick={onFocusRow}
      onContextMenu={(e) => {
        e.preventDefault();
        onMenu({ x: e.clientX, y: e.clientY });
      }}
      className={`border-b border-neutral-800/50 hover:bg-neutral-800/30 outline-none ${
        selected ? "bg-blue-950/30" : ""
      } ${cursor ? "ring-1 ring-inset ring-neutral-500" : ""}`}
    >
      <td className="py-2 pr-2">
        <input
          type="checkbox"
          checked={selected}
          aria-label={`Select ${file.filename}`}
          onClick={(e) => {
            if (e.shiftKey) {
              e.preventDefault();
              onToggleRange();
            }
          }}
          onChange={(e) => {
            if (e.nativeEvent instanceof MouseEvent && e.nativeEvent.shiftKey) return;
            onToggle();
          }}
          className="accent-blue-500"
        />
      </td>
      <td className="py-2 pr-2">
        <PinToggle file={file} onPin={onPin} />
      </td>
      <td className="py-2 pr-4">
        <span className="text-neutral-200">{file.filename}</span>
        {file.thumb_attempts >= 3 && (
          <span
            className="ml-1 text-amber-400"
            title={file.thumb_error || "thumbnail generation failed"}
          >
            &#9888;
          </span>
        )}
      </td>
      <td className="py-2 pr-4 text-neutral-500 hidden md:table-cell text-xs">
        {file.relative_path}
      </td>
      <td className="py-2 pr-4 text-neutral-400 whitespace-nowrap">
        {formatSize(file.size_bytes)}
      </td>
      <td className="py-2 pr-4 text-neutral-400 hidden sm:table-cell whitespace-nowrap">
        {formatDate(file.file_mtime)}
      </td>
      <td className={`py-2 pr-4 ${statusColor(file.status)}`}>{file.status}</td>
      <td className="py-2">
        <div className="flex gap-1 flex-wrap">
          <button
            onClick={onDownload}
            className="px-2 py-1 text-xs rounded bg-blue-600 hover:bg-blue-500"
          >
            Download
          </button>
          {file.status === "archived" && !conflictMode && (
            <button
              onClick={onRestore}
              className="px-2 py-1 text-xs rounded bg-emerald-600 hover:bg-emerald-500"
            >
              Restore
            </button>
          )}
          {file.status === "synced" && (
            <button
              onClick={onArchive}
              className="px-2 py-1 text-xs rounded bg-amber-600 hover:bg-amber-500"
            >
              Archive
            </button>
          )}
          {conflictMode && (
            <>
              <button
                onClick={() => onResolve("device")}
                className="px-2 py-1 text-xs rounded bg-emerald-700 hover:bg-emerald-600"
              >
                Keep device copy
              </button>
              <button
                onClick={() => onResolve("archive")}
                className="px-2 py-1 text-xs rounded bg-amber-700 hover:bg-amber-600"
              >
                Keep archived copy
              </button>
            </>
          )}
        </div>
      </td>
    </tr>
  );
}
