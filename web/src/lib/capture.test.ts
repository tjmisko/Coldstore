import { describe, expect, it } from "vitest";
import { aFile } from "../test/fixtures";
import {
  captureDayGroups,
  captureDayTarget,
  isValidManualCapture,
  resolveCaptureDayJump,
} from "./capture";

describe("capture dates", () => {
  const files = [
    aFile({ id: 1, captured_local: "2024-03-02 10:00:00" }),
    aFile({ id: 2, captured_local: "2024-03-02 09:00:00" }),
    aFile({ id: 3, captured_local: "2024-03-01 23:00:00" }),
    aFile({ id: 4, captured_local: "" }),
  ];

  it("should group contiguous captured rows and navigate by counted days", () => {
    expect(captureDayGroups(files).map((group) => [group.day, group.start])).toEqual([
      ["2024-03-02", 0],
      ["2024-03-01", 2],
      ["", 3],
    ]);
    expect(captureDayTarget(files, 1, 1, 0)).toBe(2);
    expect(captureDayTarget(files, 0, 1, 2)).toBe(3);
    expect(captureDayTarget(files, 3, -1, 0)).toBe(2);
    expect(captureDayTarget(files, 0, -1, 5)).toBe(0);
    expect(resolveCaptureDayJump(files, 0, 1, 2)).toEqual({ index: 3, complete: true });
    expect(resolveCaptureDayJump(files, 0, 1, 3)).toEqual({ index: 3, complete: false });
  });

  it("should reject impossible and non-local manual dates", () => {
    expect(isValidManualCapture("2024-02-29 03:04:05.006")).toBe(true);
    expect(isValidManualCapture("2024-02-30 03:04:05")).toBe(false);
    expect(isValidManualCapture("2024-02-29T03:04:05Z")).toBe(false);
    expect(isValidManualCapture("")).toBe(true);
  });
});
