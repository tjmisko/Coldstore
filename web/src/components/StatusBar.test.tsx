import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { aCollectionStatus, aFolderStatus, aStatus } from "../test/fixtures";
import { renderWithToasts, stubFetch } from "../test/harness";
import { FolderNavItem, StatusBar } from "./StatusBar";

let stub: ReturnType<typeof stubFetch> | null = null;

function noEvents() {
  return stubFetch([{ url: "/api/v1/events", body: [] }]);
}

afterEach(() => {
  stub?.restore();
  stub = null;
});

describe("StatusBar", () => {
  it("should draw the pinned segment inside the total, never beyond it", async () => {
    stub = noEvents();
    // A fully pinned folder. Drawn additively -- the obvious way -- this bar
    // reads 200% of the budget.
    const folder = aFolderStatus({
      label: "Backups",
      budget_bytes: 20_000,
      synced_size_bytes: 48_000,
      pinned_bytes: 48_000,
      over_budget: true,
      wedged: true,
    });

    renderWithToasts(<FolderNavItem folder={folder} selected onSelect={() => {}} onReview={() => {}} />);

    const meter = await screen.findByRole("meter", { name: /Backups budget/ });
    const total = Number(meter.dataset.totalPct);
    const pinned = Number(meter.dataset.pinnedPct);
    expect(total).toBeLessThanOrEqual(100);
    expect(pinned).toBeLessThanOrEqual(total);

    const widths = [...meter.querySelectorAll<HTMLElement>("[data-segment]")]
      .filter((el) => el.dataset.segment !== "overflow")
      .map((el) => Number.parseFloat(el.style.width));
    expect(widths.reduce((a, b) => a + b, 0)).toBeLessThanOrEqual(100.001);
  });

  it("should mark an over-budget folder distinctly from one exactly at budget", async () => {
    stub = noEvents();
    const atBudget = aFolderStatus({
      label: "AtBudget",
      budget_bytes: 1000,
      synced_size_bytes: 1000,
      pinned_bytes: 0,
    });
    const over = aFolderStatus({
      label: "Over",
      budget_bytes: 1000,
      synced_size_bytes: 4000,
      pinned_bytes: 0,
      over_budget: true,
    });

    renderWithToasts(
      <div>
        <FolderNavItem folder={atBudget} selected={false} onSelect={() => {}} onReview={() => {}} />
        <FolderNavItem folder={over} selected={false} onSelect={() => {}} onReview={() => {}} />
      </div>,
    );

    // Both clamp to 100% width, so only the overflow cap tells them apart.
    const atMeter = await screen.findByRole("meter", { name: /AtBudget budget/ });
    const overMeter = screen.getByRole("meter", { name: /Over budget/ });
    expect(atMeter.querySelector("[data-segment='overflow']")).toBeNull();
    expect(overMeter.querySelector("[data-segment='overflow']")).not.toBeNull();
  });

  it("should show the wedge banner with both figures and land its link on the auto-pinned filter", async () => {
    stub = noEvents();
    const onReview = vi.fn();
    const folder = aFolderStatus({
      label: "Backups",
      budget_bytes: 20_000,
      synced_size_bytes: 48_000,
      pinned_bytes: 48_000,
      over_budget: true,
      wedged: true,
    });

    renderWithToasts(<FolderNavItem folder={folder} selected onSelect={() => {}} onReview={onReview} />);

    const banner = await screen.findByRole("alert");
    expect(banner).toHaveTextContent("48 kB pinned exceeds 20 kB budget");
    expect(banner).toHaveTextContent("nothing left to offload");

    await userEvent.click(screen.getByRole("button", { name: /Review auto-pinned files/ }));
    // A remedy the owner cannot act on in one click is not a remedy.
    expect(onReview).toHaveBeenCalledOnce();
  });

  it("should render an error line instead of vanishing when status cannot be read", async () => {
    stub = noEvents();
    renderWithToasts(
      <StatusBar status={null} error="401 Unauthorized" onRefresh={() => {}} />,
    );

    // `if (!status) return null` made a rejected password and an empty install
    // look identical: nothing at all.
    expect(await screen.findByText(/status unavailable/)).toHaveTextContent("401 Unauthorized");
  });

  it("should mark disabled and orphaned folders rather than hiding them", async () => {
    stub = noEvents();
    renderWithToasts(
      <div>
        <FolderNavItem folder={aFolderStatus({ label: "Documents", enabled: false })} selected={false} onSelect={() => {}} onReview={() => {}} />
        <FolderNavItem folder={aFolderStatus({ label: "OldPhone", present_in_toml: false })} selected={false} onSelect={() => {}} onReview={() => {}} />
      </div>,
    );

    expect(await screen.findByText("disabled")).toBeInTheDocument();
    expect(screen.getByText("not in coldstore.toml")).toBeInTheDocument();
  });

  it("should warn about a folder Syncthing does not let coldstore delete from", async () => {
    stub = noEvents();
    renderWithToasts(
      <FolderNavItem folder={aFolderStatus({ syncthing_type: "receiveonly" })} selected onSelect={() => {}} onReview={() => {}} />,
    );

    expect(await screen.findByText(/receiveonly Syncthing folder/)).toBeInTheDocument();
  });

  it("should run the rescan as a job and refresh the figures when it finishes", async () => {
    stub = stubFetch([
      { url: "/api/v1/events", body: [] },
      { method: "POST", url: "/api/v1/jobs", status: 202, body: { job_id: 3, action: "scan", state: "pending", total: 0 } },
      { url: "/api/v1/jobs/3", body: { id: 3, action: "scan", folder_label: "", state: "done", total: 4, done: 4, failed_count: 0, error: "", created_at: 1, started_at: 1, finished_at: 2, failed: [] } },
    ]);
    const onRefresh = vi.fn();

    renderWithToasts(
      <StatusBar status={aStatus()} error={null} onRefresh={onRefresh} />,
    );

    await userEvent.click(await screen.findByRole("button", { name: "rescan" }));

    // The scan is a job now, not a synchronous walk inside the handler, so the
    // button waits on the job rather than on the request.
    await waitFor(() => expect(onRefresh).toHaveBeenCalled());
    const posted = stub.calls.find((c) => c.method === "POST");
    expect(posted?.body).toEqual({ action: "scan" });
  });

  it("should announce a bulk eviction and let the owner dismiss it", async () => {
    stub = stubFetch([
      {
        url: "/api/v1/events",
        body: [
          {
            id: 9,
            timestamp: "2026-08-20T10:00:00Z",
            event_type: "bulk_eviction",
            folder: "CameraRoll",
            path: "",
            details: "2483 files (13.2 GB) were archived in one enforcement pass",
          },
        ],
      },
    ]);

    renderWithToasts(
      <StatusBar status={aStatus()} error={null} onRefresh={() => {}} />,
    );

    const strip = await screen.findByText(/2483 files/);
    expect(strip).toHaveTextContent("CameraRoll");
    await userEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    await waitFor(() => expect(screen.queryByText(/2483 files/)).toBeNull());
  });

  it("should show the file count and stored size and no budget bar when the folder is a collection", async () => {
    stub = noEvents();
    renderWithToasts(
      <FolderNavItem
        folder={aCollectionStatus({
          stored_bytes: 4_200_000,
          counts: { total: 14, synced: 0, archived: 12, missing: 0, trashed: 2, pinned: 0 },
        })}
        selected={false}
        onSelect={() => {}}
        onReview={() => {}}
      />,
    );

    const nav = await screen.findByRole("button", { name: /Papers/ });
    expect(nav).toHaveTextContent("12 files");
    expect(nav).toHaveTextContent("4.2 MB");
    expect(nav).toHaveTextContent("collection");
    expect(nav).toHaveTextContent("2 in trash");
    expect(screen.queryByRole("meter")).toBeNull();
    expect(screen.queryByText("disabled")).toBeNull();
    expect(screen.queryByText("budget blocked")).toBeNull();
  });

  it("should render a sync folder exactly as before when kind is absent", async () => {
    stub = noEvents();
    const folder = aFolderStatus();
    expect(folder.kind).toBeUndefined();
    renderWithToasts(<FolderNavItem folder={folder} selected onSelect={() => {}} onReview={() => {}} />);

    expect(await screen.findByRole("meter", { name: /CameraRoll budget/ })).toBeInTheDocument();
    expect(screen.queryByText("collection")).toBeNull();
  });

  it("should show an archive warning when bad_blob_count is above zero", async () => {
    stub = noEvents();
    const archive = {
      blob_count: 10,
      blob_bytes: 1000,
      bad_blob_count: 2,
      journal_seq: 40,
      scrub_running: false,
      scrub_last_finished_at: null,
    };
    renderWithToasts(<StatusBar status={aStatus({ archive })} error={null} onRefresh={() => {}} />);

    expect(await screen.findByRole("alert")).toHaveTextContent("2 archived files failed verification");
  });

  it("should show no archive warning when bad_blob_count is zero or the archive is closed", async () => {
    stub = noEvents();
    const { rerender } = renderWithToasts(
      <StatusBar
        status={aStatus({ archive: { blob_count: 1, blob_bytes: 1, bad_blob_count: 0, journal_seq: 1, scrub_running: false, scrub_last_finished_at: null } })}
        error={null}
        onRefresh={() => {}}
      />,
    );
    await screen.findByRole("button", { name: "rescan" });
    expect(document.querySelector("[data-archive-health]")).toBeNull();

    rerender(<StatusBar status={aStatus({ archive: null })} error={null} onRefresh={() => {}} />);
    expect(document.querySelector("[data-archive-health]")).toBeNull();
  });

  it("should not offer rescan when a collection is selected", async () => {
    stub = noEvents();
    renderWithToasts(<StatusBar status={aStatus()} error={null} onRefresh={() => {}} collectionSelected />);

    await screen.findByText(/Cold:/);
    expect(screen.queryByRole("button", { name: "rescan" })).toBeNull();
  });
});
