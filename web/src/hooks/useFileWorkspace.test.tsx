import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { aFile } from "../test/fixtures";
import { stubFetch } from "../test/harness";
import { useFileWorkspace } from "./useFileWorkspace";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
});

describe("useFileWorkspace tag mutations", () => {
  it("retains the server's exact inverse for the later keyboard undo slice", async () => {
    const file = aFile({ id: 1, tags: [] });
    const tagged = { ...file, tags: ["Favorite"] };
    stub = stubFetch([
      { url: "/api/v1/files?folder=CameraRoll", body: [file] },
      { url: "/api/v1/files?ids=1", body: [tagged] },
      {
        method: "POST",
        url: "/api/v1/files/tags",
        body: {
          updated: 1,
          created: ["Favorite"],
          undo: { changes: [{ ids: [1], remove: ["Favorite"] }] },
        },
      },
    ]);
    const { result } = renderHook(() => useFileWorkspace({
      folder: "CameraRoll",
      filterKey: "all",
    }));
    await waitFor(() => expect(result.current.files.files).toHaveLength(1));

    await act(async () => {
      expect(await result.current.actions.tagFiles([1], ["Favorite"], [])).toBe(true);
    });

    expect(result.current.lastTagUndo).toEqual({
      changes: [{ ids: [1], remove: ["Favorite"] }],
    });
    expect(result.current.files.files[0].tags).toEqual(["Favorite"]);
  });

  it("removes a newly-tagged row from the active Untagged queue", async () => {
    const file = aFile({ id: 2, tags: [] });
    stub = stubFetch([
      { url: "/api/v1/files?folder=CameraRoll&untagged=1", body: [file] },
      {
        method: "POST",
        url: "/api/v1/files/tags",
        body: {
          updated: 1,
          created: ["Reviewed"],
          undo: { changes: [{ ids: [2], remove: ["Reviewed"] }] },
        },
      },
    ]);
    const { result } = renderHook(() => useFileWorkspace({
      folder: "CameraRoll",
      filterKey: "untagged",
    }));
    await waitFor(() => expect(result.current.files.files).toHaveLength(1));
    stub.route({ url: "/api/v1/files?folder=CameraRoll&untagged=1", body: [] });

    await act(async () => {
      expect(await result.current.actions.tagFiles([2], ["Reviewed"], [])).toBe(true);
    });

    expect(result.current.files.files).toEqual([]);
    expect(stub.calls.some((call) => call.url.includes("ids=2"))).toBe(false);
  });
});
