import { useCallback, useEffect, useRef, useState } from "react";
import { formatDateTime, formatSize } from "../lib/format";
import { useFocusTrap } from "../hooks/useFocusTrap";
import { useKeymapLayer } from "../hooks/useKeymapLayer";
import { useScrollLock } from "../hooks/useScrollLock";
import { resolveMotion, resolveRange, spanIndices } from "../keymap/motions";
import type { Command, MotionId } from "../keymap/types";
import { resolvePreview } from "../lib/preview";
import { Z } from "../lib/zLayers";
import type { BulkActions } from "../hooks/useBulkActions";
import type { FileActions } from "../hooks/useFileWorkspace";
import type { Selection } from "../hooks/useSelection";
import { quickSlotFromAction } from "../hooks/useQuickSlots";
import type { FileRecord } from "../types/api";
import type { NearFilter } from "../lib/api";
import { PinToggle } from "./PinToggle";
import { Portal } from "./Portal";
import { PreviewSurface } from "./PreviewSurface";
import { DescriptionEditor } from "./DescriptionEditor";
import { CaptureMeta } from "./CaptureMeta";
import { LocationEditor } from "./LocationEditor";
import { LocationMeta } from "./LocationMeta";
import { TagChips } from "./TagChips";
import { TagPrompt, type TagPromptMode } from "./TagPrompt";

interface LightboxProps {
  files: FileRecord[];
  initialIndex: number;
  onClose: () => void;
  onLoadMore: () => void;
  hasMore: boolean;
  /** The same actions the grid and the table use, so a pin applied here
   *  repaints the row behind the lightbox without a refetch. */
  actions: FileActions;
  /** The same bulk path, so `d3j` in the lightbox queues the same job the
   *  selection bar would. */
  bulk: BulkActions;
  selection: Selection;
  onFindNearby(near: NearFilter): void;
}

const ICON_BUTTON =
  "rounded-md p-2 text-ink-muted hover:bg-raised-hover hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor";

const ACTION_BUTTON =
  "px-3 py-1.5 text-meta rounded-md bg-raised text-ink ring-1 ring-line-strong hover:bg-raised-hover disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor";

function statusColor(status: FileRecord["status"]): string {
  switch (status) {
    case "archived":
      return "text-cold";
    case "missing":
      return "text-alarm";
    case "synced":
      return "text-ink-muted";
    default:
      return "text-ink";
  }
}

function pairIDs(files: FileRecord[], ids: number[]): number[] {
  const result = new Set(ids);
  const keys = new Set(files.filter((file) => result.has(file.id) && file.pair_key).map((file) => file.pair_key));
  for (const file of files) if (file.pair_key && keys.has(file.pair_key)) result.add(file.id);
  return [...result];
}

function contextMotion(files: FileRecord[], at: number, motion: MotionId, count: number): number {
  const direction = motion.endsWith("previous") ? -1 : 1;
  const distance = count > 0 ? count : 1;
  let current = at;
  for (let step = 0; step < distance; step += 1) {
    const source = files[current];
    if (!source) return at;
    let found = -1;
    const day = source.captured_local.slice(0, 10);
    const tags = new Set(source.tags.map((tag) => tag.toLocaleLowerCase()));
    const group = source.pair_key ? `pair:${source.pair_key}` : `file:${source.id}`;
    for (let candidate = current + direction; candidate >= 0 && candidate < files.length; candidate += direction) {
      const file = files[candidate];
      const matches = motion.startsWith("capture-day-")
        ? file.captured_local.slice(0, 10) !== day
        : motion.startsWith("tag-")
          ? file.tags.some((tag) => tags.has(tag.toLocaleLowerCase()))
          : motion.startsWith("untagged-")
            ? file.tags.length === 0
            : (file.pair_key ? `pair:${file.pair_key}` : `file:${file.id}`) !== group;
      if (matches) {
        found = candidate;
        break;
      }
    }
    if (found < 0) return current;
    current = found;
  }
  return current;
}

