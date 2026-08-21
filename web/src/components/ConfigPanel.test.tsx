import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { aFolderConfig, aFolderStatus, aStatus } from "../test/fixtures";
import { renderWithToasts, stubFetch } from "../test/harness";
import { ConfigPanel } from "./ConfigPanel";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
});

function baseRoutes(folder = aFolderConfig()) {
  return [
    { url: "/api/v1/folders?", body: [folder] },
    { url: "/api/v1/folders", body: [folder] },
    { url: `/api/v1/folders/${folder.folder_label}/persistent`, body: [] },
    { url: "/api/v1/events", body: [] },
  ];
}

describe("ConfigPanel", () => {
  it("should show a card per folder once reconciliation has seeded the catalog", async () => {
    stub = stubFetch(baseRoutes());
    renderWithToasts(
      <ConfigPanel status={aStatus()} selectedFolder="CameraRoll" onSelectFolder={() => {}} onChanged={() => {}} />,
    );

    // On a fresh install this panel used to show only "No folder configs in
    // database", which also made the pattern editor unreachable: it rendered
    // behind a button inside a card that never existed.
    expect(await screen.findByText("CameraRoll")).toBeInTheDocument();
    expect(screen.getByText("/home/tjmisko/CameraRoll")).toBeInTheDocument();
  });

  it("should send ONLY the budget when the budget is edited", async () => {
    const folder = aFolderConfig();
    stub = stubFetch([
      ...baseRoutes(folder),
      { method: "PUT", url: "/api/v1/folders/CameraRoll", body: { ...folder, budget_bytes: 30_000_000_000 } },
    ]);
    const onChanged = vi.fn();

    renderWithToasts(
      <ConfigPanel status={aStatus()} selectedFolder="CameraRoll" onSelectFolder={() => {}} onChanged={onChanged} />,
    );

    const input = await screen.findByLabelText("Budget");
    expect(input).toHaveValue("20 GB");
    await userEvent.clear(input);
    await userEvent.type(input, "30 GB");
    await userEvent.tab();

    await waitFor(() => expect(stub!.calls.some((c) => c.method === "PUT")).toBe(true));
    const put = stub.calls.find((c) => c.method === "PUT");
    // A Partial<FolderConfig> body would have carried syncthing_path="",
    // grace_minutes=0 and enabled=false along with the budget, disabling the
    // folder on its first save.
    expect(put?.body).toEqual({ budget_bytes: 30_000_000_000 });
    expect(onChanged).toHaveBeenCalled();
  });

  it("should refuse a budget the server would reject, without asking it", async () => {
    stub = stubFetch(baseRoutes());
    renderWithToasts(
      <ConfigPanel status={aStatus()} selectedFolder="CameraRoll" onSelectFolder={() => {}} onChanged={() => {}} />,
    );

    const input = await screen.findByLabelText("Budget");
    await userEvent.clear(input);
    await userEvent.type(input, "0 GB");
    await userEvent.tab();

    expect(await screen.findByText(/Enter a positive size/)).toBeInTheDocument();
    expect(stub.calls.some((c) => c.method === "PUT")).toBe(false);
  });

  it("should accept a zero grace period but not a negative one", async () => {
    const folder = aFolderConfig();
    stub = stubFetch([
      ...baseRoutes(folder),
      { method: "PUT", url: "/api/v1/folders/CameraRoll", body: { ...folder, grace_minutes: 0 } },
    ]);

    renderWithToasts(
      <ConfigPanel status={aStatus()} selectedFolder="CameraRoll" onSelectFolder={() => {}} onChanged={() => {}} />,
    );

    const grace = await screen.findByLabelText("Grace period (minutes)");
    await userEvent.clear(grace);
    await userEvent.type(grace, "0");
    await userEvent.tab();

    await waitFor(() => expect(stub!.calls.find((c) => c.method === "PUT")?.body).toEqual({ grace_minutes: 0 }));

    await userEvent.clear(grace);
    await userEvent.type(grace, "-5");
    await userEvent.tab();
    expect(await screen.findByText(/Enter 0 or more minutes/)).toBeInTheDocument();
    expect(stub.calls.filter((c) => c.method === "PUT")).toHaveLength(1);
  });

  it("should say plainly when a persistent pattern protects nothing", async () => {
    const folder = aFolderConfig();
    stub = stubFetch([
      ...baseRoutes(folder),
      {
        url: "/api/v1/folders/CameraRoll/persistent",
        body: [
          { id: 1, folder_label: "CameraRoll", pattern: "Albums/Starred/", match_count: 42 },
          { id: 2, folder_label: "CameraRoll", pattern: "Nope/*", match_count: 0 },
        ],
      },
    ]);

    renderWithToasts(
      <ConfigPanel status={aStatus()} selectedFolder="CameraRoll" onSelectFolder={() => {}} onChanged={() => {}} />,
    );

    expect(await screen.findByText(/protects 42 files in this folder/)).toBeInTheDocument();
    // A pattern that protects nothing looks identical to one that protects
    // everything until it says so, and the owner has trusted their favourites
    // to it.
    expect(screen.getByText(/protects 0 files in this folder -- check the pattern/)).toBeInTheDocument();
  });

  it("should read only warning-class events for the folder's activity list", async () => {
    stub = stubFetch([
      ...baseRoutes(),
      {
        url: "/api/v1/events",
        body: [
          {
            id: 3,
            timestamp: "2026-08-20T10:00:00Z",
            event_type: "folder_wedged",
            folder: "CameraRoll",
            path: "",
            details: "24.1 GB pinned exceeds the 20 GB budget",
          },
        ],
      },
    ]);

    renderWithToasts(
      <ConfigPanel status={aStatus()} selectedFolder="CameraRoll" onSelectFolder={() => {}} onChanged={() => {}} />,
    );

    expect(await screen.findByText("folder_wedged")).toBeInTheDocument();
    // Without class=warning, one enforcement pass writes hundreds of archive
    // rows and the last ten events are all of them.
    const eventsUrl = stub.urls().find((u) => u.includes("/events"));
    expect(eventsUrl).toContain("class=warning");
    expect(eventsUrl).toContain("folder=CameraRoll");
  });

  it("should show the live wedge and over-budget state on the card", async () => {
    stub = stubFetch(baseRoutes());
    renderWithToasts(
      <ConfigPanel
        status={aStatus({
          folders: [aFolderStatus({ over_budget: true, wedged: true, pinned_bytes: 24_100_000_000 })],
        })}
        selectedFolder="CameraRoll"
        onSelectFolder={() => {}}
        onChanged={() => {}}
      />,
    );

    expect(await screen.findByText("wedged")).toBeInTheDocument();
    expect(screen.getByText(/24.1 GB pinned/)).toBeInTheDocument();
  });

  it("should toggle a folder's enabled flag through the narrow update", async () => {
    const folder = aFolderConfig();
    stub = stubFetch([
      ...baseRoutes(folder),
      { method: "PUT", url: "/api/v1/folders/CameraRoll", body: { ...folder, enabled: false } },
    ]);

    renderWithToasts(
      <ConfigPanel status={aStatus()} selectedFolder="CameraRoll" onSelectFolder={() => {}} onChanged={() => {}} />,
    );

    await userEvent.click(await screen.findByLabelText("enabled"));

    await waitFor(() => expect(stub!.calls.find((c) => c.method === "PUT")?.body).toEqual({ enabled: false }));
  });
});
