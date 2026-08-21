import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Selection } from "../hooks/useSelection";
import { aFile, aJob } from "../test/fixtures";
import { renderWithToasts, stubFetch } from "../test/harness";
import { SelectionBar } from "./SelectionBar";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
});

function selectionOf(ids: number[], overrides: Partial<Selection> = {}): Selection {
  const set = new Set(ids);
  return {
    selected: set,
    isSelected: (id) => set.has(id),
    toggle: vi.fn(),
    toggleRange: vi.fn(),
    selectMany: vi.fn(),
    clear: vi.fn(),
    count: set.size,
    ids,
    ...overrides,
  };
}

const FILES = [aFile({ id: 1, filename: "one.jpg" }), aFile({ id: 2, filename: "two.jpg" })];

describe("SelectionBar", () => {
  it("should report how many files are selected", () => {
    stub = stubFetch([]);
    renderWithToasts(
      <SelectionBar selection={selectionOf([1, 2, 3])} filters={{ folder: "CameraRoll" }} files={FILES} onFinished={() => {}} />,
    );

    expect(screen.getByText("3 selected")).toBeInTheDocument();
  });

  it("should render nothing at all when nothing is selected", () => {
    stub = stubFetch([]);
    const { container } = renderWithToasts(
      <SelectionBar selection={selectionOf([])} filters={{}} files={[]} onFinished={() => {}} />,
    );

    expect(container).toBeEmptyDOMElement();
  });

  it("should pin a whole selection in ONE request", async () => {
    stub = stubFetch([
      { method: "POST", url: "/api/v1/files/pin", body: { updated: 400, folders: [], warning: null } },
    ]);
    const onFinished = vi.fn();
    const ids = Array.from({ length: 400 }, (_, i) => i + 1);

    const { toasts } = renderWithToasts(
      <SelectionBar selection={selectionOf(ids)} filters={{}} files={FILES} onFinished={onFinished} />,
    );

    await userEvent.click(screen.getByRole("button", { name: "Pin" }));

    // A loop of 400 requests would serialise 800 aggregate queries behind a
    // single SQLite connection to set 400 bits.
    await waitFor(() => expect(stub!.calls.filter((c) => c.method === "POST")).toHaveLength(1));
    expect(stub.calls[0].body).toEqual({ ids, pinned: true });
    await waitFor(() => expect(onFinished).toHaveBeenCalledWith(ids));
    expect(toasts.messages().join(" ")).toContain("Pinned 400 files");
  });

  it("should pass on the daemon's wedge warning when a bulk pin causes one", async () => {
    stub = stubFetch([
      {
        method: "POST",
        url: "/api/v1/files/pin",
        body: { updated: 2, folders: [], warning: "48 kB pinned exceeds the 20 kB budget - nothing left to offload" },
      },
    ]);

    const { toasts } = renderWithToasts(
      <SelectionBar selection={selectionOf([1, 2])} filters={{}} files={FILES} onFinished={() => {}} />,
    );

    await userEvent.click(screen.getByRole("button", { name: "Pin" }));

    await waitFor(() =>
      expect(toasts.messages().join(" ")).toContain("nothing left to offload"),
    );
  });

  it("should name the eviction count in a dialog when the restore does not fit the budget", async () => {
    stub = stubFetch([
      {
        method: "POST",
        url: "/api/v1/jobs",
        status: 409,
        body: {
          error: "restoring these files would exceed the folder budget",
          folder: "CameraRoll",
          overage_bytes: 15_000_000_000,
          estimated_evictions: 2500,
        },
      },
    ]);

    renderWithToasts(
      <SelectionBar selection={selectionOf([1, 2])} filters={{}} files={FILES} onFinished={() => {}} />,
    );

    await userEvent.click(screen.getByRole("button", { name: "Restore" }));

    // Not a toast: the daemon's answer to a budget over-run is to archive a
    // comparable volume of files the owner never selected, and every archive
    // is a Syncthing deletion on their phone.
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("CameraRoll");
    expect(dialog).toHaveTextContent("15 GB");
    expect(dialog).toHaveTextContent("2,500 other files");
    expect(dialog).toHaveTextContent(/Syncthing will delete them/);
  });

  it("should say the folder will simply stay over budget when nothing is evictable", async () => {
    stub = stubFetch([
      {
        method: "POST",
        url: "/api/v1/jobs",
        status: 409,
        body: {
          error: "restoring these files would exceed the folder budget",
          folder: "Backups",
          overage_bytes: 6_000,
          estimated_evictions: 0,
        },
      },
    ]);

    renderWithToasts(
      <SelectionBar selection={selectionOf([1])} filters={{}} files={FILES} onFinished={() => {}} />,
    );

    await userEvent.click(screen.getByRole("button", { name: "Restore" }));

    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent(/every other file is pinned/);
    expect(dialog).not.toHaveTextContent("other files to make room");
  });

  it("should re-submit with force only after the owner confirms", async () => {
    stub = stubFetch([
      {
        method: "POST",
        url: "/api/v1/jobs",
        status: 409,
        body: { error: "over budget", folder: "CameraRoll", overage_bytes: 1000, estimated_evictions: 4 },
      },
    ]);

    renderWithToasts(
      <SelectionBar selection={selectionOf([1, 2])} filters={{}} files={FILES} onFinished={() => {}} />,
    );

    await userEvent.click(screen.getByRole("button", { name: "Restore" }));
    await screen.findByRole("dialog");
    expect(stub.calls[0].body).toMatchObject({ force: false });

    stub.route({
      method: "POST",
      url: "/api/v1/jobs",
      status: 202,
      body: { job_id: 11, action: "restore", state: "pending", total: 2 },
    });
    stub.route({ url: "/api/v1/jobs/11", body: aJob({ id: 11, state: "done", total: 2, done: 2 }) });

    await userEvent.click(screen.getByRole("button", { name: "Restore anyway" }));

    await waitFor(() =>
      expect(stub!.calls.filter((c) => c.method === "POST")).toHaveLength(2),
    );
    const posts = stub.calls.filter((c) => c.method === "POST");
    expect(posts[1].body).toMatchObject({ action: "restore", ids: [1, 2], force: true });
  });

  it("should show progress while a job runs and a summary of failures when it ends", async () => {
    let finish: () => void = () => {};
    const running = new Promise<void>((resolve) => {
      finish = resolve;
    });

    stub = stubFetch([
      { method: "POST", url: "/api/v1/jobs", status: 202, body: { job_id: 7, action: "restore", state: "pending", total: 3 } },
      { url: "/api/v1/jobs/7", body: aJob({ id: 7, state: "running", total: 3, done: 1 }), delay: running },
    ]);
    const onFinished = vi.fn();

    renderWithToasts(
      <SelectionBar selection={selectionOf([1, 2, 3])} filters={{}} files={FILES} onFinished={onFinished} />,
    );

    await userEvent.click(screen.getByRole("button", { name: "Restore" }));
    finish();

    expect(await screen.findByText(/Restoring 3 files/)).toHaveTextContent("1/3");
    expect(screen.getByText(/Restoring 3 files/)).toHaveTextContent("Restoring 3 files");

    stub.route({
      url: "/api/v1/jobs/7",
      body: aJob({
        id: 7,
        state: "done",
        total: 3,
        done: 2,
        failed_count: 1,
        failed: [{ id: 2, error: "folder is not currently available" }],
      }),
    });

    // The failure list names the FILE, not the row id: "file 2 failed" is not
    // something an owner can act on.
    expect(await screen.findByText(/two.jpg: folder is not currently available/)).toBeInTheDocument();
    expect(screen.getByText(/Restored 2 of 3/)).toHaveTextContent("1 failed");
    await waitFor(() => expect(onFinished).toHaveBeenCalledWith([1, 2, 3]));
  });

  it("should select every matching file in the folder and say so when the answer was truncated", async () => {
    const ids = Array.from({ length: 20_000 }, (_, i) => i + 1);
    stub = stubFetch([
      { url: "/api/v1/files/ids", body: { ids, total: 34_000, truncated: true } },
    ]);
    const selectMany = vi.fn();

    renderWithToasts(
      <SelectionBar
        selection={selectionOf([1], { selectMany })}
        filters={{ folder: "CameraRoll", status: "archived" }}
        files={FILES}
        onFinished={() => {}}
      />,
    );

    await userEvent.click(screen.getByRole("button", { name: "Select all in folder" }));

    await waitFor(() => expect(selectMany).toHaveBeenCalledWith(ids));
    // The same filters the grid is showing, or the job acts on files the owner
    // never saw.
    expect(stub.urls()[0]).toContain("folder=CameraRoll");
    expect(stub.urls()[0]).toContain("status=archived");
    expect(await screen.findByText(/Selected the first 20,000 of 34,000/)).toBeInTheDocument();
  });
});
