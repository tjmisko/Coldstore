import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { formatCount, formatDate, formatSize } from "../lib/format";
import { useBulkActions } from "../hooks/useBulkActions";
import { useFileWorkspace, type FilterKey } from "../hooks/useFileWorkspace";
import { useGridKeymap } from "../hooks/useGridKeymap";
import { useViewport } from "../hooks/useViewport";
import type { FileRecord } from "../types/api";
import type { FileReveal } from "../types/navigation";
import { ContextMenu, type ContextMenuTarget } from "./ContextMenu";
import { FilterBar } from "./FilterBar";
import { Lightbox } from "./Lightbox";
import { PinToggle } from "./PinToggle";
import { SelectionBar } from "./SelectionBar";

interface FileBrowserProps {
  folder: string;
  filterKey: FilterKey;
  onFilterChange(key: FilterKey): void;
  onFolderChanged(): void;
  reveal?: FileReveal | null;
  onShowInFiles?(file: FileRecord): void;
}

/** A table row is about this tall, and <C-d> needs to know. Measured rather
 *  than guessed would be better; measured costs a ResizeObserver on a list that
 *  can be five hundred rows long, and being one row out on a half-page scroll
 *  is not a defect anyone will notice. */
const ROW_HEIGHT = 37;

function statusColor(status: string): string {
  switch (status) {
    case "synced":
      return "text-ink-muted";
    case "archived":
      return "text-cold";
    case "restoring":
    case "archiving":
      return "text-ink";
    case "missing":
      return "text-alarm";
    default:
      return "text-ink-muted";
  }
}

/* Row actions are monochrome and borderless. These two buttons repeat on every
   row, so a colour here says nothing and thirty outlines are still noise; the
   palette is spent on the status column instead, where it varies. `cold` is the
   one exception -- keeping the archived copy of a conflict IS a state choice. */
const ROW_ACTION = {
  neutral:
    "px-2 py-1 text-meta rounded-md bg-transparent text-ink-muted transition-colors hover:bg-raised-hover hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor",
  cold: "px-2 py-1 text-meta rounded-md bg-transparent text-cold transition-colors hover:bg-cold-dim focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor",
};

/* The native box is kept in the DOM and merely hidden, so getByRole,
   toBeChecked, the indeterminate property and the tab order all keep working;
   only the appearance is ours, drawn at a size a thumb can hit. */
const CHECKBOX_BOX =
  "block size-[18px] rounded border-2 border-ink-faint bg-transparent transition-colors peer-checked:border-ink peer-checked:bg-ink peer-checked:[&_svg]:opacity-100 peer-indeterminate:border-ink peer-indeterminate:bg-ink peer-indeterminate:[&_svg]:opacity-100 peer-focus-visible:ring-2 peer-focus-visible:ring-cursor peer-focus-visible:ring-offset-1 peer-focus-visible:ring-offset-ground";

