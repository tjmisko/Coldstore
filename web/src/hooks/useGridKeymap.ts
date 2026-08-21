import { useCallback, useEffect, useRef, useState } from "react";
import { resolveMotion, resolveRange, spanIndices, visualSpan, type Geometry } from "../keymap/motions";
import type { Command, Mode, OperatorTarget, Scope } from "../keymap/types";
import type { FileRecord } from "../types/api";
import type { FileActions } from "./useFileWorkspace";
import type { Selection } from "./useSelection";
import type { BulkActions } from "./useBulkActions";
import { useKeymapLayer } from "./useKeymapLayer";

interface Options {
  scope: Extract<Scope, "gallery" | "browser">;
  files: FileRecord[];
  /** 1 for the file table. The gallery reports what its responsive grid is
   *  actually rendering, measured rather than guessed. */
  columns: number;
  /** Rows on screen, for <C-d> and <C-f>. */
  visibleRows: number;
  selection: Selection;
  actions: FileActions;
  bulk: BulkActions;
  conflictMode: boolean;
  onOpen(index: number): void;
  onContextMenu(index: number): void;
  /** False while the lightbox is open: the cursor stops following the grid. */
  active?: boolean;
  /** A file the finder asked to be revealed, and a token that changes on every
   *  request so revealing the same file twice works. */
  reveal?: { fileID: number; token: number } | null;
}

export interface GridCursor {
  index: number;
  /** The file under the cursor, or undefined on an empty list. */
  file: FileRecord | undefined;
  /** Point the cursor at a row the owner clicked, so the keyboard carries on
   *  from where the mouse left off. */
  setIndex(index: number): void;
}

/** useGridKeymap is the whole keyboard behaviour of a file list.
 *
 *  The gallery and the file table share it: a table is a grid one column wide,
 *  so `columns` is the only difference between them and there is one
 *  implementation of `d3j` rather than two. */
