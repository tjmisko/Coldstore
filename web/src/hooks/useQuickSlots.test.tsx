import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { useQuickSlots } from "./useQuickSlots";

afterEach(() => localStorage.clear());

describe("useQuickSlots", () => {
  it("persists nine assignments per folder and notifies another mounted consumer", () => {
    const first = renderHook(() => useQuickSlots("CameraRoll"));
    const second = renderHook(() => useQuickSlots("CameraRoll"));
    const other = renderHook(() => useQuickSlots("Documents"));

    act(() => first.result.current.setSlot(3, "Trips/Iceland"));

    expect(first.result.current.slots[2]).toBe("Trips/Iceland");
    expect(second.result.current.slots[2]).toBe("Trips/Iceland");
    expect(other.result.current.slots[2]).toBe("");
    expect(JSON.parse(localStorage.getItem("coldstore.quick-tags.CameraRoll") ?? "[]")[2]).toBe("Trips/Iceland");
  });
});
