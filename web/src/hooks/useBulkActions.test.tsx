import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { aCollectionFile, aFile, aJob } from "../test/fixtures";
import { renderWithToasts, stubFetch } from "../test/harness";
import type { Selection } from "./useSelection";
import { useBulkActions, type BulkOperator } from "./useBulkActions";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
});

const SELECTION = {
  selected: new Set<number>(),
  isSelected: () => false,
  toggle: vi.fn(),
  toggleRange: vi.fn(),
  selectMany: vi.fn(),
  selectExactly: vi.fn(),
  clear: vi.fn(),
  count: 0,
  ids: [],
} as Selection;

function Probe({
  ids,
  operator,
  folder,
  onFinished,
}: {
  ids: number[];
  operator: BulkOperator;
  folder: string;
  onFinished(ids: number[]): void;
}) {
  const bulk = useBulkActions({ selection: SELECTION, filters: { folder }, onFinished });
  return (
    <>
      <button onClick={() => bulk.perform(operator, ids)}>go</button>
      <button onClick={() => bulk.runJob("archive", ids)}>archive</button>
      <span data-busy>{String(bulk.busy)}</span>
    </>
  );
}

const COLLECTION_IDS = [aCollectionFile().id, aCollectionFile().id];

describe("useBulkActions on collection files", () => {
  it("should post one collection trash request and no job when collection files are trashed", async () => {
    stub = stubFetch([
      { method: "POST", url: "/api/v1/collections/Papers/trash", body: { updated: 2 } },
    ]);
    renderWithToasts(<Probe ids={COLLECTION_IDS} operator="trash" folder="Papers" onFinished={() => {}} />);

    await userEvent.click(screen.getByText("go"));

    await waitFor(() => expect(stub!.calls).toHaveLength(1));
    expect(stub.calls[0]).toMatchObject({
      method: "POST",
      url: "/api/v1/collections/Papers/trash",
      body: { ids: COLLECTION_IDS, trashed: true },
    });
    expect(stub.urls().some((url) => url.startsWith("/api/v1/jobs"))).toBe(false);
  });

  it("should post trashed false when collection files are restored from trash", async () => {
    stub = stubFetch([
      { method: "POST", url: "/api/v1/collections/Papers/trash", body: { updated: 2 } },
    ]);
    renderWithToasts(<Probe ids={COLLECTION_IDS} operator="untrash" folder="Papers" onFinished={() => {}} />);

    await userEvent.click(screen.getByText("go"));

    await waitFor(() => expect(stub!.calls).toHaveLength(1));
    expect(stub.calls[0].body).toEqual({ ids: COLLECTION_IDS, trashed: false });
  });

  it("should repaint the trashed rows when the request succeeds", async () => {
    stub = stubFetch([
      { method: "POST", url: "/api/v1/collections/Papers/trash", body: { updated: 2 } },
    ]);
    const onFinished = vi.fn();
    const { toasts } = renderWithToasts(
      <Probe ids={COLLECTION_IDS} operator="trash" folder="Papers" onFinished={onFinished} />,
    );

    await userEvent.click(screen.getByText("go"));

    await waitFor(() => expect(onFinished).toHaveBeenCalledWith(COLLECTION_IDS));
    expect(onFinished).toHaveBeenCalledTimes(1);
    expect(toasts.messages().join(" ")).toContain("Moved 2 files to the archive's trash");
  });

  it("should show the error and change nothing when the trash request fails", async () => {
    stub = stubFetch([
      {
        method: "POST",
        url: "/api/v1/collections/Papers/trash",
        status: 400,
        body: { error: "id outside this collection" },
      },
    ]);
    const onFinished = vi.fn();
    const { toasts } = renderWithToasts(
      <Probe ids={COLLECTION_IDS} operator="trash" folder="Papers" onFinished={onFinished} />,
    );

    await userEvent.click(screen.getByText("go"));

    await waitFor(() => expect(toasts.messages()).toContain("id outside this collection"));
    expect(onFinished).not.toHaveBeenCalled();
    await waitFor(() => expect(screen.getByText("false", { selector: "[data-busy]" })).toBeInTheDocument());
  });

  it("should not post a job when archive is invoked on collection files", async () => {
    stub = stubFetch([]);
    const { toasts } = renderWithToasts(
      <Probe ids={COLLECTION_IDS} operator="archive" folder="Papers" onFinished={() => {}} />,
    );

    await userEvent.click(screen.getByText("go"));
    await userEvent.click(screen.getByText("archive"));

    expect(stub.calls).toEqual([]);
    expect(toasts.messages()).toContain("archive is not available for a collection");
  });

  it("should not ask for confirmation or call anything when pin is invoked on many collection files", async () => {
    stub = stubFetch([]);
    const many = Array.from({ length: 30 }, () => aCollectionFile().id);
    const { toasts } = renderWithToasts(
      <Probe ids={many} operator="pin" folder="Papers" onFinished={() => {}} />,
    );

    await userEvent.click(screen.getByText("go"));

    expect(stub.calls).toEqual([]);
    expect(toasts.messages()).toContain("pin is not available for a collection");
  });

  it("should still create a job when legacy files are trashed", async () => {
    stub = stubFetch([
      {
        method: "POST",
        url: "/api/v1/jobs",
        status: 202,
        body: { job_id: 7, action: "trash", state: "pending", total: 1 },
      },
      { url: "/api/v1/jobs/7", body: aJob({ id: 7, action: "trash", state: "running" }) },
    ]);
    const legacy = [aFile().id];
    renderWithToasts(<Probe ids={legacy} operator="trash" folder="CameraRoll" onFinished={() => {}} />);

    await userEvent.click(screen.getByText("go"));

    await waitFor(() => expect(stub!.calls.some((c) => c.url === "/api/v1/jobs")).toBe(true));
    expect(stub.calls.some((c) => c.url.includes("/collections/"))).toBe(false);
  });
});
