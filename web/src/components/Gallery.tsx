import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "../lib/api";
import { formatCount, formatSize } from "../lib/format";
import { useFileWorkspace, type FilterKey } from "../hooks/useFileWorkspace";
import type { FileRecord } from "../types/api";
import { FilterBar } from "./FilterBar";
import { Lightbox } from "./Lightbox";
import { PinToggle } from "./PinToggle";
import { SelectionBar } from "./SelectionBar";

interface GalleryProps {
  folder: string;
  filterKey: FilterKey;
  onFilterChange(key: FilterKey): void;
  onFolderChanged(): void;
}

export function Gallery({ folder, filterKey, onFilterChange, onFolderChanged }: GalleryProps) {
  const workspace = useFileWorkspace({ folder, filterKey, sort: "mtime", onFolderChanged });
  const { files: list, selection, actions, preset, filters } = workspace;
  const { files, loading, error, hasMore, loadMore, refetchIds } = list;

  const [lightboxIndex, setLightboxIndex] = useState<number | null>(null);
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

  // Window-level shortcuts. The target check is what keeps them out of the
  // search box and the budget editor: without it, Escape and Ctrl+A are stolen
  // from every input on the page.
  useEffect(() => {
    if (lightboxIndex !== null) return;
    const onKey = (e: KeyboardEvent) => {
      const target = e.target as HTMLElement | null;
      if (
        target &&
        (target.tagName === "INPUT" ||
          target.tagName === "TEXTAREA" ||
          target.isContentEditable)
      ) {
        return;
      }
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "a") {
        e.preventDefault();
        selection.selectMany(files.map((f) => f.id));
        return;
      }
      if (e.key === "Escape") {
        selection.clear();
        return;
      }
      if (e.key === "Enter" && files.length > 0) setLightboxIndex(0);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [lightboxIndex, files, selection]);

  const onFinished = useCallback(
    async (ids: number[]) => {
      await refetchIds(ids);
      onFolderChanged();
    },
    [refetchIds, onFolderChanged],
  );

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
          top of the first few characters of the folder name at 390px. */}
      <h2 className="text-title font-medium tracking-tight mb-4 ml-11 md:ml-0">{folder}</h2>
      <FilterBar active={filterKey} onChange={onFilterChange} />

      {error && (
        <p className="mb-3 text-meta text-alarm" role="alert">
          {error}
        </p>
      )}

      <div className="grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 gap-3 select-none">
        {files.map((file, index) => (
          <Tile
            key={file.id}
            file={file}
            selected={selection.isSelected(file.id)}
            anySelected={selection.count > 0}
            conflictMode={preset.key === "conflicts"}
            onOpen={() => setLightboxIndex(index)}
            onToggle={() => selection.toggle(file.id)}
            onToggleRange={() => selection.toggleRange(file.id)}
            onPin={(pinned) => void actions.setPinned(file, pinned)}
            onResolve={(keep) => void actions.resolve(file, keep)}
          />
        ))}
      </div>

      <div ref={sentinelRef} className="h-8" />

      {loading && <div className="text-center text-ink-faint py-4 text-meta">Loading...</div>}

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

      {lightboxIndex !== null && files[lightboxIndex] && (
        <Lightbox
          files={files}
          initialIndex={lightboxIndex}
          onClose={() => setLightboxIndex(null)}
          onLoadMore={loadMore}
          hasMore={hasMore}
          actions={actions}
        />
      )}

      <SelectionBar
        selection={selection}
        filters={filters}
        files={files}
        onFinished={onFinished}
      />
    </div>
  );
}

