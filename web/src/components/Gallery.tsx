import { useCallback, useRef, useState } from "react";
import { api } from "../lib/api";
import { formatCount, formatSize } from "../lib/format";
import { useBulkActions } from "../hooks/useBulkActions";
import { useFileWorkspace, type FilterKey } from "../hooks/useFileWorkspace";
import { useGridKeymap } from "../hooks/useGridKeymap";
import { galleryColumns, useViewport } from "../hooks/useViewport";
import type { FileRecord } from "../types/api";
import { ContextMenu, type ContextMenuTarget } from "./ContextMenu";
import { FilterBar } from "./FilterBar";
import { Lightbox } from "./Lightbox";
import { PinToggle } from "./PinToggle";
import { SelectionBar } from "./SelectionBar";

interface GalleryProps {
  folder: string;
  filterKey: FilterKey;
  onFilterChange(key: FilterKey): void;
  onFolderChanged(): void;
  /** A file the finder asked to be revealed. */
  reveal?: { fileID: number; token: number } | null;
}

export function Gallery({ folder, filterKey, onFilterChange, onFolderChanged, reveal = null }: GalleryProps) {
  const workspace = useFileWorkspace({ folder, filterKey, sort: "mtime", onFolderChanged });
  const { files: list, selection, actions, preset, filters } = workspace;
  const { files, loading, error, hasMore, loadMore, refetchIds } = list;

  const [lightboxIndex, setLightboxIndex] = useState<number | null>(null);
  const [menu, setMenu] = useState<ContextMenuTarget | null>(null);
  const observer = useRef<IntersectionObserver | null>(null);

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

  const onFinished = useCallback(
    async (ids: number[]) => {
      await refetchIds(ids);
      onFolderChanged();
    },
    [refetchIds, onFolderChanged],
  );

  const bulk = useBulkActions({ selection, filters, onFinished });

  const viewport = useViewport();
  const columns = galleryColumns(viewport.width);
  // The sidebar is 288px from md up, and the grid has p-6 plus a 8px gap.
  const gridWidth = Math.max(200, viewport.width - (viewport.width >= 768 ? 288 : 0) - 56);
  const tile = gridWidth / columns;
  const visibleRows = Math.max(1, Math.floor(viewport.height / (tile + 8)));

  const openMenu = useCallback(
    (index: number, at: { x: number; y: number } | null) => {
      const file = files[index];
      if (!file) return;
      // Right-clicking INSIDE a selection acts on the whole selection; outside
      // one it acts on the file clicked, and does not disturb the selection.
      const inSelection = selection.isSelected(file.id) && selection.count > 0;
      setMenu({ file, ids: inSelection ? selection.ids : [file.id], at });
    },
    [files, selection],
  );

  const cursor = useGridKeymap({
    scope: "gallery",
    files,
    columns,
    visibleRows,
    selection,
    actions,
    bulk,
    conflictMode: preset.key === "conflicts",
    onOpen: setLightboxIndex,
    onContextMenu: (index) => openMenu(index, null),
    active: lightboxIndex === null && menu === null,
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

      <div className="grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 gap-2 select-none">
        {files.map((file, index) => (
          <Tile
            key={file.id}
            file={file}
            selected={selection.isSelected(file.id)}
            anySelected={selection.count > 0}
            cursor={index === cursor.index}
            conflictMode={preset.key === "conflicts"}
            onOpen={() => {
              cursor.setIndex(index);
              setLightboxIndex(index);
            }}
            onToggle={() => {
              cursor.setIndex(index);
              selection.toggle(file.id);
            }}
            onToggleRange={() => selection.toggleRange(file.id)}
            onPin={(pinned) => void actions.setPinned(file, pinned)}
            onResolve={(keep) => void actions.resolve(file, keep)}
            onMenu={(at) => {
              cursor.setIndex(index);
              openMenu(index, at);
            }}
          />
        ))}
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

      {lightboxIndex !== null && files[lightboxIndex] && (
        <Lightbox
          files={files}
          initialIndex={lightboxIndex}
          onClose={() => setLightboxIndex(null)}
          onLoadMore={loadMore}
          hasMore={hasMore}
          actions={actions}
          bulk={bulk}
        />
      )}

      {menu && (
        <ContextMenu
          target={menu}
          conflictMode={preset.key === "conflicts"}
          actions={actions}
          bulk={bulk}
          onOpen={() => setLightboxIndex(files.findIndex((f) => f.id === menu.file.id))}
          onClose={() => setMenu(null)}
        />
      )}

      <SelectionBar selection={selection} files={files} bulk={bulk} />
    </div>
  );
}

interface TileProps {
  file: FileRecord;
  selected: boolean;
  anySelected: boolean;
  cursor: boolean;
  conflictMode: boolean;
  onOpen(): void;
  onToggle(): void;
  onToggleRange(): void;
  onPin(pinned: boolean): void;
  onResolve(keep: "device" | "archive"): void;
  onMenu(at: { x: number; y: number }): void;
}

/** The tile is a div with role="button", not a <button>.
 *
 *  A checkbox and a pin toggle inside a button are nested interactive
 *  elements: invalid HTML, a React warning, and -- the part the owner notices
 *  -- a click on the checkbox that also opens the lightbox. */
function Tile({
  file,
  selected,
  anySelected,
  cursor,
  conflictMode,
  onOpen,
  onToggle,
  onToggleRange,
  onPin,
  onResolve,
  onMenu,
}: TileProps) {
  const thumbnail = file.thumbnail_path ? api.thumbnailUrl(file.thumbnail_path) : "";
  const thumbFailed = file.thumb_attempts >= 3;

  const handleClick = (e: React.MouseEvent) => {
    if (e.shiftKey) {
      onToggleRange();
      return;
    }
    if (e.metaKey || e.ctrlKey) {
      onToggle();
      return;
    }
    onOpen();
  };

  return (
    <div
      role="button"
      tabIndex={0}
      aria-label={file.filename}
      data-file-id={file.id}
      data-selected={selected}
      data-cursor={cursor}
      data-cursor-scope="gallery"
      onClick={handleClick}
      onContextMenu={(e) => {
        e.preventDefault();
        onMenu({ x: e.clientX, y: e.clientY });
      }}
      className={`relative aspect-square bg-neutral-800 rounded-lg overflow-hidden group cursor-pointer focus:outline-none focus:ring-2 focus:ring-blue-500 ${
        selected ? "ring-2 ring-blue-500" : ""
      } ${cursor && !selected ? "ring-1 ring-neutral-400" : ""}`}
    >
      {thumbnail ? (
        <img
          src={thumbnail}
          alt={file.filename}
          className="w-full h-full object-cover"
          loading="lazy"
        />
      ) : (
        <div className="w-full h-full flex flex-col items-center justify-center text-neutral-500 text-xs gap-1 p-2 text-center">
          {thumbFailed ? (
            <span title={file.thumb_error || "thumbnail generation failed"} aria-label="thumbnail failed">
              &#9888;
            </span>
          ) : null}
          <span className="truncate w-full">{file.extension || file.filename}</span>
        </div>
      )}

      <label
        className={`absolute top-1 left-1 p-1 rounded bg-black/50 transition-opacity ${
          selected || anySelected ? "opacity-100" : "opacity-0 group-hover:opacity-100 focus-within:opacity-100"
        }`}
        onClick={(e) => e.stopPropagation()}
      >
        <input
          type="checkbox"
          checked={selected}
          aria-label={`Select ${file.filename}`}
          onClick={(e) => {
            e.stopPropagation();
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
      </label>

      <div className="absolute top-1 right-1" onClick={(e) => e.stopPropagation()}>
        <PinToggle file={file} onPin={onPin} compact />
      </div>

      <div className="absolute bottom-1 right-1 flex gap-1">
        {file.pinned && (
          <span className="bg-indigo-600/85 text-[10px] px-1.5 py-0.5 rounded-full" data-badge="pinned">
            pinned
          </span>
        )}
        {file.status === "archived" && (
          <span className="bg-amber-600/85 text-[10px] px-1.5 py-0.5 rounded-full" data-badge="archived">
            archived
          </span>
        )}
        {file.status === "missing" && (
          <span className="bg-red-700/85 text-[10px] px-1.5 py-0.5 rounded-full" data-badge="missing">
            missing
          </span>
        )}
      </div>

      {conflictMode && (
        <div
          className="absolute inset-x-0 bottom-0 bg-black/80 p-1.5 flex gap-1 text-[11px]"
          onClick={(e) => e.stopPropagation()}
        >
          <button
            onClick={() => onResolve("device")}
            className="flex-1 px-1 py-0.5 rounded bg-emerald-700 hover:bg-emerald-600"
          >
            Keep device
          </button>
          <button
            onClick={() => onResolve("archive")}
            className="flex-1 px-1 py-0.5 rounded bg-amber-700 hover:bg-amber-600"
          >
            Keep archived
          </button>
        </div>
      )}

      <div className="absolute inset-0 bg-black/0 group-hover:bg-black/20 transition-colors pointer-events-none" />
      <span className="sr-only">{formatSize(file.size_bytes)}</span>
    </div>
  );
}