export function useGridKeymap(options: Options): GridCursor {
  const {
    scope, files, columns, visibleRows, selection, actions, bulk,
    conflictMode, onOpen, onContextMenu, active = true, reveal = null,
  } = options;

  const [index, setIndexState] = useState(0);
  const [revealedToken, setRevealedToken] = useState(0);
  /** Bumped whenever the cursor was MOVED rather than merely re-derived.
   *
   *  State, not a ref: it is also bumped from the reveal branch below, which
   *  runs during render, and a ref written during render is a bug React's lint
   *  rule is right to refuse. */
  const [focusToken, setFocusToken] = useState(0);
  const appliedFocus = useRef(0);
  const visual = useRef<{ anchor: number; mode: Mode; snapshot: ReadonlySet<number> } | null>(null);

  // Adjusting state during render, the same pattern Shell uses to auto-select
  // the first folder: a filter change can shrink the list under the cursor, and
  // an effect for this would paint one frame with a cursor past the end.
  let next = files.length === 0 ? 0 : Math.min(index, files.length - 1);

  if (reveal && reveal.token !== revealedToken) {
    const at = files.findIndex((file) => file.id === reveal.fileID);
    // The token is consumed only once the file has actually been found. The
    // finder switches folder to reveal a file, which remounts this list empty:
    // consuming the request on that first render would drop it before the page
    // it names has loaded.
    if (at >= 0) {
      setRevealedToken(reveal.token);
      setFocusToken((token) => token + 1);
      next = at;
    }
  }

  const clamped = next;
  if (clamped !== index) setIndexState(clamped);

  const geometry = useCallback(
    (at = clamped): Geometry => ({
      index: at,
      total: files.length,
      columns: Math.max(1, columns),
      rows: Math.max(1, visibleRows),
    }),
    [clamped, files.length, columns, visibleRows],
  );

  const setIndex = useCallback((next: number) => {
    setIndexState(next);
  }, []);

  const moveCursor = useCallback((at: number) => {
    setFocusToken((token) => token + 1);
    setIndexState(at);
  }, []);

  // Focus follows the cursor, and only ever when the cursor was MOVED. Focusing
  // on every render would steal focus from the sidebar the moment a page of
  // thumbnails arrived.
  useEffect(() => {
    if (appliedFocus.current === focusToken) return;
    appliedFocus.current = focusToken;
    const file = files[clamped];
    if (!file) return;
    const el = document.querySelector<HTMLElement>(
      `[data-cursor-scope="${scope}"][data-file-id="${file.id}"]`,
    );
    if (!el) return;
    el.focus({ preventScroll: true });
    // jsdom has no scrollIntoView, and a keymap test should not be the thing
    // that discovers that.
    if (typeof el.scrollIntoView === "function") el.scrollIntoView({ block: "nearest" });
  }, [focusToken, clamped, files, scope]);

  const idsFor = useCallback(
    (indices: number[]): number[] => {
      const out: number[] = [];
      for (const i of indices) {
        const file = files[i];
        if (file) out.push(file.id);
      }
      return out;
    },
    [files],
  );

  /** The ids an operator should act on.
   *
   *  A visual span wins, then a live selection, then the range the motion
   *  described. The order matters: `d` pressed with fifty files selected must
   *  not archive only the one under the cursor. */
  const targetIDs = useCallback(
    (target: OperatorTarget): number[] => {
      if (target.kind === "selection") {
        const live = visual.current;
        if (live) {
          const span = visualSpan(live.anchor, clamped, live.mode === "visual-line", geometry());
          return [...new Set([...live.snapshot, ...idsFor(spanIndices(span))])];
        }
        if (selection.count > 0) return selection.ids;
        // The rule the palette states in its own footer, applied in exactly one
        // place: the selection when there is one, the cursor file when there is
        // not.
        const cursor = files[clamped];
        return cursor ? [cursor.id] : [];
      }
      if (selection.count > 0 && target.kind === "line" && target.count <= 1) {
        // `dd` with a selection on screen means the selection, not the one file
        // the cursor happens to be over.
        return selection.ids;
      }
      return idsFor(spanIndices(resolveRange(target, geometry())));
    },
    [clamped, files, geometry, idsFor, selection],
  );

  const applyVisual = useCallback(
    (at: number) => {
      const live = visual.current;
      if (!live) return;
      const span = visualSpan(live.anchor, at, live.mode === "visual-line", geometry(at));
      const ids = idsFor(spanIndices(span));
      selection.selectExactly([...new Set([...live.snapshot, ...ids])]);
    },
    [geometry, idsFor, selection],
  );

  const resolveConflict = useCallback(
    (keep: "device" | "archive"): boolean => {
      const file = files[clamped];
      if (!file || !conflictMode) return false;
      void actions.resolve(file, keep);
      return true;
    },
    [files, clamped, conflictMode, actions],
  );

  const handle = useCallback(
    (command: Command): boolean => {
      if (files.length === 0 && command.kind !== "action") return false;

      switch (command.kind) {
        case "motion": {
          const next = resolveMotion(command.motion, command.count, geometry());
          moveCursor(next);
          if (command.extend) applyVisual(next);
          return true;
        }

        case "operator": {
          const ids = targetIDs(command.target);
          bulk.perform(command.operator, ids);
          return true;
        }

        case "mode": {
          if (command.mode === "normal") {
            // Leaving visual COMMITS the highlighted span. In vim nothing stays
            // selected; here the point of ggVG is to end up with everything
            // selected, and <Space>c is how a selection goes away.
            visual.current = null;
            return true;
          }
          visual.current = { anchor: clamped, mode: command.mode, snapshot: selection.selected };
          applyVisual(clamped);
          return true;
        }

        case "action":
          switch (command.action) {
            case "open":
              if (files[clamped]) onOpen(clamped);
              return true;
            case "toggle-select":
              if (files[clamped]) selection.toggle(files[clamped].id);
              return true;
            case "select-all-loaded":
              selection.selectMany(files.map((f) => f.id));
              return true;
            case "select-all-folder":
              void bulk.selectAllInFolder();
              return true;
            case "clear-selection":
              visual.current = null;
              selection.clear();
              bulk.clearNote();
              return true;
            case "context-menu":
              if (files[clamped]) onContextMenu(clamped);
              return true;
            case "resolve-device":
              return resolveConflict("device");
            case "resolve-archive":
              return resolveConflict("archive");
            case "cancel":
              visual.current = null;
              if (selection.count === 0) return false;
              selection.clear();
              bulk.clearNote();
              return true;
            default:
              return false;
          }
      }
    },
    [
      files, clamped, geometry, moveCursor, applyVisual, targetIDs, bulk,
      selection, onOpen, onContextMenu, resolveConflict,
    ],
  );

  useKeymapLayer(scope, handle, active);

  return { index: clamped, file: files[clamped], setIndex };
}

/** The operator ids a layer outside the grid needs, exported so the palette and
 *  the context menu use the same rule: the selection when there is one, the
 *  cursor file when there is not. */
export function operatorTargets(
  selection: Selection,
  cursor: FileRecord | undefined,
): number[] {
  if (selection.count > 0) return selection.ids;
  return cursor ? [cursor.id] : [];
}
