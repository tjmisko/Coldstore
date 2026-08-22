import { useEffect, useState } from "react";
import { formatAgo, formatCount, formatSize } from "../lib/format";
import { useEvents } from "../hooks/useConfig";
import { useJobRunner } from "../hooks/useJob";
import type { FolderStatus, StatusResponse } from "../types/api";

interface StatusBarProps {
  status: StatusResponse | null;
  error: string | null;
  onRefresh(): Promise<void> | void;
}

export function StatusBar({ status, error, onRefresh }: StatusBarProps) {
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
    <div className="max-h-[35%] shrink-0 overflow-y-auto p-3 border-t border-line text-meta text-ink-muted space-y-3">
      {error && (
        // Never `return null` on an error. A 401 and an empty install used to
        // render identically: nothing at all.
        <p className="text-alarm" role="status">
          status unavailable &mdash; {error}
        </p>
      )}

      {status && (
        <div className="flex justify-between text-ink-muted tabular">
          <span>{formatCount(status.synced_files)} synced</span>
          <span>{formatCount(status.archived_files)} archived</span>
        </div>
      )}

      <BulkEvictionStrip />

      <div className="flex justify-between items-center pt-1 border-t border-line/70">
        <span className="text-ink-faint">
          {status ? `Cold: ${formatSize(status.cold_storage_free_bytes)} free` : "Cold: -"}
        </span>
        <button
          onClick={() => void scan.start({ action: "scan" })}
          disabled={scanning}
          className="text-ink-muted hover:text-ink disabled:text-ink-faint underline underline-offset-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor rounded"
        >
          {scanning
            ? `scanning ${scan.job ? `${scan.job.done}/${scan.job.total}` : ""}`
            : "rescan"}
        </button>
      </div>
      {scan.error && <p className="text-alarm">scan failed: {scan.error}</p>}
    </div>
  );
}

export function FolderNavItem({
  folder,
  selected,
  onSelect,
  onReview,
}: {
  folder: FolderStatus;
  selected: boolean;
  onSelect(): void;
  onReview(): void;
}) {
  // Two NESTED segments, never two additive ones: pinned_bytes is a subset of
  // synced_size_bytes, so stacking them would draw a fully pinned folder at
  // 200% of its budget.
  const budget = folder.budget_bytes;
  const totalPct = budget > 0 ? Math.min(folder.synced_size_bytes / budget, 1) * 100 : 0;
  const pinnedPct = budget > 0 ? Math.min(folder.pinned_bytes / budget, 1) * 100 : 0;
  const unpinnedPct = Math.max(totalPct - pinnedPct, 0);

  // The old bar went green under 70%, amber under 90%, red above -- the generic
  // dashboard gradient. Two problems: green restated "normal", which by this
  // system's rule should carry no hue at all, and amber is the pinned colour, so
  // a nearly-full folder and a heavily-pinned one turned the same shade for two
  // unrelated reasons. Neutral until it needs attention, then alarm.
  const pressured = totalPct > 90 || folder.over_budget;

  return (
    <div className={`rounded-lg ${selected ? "bg-raised-hover" : "hover:bg-raised"}`}>
      <button
        type="button"
        data-folder-nav={folder.label}
        aria-current={selected ? "page" : undefined}
        onClick={onSelect}
        className="w-full rounded-lg px-3 py-2.5 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
      >
        <div
          className={`flex items-start justify-between gap-2 text-meta ${
            folder.enabled && folder.present_in_toml ? "text-ink" : "text-ink-faint"
          }`}
        >
          <span className="min-w-0 truncate font-medium">{folder.display_name || folder.label}</span>
          <span className="shrink-0 whitespace-nowrap font-mono text-ink-muted tabular">
            {formatSize(folder.synced_size_bytes)} / {formatSize(budget)}
          </span>
        </div>

        <div
          className="relative mt-2 flex h-2 overflow-hidden rounded-full bg-ground ring-1 ring-inset ring-line"
          role="meter"
          aria-label={`${folder.label} budget`}
          aria-valuenow={Math.round(totalPct)}
          aria-valuemin={0}
          aria-valuemax={100}
          data-total-pct={totalPct.toFixed(2)}
          data-pinned-pct={pinnedPct.toFixed(2)}
        >
          <span className="h-full bg-held" style={{ width: `${pinnedPct}%` }} data-segment="pinned" />
          <span
            className={`h-full ${pressured ? "bg-alarm" : "bg-ink-muted"}`}
            style={{ width: `${unpinnedPct}%` }}
            data-segment="unpinned"
          />
          {folder.over_budget && (
            <span
              className="absolute right-0 top-0 h-full w-1 bg-alarm brightness-125"
              data-segment="overflow"
              title="over budget"
            />
          )}
        </div>

        <span className="mt-1.5 flex flex-wrap gap-x-2 text-micro text-ink-faint">
          {folder.pinned_bytes > 0 && (
            <span className="text-held/85 tabular">
              {formatSize(folder.pinned_bytes)} pinned
            </span>
          )}
          {!folder.enabled && <span>disabled</span>}
          {!folder.present_in_toml && <span>not in coldstore.toml</span>}
          {folder.wedged && <span className="text-alarm">budget blocked</span>}
          <span>checked {formatAgo(folder.last_enforced_at)}</span>
        </span>
      </button>

      {folder.wedged && (
        <div className="mx-2 mb-2 rounded-md border border-alarm/45 bg-alarm-dim p-2.5 text-meta" role="alert" data-wedge-banner={folder.label}>
          <p className="text-ink leading-snug">
            {formatSize(folder.pinned_bytes)} pinned exceeds {formatSize(budget)} budget; nothing left to offload.
          </p>
          <button
            type="button"
            onClick={onReview}
            className="mt-1 rounded text-alarm underline underline-offset-2 hover:brightness-125 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
          >
            Review auto-pinned files &rarr;
          </button>
        </div>
      )}

      {folder.syncthing_type !== "" && folder.syncthing_type !== "sendreceive" && (
        <div className="mx-2 mb-2 rounded-md border border-alarm/45 bg-alarm-dim p-2.5 text-meta" role="alert">
          <p className="text-ink leading-snug">
            This is a {folder.syncthing_type} Syncthing folder; coldstore cannot free device space.
          </p>
        </div>
      )}
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
      className="rounded-md border border-alarm/40 bg-alarm-dim/70 p-2.5 flex gap-2"
      role="status"
      data-bulk-eviction
    >
      <div className="flex-1">
        <p className="text-ink leading-snug">
          {latest.folder}: {latest.details}
        </p>
        <p className="text-ink-muted">and removed from your synced devices.</p>
      </div>
      <button
        onClick={() => setDismissed((prev) => [...prev, latest.id])}
        aria-label="Dismiss"
        className="text-ink-faint hover:text-ink leading-none focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor rounded"
      >
        &times;
      </button>
    </div>
  );
}
