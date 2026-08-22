import { describe, expect, it } from "vitest";
import { resolveMotion, resolveRange, spanIndices, visualSpan, type Geometry } from "./motions";

/** A 5-wide grid of 13 files: rows 0-4, 5-9, 10-12. The last row is ragged on
 *  purpose -- that is where every clamp goes wrong. */
function grid(index: number, overrides: Partial<Geometry> = {}): Geometry {
  return { index, total: 13, columns: 5, rows: 2, ...overrides };
}

/** The file table and the lightbox are one column wide, which is the whole
 *  reason they get every motion for free. */
function list(index: number, total = 10): Geometry {
  return { index, total, columns: 1, rows: 4 };
}

describe("resolveMotion in a grid", () => {
  it("should move within the row and stop at its edges when h or l runs out", () => {
    expect(resolveMotion("right", 1, grid(0))).toBe(1);
    expect(resolveMotion("right", 9, grid(0))).toBe(4);
    expect(resolveMotion("left", 1, grid(3))).toBe(2);
    expect(resolveMotion("left", 9, grid(3))).toBe(0);
  });

  it("should stop l at the last real file when the bottom row is ragged", () => {
    // Row 2 holds 10, 11, 12 in a 5-wide grid; l must not land on 13 and 14.
    expect(resolveMotion("right", 9, grid(10))).toBe(12);
    expect(resolveMotion("row-end", 0, grid(10))).toBe(12);
  });

  it("should keep the column when j and k move by whole rows", () => {
    expect(resolveMotion("down", 1, grid(2))).toBe(7);
    expect(resolveMotion("up", 1, grid(7))).toBe(2);
    expect(resolveMotion("down", 2, grid(1))).toBe(11);
  });

  it("should stay put when j would leave the grid, rather than jumping sideways", () => {
    // A clamp to total-1 would have moved the cursor from column 3 to column 2,
    // which is not what j means anywhere else the owner types it.
    expect(resolveMotion("down", 1, grid(8))).toBe(8);
    expect(resolveMotion("down", 5, grid(3))).toBe(8);
    expect(resolveMotion("up", 5, grid(7))).toBe(2);
  });

  it("should honour a count on j and clamp by whole rows", () => {
    expect(resolveMotion("down", 3, grid(1))).toBe(11);
    expect(resolveMotion("down", 99, grid(0))).toBe(10);
  });

  it("should put 0 and $ at the ends of the row the cursor is drawn in", () => {
    expect(resolveMotion("row-start", 0, grid(7))).toBe(5);
    expect(resolveMotion("row-end", 0, grid(7))).toBe(9);
  });

  it("uses explicit painted rows when independent grids restart after ragged groups", () => {
    const captured = grid(0, { total: 7, columns: 5, rowStarts: [0, 2] });
    expect(resolveMotion("down", 1, captured)).toBe(2);
    expect(resolveMotion("right", 99, captured)).toBe(1);
    expect(resolveMotion("row-end", 0, captured)).toBe(1);
    expect(visualSpan(0, 3, true, { ...captured, index: 3 })).toEqual({ from: 0, to: 6 });
  });

  it("should send gg to the first file and G to the last when no count is given", () => {
    expect(resolveMotion("first", 0, grid(7))).toBe(0);
    expect(resolveMotion("last", 0, grid(7))).toBe(12);
  });

  it("should send 12gg and 12G both to file 12 when a count is given", () => {
    expect(resolveMotion("first", 12, grid(0))).toBe(11);
    expect(resolveMotion("last", 12, grid(0))).toBe(11);
    expect(resolveMotion("last", 999, grid(0))).toBe(12);
  });

  it("should step half a screen of rows on ctrl-d and a whole screen on ctrl-f", () => {
    // rows: 2, so half is 1 row (5 files) and a page is 2 rows (10 files).
    expect(resolveMotion("half-page-down", 0, grid(0))).toBe(5);
    expect(resolveMotion("page-down", 0, grid(0))).toBe(10);
    expect(resolveMotion("half-page-up", 0, grid(10))).toBe(5);
    expect(resolveMotion("page-up", 0, grid(12))).toBe(2);
  });
});

describe("resolveMotion in a one-column list", () => {
  it("should degenerate j and k to next and previous", () => {
    expect(resolveMotion("down", 1, list(3))).toBe(4);
    expect(resolveMotion("up", 1, list(3))).toBe(2);
    expect(resolveMotion("down", 100, list(3))).toBe(9);
    expect(resolveMotion("up", 100, list(3))).toBe(0);
  });

  it("should make h and l no-ops when there is only one column", () => {
    expect(resolveMotion("left", 3, list(4))).toBe(4);
    expect(resolveMotion("right", 3, list(4))).toBe(4);
  });

  it("should return 0 for every motion when the list is empty", () => {
    for (const motion of ["down", "up", "first", "last", "row-end"] as const) {
      expect(resolveMotion(motion, 3, list(0, 0))).toBe(0);
    }
  });
});

describe("resolveRange", () => {
  it("should include both ends when an operator takes a motion", () => {
    // d3j from file 0 in a one-column list covers four files, not three: an
    // exclusive range archives one fewer than the eye counted.
    expect(resolveRange({ kind: "motion", motion: "down", count: 3 }, list(0))).toEqual({
      from: 0,
      to: 3,
    });
  });

  it("should order the span when the motion goes backwards", () => {
    expect(resolveRange({ kind: "motion", motion: "up", count: 2 }, list(5))).toEqual({
      from: 3,
      to: 5,
    });
  });

  it("should cover only the cursor file when the operator is doubled", () => {
    expect(resolveRange({ kind: "line", count: 0 }, grid(7))).toEqual({ from: 7, to: 7 });
    expect(resolveRange({ kind: "line", count: 3 }, grid(7))).toEqual({ from: 7, to: 9 });
  });

  it("should clamp a doubled operator to the end of the list", () => {
    expect(resolveRange({ kind: "line", count: 99 }, list(8))).toEqual({ from: 8, to: 9 });
  });

  it("should return null when the target is the live selection", () => {
    expect(resolveRange({ kind: "selection" }, list(3))).toBeNull();
  });

  it("should return null when there are no files at all", () => {
    expect(resolveRange({ kind: "line", count: 1 }, list(0, 0))).toBeNull();
  });
});

describe("visualSpan", () => {
  it("should cover exactly the files between the anchor and the cursor", () => {
    expect(visualSpan(2, 6, false, grid(6))).toEqual({ from: 2, to: 6 });
    expect(visualSpan(6, 2, false, grid(2))).toEqual({ from: 2, to: 6 });
  });

  it("should widen to whole rows when the selection is linewise", () => {
    expect(visualSpan(2, 6, true, grid(6))).toEqual({ from: 0, to: 9 });
  });

  it("should stop a linewise span at the last file when the bottom row is ragged", () => {
    // This is what makes ggVG mean "everything" rather than "everything plus
    // two files that do not exist".
    expect(visualSpan(0, 12, true, grid(12))).toEqual({ from: 0, to: 12 });
  });

  it("should produce an empty span when there are no files", () => {
    expect(spanIndices(visualSpan(0, 0, true, { index: 0, total: 0, columns: 5, rows: 2 }))).toEqual([]);
  });
});
