import { useCallback, useEffect, useRef, useState } from "react";
import { formatDateTime, formatSize } from "../lib/format";
import { useFocusTrap } from "../hooks/useFocusTrap";
import { useKeymapLayer } from "../hooks/useKeymapLayer";
import { useScrollLock } from "../hooks/useScrollLock";
import { resolveMotion, resolveRange, spanIndices } from "../keymap/motions";
import type { Command } from "../keymap/types";
import { contextOperatorEndpoint, isContextMotion, resolveContextMotion } from "../lib/contextMotion";
import { resolvePreview } from "../lib/preview";
import { Z } from "../lib/zLayers";
import type { BulkActions } from "../hooks/useBulkActions";
import type { FileActions, TagOperation } from "../hooks/useFileWorkspace";
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
  lastTagOperation: TagOperation | null;
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

export function Lightbox({
  files,
  initialIndex,
  onClose,
  onLoadMore,
  hasMore,
  actions,
  bulk,
  selection,
  lastTagOperation,
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
          const contextual = isContextMotion(command.motion);
          const result = contextual
            ? resolveContextMotion(files, index, command.motion, command.count, hasMore)
            : { index: resolveMotion(command.motion, command.count, geometry), complete: true };
          if (!result.complete) {
            pendingContext.current = { command, loadedLength: files.length };
            onLoadMore();
            return true;
          }
          goTo(result.index);
          return true;
        }

        case "operator": {
          const selectionWins = (command.operator === "tag" || command.operator === "untag") && selection.count > 0;
          const contextual = command.target.kind === "motion" && isContextMotion(command.target.motion);
          const contextResult = contextual && command.target.kind === "motion"
            ? resolveContextMotion(files, index, command.target.motion, command.target.count, hasMore)
            : { index, complete: true };
          if (!contextResult.complete) {
            pendingContext.current = { command, loadedLength: files.length };
            onLoadMore();
            return true;
          }
          const contextTarget = contextual && command.target.kind === "motion"
            ? contextOperatorEndpoint(command.target.motion, index, contextResult.index)
            : index;
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
            case "toggle-select":
              selection.toggle(file.id);
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
    if (!pending || (files.length <= pending.loadedLength && hasMore)) return;
    pendingContext.current = null;
    handle(pending.command);
  }, [files.length, handle, hasMore]);

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
        className="fixed inset-0 grid h-[100dvh] grid-rows-[minmax(0,1fr)_auto] overflow-hidden bg-ground/95 outline-none"
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
            className={`absolute left-1 top-[35%] -translate-y-1/2 bg-ground/55 z-10 sm:left-4 sm:top-1/2 ${ICON_BUTTON}`}
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
            className={`absolute right-1 top-[35%] -translate-y-1/2 bg-ground/55 z-10 sm:right-4 sm:top-1/2 ${ICON_BUTTON}`}
          >
            <svg className="w-8 h-8" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 5l7 7-7 7" />
            </svg>
          </button>
        )}

        {/* Main viewport */}
        <div
          className="flex min-h-0 w-full items-center justify-center overflow-hidden px-3 pb-3 pt-14 sm:px-14 sm:pb-6"
          data-preview-kind={preview.kind}
          data-lightbox-viewport
        >
          <PreviewSurface file={file} preview={preview} />
        </div>

        {/* Bottom bar */}
        <div
          data-lightbox-details
          className="max-h-[48dvh] overflow-y-auto border-t border-line bg-surface/95 px-3 py-3 pb-[max(0.75rem,env(safe-area-inset-bottom))] backdrop-blur-sm sm:px-4"
        >
          <div className="mx-auto flex max-w-4xl flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
            <div className="flex-1 min-w-0" data-lightbox-summary>
              <p className="text-base font-medium truncate">{file.filename}</p>
              <p className="text-meta leading-relaxed text-ink-muted">
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

            <div className="flex flex-wrap items-center gap-2 sm:ml-4 sm:justify-end" data-lightbox-actions>
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
          lastTagOperation={lastTagOperation}
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
