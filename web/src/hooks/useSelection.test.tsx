import { act, renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { useSelection } from "./useSelection";

const ORDER = [10, 11, 12, 13, 14];

describe("useSelection", () => {
  it("should select the whole range when shift-clicking after an anchor", () => {
    const { result } = renderHook(() => useSelection(ORDER));

    act(() => result.current.toggle(11));
    act(() => result.current.toggleRange(14));

    expect([...result.current.selected].sort()).toEqual([11, 12, 13, 14]);
    expect(result.current.count).toBe(4);
  });

  it("should select the range in either direction", () => {
    const { result } = renderHook(() => useSelection(ORDER));

    act(() => result.current.toggle(13));
    act(() => result.current.toggleRange(11));

    expect([...result.current.selected].sort()).toEqual([11, 12, 13]);
  });

  it("should DESELECT the range when the anchor itself was deselected", () => {
    // Shift-click after a correction must undo, not re-select: an owner who
    // has just unticked a photo and shift-clicks to unpick the rest is asking
    // for fewer files, not more.
    const { result } = renderHook(() => useSelection(ORDER));

    act(() => result.current.selectMany(ORDER));
    act(() => result.current.toggle(11)); // now deselected, and the anchor
    act(() => result.current.toggleRange(13));

    expect([...result.current.selected].sort()).toEqual([10, 14]);
  });

  it("should behave as a plain toggle when there is no anchor yet", () => {
    const { result } = renderHook(() => useSelection(ORDER));

    act(() => result.current.toggleRange(12));

    expect([...result.current.selected]).toEqual([12]);
  });

  it("should keep ids that are no longer rendered", () => {
    // Selection is a set of ids, not an index range. "Select all in folder"
    // returns up to 20,000 ids while sixty rows are on screen, and every one
    // of them has to survive to the job request.
    const { result, rerender } = renderHook(({ ids }) => useSelection(ids), {
      initialProps: { ids: ORDER },
    });

    act(() => result.current.selectMany([10, 11, 999]));
    rerender({ ids: [12, 13] });

    expect(result.current.count).toBe(3);
    expect(result.current.ids.sort()).toEqual([10, 11, 999]);
  });

  it("should clear both the selection and the anchor", () => {
    const { result } = renderHook(() => useSelection(ORDER));

    act(() => result.current.toggle(10));
    act(() => result.current.clear());
    act(() => result.current.toggleRange(13));

    expect([...result.current.selected]).toEqual([13]);
  });
});
