import { useCallback, useEffect, useRef, useState } from "react";
import { resolveMotion, resolveRange, spanIndices, visualSpan, type Geometry } from "../keymap/motions";
import type { Command, Mode, MotionId, OperatorTarget, Scope } from "../keymap/types";
import type { FileRecord } from "../types/api";
import type { FileReveal } from "../types/navigation";
import type { FileActions } from "./useFileWorkspace";
import type { Selection } from "./useSelection";
import type { BulkActions } from "./useBulkActions";
import { useKeymapLayer } from "./useKeymapLayer";
import { quickSlotFromAction } from "./useQuickSlots";
import { resolveCaptureDayJump } from "../lib/capture";

interface ContextMotionResult {
  index: number;
  complete: boolean;
}

type MotionCommand = Extract<Command, { kind: "motion" }>;
type OperatorCommand = Extract<Command, { kind: "operator" }>;

const noop = () => {};

function isContextMotion(motion: MotionId): boolean {
  return [
    "capture-day-next", "capture-day-previous", "tag-next", "tag-previous",
    "untagged-next", "untagged-previous", "pair-next", "pair-previous",
  ].includes(motion);
}

function motionDirection(motion: MotionId): -1 | 1 {
  return motion.endsWith("previous") ? -1 : 1;
}

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
  onEditDescription(index: number): void;
  /** Resolve a capture-day jump against the ordering the gallery is showing. */
  resolveCaptureDay?(index: number, direction: -1 | 1, count: number): ContextMotionResult;
  hasMore?: boolean;
  loading?: boolean;
  loadMore?(): void;
  onTagMode?(index: number): void;
  /** False while the lightbox is open: the cursor stops following the grid. */
  active?: boolean;
  /** A file the finder asked to be revealed, and a token that changes on every
   *  request so revealing the same file twice works. */
  reveal?: FileReveal | null;
}

export interface GridCursor {
  index: number;
  /** The file under the cursor, or undefined on an empty list. */
  file: FileRecord | undefined;
  /** Point the cursor at a row the owner clicked, so the keyboard carries on
   *  from where the mouse left off. */
  setIndex(index: number): void;
  /** Move and focus as if a keyboard motion resolved to this index. */
  moveTo(index: number): void;
}

/** useGridKeymap is the whole keyboard behaviour of a file list.
 *
 *  The gallery and the file table share it: a table is a grid one column wide,
 *  so `columns` is the only difference between them and there is one
 *  implementation of `d3j` rather than two. */
