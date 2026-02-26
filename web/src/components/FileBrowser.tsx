import { useRef, useCallback } from "react";
import { useFiles } from "../hooks/useFiles";
import type { FileRecord } from "../types/api";

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
  });
}

function statusColor(status: string): string {
  switch (status) {
    case "synced": return "text-green-400";
    case "archived": return "text-amber-400";
    case "restoring": return "text-blue-400";
    case "missing": return "text-red-400";
    default: return "text-neutral-400";
  }
}

interface FileBrowserProps {
  folder: string;
}

export function FileBrowser({ folder }: FileBrowserProps) {
  const { files, loading, hasMore, loadMore } = useFiles({ folder, sort: "name" });
  const observerRef = useRef<IntersectionObserver | null>(null);

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

  const handleDownload = (file: FileRecord) => {
    window.open(`/api/v1/files/${file.id}/download`, "_blank");
  };

  const handleRestore = async (file: FileRecord) => {
    try {
      await fetch(`/api/v1/files/${file.id}/restore`, { method: "POST" });
    } catch (err) {
      console.error("Restore failed:", err);
    }
  };

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

      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="text-left text-neutral-500 border-b border-neutral-800">
              <th className="pb-2 pr-4 font-medium">Name</th>
              <th className="pb-2 pr-4 font-medium hidden md:table-cell">Path</th>
              <th className="pb-2 pr-4 font-medium">Size</th>
              <th className="pb-2 pr-4 font-medium hidden sm:table-cell">Modified</th>
              <th className="pb-2 pr-4 font-medium">Status</th>
              <th className="pb-2 font-medium">Actions</th>
            </tr>
          </thead>
          <tbody>
            {files.map((file) => (
              <tr key={file.id} className="border-b border-neutral-800/50 hover:bg-neutral-800/30">
                <td className="py-2 pr-4">
                  <span className="text-neutral-200">{file.filename}</span>
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
                  <div className="flex gap-1">
                    <button
                      onClick={() => handleDownload(file)}
                      className="px-2 py-1 text-xs rounded bg-blue-600 hover:bg-blue-500 transition-colors"
                    >
                      Download
                    </button>
                    {file.status === "archived" && (
                      <button
                        onClick={() => handleRestore(file)}
                        className="px-2 py-1 text-xs rounded bg-green-600 hover:bg-green-500 transition-colors"
                      >
                        Restore
                      </button>
                    )}
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <div ref={sentinelRef} className="h-8" />

      {loading && (
        <div className="text-center text-neutral-500 py-4">Loading...</div>
      )}

      {!hasMore && files.length > 0 && (
        <div className="text-center text-neutral-600 py-4 text-sm">
          {files.length} files
        </div>
      )}
    </div>
  );
}
