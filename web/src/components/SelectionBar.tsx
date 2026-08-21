import { useCallback, useEffect, useRef, useState } from "react";
import { api, errorMessage, type FileFilters } from "../lib/api";
import { formatCount, formatSize } from "../lib/format";
import { useJobRunner } from "../hooks/useJob";
import { useToast } from "../lib/toast";
import type { Selection } from "../hooks/useSelection";
import type { FileRecord } from "../types/api";
import { ConfirmDialog } from "./ConfirmDialog";

interface SelectionBarProps {
  selection: Selection;
  /** The filters currently on screen, so "select all in folder" resolves the
   *  same set the grid is showing. */
  filters: FileFilters;
  /** The rows currently loaded, used only to name files in a failure list. */
  files: FileRecord[];
  /** Called with the ids a completed job touched. */
  onFinished(ids: number[]): void | Promise<void>;
}

export function SelectionBar({ selection, filters, files, onFinished }: SelectionBarProps) {
  const runner = useJobRunner();
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const [selectAllNote, setSelectAllNote] = useState<string | null>(null);
  const startedIDs = useRef<number[]>([]);
  const settled = useRef<number | null>(null);

  const { job } = runner;
  const finished = job !== null && (job.state === "done" || job.state === "failed");

  // Repaint exactly the rows the job touched, once, when it settles.
  useEffect(() => {
    if (!job || (job.state !== "done" && job.state !== "failed")) return;
    if (settled.current === job.id) return;
    settled.current = job.id;
    void (async () => {
      await onFinished(startedIDs.current);
    })();
  }, [job, onFinished]);

  const runJob = useCallback(
    (action: "restore" | "archive", force = false) => {
      startedIDs.current = selection.ids;
      settled.current = null;
      void runner.start({ action, ids: selection.ids, force });
    },
    [runner, selection.ids],
  );

  const setPinned = useCallback(
    async (pinned: boolean) => {
      const ids = selection.ids;
      setBusy(true);
      try {
        // ONE request. A loop of 400 pin calls would serialise 800 aggregate
        // queries behind a single SQLite connection to set 400 bits.
        const res = await api.bulkPin(ids, pinned);
        toast.show(
          "success",
          `${pinned ? "Pinned" : "Unpinned"} ${formatCount(res.updated)} file${res.updated === 1 ? "" : "s"}`,
        );
        if (res.warning) toast.show("info", res.warning);
        await onFinished(ids);
      } catch (err) {
        toast.show("error", errorMessage(err));
      } finally {
        setBusy(false);
      }
    },
    [selection.ids, toast, onFinished],
  );

  const selectAll = useCallback(async () => {
    setBusy(true);
    try {
      const res = await api.fileIDs(filters);
      selection.selectMany(res.ids);
      setSelectAllNote(
        res.truncated
          ? `Selected the first ${formatCount(res.ids.length)} of ${formatCount(res.total)} matching files.`
          : null,
      );
    } catch (err) {
      toast.show("error", errorMessage(err));
    } finally {
      setBusy(false);
    }
  }, [filters, selection, toast]);

  if (selection.count === 0 && !runner.running && !finished) return null;

  const disabled = busy || runner.running;

  return (
    <>
      <div
        className="fixed bottom-0 left-0 right-0 z-45 border-t border-line-strong bg-surface/95 backdrop-blur px-4 py-2.5"
        data-selection-bar
      >
        <div className="mx-auto max-w-5xl flex flex-wrap items-center gap-2 text-base">
          {runner.running && job ? (
            <span className="font-medium" data-job-progress>
              {job.action === "restore" ? "Restoring" : "Archiving"} {formatCount(job.total)} files
              &mdash; {formatCount(job.done)}/{formatCount(job.total)}
            </span>
          ) : (
            <span className="font-medium" data-selection-count>
              {formatCount(selection.count)} selected
            </span>
          )}

          {!runner.running && (
            <>
              <button
                onClick={() => void selectAll()}
                disabled={disabled}
                className="px-2.5 py-1 rounded-md text-ink-muted ring-1 ring-line-strong hover:bg-raised-hover hover:text-ink disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
              >
                Select all in folder
              </button>
              <span className="flex-1" />
              <button
                onClick={() => runJob("restore")}
                disabled={disabled || selection.count === 0}
                className="px-3 py-1 rounded-md bg-ink text-ground font-medium hover:brightness-95 disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor focus-visible:ring-offset-2 focus-visible:ring-offset-surface"
              >
                Restore
              </button>
              <button
                onClick={() => runJob("archive")}
                disabled={disabled || selection.count === 0}
                className="px-3 py-1 rounded-md bg-cold-dim text-cold ring-1 ring-cold/40 hover:bg-cold/25 disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
              >
                Archive
              </button>
              <button
                onClick={() => void setPinned(true)}
                disabled={disabled || selection.count === 0}
                className="px-3 py-1 rounded-md bg-held-dim text-held ring-1 ring-held/40 hover:bg-held/25 disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
              >
                Pin
              </button>
              <button
                onClick={() => void setPinned(false)}
                disabled={disabled || selection.count === 0}
                className="px-2.5 py-1 rounded-md text-ink-muted ring-1 ring-line-strong hover:bg-raised-hover hover:text-ink disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
              >
                Unpin
              </button>
              <button
                onClick={() => {
                  selection.clear();
                  setSelectAllNote(null);
                  runner.dismiss();
                }}
                className="px-2 py-1 rounded-md text-ink-faint hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
              >
                Clear
              </button>
            </>
          )}
        </div>

        {selectAllNote && (
          <p className="mx-auto max-w-5xl text-meta text-ink-muted pt-1.5">{selectAllNote}</p>
        )}

        {runner.error && (
          <p className="mx-auto max-w-5xl text-meta text-alarm pt-1.5" role="alert">
            {runner.error}
          </p>
        )}

        {finished && job && (
          <div className="mx-auto max-w-5xl text-meta pt-1.5" data-job-summary>
            <p className={job.failed_count > 0 ? "text-alarm" : "text-ink-muted"}>
              {job.action === "restore" ? "Restored" : "Archived"} {formatCount(job.done)} of{" "}
              {formatCount(job.total)}
              {job.failed_count > 0 && `, ${formatCount(job.failed_count)} failed`}
              {job.error !== "" && ` -- ${job.error}`}
            </p>
            {job.failed.length > 0 && (
              <ul className="mt-1 space-y-0.5 text-ink-faint font-mono">
                {job.failed.slice(0, 5).map((failure) => (
                  <li key={failure.id}>
                    {files.find((f) => f.id === failure.id)?.filename ?? `file ${failure.id}`}:{" "}
                    {failure.error}
                  </li>
                ))}
                {job.failed.length > 5 && <li>and {job.failed.length - 5} more</li>}
              </ul>
            )}
          </div>
        )}
      </div>

      {runner.preflight && (
        <ConfirmDialog
          title="This restore does not fit in the folder budget"
          confirmLabel="Restore anyway"
          destructive
          onCancel={runner.dismiss}
          onConfirm={() => runJob("restore", true)}
        >
          <p>
            Restoring these {formatCount(selection.count)} files will exceed{" "}
            <span className="font-medium">{runner.preflight.folder}</span>&rsquo;s budget by{" "}
            <span className="font-medium">{formatSize(runner.preflight.overage_bytes)}</span>.
          </p>
          {runner.preflight.estimated_evictions > 0 ? (
            <p>
              Coldstore will archive about{" "}
              <span className="font-medium">
                {formatCount(runner.preflight.estimated_evictions)} other files
              </span>{" "}
              to make room, and Syncthing will delete them from your devices.
            </p>
          ) : (
            <p>
              Nothing in this folder is eligible to be archived &mdash; every other file is pinned
              or covered by a persistent pattern &mdash; so the folder will simply stay over budget.
            </p>
          )}
        </ConfirmDialog>
      )}
    </>
  );
}