export function useGridKeymap(options: Options): GridCursor {
  const {
    scope, files, columns, visibleRows, selection, actions, bulk,
    conflictMode, onOpen, onContextMenu, onEditDescription, resolveCaptureDay,
    hasMore = false, loading = false, loadMore = noop, onTagMode = noop,
    active = true, reveal = null,
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
  const pendingContext = useRef<{
    command: MotionCommand | OperatorCommand;
    fromID: number;
    loadedLength: number;
  } | null>(null);

  // Adjusting state during render, the same pattern Shell uses to auto-select
  // the first folder: a filter change can shrink the list under the cursor, and
  // an effect for this would paint one frame with a cursor past the end.
  let next = files.length === 0 ? 0 : Math.min(index, files.length - 1);

  if (reveal && reveal.token !== revealedToken) {
    const at = files.findIndex((file) => file.id === reveal.file.id);
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

  const expandPairIDs = useCallback((ids: number[]): number[] => {
    const wanted = new Set(ids);
    const pairKeys = new Set<string>();
    for (const file of files) {
      if (wanted.has(file.id) && file.pair_key) pairKeys.add(file.pair_key);
    }
    if (pairKeys.size === 0) return ids;
    const result = [...ids];
    for (const file of files) {
      if (file.pair_key && pairKeys.has(file.pair_key) && !wanted.has(file.id)) {
        wanted.add(file.id);
        result.push(file.id);
      }
    }
    return result;
  }, [files]);

  const resolveContext = useCallback((motion: MotionId, count: number, at: number): ContextMotionResult => {
    const direction = motionDirection(motion);
    if (motion === "capture-day-next" || motion === "capture-day-previous") {
      const result = resolveCaptureDay?.(at, direction, count) ?? resolveCaptureDayJump(files, at, direction, count);
      if (direction > 0 && !result.complete && !hasMore) {
        return { index: Math.max(at, files.length - 1), complete: true };
      }
      return { ...result, complete: result.complete || direction < 0 || !hasMore };
    }

    const distance = count > 0 ? count : 1;
    let current = at;
    for (let step = 0; step < distance; step += 1) {
      const source = files[current];
      if (!source) return { index: at, complete: true };
      let found = -1;
      if (motion === "tag-next" || motion === "tag-previous") {
        if (source.tags.length === 0) return { index: current, complete: true };
        const sourceTags = new Set(source.tags.map((tag) => tag.toLocaleLowerCase()));
        for (let index = current + direction; index >= 0 && index < files.length; index += direction) {
          if (files[index].tags.some((tag) => sourceTags.has(tag.toLocaleLowerCase()))) {
            found = index;
            break;
          }
        }
      } else if (motion === "untagged-next" || motion === "untagged-previous") {
        for (let index = current + direction; index >= 0 && index < files.length; index += direction) {
          if (files[index].tags.length === 0) {
            found = index;
            break;
          }
        }
      } else {
        const group = source.pair_key ? `pair:${source.pair_key}` : `file:${source.id}`;
        for (let index = current + direction; index >= 0 && index < files.length; index += direction) {
          const candidate = files[index];
          const candidateGroup = candidate.pair_key ? `pair:${candidate.pair_key}` : `file:${candidate.id}`;
          if (candidateGroup !== group) {
            found = index;
            break;
          }
        }
      }
      if (found < 0) {
        return { index: current, complete: direction < 0 || !hasMore };
      }
      current = found;
    }
    return { index: current, complete: true };
  }, [files, hasMore, resolveCaptureDay]);

  /** The ids an operator should act on.
   *
   *  A visual span wins, then a live selection, then the range the motion
   *  described. The order matters: `d` pressed with fifty files selected must
   *  not archive only the one under the cursor. */
  const targetIDs = useCallback(
    (target: OperatorTarget, at = clamped, resolvedTo?: number, keepPairs = false): number[] => {
      if (target.kind === "selection") {
        const live = visual.current;
        if (live) {
          const span = visualSpan(live.anchor, at, live.mode === "visual-line", geometry(at));
          const ids = [...new Set([...live.snapshot, ...idsFor(spanIndices(span))])];
          return keepPairs ? expandPairIDs(ids) : ids;
        }
        if (selection.count > 0) return keepPairs ? expandPairIDs(selection.ids) : selection.ids;
        // The rule the palette states in its own footer, applied in exactly one
        // place: the selection when there is one, the cursor file when there is
        // not.
        const cursor = files[at];
        const ids = cursor ? [cursor.id] : [];
        return keepPairs ? expandPairIDs(ids) : ids;
      }
      if (selection.count > 0 && target.kind === "line" && target.count <= 1) {
        // `dd` with a selection on screen means the selection, not the one file
        // the cursor happens to be over.
        return keepPairs ? expandPairIDs(selection.ids) : selection.ids;
      }
      const span = target.kind === "motion" && resolvedTo !== undefined
        ? { from: Math.min(at, resolvedTo), to: Math.max(at, resolvedTo) }
        : resolveRange(target, geometry(at));
      const ids = idsFor(spanIndices(span));
      return keepPairs ? expandPairIDs(ids) : ids;
    },
    [clamped, expandPairIDs, files, geometry, idsFor, selection],
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

  const queueContext = useCallback((command: MotionCommand | OperatorCommand, at: number) => {
    const file = files[at];
    if (!file || !hasMore) return;
    pendingContext.current = { command, fromID: file.id, loadedLength: files.length };
    loadMore();
  }, [files, hasMore, loadMore]);

  const executeMotion = useCallback((command: MotionCommand, at = clamped): boolean => {
    let next: number;
    if (isContextMotion(command.motion)) {
      const result = resolveContext(command.motion, command.count, at);
      if (!result.complete) {
        queueContext(command, at);
        return true;
      }
      next = result.index;
    } else {
      next = resolveMotion(command.motion, command.count, geometry(at));
    }
    moveCursor(next);
    if (command.extend) applyVisual(next);
    if (next >= files.length - 5 && hasMore && !loading) loadMore();
    return true;
  }, [applyVisual, clamped, files.length, geometry, hasMore, loadMore, loading, moveCursor, queueContext, resolveContext]);

  const executeOperator = useCallback((command: OperatorCommand, at = clamped): boolean => {
    const forceSelection = (command.operator === "tag" || command.operator === "untag") && selection.count > 0;
    const keepPairs = command.operator === "tag" || command.operator === "untag";
    if (forceSelection) {
      bulk.perform(command.operator, targetIDs({ kind: "selection" }, at, undefined, keepPairs));
      return true;
    }
    let resolvedTo: number | undefined;
    if (command.target.kind === "motion" && isContextMotion(command.target.motion)) {
      const result = resolveContext(command.target.motion, command.target.count, at);
      if (!result.complete) {
        queueContext(command, at);
        return true;
      }
      resolvedTo = result.index;
      if (command.target.motion === "capture-day-next" && resolvedTo > at) resolvedTo -= 1;
    }
    bulk.perform(command.operator, targetIDs(command.target, at, resolvedTo, keepPairs));
    return true;
  }, [bulk, clamped, queueContext, resolveContext, selection.count, targetIDs]);

  useEffect(() => {
    const pending = pendingContext.current;
    if (!pending || loading) return;
    if (files.length <= pending.loadedLength) {
      pendingContext.current = null;
      return;
    }
    const at = files.findIndex((file) => file.id === pending.fromID);
    pendingContext.current = null;
    if (at < 0) return;
    if (pending.command.kind === "motion") executeMotion(pending.command, at);
    else executeOperator(pending.command, at);
  }, [executeMotion, executeOperator, files, loading]);

  const handle = useCallback(
    (command: Command): boolean => {
      if (files.length === 0 && command.kind !== "action") return false;

      switch (command.kind) {
        case "motion": {
          return executeMotion(command);
        }

        case "operator": {
          return executeOperator(command);
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

        case "action": {
          const slot = quickSlotFromAction(command.action);
          if (slot !== null) {
            const cursor = files[clamped];
            const ids = expandPairIDs(selection.count > 0 ? selection.ids : cursor ? [cursor.id] : []);
            void actions.applyQuickSlot(ids, slot);
            return true;
          }
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
            case "edit-description":
              if (files[clamped]) onEditDescription(clamped);
              return true;
            case "repeat-tags": {
              const cursor = files[clamped];
              if (cursor) void actions.repeatTags(expandPairIDs([cursor.id]));
              return true;
            }
            case "tag-undo":
              void actions.undoTags();
              return true;
            case "tag-redo":
              void actions.redoTags();
              return true;
            case "tag-mode":
              if (files[clamped]) onTagMode(clamped);
              return true;
            case "tag-current":
            case "untag-current": {
              const cursor = files[clamped];
              const ids = expandPairIDs(selection.count > 0 ? selection.ids : cursor ? [cursor.id] : []);
              bulk.perform(command.action === "tag-current" ? "tag" : "untag", ids);
              return true;
            }
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
      }
    },
    [
      files, clamped, executeMotion, executeOperator, expandPairIDs, applyVisual, actions,
      selection, bulk, onOpen, onContextMenu, onEditDescription, onTagMode, resolveConflict,
    ],
  );

  useKeymapLayer(scope, handle, active);

  return { index: clamped, file: files[clamped], setIndex, moveTo: moveCursor };
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
