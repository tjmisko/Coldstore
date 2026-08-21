import { useCallback, useMemo, useRef, useState } from "react";

export interface Selection {
  selected: ReadonlySet<number>;
  isSelected(id: number): boolean;
  /** A plain click. Also moves the shift-click anchor. */
  toggle(id: number): void;
  /** Shift-click: everything from the anchor to id inclusive takes the
   *  anchor's state. With no anchor it is an ordinary toggle. */
  toggleRange(id: number): void;
  selectMany(ids: number[]): void;
  /** Replace the whole set. Visual mode needs it: every motion redraws the
   *  highlighted span, and a span that could only ever grow would leave the
   *  files behind the cursor selected when the owner reversed direction. */
  selectExactly(ids: number[]): void;
  clear(): void;
  count: number;
  ids: number[];
}

/** useSelection holds ids, never indices.
 *
 *  orderedIds is the order currently on screen, so a shift-click range means
 *  what the owner sees. Ids that scroll out of the loaded window stay
 *  selected: the set is the selection, and the rendered page is just a view
 *  onto it. That is what lets "select all in folder" hand 20,000 ids to a job
 *  when only 60 rows have ever been rendered. */
export function useSelection(orderedIds: number[]): Selection {
  const [selected, setSelected] = useState<ReadonlySet<number>>(() => new Set());
  const anchor = useRef<number | null>(null);

  const toggle = useCallback((id: number) => {
    anchor.current = id;
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const toggleRange = useCallback(
    (id: number) => {
      const from = anchor.current;
      if (from === null || from === id) {
        toggle(id);
        return;
      }
      const start = orderedIds.indexOf(from);
      const end = orderedIds.indexOf(id);
      if (start === -1 || end === -1) {
        toggle(id);
        return;
      }
      const [lo, hi] = start <= end ? [start, end] : [end, start];
      const range = orderedIds.slice(lo, hi + 1);
      setSelected((prev) => {
        const next = new Set(prev);
        // The anchor's state, not "select everything": shift-clicking after
        // deselecting the anchor must deselect the range, or a correction
        // turns into a re-selection.
        const select = prev.has(from);
        for (const rangeID of range) {
          if (select) next.add(rangeID);
          else next.delete(rangeID);
        }
        return next;
      });
    },
    [orderedIds, toggle],
  );

  const selectMany = useCallback((ids: number[]) => {
    setSelected((prev) => {
      const next = new Set(prev);
      for (const id of ids) next.add(id);
      return next;
    });
  }, []);

  const selectExactly = useCallback((ids: number[]) => {
    setSelected(new Set(ids));
  }, []);

  const clear = useCallback(() => {
    anchor.current = null;
    setSelected(new Set());
  }, []);

  const isSelected = useCallback((id: number) => selected.has(id), [selected]);
  const ids = useMemo(() => [...selected], [selected]);

  return {
    selected,
    isSelected,
    toggle,
    toggleRange,
    selectMany,
    selectExactly,
    clear,
    count: selected.size,
    ids,
  };
}
