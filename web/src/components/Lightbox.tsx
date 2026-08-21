import { useCallback, useRef, useState } from "react";
import { formatDateTime, formatSize } from "../lib/format";
import { useFocusTrap } from "../hooks/useFocusTrap";
import { useKeymapLayer } from "../hooks/useKeymapLayer";
import { useScrollLock } from "../hooks/useScrollLock";
import { resolveMotion, resolveRange, spanIndices } from "../keymap/motions";
import type { Command } from "../keymap/types";
import { resolvePreview } from "../lib/preview";
import { Z } from "../lib/zLayers";
import type { BulkActions } from "../hooks/useBulkActions";
import type { FileActions } from "../hooks/useFileWorkspace";
import type { FileRecord } from "../types/api";
import { PinToggle } from "./PinToggle";
import { Portal } from "./Portal";
import { PreviewSurface } from "./PreviewSurface";

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
}

export function Lightbox({
  files,
  initialIndex,
  onClose,
  onLoadMore,
  hasMore,
  actions,
  bulk,
}: LightboxProps) {
  const [index, setIndex] = useState(initialIndex);
  const [showMeta, setShowMeta] = useState(false);
  const [busy, setBusy] = useState(false);
  const touchStartRef = useRef<{ x: number; y: number } | null>(null);
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
        case "motion":
          goTo(resolveMotion(command.motion, command.count, geometry));
          return true;

        case "operator": {
          const span =
            command.target.kind === "selection"
              ? { from: index, to: index }
              : resolveRange(command.target, geometry);
          const ids = spanIndices(span)
            .map((at) => files[at]?.id)
            .filter((id): id is number => id !== undefined);
          bulk.perform(command.operator, ids);
          return true;
        }

        case "action":
          switch (command.action) {
            case "close":
            case "cancel":
              onClose();
              return true;
            case "info":
              setShowMeta((shown) => !shown);
              return true;
            default:
              return false;
          }

        default:
          return false;
      }
    },
    [bulk, files, goTo, index, onClose],
  );
  useKeymapLayer("lightbox", handle);

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
        style={{ zIndex: Z.lightbox }}
        className="fixed inset-0 bg-black/95 flex items-center justify-center outline-none"
        onTouchStart={handleTouchStart}
        onTouchEnd={handleTouchEnd}
      >
        {/* Close button */}
        <button
          onClick={onClose}
          aria-label="Close"
          className="absolute top-4 right-4 text-neutral-400 hover:text-white z-10 p-2"
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
            className="absolute left-4 top-1/2 -translate-y-1/2 text-neutral-400 hover:text-white p-2 z-10"
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
            className="absolute right-4 top-1/2 -translate-y-1/2 text-neutral-400 hover:text-white p-2 z-10"
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
        <div className="absolute bottom-0 left-0 right-0 bg-neutral-900/80 backdrop-blur-sm px-4 py-3">
          <div className="flex items-center justify-between max-w-4xl mx-auto">
            <div className="flex-1 min-w-0">
              <p className="text-sm font-medium truncate">{file.filename}</p>
              <p className="text-xs text-neutral-400">
                {formatSize(file.size_bytes)} &middot; {formatDateTime(file.file_mtime)} &middot;{" "}
                <span className={file.status === "archived" ? "text-amber-400" : "text-green-400"}>
                  {file.status}
                </span>
                {file.pinned && (
                  <span className="text-indigo-300">
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
                className="px-3 py-1.5 text-xs rounded-md bg-neutral-700 hover:bg-neutral-600 transition-colors"
              >
                Info
              </button>
              <button
                onClick={() => actions.download(file)}
                className="px-3 py-1.5 text-xs rounded-md bg-blue-600 hover:bg-blue-500 transition-colors"
              >
                Download
              </button>
              {file.status === "archived" && (
                <button
                  onClick={() => void run(() => actions.restore(file))}
                  disabled={busy}
                  className="px-3 py-1.5 text-xs rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 transition-colors"
                >
                  {busy ? "Working..." : "Restore to Sync"}
                </button>
              )}
              {file.status === "synced" && (
                <button
                  onClick={() => void run(() => actions.archive(file))}
                  disabled={busy}
                  className="px-3 py-1.5 text-xs rounded-md bg-amber-600 hover:bg-amber-500 disabled:opacity-50 transition-colors"
                >
                  {busy ? "Working..." : "Archive"}
                </button>
              )}
            </div>
          </div>

          {/* Metadata panel */}
          {showMeta && (
            <div className="max-w-4xl mx-auto mt-3 pt-3 border-t border-neutral-700 grid grid-cols-2 md:grid-cols-4 gap-2 text-xs text-neutral-400">
              <div>
                <span className="text-neutral-500">Path:</span> {file.relative_path}
              </div>
              <div>
                <span className="text-neutral-500">Folder:</span> {file.folder_label}
              </div>
              <div>
                <span className="text-neutral-500">Type:</span> {file.mime_type || file.extension}
              </div>
              <div>
                <span className="text-neutral-500">First seen:</span> {formatDateTime(file.first_seen)}
              </div>
              {file.archived_at && (
                <div>
                  <span className="text-neutral-500">Archived:</span>{" "}
                  {formatDateTime(file.archived_at)}
                </div>
              )}
              <div>
                <span className="text-neutral-500">Index:</span> {index + 1} / {files.length}
              </div>
            </div>
          )}
        </div>
      </div>
    </Portal>
  );
}
