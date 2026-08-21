import { useCallback, useEffect, useRef, useState } from "react";
import { api, errorMessage, type FileFilters } from "../lib/api";
import { formatCount } from "../lib/format";
import { useToast } from "../lib/toast";
import { useJobRunner, type JobRunner } from "./useJob";
import type { Selection } from "./useSelection";
import type { OperatorId } from "../keymap/types";

/** Above this many files, an operator asks first.
 *
 *  The selection bar's buttons sit next to a visible "400 selected" and do not
 *  need it. `dG` does: it names no number, it is one keystroke away from `dg`,
 *  and every archive is a Syncthing deletion on every device the owner has. */
export const CONFIRM_ABOVE = 10;

/** Browsers block a burst of window.open, and a hundred simultaneous downloads
 *  off a 1.5 GHz box is not a thing anyone wants either. */
export const MAX_DOWNLOADS = 5;

export interface PendingBulkConfirm {
  operator: OperatorId;
  ids: number[];
}

export interface BulkActions {
  runner: JobRunner;
  busy: boolean;
  selectAllNote: string | null;
  /** A confirmation an operator raised, or null. */
  confirm: PendingBulkConfirm | null;
  /** The mouse path: run the job now, no questions, because the bar already
   *  shows the count next to the button. */
  runJob(action: "restore" | "archive", ids: number[], force?: boolean): void;
  setPinned(ids: number[], pinned: boolean): Promise<void>;
  download(ids: number[]): void;
  /** The keyboard path: confirms first when the range is wide. */
  perform(operator: OperatorId, ids: number[]): void;
  confirmProceed(): void;
  confirmCancel(): void;
  selectAllInFolder(): Promise<void>;
  clearNote(): void;
  /** Re-submit the last restore with force, after the budget pre-flight. */
  forceLastRestore(): void;
}

interface Options {
  selection: Selection;
  /** The filters currently on screen, so "select all in folder" resolves the
   *  same set the grid is showing. */
  filters: FileFilters;
  /** Called with the ids a completed job touched. */
  onFinished(ids: number[]): void | Promise<void>;
}

/** useBulkActions is every operation that acts on more than one file.
 *
 *  It was inline in SelectionBar. It is a hook now because the keyboard needs
 *  the same code: an operator that reimplemented "archive these ids" would be a
 *  second path without the budget pre-flight, without the progress readout and
 *  without the post-job repaint -- which is exactly how one of the two restore
 *  implementations ended up without its res.ok check. */
export function useBulkActions({ selection, filters, onFinished }: Options): BulkActions {
  const runner = useJobRunner();
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const [selectAllNote, setSelectAllNote] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<PendingBulkConfirm | null>(null);
  const startedIDs = useRef<number[]>([]);
  const settled = useRef<number | null>(null);

  const { job } = runner;

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
    (action: "restore" | "archive", ids: number[], force = false) => {
      if (ids.length === 0) return;
      startedIDs.current = ids;
      settled.current = null;
      void runner.start({ action, ids, force });
    },
    [runner],
  );

  const forceLastRestore = useCallback(() => {
    runJob("restore", startedIDs.current, true);
  }, [runJob]);

  const setPinned = useCallback(
    async (ids: number[], pinned: boolean) => {
      if (ids.length === 0) return;
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
    [toast, onFinished],
  );

  const download = useCallback(
    (ids: number[]) => {
      if (ids.length === 0) return;
      const taking = ids.slice(0, MAX_DOWNLOADS);
      for (const id of taking) window.open(api.downloadUrl(id), "_blank", "noopener");
      if (ids.length > taking.length) {
        toast.show(
          "info",
          `Downloaded the first ${taking.length} of ${formatCount(ids.length)} files; browsers block more than a handful at once.`,
        );
      }
    },
    [toast],
  );

  const execute = useCallback(
    (operator: OperatorId, ids: number[]) => {
      switch (operator) {
        case "archive":
          runJob("archive", ids);
          return;
        case "restore":
          runJob("restore", ids);
          return;
        case "pin":
          void setPinned(ids, true);
          return;
        case "unpin":
          void setPinned(ids, false);
          return;
        case "download":
          download(ids);
          return;
      }
    },
    [runJob, setPinned, download],
  );

  const perform = useCallback(
    (operator: OperatorId, ids: number[]) => {
      if (ids.length === 0) return;
      // Download is capped rather than confirmed: it changes nothing.
      if (ids.length > CONFIRM_ABOVE && operator !== "download") {
        setConfirm({ operator, ids });
        return;
      }
      execute(operator, ids);
    },
    [execute],
  );

  // Read outside the updater on purpose: a setState updater that also fires a
  // job would post it twice under StrictMode, and a duplicated archive job is a
  // duplicated set of Syncthing deletions.
  const confirmProceed = useCallback(() => {
    if (!confirm) return;
    execute(confirm.operator, confirm.ids);
    setConfirm(null);
  }, [confirm, execute]);

  const confirmCancel = useCallback(() => setConfirm(null), []);

  const selectAllInFolder = useCallback(async () => {
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

  const clearNote = useCallback(() => setSelectAllNote(null), []);

  return {
    runner,
    busy,
    selectAllNote,
    confirm,
    runJob,
    setPinned,
    download,
    perform,
    confirmProceed,
    confirmCancel,
    selectAllInFolder,
    clearNote,
    forceLastRestore,
  };
}

/** The sentence the confirmation asks. Named per operator because "are you
 *  sure?" is the question that gets clicked through. */
export function describeBulk(operator: OperatorId, count: number): { title: string; body: string } {
  const files = `${formatCount(count)} files`;
  switch (operator) {
    case "archive":
      return {
        title: `Archive ${files}?`,
        body: `Coldstore will move ${files} to cold storage, and Syncthing will delete them from every device you sync this folder to.`,
      };
    case "restore":
      return {
        title: `Restore ${files}?`,
        body: `Coldstore will bring ${files} back onto the device and pin each one, which counts against the folder's budget.`,
      };
    case "pin":
      return { title: `Pin ${files}?`, body: `Coldstore will never archive these ${files}, whatever the budget says.` };
    case "unpin":
      return {
        title: `Unpin ${files}?`,
        body: `These ${files} become eligible for archiving again the next time the folder is over budget.`,
      };
    case "download":
      return { title: `Download ${files}?`, body: `Your browser will be asked for ${files}.` };
  }
}
