import { useEffect, useState } from "react";
import { formatAgo, formatCount, formatSize } from "../lib/format";
import { useEvents } from "../hooks/useConfig";
import { useJobRunner } from "../hooks/useJob";
import type { FolderStatus, StatusResponse } from "../types/api";

interface StatusBarProps {
  status: StatusResponse | null;
  error: string | null;
  /** Sends the owner to a folder with a filter already applied. The wedge
   *  banner's remedy is only a remedy if it is one click away. */
  onReview(folder: string, filter: "pinned-auto"): void;
  onRefresh(): Promise<void> | void;
}

export function StatusBar({ status, error, onReview, onRefresh }: StatusBarProps) {
  const scan = useJobRunner();
  const scanning = scan.running;

  // The scan is a job now, not a synchronous walk inside the HTTP handler, so
  // the button has to wait for the job rather than for the request.
  useEffect(() => {
    if (scan.job?.state !== "done") return;
    void (async () => {
      await onRefresh();
    })();
  }, [scan.job, onRefresh]);

  return (
    <div className="flex-1 min-h-0 overflow-y-auto p-3 border-t border-neutral-800 text-xs text-neutral-400 space-y-3">
      {error && (
        // Never `return null` on an error. A 401 and an empty install used to
        // render identically: nothing at all.
        <p className="text-red-400" role="status">
          status unavailable &mdash; {error}
        </p>
      )}

      {status && (
        <div className="flex justify-between text-neutral-500">
          <span>{formatCount(status.synced_files)} synced</span>
          <span>{formatCount(status.archived_files)} archived</span>
        </div>
      )}

      {status?.folders.map((folder) => (
        <FolderMeter key={folder.label} folder={folder} onReview={onReview} />
      ))}

      <BulkEvictionStrip />

      <div className="flex justify-between items-center pt-1 border-t border-neutral-800/60">
        <span className="text-neutral-500">
          {status ? `Cold: ${formatSize(status.cold_storage_free_bytes)} free` : "Cold: -"}
        </span>
        <button
          onClick={() => void scan.start({ action: "scan" })}
          disabled={scanning}
          className="text-blue-400 hover:text-blue-300 disabled:text-neutral-600"
        >
          {scanning
            ? `scanning ${scan.job ? `${scan.job.done}/${scan.job.total}` : ""}`
            : "rescan"}
        </button>
      </div>
      {scan.error && <p className="text-red-400">scan failed: {scan.error}</p>}
    </div>
  );
}

function FolderMeter({
  folder,
  onReview,
}: {
  folder: FolderStatus;
  onReview: StatusBarProps["onReview"];
}) {
  // Two NESTED segments, never two additive ones: pinned_bytes is a subset of
  // synced_size_bytes, so stacking them would draw a fully pinned folder at
  // 200% of its budget.
  const budget = folder.budget_bytes;
  const totalPct = budget > 0 ? Math.min(folder.synced_size_bytes / budget, 1) * 100 : 0;
  const pinnedPct = budget > 0 ? Math.min(folder.pinned_bytes / budget, 1) * 100 : 0;
  const unpinnedPct = Math.max(totalPct - pinnedPct, 0);

  const fillColor =
    totalPct > 90 ? "bg-red-500" : totalPct > 70 ? "bg-amber-500" : "bg-emerald-500";

  return (
    <div className="space-y-1">
      <div className="flex justify-between gap-2">
        <span className={folder.enabled && folder.present_in_toml ? "" : "text-neutral-600"}>
          {folder.label}
          {!folder.enabled && <span className="ml-1 text-neutral-600">(disabled)</span>}
          {!folder.present_in_toml && (
            <span className="ml-1 text-neutral-600">(not in coldstore.toml)</span>
          )}
        </span>
        <span className="whitespace-nowrap text-neutral-500">
          {formatSize(folder.synced_size_bytes)} / {formatSize(budget)}
        </span>
      </div>

      <div
        className="relative h-1.5 bg-neutral-800 rounded-full overflow-hidden flex"
        role="meter"
        aria-label={`${folder.label} budget`}
        aria-valuenow={Math.round(totalPct)}
        aria-valuemin={0}
        aria-valuemax={100}
        data-total-pct={totalPct.toFixed(2)}
        data-pinned-pct={pinnedPct.toFixed(2)}
      >
        <div
          className="h-full bg-indigo-500"
          style={{ width: `${pinnedPct}%` }}
          data-segment="pinned"
        />
        <div
          className={`h-full ${fillColor}`}
          style={{ width: `${unpinnedPct}%` }}
          data-segment="unpinned"
        />
        {folder.over_budget && (
          // At budget and over budget both clamp to 100% width, so without
          // this they look identical.
          <div
            className="absolute right-0 top-0 h-full w-1 bg-red-400"
            data-segment="overflow"
            title="over budget"
          />
        )}
      </div>

      {folder.pinned_bytes > 0 && (
        <p className="text-[11px] text-indigo-300/80">
          {formatSize(folder.pinned_bytes)} pinned ({formatCount(folder.counts.pinned)} files)
        </p>
      )}

      {folder.wedged && (
        <div
          className="rounded-md border border-amber-700 bg-amber-950/60 p-2 space-y-1"
          role="alert"
          data-wedge-banner={folder.label}
        >
          <p className="text-amber-200 font-medium leading-snug">
            {formatSize(folder.pinned_bytes)} pinned exceeds {formatSize(budget)} budget &mdash;
            nothing left to offload.
          </p>
          <p className="text-amber-300/80">
            Unpin or archive some files to let coldstore free space.
          </p>
          <button
            onClick={() => onReview(folder.label, "pinned-auto")}
            className="text-amber-300 hover:text-amber-100 underline underline-offset-2"
          >
            Review auto-pinned files &rarr;
          </button>
        </div>
      )}

      {folder.syncthing_type !== "" && folder.syncthing_type !== "sendreceive" && (
        <div className="rounded-md border border-red-800 bg-red-950/60 p-2" role="alert">
          <p className="text-red-200 leading-snug">
            {folder.label} is a {folder.syncthing_type} Syncthing folder &mdash; coldstore cannot
            free space on your devices.
          </p>
          <p className="text-red-300/80">
            Change it to Send &amp; Receive in Syncthing, then re-enable the folder here.
          </p>
        </div>
      )}

      <p className="text-[11px] text-neutral-600">
        checked {formatAgo(folder.last_enforced_at)}
      </p>
    </div>
  );
}

/** The bulk-eviction strip. An enforcement pass that archives thousands of
 *  files deletes each of them from every synced device; that is the product
 *  working as designed, and it is also the one thing the owner must never
 *  discover by noticing photos missing from their phone. */
function BulkEvictionStrip() {
  const { events } = useEvents("", { warningsOnly: false, type: "bulk_eviction", limit: 1 });
  const [dismissed, setDismissed] = useState<number[]>([]);
  const latest = events.find((e) => !dismissed.includes(e.id));
  if (!latest) return null;

  return (
    <div
      className="rounded-md border border-amber-800 bg-amber-950/40 p-2 flex gap-2"
      role="status"
      data-bulk-eviction
    >
      <div className="flex-1">
        <p className="text-amber-200 leading-snug">
          {latest.folder}: {latest.details}
        </p>
        <p className="text-amber-300/70">and removed from your synced devices.</p>
      </div>
      <button
        onClick={() => setDismissed((prev) => [...prev, latest.id])}
        aria-label="Dismiss"
        className="text-amber-500 hover:text-amber-200 leading-none"
      >
        &times;
      </button>
    </div>
  );
}
