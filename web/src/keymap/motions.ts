import type { MotionId, OperatorTarget } from "./types";

/** The grid a motion moves over.
 *
 *  `columns` is 1 for the file table and for the lightbox, which is the whole
 *  reason those two get every motion for free: a table is a grid one column
 *  wide. `rows` is how many rows are on screen, and only <C-d>/<C-f> read it. */
export interface Geometry {
  index: number;
  total: number;
  columns: number;
  rows: number;
}

export interface Span {
  from: number;
  to: number;
}

function sane(geometry: Geometry): Geometry {
  const total = Math.max(0, Math.floor(geometry.total));
  const columns = Math.max(1, Math.floor(geometry.columns));
  const rows = Math.max(1, Math.floor(geometry.rows));
  const index = total === 0 ? 0 : Math.min(Math.max(0, Math.floor(geometry.index)), total - 1);
  return { index, total, columns, rows };
}

/** moveRows is where every vertical motion ends up, so `j`, `<C-d>` and
 *  `<C-f>` cannot disagree about what happens at the edges.
 *
 *  Downward it steps back a WHOLE ROW at a time until the target exists, so `j`
 *  on the bottom row stays in its column instead of teleporting to the last
 *  tile in the folder; a clamp to `total - 1` would have moved the cursor
 *  sideways, which is not what `j` means anywhere else the owner types it. */
function moveRows(delta: number, geometry: Geometry): number {
  const { index, total, columns } = geometry;
  if (delta === 0 || total === 0) return index;

  let target = index + delta * columns;
  if (delta > 0) {
    while (target >= total) target -= columns;
    return target <= index ? index : target;
  }
  if (target < 0) return index % columns;
  return target;
}

/** resolveMotion is total: a motion that cannot move returns the index it was
 *  given, and the interpreter treats that as a no-op. Nothing here throws, and
 *  nothing returns an index outside the list. */
export function resolveMotion(motion: MotionId, count: number, geometry: Geometry): number {
  const geo = sane(geometry);
  const { index, total, columns, rows } = geo;
  if (total === 0) return 0;

  const n = count > 0 ? count : 1;
  const column = index % columns;
  const rowStart = index - column;

  switch (motion) {
    case "left":
      return rowStart + Math.max(0, column - n);
    case "right": {
      const lastColumn = Math.min(columns - 1, total - 1 - rowStart);
      return rowStart + Math.min(lastColumn, column + n);
    }
    case "up":
      return moveRows(-n, geo);
    case "down":
      return moveRows(n, geo);
    case "row-start":
      return rowStart;
    case "row-end":
      return Math.min(rowStart + columns - 1, total - 1);
    case "first":
      // `gg` is the first item; `12gg` is item 12. Vim's rule, and the reason
      // NO_COUNT has to be distinguishable from a count of 1.
      return count > 0 ? Math.min(count, total) - 1 : 0;
    case "last":
      return count > 0 ? Math.min(count, total) - 1 : total - 1;
    case "half-page-down":
      return moveRows(Math.max(1, Math.floor(rows / 2)) * n, geo);
    case "half-page-up":
      return moveRows(-Math.max(1, Math.floor(rows / 2)) * n, geo);
    case "page-down":
      return moveRows(rows * n, geo);
    case "page-up":
      return moveRows(-rows * n, geo);
    case "next-file":
      return Math.min(index + n, total - 1);
    case "previous-file":
      return Math.max(index - n, 0);
    case "capture-day-next":
    case "capture-day-previous":
    case "tag-next":
    case "tag-previous":
    case "untagged-next":
    case "untagged-previous":
    case "pair-next":
    case "pair-previous":
      // These need file metadata and are resolved by the active file layer.
      return index;
  }
}

/** resolveRange turns an operator target into an inclusive span of indices.
 *
 *  Inclusive at both ends: there is no charwise/linewise distinction worth
 *  having when the unit is a file, and an exclusive `d3j` that archives three
 *  of the four files the eye counted is a bug report waiting to happen.
 *
 *  Returns null for a selection target, which geometry cannot answer -- the
 *  caller holds the selected ids. */
export function resolveRange(target: OperatorTarget, geometry: Geometry): Span | null {
  const geo = sane(geometry);
  if (geo.total === 0) return null;

  if (target.kind === "selection") return null;

  if (target.kind === "line") {
    const n = target.count > 0 ? target.count : 1;
    return { from: geo.index, to: Math.min(geo.index + n - 1, geo.total - 1) };
  }

  const to = resolveMotion(target.motion, target.count, geo);
  return { from: Math.min(geo.index, to), to: Math.max(geo.index, to) };
}

/** The span a visual selection covers. Linewise widens to whole grid rows,
 *  which is what makes `ggVG` mean everything even when the last row is
 *  ragged. */
export function visualSpan(
  anchor: number,
  cursor: number,
  linewise: boolean,
  geometry: Geometry,
): Span {
  const geo = sane(geometry);
  if (geo.total === 0) return { from: 0, to: -1 };

  const lo = Math.min(anchor, cursor);
  const hi = Math.max(anchor, cursor);
  const from = Math.max(0, Math.min(lo, geo.total - 1));
  const to = Math.max(0, Math.min(hi, geo.total - 1));
  if (!linewise) return { from, to };

  const rowStart = from - (from % geo.columns);
  const rowEnd = Math.min(to - (to % geo.columns) + geo.columns - 1, geo.total - 1);
  return { from: rowStart, to: rowEnd };
}

/** The indices a span covers, in order. */
export function spanIndices(span: Span | null): number[] {
  if (!span || span.to < span.from) return [];
  const out: number[] = [];
  for (let i = span.from; i <= span.to; i++) out.push(i);
  return out;
}
