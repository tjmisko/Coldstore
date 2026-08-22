import { fireEvent, screen, waitFor } from "@testing-library/react";
import { render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import App from "./App";
import { aFile, aStatus } from "./test/fixtures";
import { stubFetch } from "./test/harness";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
});

describe("App", () => {
  it("should explain a rejected credential instead of rendering an empty shell", async () => {
    stub = stubFetch([{ url: "/api/v1/status", status: 401, body: { error: "unauthorized" } }]);

    render(<App />);

    // Any API failure used to produce an empty sidebar, a null StatusBar and
    // "Select a folder from the sidebar" -- with no message anywhere.
    expect(await screen.findByText("Not authorised")).toBeInTheDocument();
    expect(
      screen.getByText(/username and password in coldstore.toml/),
    ).toBeInTheDocument();
  });

  it("should say the daemon is unreachable when the request never lands", async () => {
    stub = stubFetch([]);
    stub.route({
      url: "/api/v1/status",
      get body(): never {
        throw new TypeError("Failed to fetch");
      },
    });

    render(<App />);

    expect(await screen.findByText("Cannot reach coldstore")).toBeInTheDocument();
    expect(screen.getByText(/daemon may not be running/)).toBeInTheDocument();
  });

  it("should select the first folder as soon as status arrives", async () => {
    stub = stubFetch([
      { url: "/api/v1/status", body: aStatus() },
      { url: "/api/v1/files", body: [] },
      { url: "/api/v1/events", body: [] },
    ]);

    render(<App />);

    expect(await screen.findByRole("heading", { name: "CameraRoll", level: 2 })).toBeInTheDocument();
  });

  it("should name the tab after the selected folder instead of Vite's scaffold title", async () => {
    stub = stubFetch([
      { url: "/api/v1/status", body: aStatus() },
      { url: "/api/v1/files", body: [] },
      { url: "/api/v1/events", body: [] },
    ]);

    render(<App />);

    // The shipped build said "web" in every tab, because index.html still
    // carried the `npm create vite` default.
    await screen.findByRole("heading", { name: "CameraRoll", level: 2 });
    expect(document.title).toBe("CameraRoll — Coldstore");
  });

  it("should render a cosmetic folder name while keeping the stable label in selectors", async () => {
    stub = stubFetch([
      {
        url: "/api/v1/status",
        body: aStatus({
          folders: [{ ...aStatus().folders[0], display_name: "Family Photos" }],
        }),
      },
      { url: "/api/v1/files", body: [] },
      { url: "/api/v1/events", body: [] },
    ]);
    render(<App />);

    expect(
      await screen.findByRole("heading", { name: "Family Photos", level: 2 }),
    ).toBeInTheDocument();
    expect(document.querySelector('[data-folder-nav="CameraRoll"]')).toHaveTextContent(
      "Family Photos",
    );
    expect(document.title).toBe("Family Photos — Coldstore");
  });

  it("should name the tab Settings when the config view is showing", async () => {
    stub = stubFetch([
      { url: "/api/v1/status", body: aStatus() },
      { url: "/api/v1/files", body: [] },
      { url: "/api/v1/events", body: [] },
    ]);

    render(<App />);

    await userEvent.click(await screen.findByRole("button", { name: /Settings/ }));

    expect(document.title).toBe("Settings — Coldstore");
  });

  it("should keep the active folder, view, and filters across a refresh", async () => {
    localStorage.setItem("coldstore.workspace.v1", JSON.stringify({
      folder: "Documents",
      allFolders: false,
      view: "files",
      filterKey: "archived",
      tags: ["Work/Reports"],
    }));
    stub = stubFetch([
      {
        url: "/api/v1/status",
        body: aStatus({
          folders: [
            { ...aStatus().folders[0], label: "Articles" },
            { ...aStatus().folders[0], label: "Documents" },
          ],
        }),
      },
      { url: "/api/v1/files", body: [] },
      { url: "/api/v1/events", body: [] },
    ]);

    render(<App />);

    expect(await screen.findByRole("heading", { name: "Documents", level: 2 })).toBeInTheDocument();
    expect(document.querySelector('[data-view-nav="files"]')).toHaveAttribute("aria-current", "page");
    await waitFor(() => {
      const request = stub!.urls().find((url) => url.startsWith("/api/v1/files?"));
      const params = new URLSearchParams(request?.split("?")[1]);
      expect(params.get("folder")).toBe("Documents");
      expect(params.get("status")).toBe("archived");
      expect(params.getAll("tag")).toEqual(["Work/Reports"]);
    });
  });

  it("should filter by tags inside one folder and across all folders", async () => {
    stub = stubFetch([
      { url: "/api/v1/status", body: aStatus() },
      { url: "/api/v1/files", body: [] },
      { url: "/api/v1/events", body: [] },
      { url: "/api/v1/tags", body: [{ id: 1, name: "Trips", count: 3, descendant_count: 3 }] },
    ]);
    render(<App />);
    await screen.findByRole("heading", { name: "CameraRoll", level: 2 });

    const input = screen.getByRole("combobox", { name: "Filter CameraRoll by tag" });
    await userEvent.type(input, "Trips");
    await userEvent.click(screen.getByRole("button", { name: "Add" }));

    await waitFor(() => expect(stub!.urls().some((url) => {
      if (!url.startsWith("/api/v1/files?")) return false;
      const params = new URLSearchParams(url.split("?")[1]);
      return params.get("folder") === "CameraRoll" && params.get("tag") === "Trips";
    })).toBe(true));

    await userEvent.click(screen.getByRole("button", { name: /All folders/ }));
    expect(await screen.findByRole("heading", { name: "All folders", level: 2 })).toBeInTheDocument();
    await waitFor(() => expect(stub!.urls().some((url) => {
      if (!url.startsWith("/api/v1/files?")) return false;
      const params = new URLSearchParams(url.split("?")[1]);
      return !params.has("folder") && params.get("tag") === "Trips";
    })).toBe(true));
  });

  it("should replace the finder with a preview when Enter opens its result", async () => {
    const file = aFile({ id: 44, filename: "concert.jpg", relative_path: "events/concert.jpg" });
    stub = stubFetch([
      { url: "/api/v1/status", body: aStatus() },
      { url: "/api/v1/files", body: [file] },
      { url: "/api/v1/events", body: [] },
    ]);
    render(<App />);
    await screen.findByRole("heading", { name: "CameraRoll", level: 2 });

    fireEvent.keyDown(window, { key: "F10" });
    const input = await screen.findByRole("textbox", { name: "Find a file" });
    await screen.findByRole("option", { name: /concert.jpg/ });
    fireEvent.keyDown(input, { key: "Enter" });

    expect(await screen.findByRole("dialog", { name: "Preview concert.jpg" })).toBeInTheDocument();
    expect(document.querySelector('[data-finder-open-preview="44"] [data-preview-image]')).not.toBeNull();
    expect(document.querySelector("[data-fuzzy-finder]")).toBeNull();
  });

  it("should switch to Files and focus the complete finder result with Ctrl-g", async () => {
    const file = aFile({ id: 45, filename: "far-away.jpg", relative_path: "events/far-away.jpg" });
    stub = stubFetch([
      { url: "/api/v1/status", body: aStatus() },
      { url: "/api/v1/files", body: [file] },
      { url: "/api/v1/events", body: [] },
    ]);
    render(<App />);
    await screen.findByRole("heading", { name: "CameraRoll", level: 2 });

    fireEvent.keyDown(window, { key: "F10" });
    const input = await screen.findByRole("textbox", { name: "Find a file" });
    await screen.findByRole("option", { name: /far-away.jpg/ });
    fireEvent.keyDown(input, { key: "g", ctrlKey: true });

    await waitFor(() => {
      const target = document.querySelector<HTMLElement>('tr[data-file-id="45"]');
      expect(target).toHaveAttribute("data-cursor", "true");
      expect(target).toBe(document.activeElement);
    });
    expect(document.querySelector('[data-view-nav="files"]')).toHaveClass("bg-raised-hover");
    expect(document.querySelector("[data-fuzzy-finder]")).toBeNull();
  });
});
