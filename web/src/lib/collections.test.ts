import { describe, expect, it } from "vitest";
import { aCollectionStatus, aStatus } from "../test/fixtures";
import { COLLECTION_ID_BASE, isCollectionFileId, isCollectionFolder } from "./collections";

describe("isCollectionFileId", () => {
  it("should classify an id as a collection file when it is at or above 2^52", () => {
    expect(COLLECTION_ID_BASE).toBe(4503599627370496);
    expect(isCollectionFileId(2 ** 52)).toBe(true);
    expect(isCollectionFileId(2 ** 52 + 1)).toBe(true);
    expect(isCollectionFileId(Number.MAX_SAFE_INTEGER)).toBe(true);
  });

  it("should classify a legacy id as not a collection file", () => {
    expect(isCollectionFileId(1)).toBe(false);
    expect(isCollectionFileId(2_000_000_000)).toBe(false);
    expect(isCollectionFileId(2 ** 52 - 1)).toBe(false);
  });
});

describe("isCollectionFolder", () => {
  it("should be true when the labelled folder has kind collection", () => {
    const status = aStatus({ folders: [...aStatus().folders, aCollectionStatus()] });
    expect(isCollectionFolder(status, "Papers")).toBe(true);
  });

  it("should treat a folder with no kind as a sync folder", () => {
    expect(isCollectionFolder(aStatus(), "CameraRoll")).toBe(false);
  });

  it("should be false when the label is unknown or empty or the status is missing", () => {
    expect(isCollectionFolder(aStatus(), "Nope")).toBe(false);
    expect(isCollectionFolder(aStatus(), "")).toBe(false);
    expect(isCollectionFolder(null, "Papers")).toBe(false);
    expect(isCollectionFolder(undefined, "Papers")).toBe(false);
  });
});
