import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { aFile } from "../test/fixtures";
import { stubFetch } from "../test/harness";
import { useFiles } from "./useFiles";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
});

describe("useFiles", () => {
  it("should ask the API for exactly the filter it was given", async () => {
    stub = stubFetch([{ url: "/api/v1/files", body: [aFile()] }]);

    const { result } = renderHook(() =>
      useFiles({ folder: "CameraRoll", pinned: true, pinSource: "restore", sort: "pinned_at" }),
    );

    await waitFor(() => expect(result.current.files).toHaveLength(1));
    const url = stub.urls()[0];
    expect(url).toContain("folder=CameraRoll");
    expect(url).toContain("pinned=1");
    expect(url).toContain("pin_source=restore");
    expect(url).toContain("sort=pinned_at");
  });

  it("should not show the previous filter's rows while the new filter is loading", async () => {
    const archived = aFile({ id: 1, status: "archived" });
    const synced = aFile({ id: 2, status: "synced" });
    stub = stubFetch([
      { url: "/api/v1/files?folder=CameraRoll&status=archived", body: [archived] },
      { url: "/api/v1/files?folder=CameraRoll&status=synced", body: [synced] },
    ]);

    const { result, rerender } = renderHook(
      ({ status }: { status: "archived" | "synced" }) =>
        useFiles({ folder: "CameraRoll", status }),
      { initialProps: { status: "archived" as "archived" | "synced" } },
    );

    await waitFor(() => expect(result.current.files).toEqual([archived]));

    rerender({ status: "synced" });
    // The moment the filter changes the old rows are gone, before the new
    // request has answered: a grid captioned "Synced" showing archived files
    // is worse than an empty one.
    expect(result.current.files).toEqual([]);
    expect(result.current.loading).toBe(true);

    await waitFor(() => expect(result.current.files).toEqual([synced]));
  });

  it("should ignore a stale response when the filter changed while it was in flight", async () => {
    let releaseFirst: () => void = () => {};
    const firstInFlight = new Promise<void>((resolve) => {
      releaseFirst = resolve;
    });
    const stale = aFile({ id: 1, filename: "stale.jpg" });
    const fresh = aFile({ id: 2, filename: "fresh.jpg" });

    stub = stubFetch([
      { url: "/api/v1/files?folder=Old", body: [stale], delay: firstInFlight },
      { url: "/api/v1/files?folder=New", body: [fresh] },
    ]);

    const { result, rerender } = renderHook(
      ({ folder }: { folder: string }) => useFiles({ folder }),
      { initialProps: { folder: "Old" } },
    );

    rerender({ folder: "New" });
    await waitFor(() => expect(result.current.files).toEqual([fresh]));

    // The first request answers last. The old guard -- an early return on a
    // `loading` flag that was also a dependency -- let exactly this overwrite
    // the newer page.
    await act(async () => {
      releaseFirst();
      await firstInFlight;
    });
    expect(result.current.files).toEqual([fresh]);
  });

  it("should patch a row in place so an action does not need a reload", async () => {
    const file = aFile({ id: 5, status: "archived", pinned: false });
    stub = stubFetch([{ url: "/api/v1/files", body: [file] }]);

    const { result } = renderHook(() => useFiles({ folder: "CameraRoll" }));
    await waitFor(() => expect(result.current.files).toHaveLength(1));

    act(() => result.current.patchFile(5, { status: "synced", pinned: true, pin_source: "restore" }));

    expect(result.current.files[0]).toMatchObject({
      id: 5,
      status: "synced",
      pinned: true,
      pin_source: "restore",
    });
  });

  it("should repaint exactly the ids a job touched, chunked at the server's cap", async () => {
    const page = [aFile({ id: 1 }), aFile({ id: 2 })];
    stub = stubFetch([{ url: "/api/v1/files", body: page }]);

    const { result } = renderHook(() => useFiles({ folder: "CameraRoll" }));
    await waitFor(() => expect(result.current.files).toHaveLength(2));

    const restored = { ...page[0], status: "synced" as const, pinned: true };
    stub.route({ url: "/api/v1/files?ids=", body: [restored] });

    const ids = Array.from({ length: 501 }, (_, i) => i + 1);
    await act(async () => {
      await result.current.refetchIds(ids);
    });

    const idRequests = stub.urls().filter((u) => u.includes("ids="));
    expect(idRequests).toHaveLength(2); // 501 ids, capped at 500 per request
    expect(idRequests[0].split("ids=")[1].split(",")).toHaveLength(500);
    expect(result.current.files[0]).toMatchObject({ id: 1, status: "synced", pinned: true });
    expect(result.current.files[1]).toMatchObject({ id: 2, status: "synced" });
  });

  it("should report the daemon's error instead of an empty list", async () => {
    stub = stubFetch([{ url: "/api/v1/files", status: 500, body: { error: "failed to list files" } }]);

    const { result } = renderHook(() => useFiles({ folder: "CameraRoll" }));

    await waitFor(() => expect(result.current.error).toBe("failed to list files"));
  });
});