export function FileBrowser({
  folder,
  filterKey,
  onFilterChange,
  onFolderChanged,
  reveal = null,
  onShowInFiles = () => {},
}: FileBrowserProps) {
  const workspace = useFileWorkspace({
    folder,
    filterKey,
    sort: "name",
    onFolderChanged,
  });
  const { files: list, selection, actions, preset, filters } = workspace;
  const { files: loadedFiles, loading, error, hasMore, loadMore, refetchIds } = list;

  // Finder results carry their complete record. If the target is beyond the
  // initial 60-row page, append it until normal paging reaches it so the grid
  // can render, focus, and scroll it instead of silently dropping the request.
  const files = useMemo(() => {
    const target = reveal?.file;
    if (
      !target ||
      loading ||
      filterKey !== "all" ||
      target.folder_label !== folder ||
      loadedFiles.some((file) => file.id === target.id)
    ) {
      return loadedFiles;
    }
    return [...loadedFiles, target];
  }, [filterKey, folder, loadedFiles, loading, reveal]);

  const observer = useRef<IntersectionObserver | null>(null);
  const headerCheckbox = useRef<HTMLInputElement>(null);
  const [menu, setMenu] = useState<ContextMenuTarget | null>(null);
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
      headerCheckbox.current.indeterminate =
        loadedSelected > 0 && !allLoadedSelected;
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
    onOpen: setPreviewIndex,
    onContextMenu: (index) => openMenu(index, null),
    active: previewIndex === null && menu === null,
    reveal,
  });

  if (!folder) {
    return (
      <div className="flex items-center justify-center h-full text-ink-muted">
        Select a folder from the sidebar
      </div>
    );
  }

  return (
    <div className="p-4 md:p-6 pb-24">
      {/* ml-11 clears the fixed sidebar toggle, which is md:hidden and sat on
          top of the first few characters of the folder name at 390px. Margin,
          not padding: the box has to move for a bounding-box test to see it. */}
      <h2 className="text-title font-medium tracking-tight mb-4 ml-11 md:ml-0">{folder}</h2>
      <FilterBar active={filterKey} onChange={onFilterChange} />

      {error && (
        <p className="mb-3 text-meta text-alarm" role="alert">
          {error}
        </p>
      )}

      <div className="overflow-x-auto">
        <table className="w-full text-base">
          <thead>
            <tr className="text-left text-ink-faint border-b border-line-strong text-micro uppercase tracking-wider">
              <th className="pb-2 pr-2 w-8">
                <label className="inline-flex p-1 cursor-pointer align-middle">
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
                    className="peer sr-only"
                  />
                  <span aria-hidden="true" className={CHECKBOX_BOX}>
                    <svg
                      viewBox="0 0 20 20"
                      className="size-full text-ground opacity-0"
                    >
                      <path
                        d="M5 10.5l3.2 3.2L15 7"
                        fill="none"
                        stroke="currentColor"
                        strokeWidth="2.75"
                        strokeLinecap="round"
                        strokeLinejoin="round"
                      />
                    </svg>
                  </span>
                </label>
              </th>
              <th
                className="pb-2 pr-2 w-8"
                title="Pinned files are never archived"
              >
                Pin
              </th>
              <th className="pb-2 pr-4 font-medium">Name</th>
              <th className="pb-2 pr-4 font-medium hidden md:table-cell">
                Path
              </th>
              <th className="pb-2 pr-4 font-medium">Size</th>
              <th className="pb-2 pr-4 font-medium hidden sm:table-cell">
                Modified
              </th>
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
                onPreview={() => setPreviewIndex(index)}
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

      {loading && (
        <div className="text-center text-ink-faint py-4 text-meta">
          Loading...
        </div>
      )}

      {!loading && files.length === 0 && (
        <div className="text-center text-ink-muted py-12 text-meta">
          No files match this filter.
        </div>
      )}

      {!hasMore && files.length > 0 && (
        <div className="text-center text-ink-faint py-4 text-meta tabular">
          {formatCount(files.length)} files
        </div>
      )}

      {previewIndex !== null && files[previewIndex] && (
        <Lightbox
          files={files}
          initialIndex={previewIndex}
          onClose={() => setPreviewIndex(null)}
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
          onOpen={() => setPreviewIndex(files.findIndex((file) => file.id === menu.file.id))}
          onShowInFiles={() => onShowInFiles(menu.file)}
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
  onPreview(): void;
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
  onPreview,
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
      className={`group border-b border-line/70 hover:bg-raised/50 outline-none ${
        selected ? "bg-ink/[0.055]" : ""
      } ${cursor ? "ring-2 ring-inset ring-cursor" : ""}`}
    >
      <td className="py-2.5 pr-2">
        <label className="inline-flex p-1 cursor-pointer align-middle">
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
              if (e.nativeEvent instanceof MouseEvent && e.nativeEvent.shiftKey)
                return;
              onToggle();
            }}
            className="peer sr-only"
          />
          <span aria-hidden="true" className={CHECKBOX_BOX}>
            <svg
              viewBox="0 0 20 20"
              className="size-full text-ground opacity-0"
            >
              <path
                d="M5 10.5l3.2 3.2L15 7"
                fill="none"
                stroke="currentColor"
                strokeWidth="2.75"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          </span>
        </label>
      </td>
      <td className="py-2.5 pr-2">
        <PinToggle file={file} onPin={onPin} />
      </td>
      <td className="py-2.5 pr-4">
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
          className="text-ink hover:underline text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
        >
          {file.filename}
        </button>
        {file.thumb_attempts >= 3 && (
          <span
            className="ml-1.5 text-alarm"
            title={file.thumb_error || "thumbnail generation failed"}
          >
            &#9888;
          </span>
        )}
      </td>
      <td className="py-2.5 pr-4 text-ink-faint hidden md:table-cell text-meta font-mono">
        {file.relative_path}
      </td>
      <td className="py-2.5 pr-4 text-ink-muted whitespace-nowrap font-mono text-meta tabular">
        {formatSize(file.size_bytes)}
      </td>
      <td className="py-2.5 pr-4 text-ink-muted hidden sm:table-cell whitespace-nowrap font-mono text-meta tabular">
        {formatDate(file.file_mtime)}
      </td>
      <td className={`py-2.5 pr-4 text-meta ${statusColor(file.status)}`}>
        {file.status}
      </td>
      <td className="py-2.5">
        <div className="flex gap-1 flex-wrap">
          <button onClick={onDownload} className={ROW_ACTION.neutral}>
            Download
          </button>
          {file.status === "archived" && !conflictMode && (
            <button onClick={onRestore} className={ROW_ACTION.neutral}>
              Restore
            </button>
          )}
          {file.status === "synced" && (
            <button onClick={onArchive} className={ROW_ACTION.neutral}>
              Archive
            </button>
          )}
          {conflictMode && (
            <>
              <button
                onClick={() => onResolve("device")}
                className={ROW_ACTION.neutral}
              >
                Keep device copy
              </button>
              <button
                onClick={() => onResolve("archive")}
                className={ROW_ACTION.cold}
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
