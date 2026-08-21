import { useCallback, useEffect, useRef, useState } from "react";
import { formatCount, formatDate, formatSize } from "../lib/format";
import { useFileWorkspace, type FilterKey } from "../hooks/useFileWorkspace";
import type { FileRecord } from "../types/api";
import { FilterBar } from "./FilterBar";
import { Lightbox } from "./Lightbox";
import { PinToggle } from "./PinToggle";
import { SelectionBar } from "./SelectionBar";

interface FileBrowserProps {
  folder: string;
  filterKey: FilterKey;
  onFilterChange(key: FilterKey): void;
  onFolderChanged(): void;
}

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
}: FileBrowserProps) {
  const workspace = useFileWorkspace({ folder, filterKey, sort: "name", onFolderChanged });
  const { files: list, selection, actions, preset, filters } = workspace;
  const { files, loading, error, hasMore, loadMore, refetchIds } = list;

  const observer = useRef<IntersectionObserver | null>(null);
  const headerCheckbox = useRef<HTMLInputElement>(null);
  const [previewIndex, setPreviewIndex] = useState<number | null>(null);

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

  if (!folder) {
    return (
      <div className="flex items-center justify-center h-full text-neutral-500">
        Select a folder from the sidebar
      </div>
    );
  }

  return (
    <div className="p-4 md:p-6 pb-24">
      {/* ml-11 clears the fixed sidebar toggle, which is md:hidden and sat on
          top of the first few characters of the folder name at 390px. */}
      <h2 className="text-xl font-semibold mb-3 ml-11 md:ml-0">{folder}</h2>
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
                conflictMode={preset.key === "conflicts"}
                onToggle={() => selection.toggle(file.id)}
                onToggleRange={() => selection.toggleRange(file.id)}
                onPin={(pinned) => void actions.setPinned(file, pinned)}
                onRestore={() => void actions.restore(file)}
                onArchive={() => void actions.archive(file)}
                onResolve={(keep) => void actions.resolve(file, keep)}
                onDownload={() => actions.download(file)}
                onPreview={() => setPreviewIndex(index)}
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

      <SelectionBar
        selection={selection}
        filters={filters}
        files={files}
        onFinished={onFinished}
      />

      {previewIndex !== null && files[previewIndex] && (
        <Lightbox
          files={files}
          initialIndex={previewIndex}
          onClose={() => setPreviewIndex(null)}
          onLoadMore={loadMore}
          hasMore={hasMore}
          actions={actions}
        />
      )}
    </div>
  );
}

interface RowProps {
  file: FileRecord;
  selected: boolean;
  conflictMode: boolean;
  onToggle(): void;
  onToggleRange(): void;
  onPin(pinned: boolean): void;
  onRestore(): void;
  onArchive(): void;
  onResolve(keep: "device" | "archive"): void;
  onDownload(): void;
  onPreview(): void;
}

function Row({
  file,
  selected,
  conflictMode,
  onToggle,
  onToggleRange,
  onPin,
  onRestore,
  onArchive,
  onResolve,
  onDownload,
  onPreview,
}: RowProps) {
  return (
    <tr
      className={`border-b border-neutral-800/50 hover:bg-neutral-800/30 ${
        selected ? "bg-blue-950/30" : ""
      }`}
      data-file-id={file.id}
      data-selected={selected}
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
        {/* A button rather than a row click, so the checkbox and shift-range
            selection above keep behaving exactly as they did. Documents open
            in this view, not the gallery -- App picks the gallery only for
            folders whose name looks photographic -- so without an opener here
            the document readers are unreachable from the one place documents
            are. */}
        <button
          type="button"
          data-preview-open
          onClick={onPreview}
          className="text-neutral-200 hover:text-white hover:underline text-left"
        >
          {file.filename}
        </button>
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
