import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { aFolderStatus, aStatus } from "../test/fixtures";
import { renderWithToasts, stubFetch } from "../test/harness";
import { StatusBar } from "./StatusBar";

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

    renderWithToasts(
      <StatusBar status={aStatus({ folders: [folder] })} error={null} onReview={() => {}} onRefresh={() => {}} />,
    );

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
      <StatusBar status={aStatus({ folders: [atBudget, over] })} error={null} onReview={() => {}} onRefresh={() => {}} />,
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

    renderWithToasts(
      <StatusBar status={aStatus({ folders: [folder] })} error={null} onReview={onReview} onRefresh={() => {}} />,
    );

    const banner = await screen.findByRole("alert");
    expect(banner).toHaveTextContent("48 kB pinned exceeds 20 kB budget");
    expect(banner).toHaveTextContent("nothing left to offload");

    await userEvent.click(screen.getByRole("button", { name: /Review auto-pinned files/ }));
    // A remedy the owner cannot act on in one click is not a remedy.
    expect(onReview).toHaveBeenCalledWith("Backups", "pinned-auto");
  });

  it("should render an error line instead of vanishing when status cannot be read", async () => {
    stub = noEvents();
    renderWithToasts(
      <StatusBar status={null} error="401 Unauthorized" onReview={() => {}} onRefresh={() => {}} />,
    );

    // `if (!status) return null` made a rejected password and an empty install
    // look identical: nothing at all.
    expect(await screen.findByText(/status unavailable/)).toHaveTextContent("401 Unauthorized");
  });

  it("should mark disabled and orphaned folders rather than hiding them", async () => {
    stub = noEvents();
    renderWithToasts(
      <StatusBar
        status={aStatus({
          folders: [
            aFolderStatus({ label: "Documents", enabled: false }),
            aFolderStatus({ label: "OldPhone", present_in_toml: false }),
          ],
        })}
        error={null}
        onReview={() => {}}
        onRefresh={() => {}}
      />,
    );

    expect(await screen.findByText("(disabled)")).toBeInTheDocument();
    expect(screen.getByText("(not in coldstore.toml)")).toBeInTheDocument();
  });

  it("should warn about a folder Syncthing does not let coldstore delete from", async () => {
    stub = noEvents();
    renderWithToasts(
      <StatusBar
        status={aStatus({ folders: [aFolderStatus({ syncthing_type: "receiveonly" })] })}
        error={null}
        onReview={() => {}}
        onRefresh={() => {}}
      />,
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
      <StatusBar status={aStatus()} error={null} onReview={() => {}} onRefresh={onRefresh} />,
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
      <StatusBar status={aStatus()} error={null} onReview={() => {}} onRefresh={() => {}} />,
    );

    const strip = await screen.findByText(/2483 files/);
    expect(strip).toHaveTextContent("CameraRoll");
    await userEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    await waitFor(() => expect(screen.queryByText(/2483 files/)).toBeNull());
  });
});