interface TileProps {
  file: FileRecord;
  selected: boolean;
  anySelected: boolean;
  conflictMode: boolean;
  onOpen(): void;
  onToggle(): void;
  onToggleRange(): void;
  onPin(pinned: boolean): void;
  onResolve(keep: "device" | "archive"): void;
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
  conflictMode,
  onOpen,
  onToggle,
  onToggleRange,
  onPin,
  onResolve,
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
      onClick={handleClick}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onOpen();
        }
      }}
      className={`relative aspect-square bg-raised rounded-lg overflow-hidden group cursor-pointer outline-none focus-visible:ring-2 focus-visible:ring-cursor focus-visible:ring-offset-2 focus-visible:ring-offset-ground ${
        selected ? "ring-2 ring-ink" : ""
      }`}
    >
      {thumbnail ? (
        <img
          src={thumbnail}
          alt={file.filename}
          className="w-full h-full object-cover"
          loading="lazy"
        />
      ) : (
        <div className="w-full h-full flex flex-col items-center justify-center text-ink-faint text-meta gap-1.5 p-2 text-center">
          {thumbFailed ? (
            <span
              className="text-alarm"
              title={file.thumb_error || "thumbnail generation failed"}
              aria-label="thumbnail failed"
            >
              &#9888;
            </span>
          ) : null}
          <span className="truncate w-full font-mono">{file.extension || file.filename}</span>
        </div>
      )}

      {/* The checkbox is ALWAYS rendered and always hittable.
       *
       *  It used to be `opacity-0 group-hover:opacity-100`, which meant a phone
       *  -- where there is no hover -- could never reveal it, and the
       *  `anySelected` escape hatch was unreachable because nothing could be
       *  selected first. The gallery's whole multi-select feature was dead on
       *  the device the photos are actually browsed from. It now sits at 60%
       *  until hovered or selected, which is legible on a photo without
       *  shouting, and the padding gives it a 36px touch target rather than the
       *  13px of a native box. */}
      <label
        className={`absolute top-0 left-0 p-2 cursor-pointer transition-opacity ${
          selected || anySelected ? "opacity-100" : "opacity-60 group-hover:opacity-100 focus-within:opacity-100"
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
          className="peer sr-only"
        />
        <span
          aria-hidden="true"
          className="block size-5 rounded-[5px] border-2 border-ink/70 bg-ground/55 shadow-[0_1px_3px_rgba(0,0,0,0.6)] backdrop-blur-[2px] transition-colors peer-checked:border-ink peer-checked:bg-ink peer-checked:[&_svg]:opacity-100 peer-focus-visible:ring-2 peer-focus-visible:ring-cursor peer-focus-visible:ring-offset-1 peer-focus-visible:ring-offset-ground"
        >
          <svg viewBox="0 0 20 20" className="size-full text-ground opacity-0">
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

      <div className="absolute top-1 right-1" onClick={(e) => e.stopPropagation()}>
        <PinToggle file={file} onPin={onPin} compact />
      </div>

      {/* Badges carry the state hues. Dim ground plus a coloured hairline and
          coloured text, rather than a saturated fill -- on a photograph a solid
          chip is a hole punched in the image, and there are up to three of them. */}
      <div className="absolute bottom-1.5 right-1.5 flex gap-1">
        {file.pinned && (
          <span
            className="text-micro uppercase tracking-wider px-1.5 py-0.5 rounded-full bg-held-dim/90 text-held ring-1 ring-held/40 backdrop-blur-[2px]"
            data-badge="pinned"
          >
            pinned
          </span>
        )}
        {file.status === "archived" && (
          <span
            className="text-micro uppercase tracking-wider px-1.5 py-0.5 rounded-full bg-cold-dim/90 text-cold ring-1 ring-cold/40 backdrop-blur-[2px]"
            data-badge="archived"
          >
            archived
          </span>
        )}
        {file.status === "missing" && (
          <span
            className="text-micro uppercase tracking-wider px-1.5 py-0.5 rounded-full bg-alarm-dim/90 text-alarm ring-1 ring-alarm/40 backdrop-blur-[2px]"
            data-badge="missing"
          >
            missing
          </span>
        )}
      </div>

      {conflictMode && (
        <div
          className="absolute inset-x-0 bottom-0 bg-ground/85 backdrop-blur-sm p-1.5 flex gap-1.5 text-micro"
          onClick={(e) => e.stopPropagation()}
        >
          {/* The two outcomes are not symmetrical, so they do not look alike:
              keeping the archived copy is the cold-side choice and wears the
              cold hue; keeping what is on the device is the neutral default. */}
          <button
            onClick={() => onResolve("device")}
            className="flex-1 px-1.5 py-1 rounded-md bg-raised text-ink ring-1 ring-line-strong hover:bg-raised-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
          >
            Keep device
          </button>
          <button
            onClick={() => onResolve("archive")}
            className="flex-1 px-1.5 py-1 rounded-md bg-cold-dim text-cold ring-1 ring-cold/40 hover:bg-cold/25 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
          >
            Keep archived
          </button>
        </div>
      )}

      <div className="absolute inset-0 bg-ground/0 group-hover:bg-ground/25 transition-colors pointer-events-none" />
      <span className="sr-only">{formatSize(file.size_bytes)}</span>
    </div>
  );
}