export function Lightbox({
  files,
  initialIndex,
  onClose,
  onLoadMore,
  hasMore,
  actions,
  bulk,
  selection,
  onFindNearby,
}: LightboxProps) {
  const [index, setIndex] = useState(initialIndex);
  const [showMeta, setShowMeta] = useState(false);
  const [busy, setBusy] = useState(false);
  const [descriptionOpen, setDescriptionOpen] = useState(false);
  const [locationOpen, setLocationOpen] = useState(false);
  const [tagPrompt, setTagPrompt] = useState<TagPromptMode | null>(null);
  const touchStartRef = useRef<{ x: number; y: number } | null>(null);
  const pendingContext = useRef<{ command: Command; loadedLength: number } | null>(null);
  const containerRef = useFocusTrap(true);
  useScrollLock(true);

  const file = files[index];

  const goTo = useCallback(
    (next: number) => {
      const clamped = Math.max(0, Math.min(next, files.length - 1));
      setIndex(clamped);
      // Prefetch when the end is in sight, the same rule as before.
      if (clamped >= files.length - 5 && hasMore) onLoadMore();
    },
    [files.length, hasMore, onLoadMore],
  );

  const goNext = useCallback(() => goTo(index + 1), [goTo, index]);
  const goPrev = useCallback(() => goTo(index - 1), [goTo, index]);

  // Every action routes through the workspace, which checks the response and
  // writes the returned record back into the list. The old handler awaited
  // fetch() with no res.ok check and no repaint: a restore that 409ed and a
  // restore that worked both left the button exactly as it was.
  const run = useCallback(
    async (action: () => Promise<void>) => {
      if (busy) return;
      setBusy(true);
      try {
        await action();
      } finally {
        setBusy(false);
      }
    },
    [busy],
  );

  /** The lightbox is a one-column grid of the same files, so every motion the
   *  grid has works here for nothing -- and it is NOT an exclusive scope, so
   *  `d`, `p` and `y` still mean what they mean. That is the point of having a
   *  grammar rather than a switch statement per component. */
  const handle = useCallback(
    (command: Command): boolean => {
      const geometry = { index, total: files.length, columns: 1, rows: 10 };

      switch (command.kind) {
        case "motion": {
          const contextual = ["capture-day-next", "capture-day-previous", "tag-next", "tag-previous", "untagged-next", "untagged-previous", "pair-next", "pair-previous"].includes(command.motion);
          const next = contextual
            ? contextMotion(files, index, command.motion, command.count)
            : resolveMotion(command.motion, command.count, geometry);
          if (contextual && next === index && command.motion.endsWith("next") && hasMore) {
            pendingContext.current = { command, loadedLength: files.length };
            onLoadMore();
            return true;
          }
          goTo(next);
          return true;
        }

        case "operator": {
          const selectionWins = (command.operator === "tag" || command.operator === "untag") && selection.count > 0;
          const contextual = command.target.kind === "motion" && ["capture-day-next", "capture-day-previous", "tag-next", "tag-previous", "untagged-next", "untagged-previous", "pair-next", "pair-previous"].includes(command.target.motion);
          const contextTarget = contextual && command.target.kind === "motion"
            ? contextMotion(files, index, command.target.motion, command.target.count)
            : index;
          if (contextual && command.target.kind === "motion" && contextTarget === index && command.target.motion.endsWith("next") && hasMore) {
            pendingContext.current = { command, loadedLength: files.length };
            onLoadMore();
            return true;
          }
          const span = selectionWins || command.target.kind === "selection"
              ? { from: index, to: index }
              : contextual && command.target.kind === "motion"
                ? { from: Math.min(index, contextTarget), to: Math.max(index, contextTarget) }
                : resolveRange(command.target, geometry);
          const resolved = selectionWins
            ? selection.ids
            : spanIndices(span)
            .map((at) => files[at]?.id)
            .filter((id): id is number => id !== undefined);
          const ids = command.operator === "tag" || command.operator === "untag" ? pairIDs(files, resolved) : resolved;
          bulk.perform(command.operator, ids);
          return true;
        }

        case "action": {
          const slot = quickSlotFromAction(command.action);
          if (slot !== null) {
            void actions.applyQuickSlot(pairIDs(files, selection.count > 0 ? selection.ids : [file.id]), slot);
            return true;
          }
          switch (command.action) {
            case "close":
            case "cancel":
              onClose();
              return true;
            case "info":
              setShowMeta((shown) => !shown);
              return true;
            case "edit-description":
              setDescriptionOpen(true);
              return true;
            case "repeat-tags":
              void actions.repeatTags(pairIDs(files, [file.id]));
              return true;
            case "tag-undo":
              void actions.undoTags();
              return true;
            case "tag-redo":
              void actions.redoTags();
              return true;
            default:
              return false;
          }
        }

        default:
          return false;
      }
    },
    [actions, bulk, file.id, files, goTo, hasMore, index, onClose, onLoadMore, selection],
  );
  useKeymapLayer("lightbox", handle);

  useEffect(() => {
    const pending = pendingContext.current;
    if (!pending || files.length <= pending.loadedLength) return;
    pendingContext.current = null;
    handle(pending.command);
  }, [files.length, handle]);

  // Touch/swipe support
  const handleTouchStart = (e: React.TouchEvent) => {
    touchStartRef.current = { x: e.touches[0].clientX, y: e.touches[0].clientY };
  };

  const handleTouchEnd = (e: React.TouchEvent) => {
    if (!touchStartRef.current) return;
    const dx = e.changedTouches[0].clientX - touchStartRef.current.x;
    const dy = e.changedTouches[0].clientY - touchStartRef.current.y;
    touchStartRef.current = null;

    if (Math.abs(dx) > Math.abs(dy) && Math.abs(dx) > 50) {
      if (dx < 0) goNext();
      else goPrev();
    } else if (dy > 80) {
      onClose();
    }
  };

  // What this file actually is, per type -- not "it has a thumbnail, so it
  // must be an image". That proxy was wrong for every format the daemon
  // thumbnails but a browser cannot draw: opening a video handed the browser
  // the full original as an <img src>, downloading every byte of it over the
  // LAN to render a broken-image icon.
  const preview = resolvePreview(file);

  return (
    <Portal>
      <div
        ref={(node) => {
          containerRef.current = node;
        }}
        role="dialog"
        aria-modal="true"
        aria-label={file.filename}
        tabIndex={-1}
        data-lightbox={file.id}
        data-keyscope="lightbox"
        style={{ zIndex: Z.lightbox }}
        className="fixed inset-0 bg-ground/95 flex items-center justify-center outline-none"
        onTouchStart={handleTouchStart}
        onTouchEnd={handleTouchEnd}
      >
        {/* Close button */}
        <button
          onClick={onClose}
          aria-label="Close"
          className={`absolute top-4 right-4 z-10 ${ICON_BUTTON}`}
        >
          <svg className="w-6 h-6" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M6 18L18 6M6 6l12 12" />
          </svg>
        </button>

        {/* Navigation arrows */}
        {index > 0 && (
          <button
            onClick={goPrev}
            aria-label="Previous"
            className={`absolute left-4 top-1/2 -translate-y-1/2 z-10 ${ICON_BUTTON}`}
          >
            <svg className="w-8 h-8" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M15 19l-7-7 7-7" />
            </svg>
          </button>
        )}
        {index < files.length - 1 && (
          <button
            onClick={goNext}
            aria-label="Next"
            className={`absolute right-4 top-1/2 -translate-y-1/2 z-10 ${ICON_BUTTON}`}
          >
            <svg className="w-8 h-8" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 5l7 7-7 7" />
            </svg>
          </button>
        )}

        {/* Main viewport */}
        <div
          className="max-w-[90vw] max-h-[80vh] w-full flex items-center justify-center"
          data-preview-kind={preview.kind}
        >
          <PreviewSurface file={file} preview={preview} />
        </div>

        {/* Bottom bar */}
        <div className="absolute bottom-0 left-0 right-0 border-t border-line bg-surface/90 backdrop-blur-sm px-4 py-3">
          <div className="flex items-center justify-between max-w-4xl mx-auto">
            <div className="flex-1 min-w-0">
              <p className="text-base font-medium truncate">{file.filename}</p>
              <p className="text-meta text-ink-muted">
                {formatSize(file.size_bytes)} &middot; {formatDateTime(file.file_mtime)} &middot;{" "}
                <span className={statusColor(file.status)}>
                  {file.status}
                </span>
                {file.pinned && (
                  <span className="text-held">
                    {" "}
                    &middot; pinned{file.pin_source === "restore" ? " on restore" : " by you"}
                  </span>
                )}
              </p>
            </div>

            <div className="flex items-center gap-2 ml-4">
              <PinToggle
                file={file}
                onPin={(pinned) => void run(() => actions.setPinned(file, pinned))}
              />
              <button
                onClick={() => setShowMeta(!showMeta)}
                className={ACTION_BUTTON}
              >
                Info
              </button>
              <button
                onClick={() => setDescriptionOpen(true)}
                className={ACTION_BUTTON}
              >
                Description
              </button>
              <button
                onClick={() => setLocationOpen(true)}
                className={ACTION_BUTTON}
              >
                Location
              </button>
              <button
                onClick={() => setTagPrompt("add")}
                className={ACTION_BUTTON}
              >
                Add tags
              </button>
              {file.tags.length > 0 && (
                <button
                  onClick={() => setTagPrompt("remove")}
                  className={ACTION_BUTTON}
                >
                  Remove tags
                </button>
              )}
              <button
                onClick={() => actions.download(file)}
                className={ACTION_BUTTON}
              >
                Download
              </button>
              {file.status === "archived" && (
                <button
                  onClick={() => void run(() => actions.restore(file))}
                  disabled={busy}
                  className={ACTION_BUTTON}
                >
                  {busy ? "Working..." : "Restore to Sync"}
                </button>
              )}
              {file.status === "synced" && (
                <button
                  onClick={() => void run(() => actions.archive(file))}
                  disabled={busy}
                  className={ACTION_BUTTON}
                >
                  {busy ? "Working..." : "Archive"}
                </button>
              )}
            </div>
          </div>

          {/* Metadata panel */}
          {showMeta && (
            <div className="max-w-4xl mx-auto mt-3 pt-3 border-t border-line grid grid-cols-2 md:grid-cols-4 gap-2 text-meta text-ink-muted">
              <div>
                <span className="text-ink-faint">Path:</span> {file.relative_path}
              </div>
              <div>
                <span className="text-ink-faint">Folder:</span> {file.folder_label}
              </div>
              <div>
                <span className="text-ink-faint">Type:</span> {file.mime_type || file.extension}
              </div>
              <div>
                <span className="text-ink-faint">First seen:</span> {formatDateTime(file.first_seen)}
              </div>
              <div className="col-span-2">
                <span className="text-ink-faint">Captured:</span> <CaptureMeta file={file} />
              </div>
              {file.archived_at && (
                <div>
                  <span className="text-ink-faint">Archived:</span>{" "}
                  {formatDateTime(file.archived_at)}
                </div>
              )}
              <div>
                <span className="text-ink-faint">Index:</span> {index + 1} / {files.length}
              </div>
              <div className="col-span-2 md:col-span-4">
                <span className="text-ink-faint">Location:</span> <LocationMeta file={file} />
              </div>
              <div className="col-span-2 md:col-span-4 flex items-center gap-2">
                <span className="text-ink-faint">Tags:</span>
                {file.tags.length > 0 ? <TagChips tags={file.tags} max={file.tags.length} /> : <span>None</span>}
              </div>
              {file.description && (
                <div className="col-span-2 md:col-span-4 whitespace-pre-wrap text-ink-muted">
                  <span className="text-ink-faint">Description:</span> {file.description}
                </div>
              )}
            </div>
          )}
        </div>
      </div>
      {descriptionOpen && (
        <DescriptionEditor
          files={files}
          initialIndex={index}
          actions={actions}
          onIndexChange={goTo}
          onClose={() => setDescriptionOpen(false)}
        />
      )}
      {locationOpen && (
        <LocationEditor
          file={file}
          actions={actions}
          onFindNearby={(near) => {
            setLocationOpen(false);
            onFindNearby(near);
            onClose();
          }}
          onClose={() => setLocationOpen(false)}
        />
      )}
      {tagPrompt && (
        <TagPrompt
          folder={file.folder_label}
          ids={[file.id]}
          mode={tagPrompt}
          onApply={(add, remove) => actions.tagFiles([file.id], add, remove)}
          onClose={() => setTagPrompt(null)}
        />
      )}
    </Portal>
  );
}
