import { useRef, useCallback, useEffect, useState } from "react";
import { useFiles } from "../hooks/useFiles";
import { Lightbox } from "./Lightbox";
import type { FileRecord } from "../types/api";

function thumbnailUrl(file: FileRecord): string {
  if (file.thumbnail_path) {
    return `/api/v1/thumbnails/${file.thumbnail_path}`;
  }
  return "";
}

interface GalleryProps {
  folder: string;
}

export function Gallery({ folder }: GalleryProps) {
  const { files, loading, hasMore, loadMore } = useFiles({ folder, sort: "mtime" });
  const [lightboxIndex, setLightboxIndex] = useState<number | null>(null);
  const observerRef = useRef<IntersectionObserver | null>(null);

  // Infinite scroll sentinel
  const sentinelRef = useCallback(
    (node: HTMLDivElement | null) => {
      if (observerRef.current) observerRef.current.disconnect();
      if (!node) return;

      observerRef.current = new IntersectionObserver(
        (entries) => {
          if (entries[0].isIntersecting && hasMore && !loading) {
            loadMore();
          }
        },
        { rootMargin: "200px" },
      );
      observerRef.current.observe(node);
    },
    [hasMore, loading, loadMore],
  );

  // Keyboard navigation for grid
  useEffect(() => {
    if (lightboxIndex !== null) return;

    const handleKey = (e: KeyboardEvent) => {
      if (e.key === "Enter" && files.length > 0) {
        setLightboxIndex(0);
      }
    };
    window.addEventListener("keydown", handleKey);
    return () => window.removeEventListener("keydown", handleKey);
  }, [lightboxIndex, files.length]);

  if (!folder) {
    return (
      <div className="flex items-center justify-center h-full text-neutral-500">
        Select a folder from the sidebar
      </div>
    );
  }

  return (
    <div className="p-4 md:p-6">
      <h2 className="text-xl font-semibold mb-4">{folder}</h2>

      <div className="grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 gap-2">
        {files.map((file, i) => (
          <button
            key={file.id}
            className="relative aspect-square bg-neutral-800 rounded-lg overflow-hidden group cursor-pointer focus:outline-none focus:ring-2 focus:ring-blue-500"
            onClick={() => setLightboxIndex(i)}
          >
            {thumbnailUrl(file) ? (
              <img
                src={thumbnailUrl(file)}
                alt={file.filename}
                className="w-full h-full object-cover"
                loading="lazy"
              />
            ) : (
              <div className="w-full h-full flex items-center justify-center text-neutral-500 text-xs">
                {file.extension}
              </div>
            )}

            {/* Status badge */}
            {file.status === "archived" && (
              <span className="absolute top-1 right-1 bg-amber-600/80 text-[10px] px-1.5 py-0.5 rounded-full">
                archived
              </span>
            )}

            {/* Hover overlay */}
            <div className="absolute inset-0 bg-black/0 group-hover:bg-black/30 transition-colors" />
          </button>
        ))}
      </div>

      {/* Infinite scroll sentinel */}
      <div ref={sentinelRef} className="h-8" />

      {loading && (
        <div className="text-center text-neutral-500 py-4">Loading...</div>
      )}

      {!hasMore && files.length > 0 && (
        <div className="text-center text-neutral-600 py-4 text-sm">
          {files.length} files
        </div>
      )}

      {/* Lightbox */}
      {lightboxIndex !== null && (
        <Lightbox
          files={files}
          initialIndex={lightboxIndex}
          onClose={() => setLightboxIndex(null)}
          onLoadMore={loadMore}
          hasMore={hasMore}
        />
      )}
    </div>
  );
}
