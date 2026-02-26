import { useState, useEffect, useCallback, useRef } from "react";
import type { FileRecord } from "../types/api";

interface LightboxProps {
  files: FileRecord[];
  initialIndex: number;
  onClose: () => void;
  onLoadMore: () => void;
  hasMore: boolean;
}

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`;
}

function formatDate(iso: string): string {
  return new Date(iso).toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function Lightbox({ files, initialIndex, onClose, onLoadMore, hasMore }: LightboxProps) {
  const [index, setIndex] = useState(initialIndex);
  const [showMeta, setShowMeta] = useState(false);
  const [restoring, setRestoring] = useState(false);
  const touchStartRef = useRef<{ x: number; y: number } | null>(null);

  const file = files[index];

  const goNext = useCallback(() => {
    if (index < files.length - 1) {
      setIndex(index + 1);
      // Prefetch more when near the end
      if (index >= files.length - 5 && hasMore) {
        onLoadMore();
      }
    }
  }, [index, files.length, hasMore, onLoadMore]);

  const goPrev = useCallback(() => {
    if (index > 0) {
      setIndex(index - 1);
    }
  }, [index]);

  const handleRestore = useCallback(async () => {
    if (restoring || file.status !== "archived") return;
    setRestoring(true);
    try {
      await fetch(`/api/v1/files/${file.id}/restore`, { method: "POST" });
    } catch (err) {
      console.error("Restore failed:", err);
    } finally {
      setRestoring(false);
    }
  }, [file, restoring]);

  const handleDownload = useCallback(() => {
    window.open(`/api/v1/files/${file.id}/download`, "_blank");
  }, [file]);

  // Keyboard navigation
  useEffect(() => {
    const handleKey = (e: KeyboardEvent) => {
      switch (e.key) {
        case "Escape":
          onClose();
          break;
        case "ArrowRight":
        case "ArrowDown":
          goNext();
          break;
        case "ArrowLeft":
        case "ArrowUp":
          goPrev();
          break;
        case "PageDown":
          e.preventDefault();
          setIndex((i) => Math.min(i + 10, files.length - 1));
          break;
        case "PageUp":
          e.preventDefault();
          setIndex((i) => Math.max(i - 10, 0));
          break;
        case "Home":
          e.preventDefault();
          setIndex(0);
          break;
        case "End":
          e.preventDefault();
          setIndex(files.length - 1);
          break;
        case " ":
          e.preventDefault();
          setShowMeta((s) => !s);
          break;
      }
    };
    window.addEventListener("keydown", handleKey);
    return () => window.removeEventListener("keydown", handleKey);
  }, [onClose, goNext, goPrev, files.length]);

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

  const imgSrc = file.thumbnail_path
    ? `/api/v1/files/${file.id}/download`
    : "";

  return (
    <div
      className="fixed inset-0 z-50 bg-black/95 flex items-center justify-center"
      onTouchStart={handleTouchStart}
      onTouchEnd={handleTouchEnd}
    >
      {/* Close button */}
      <button
        onClick={onClose}
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
          className="absolute right-4 top-1/2 -translate-y-1/2 text-neutral-400 hover:text-white p-2 z-10"
        >
          <svg className="w-8 h-8" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 5l7 7-7 7" />
          </svg>
        </button>
      )}

      {/* Main image */}
      <div className="max-w-[90vw] max-h-[80vh] flex items-center justify-center">
        {imgSrc ? (
          <img
            src={imgSrc}
            alt={file.filename}
            className="max-w-full max-h-[80vh] object-contain"
          />
        ) : (
          <div className="text-neutral-500 text-lg">{file.filename}</div>
        )}
      </div>

      {/* Bottom bar */}
      <div className="absolute bottom-0 left-0 right-0 bg-neutral-900/80 backdrop-blur-sm px-4 py-3">
        <div className="flex items-center justify-between max-w-4xl mx-auto">
          <div className="flex-1 min-w-0">
            <p className="text-sm font-medium truncate">{file.filename}</p>
            <p className="text-xs text-neutral-400">
              {formatSize(file.size_bytes)} &middot; {formatDate(file.file_mtime)} &middot;{" "}
              <span className={file.status === "archived" ? "text-amber-400" : "text-green-400"}>
                {file.status}
              </span>
            </p>
          </div>

          <div className="flex items-center gap-2 ml-4">
            <button
              onClick={() => setShowMeta(!showMeta)}
              className="px-3 py-1.5 text-xs rounded-md bg-neutral-700 hover:bg-neutral-600 transition-colors"
            >
              Info
            </button>
            <button
              onClick={handleDownload}
              className="px-3 py-1.5 text-xs rounded-md bg-blue-600 hover:bg-blue-500 transition-colors"
            >
              Download
            </button>
            {file.status === "archived" && (
              <button
                onClick={handleRestore}
                disabled={restoring}
                className="px-3 py-1.5 text-xs rounded-md bg-green-600 hover:bg-green-500 disabled:opacity-50 transition-colors"
              >
                {restoring ? "Restoring..." : "Restore to Sync"}
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
              <span className="text-neutral-500">First seen:</span> {formatDate(file.first_seen)}
            </div>
            {file.archived_at && (
              <div>
                <span className="text-neutral-500">Archived:</span> {formatDate(file.archived_at)}
              </div>
            )}
            <div>
              <span className="text-neutral-500">Index:</span> {index + 1} / {files.length}
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
