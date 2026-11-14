import { isCollectionFileId } from "../lib/collections";
import { formatCount, formatSize } from "../lib/format";
import { describeBulk, type BulkActions } from "../hooks/useBulkActions";
import { Z } from "../lib/zLayers";
import type { Selection } from "../hooks/useSelection";
import type { FileRecord } from "../types/api";
import { ConfirmDialog } from "./ConfirmDialog";

interface SelectionBarProps {
  selection: Selection;
  /** The rows currently loaded, used only to name files in a failure list. */
  files: FileRecord[];
  /** Owned by the view, because the keyboard needs the same instance: an
   *  operator and a button press must queue the same job, not two. */
  bulk: BulkActions;
  /** Trash swaps archive/pin controls for restore and permanent deletion. */
  trashMode?: boolean;
}

function jobVerb(action: string, complete = false): string {
  const words: Record<string, [string, string]> = {
    restore: ["Restoring", "Restored"],
    archive: ["Archiving", "Archived"],
    trash: ["Deleting", "Deleted"],
    untrash: ["Restoring from trash", "Restored from trash"],
    delete: ["Permanently deleting", "Permanently deleted"],
  };
  return (words[action] ?? ["Processing", "Processed"])[complete ? 1 : 0];
}

export function SelectionBar({ selection, files, bulk, trashMode = false }: SelectionBarProps) {
  const { runner } = bulk;
  const { job } = runner;
  const finished = job !== null && (job.state === "done" || job.state === "failed");
  const showBar = selection.count > 0 || runner.running || finished;
  const disabled = bulk.busy || runner.running;
  // Files of a collection have no device copy, no pin and no purge.
  const collection = selection.ids.some(isCollectionFileId);
  const buttonBase =
    "px-2.5 py-1 rounded-md text-ink-muted ring-1 ring-line-strong hover:bg-raised-hover hover:text-ink disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor";

  return (
    <>
      {showBar && (
        <div
          style={{ zIndex: Z.selectionBar }}
          className="fixed bottom-0 left-0 right-0 border-t border-line-strong bg-surface/95 backdrop-blur px-4 py-2.5"
          data-selection-bar
        >
          <div className="mx-auto max-w-5xl flex flex-wrap items-center gap-2 text-base">
            {runner.running && job ? (
              <span className="font-medium" data-job-progress>
                {jobVerb(job.action)} {formatCount(job.total)} files
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
                  onClick={() => void bulk.selectAllInFolder()}
                  disabled={disabled}
                  className="px-2.5 py-1 rounded-md text-ink-muted ring-1 ring-line-strong hover:bg-raised-hover hover:text-ink disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
                >
                  Select all in folder
                </button>
                <span className="flex-1" />
                {trashMode ? (
                  <>
                    <button
                      onClick={() => bulk.runJob("untrash", selection.ids)}
                      disabled={disabled || selection.count === 0}
                      className="px-3 py-1 rounded-md bg-ink text-ground font-medium hover:brightness-95 disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor focus-visible:ring-offset-2 focus-visible:ring-offset-surface"
                    >
                      Restore from Trash
                    </button>
                    {!collection && (
                      <button
                        onClick={() => bulk.perform("delete", selection.ids)}
                        disabled={disabled || selection.count === 0}
                        className="px-3 py-1 rounded-md bg-alarm-dim text-alarm ring-1 ring-alarm/40 hover:brightness-125 disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
                      >
                        Delete permanently
                      </button>
                    )}
                  </>
                ) : collection ? (
                  <>
                    <button
                      onClick={() => bulk.download(selection.ids)}
                      disabled={disabled || selection.count === 0}
                      className={buttonBase}
                    >
                      Download
                    </button>
                    <button
                      onClick={() => bulk.perform("tag", selection.ids)}
                      disabled={disabled || selection.count === 0}
                      className={buttonBase}
                    >
                      Tag
                    </button>
                    <button
                      onClick={() => bulk.runJob("trash", selection.ids)}
                      disabled={disabled || selection.count === 0}
                      className="px-2.5 py-1 rounded-md text-alarm ring-1 ring-alarm/40 hover:bg-alarm-dim disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
                    >
                      Delete
                    </button>
                  </>
                ) : (
                  <>
                    <button
                      onClick={() => bulk.runJob("restore", selection.ids)}
                      disabled={disabled || selection.count === 0}
                      className="px-3 py-1 rounded-md bg-ink text-ground font-medium hover:brightness-95 disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor focus-visible:ring-offset-2 focus-visible:ring-offset-surface"
                    >
                      Restore
                    </button>
                    <button
                      onClick={() => bulk.runJob("archive", selection.ids)}
                      disabled={disabled || selection.count === 0}
                      className="px-3 py-1 rounded-md bg-cold-dim text-cold ring-1 ring-cold/40 hover:bg-cold/25 disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
                    >
                      Archive
                    </button>
                    <button
                      onClick={() => void bulk.setPinned(selection.ids, true)}
                      disabled={disabled || selection.count === 0}
                      className="px-3 py-1 rounded-md bg-held-dim text-held ring-1 ring-held/40 hover:bg-held/25 disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
                    >
                      Pin
                    </button>
                    <button
                      onClick={() => void bulk.setPinned(selection.ids, false)}
                      disabled={disabled || selection.count === 0}
                      className="px-2.5 py-1 rounded-md text-ink-muted ring-1 ring-line-strong hover:bg-raised-hover hover:text-ink disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
                    >
                      Unpin
                    </button>
                    <button
                      onClick={() => bulk.runJob("trash", selection.ids)}
                      disabled={disabled || selection.count === 0}
                      className="px-2.5 py-1 rounded-md text-alarm ring-1 ring-alarm/40 hover:bg-alarm-dim disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
                    >
                      Delete
                    </button>
                  </>
                )}
                <button
                  onClick={() => {
                    selection.clear();
                    bulk.clearNote();
                    runner.dismiss();
                  }}
                  className="px-2 py-1 rounded-md text-ink-faint hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
                >
                  Clear
                </button>
              </>
            )}
          </div>

          {bulk.selectAllNote && (
            <p className="mx-auto max-w-5xl text-meta text-ink-muted pt-1.5">{bulk.selectAllNote}</p>
          )}

          {runner.error && (
            <p className="mx-auto max-w-5xl text-meta text-alarm pt-1.5" role="alert">
              {runner.error}
            </p>
          )}

          {finished && job && (
            <div className="mx-auto max-w-5xl text-meta pt-1.5" data-job-summary>
              <p className={job.failed_count > 0 ? "text-alarm" : "text-ink-muted"}>
                {jobVerb(job.action, true)} {formatCount(job.done)} of{" "}
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
      )}

      {runner.preflight && (
        <ConfirmDialog
          title="This restore does not fit in the folder budget"
          confirmLabel="Restore anyway"
          destructive
          onCancel={runner.dismiss}
          onConfirm={bulk.forceLastRestore}
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

      {/* The keyboard's confirmation. `dG` names no number and is one key away
          from `dg`; the buttons above sit next to a visible count and do not
          need asking. */}
      {bulk.confirm && (
        <ConfirmDialog
          title={describeBulk(bulk.confirm.operator, bulk.confirm.ids.length, bulk.confirm.ids.some(isCollectionFileId)).title}
          confirmLabel="Yes, do it"
          destructive={["archive", "trash", "delete"].includes(bulk.confirm.operator)}
          onCancel={bulk.confirmCancel}
          onConfirm={bulk.confirmProceed}
        >
          <p>{describeBulk(bulk.confirm.operator, bulk.confirm.ids.length, bulk.confirm.ids.some(isCollectionFileId)).body}</p>
        </ConfirmDialog>
      )}
    </>
  );
}
