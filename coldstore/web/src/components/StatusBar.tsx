import type { StatusResponse } from "../types/api";

function formatSize(bytes: number): string {
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(0)} MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)} GB`;
}

interface StatusBarProps {
  status: StatusResponse | null;
  onRefresh: () => void;
}

export function StatusBar({ status, onRefresh }: StatusBarProps) {
  if (!status) return null;

  return (
    <div className="p-3 border-t border-neutral-800 text-xs text-neutral-500 space-y-2">
      <div className="flex justify-between">
        <span>{status.synced_files} synced</span>
        <span>{status.archived_files} archived</span>
      </div>

      {status.folders.map((f) => {
        const pct = f.budget_bytes > 0 ? (f.synced_size_bytes / f.budget_bytes) * 100 : 0;
        return (
          <div key={f.label}>
            <div className="flex justify-between mb-0.5">
              <span>{f.label}</span>
              <span>{formatSize(f.synced_size_bytes)} / {formatSize(f.budget_bytes)}</span>
            </div>
            <div className="h-1 bg-neutral-800 rounded-full overflow-hidden">
              <div
                className={`h-full rounded-full transition-all ${
                  pct > 90 ? "bg-red-500" : pct > 70 ? "bg-amber-500" : "bg-green-500"
                }`}
                style={{ width: `${Math.min(pct, 100)}%` }}
              />
            </div>
          </div>
        );
      })}

      <div className="flex justify-between pt-1">
        <span>Cold: {formatSize(status.cold_storage_free_bytes)} free</span>
        <button onClick={onRefresh} className="text-blue-400 hover:text-blue-300">
          refresh
        </button>
      </div>
    </div>
  );
}
