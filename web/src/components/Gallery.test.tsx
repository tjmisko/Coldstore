import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { aFile } from "../test/fixtures";
import { renderWithToasts, stubFetch } from "../test/harness";
import { Gallery } from "./Gallery";
import { KeymapProvider } from "./KeymapProvider";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
});

const PAGE = [
  aFile({ id: 1, filename: "one.jpg", thumbnail_path: "CameraRoll/one.jpg.thumb.jpg" }),
  aFile({ id: 2, filename: "two.jpg", thumbnail_path: "CameraRoll/two.jpg.thumb.jpg" }),
  aFile({ id: 3, filename: "three.jpg", thumbnail_path: "CameraRoll/three.jpg.thumb.jpg" }),
  aFile({ id: 4, filename: "four.jpg", thumbnail_path: "CameraRoll/four.jpg.thumb.jpg" }),
];

function renderGallery(files = PAGE, filterKey: "all" | "conflicts" = "all") {
  stub = stubFetch([{ url: "/api/v1/files", body: files }]);
  const onFilterChange = vi.fn();
  // Inside the provider, because the keyboard is no longer three window
  // listeners the components own. Every assertion below is unchanged.
  const view = renderWithToasts(
    <KeymapProvider>
      <Gallery
        folder="CameraRoll"
        filterKey={filterKey}
        onFilterChange={onFilterChange}
        onFolderChanged={() => {}}
      />
    </KeymapProvider>,
  );
  return { ...view, onFilterChange };
}

function tile(id: number): HTMLElement {
  return document.querySelector(`[data-file-id="${id}"]`) as HTMLElement;
}

describe("Gallery", () => {
  it("should open the lightbox on a plain click", async () => {
    renderGallery();
    await screen.findByLabelText("one.jpg");

    await userEvent.click(tile(1));

    expect(await screen.findByRole("button", { name: "Download" })).toBeInTheDocument();
  });

  it("should toggle selection on ctrl-click instead of opening the lightbox", async () => {
    renderGallery();
    await screen.findByLabelText("one.jpg");

    fireEvent.click(tile(1), { ctrlKey: true });

    expect(tile(1).dataset.selected).toBe("true");
    expect(screen.queryByRole("button", { name: "Download" })).toBeNull();
    expect(screen.getByText("1 selected")).toBeInTheDocument();
  });

  it("should select a contiguous range on shift-click", async () => {
    renderGallery();
    await screen.findByLabelText("one.jpg");

    fireEvent.click(tile(1), { ctrlKey: true });
    fireEvent.click(tile(3), { shiftKey: true });

    expect(screen.getByText("3 selected")).toBeInTheDocument();
    expect(tile(2).dataset.selected).toBe("true");
    expect(tile(4).dataset.selected).toBe("false");
  });

  it("should pin from the tile without opening the lightbox, and repaint the badge", async () => {
    renderGallery();
    await screen.findByLabelText("one.jpg");

    stub!.route({
      method: "POST",
      url: "/api/v1/files/1/pin",
      body: {
        file: { ...PAGE[0], pinned: true, pin_source: "user", pinned_at: 1_770_000_000 },
        folder: null,
        warning: null,
      },
    });

    await userEvent.click(screen.getByRole("button", { name: "Pin one.jpg" }));

    // A checkbox and a pin inside a <button> tile would have opened the
    // lightbox on the same click.
    expect(screen.queryByRole("button", { name: "Download" })).toBeNull();
    await waitFor(() => expect(within(tile(1)).getByText("pinned")).toBeInTheDocument());
  });

  it("should pass the daemon's pin warning on to the owner", async () => {
    const { toasts } = renderGallery();
    await screen.findByLabelText("one.jpg");

    stub!.route({
      method: "POST",
      url: "/api/v1/files/1/pin",
      body: {
        file: { ...PAGE[0], pinned: true, pin_source: "user" },
        folder: null,
        warning: "this file is in cold storage; the pin will take effect when you restore it",
      },
    });

    await userEvent.click(screen.getByRole("button", { name: "Pin one.jpg" }));

    await waitFor(() =>
      expect(toasts.messages().join(" ")).toContain("the pin will take effect when you restore it"),
    );
  });

  it("should apply tags by mouse to the selection through one exact bulk request", async () => {
    renderGallery();
    await screen.findByLabelText("one.jpg");
    fireEvent.click(tile(1), { ctrlKey: true });
    fireEvent.click(tile(2), { ctrlKey: true });
    stub!.route({ url: "/api/v1/tags", body: [] });
    stub!.route({
      method: "POST",
      url: "/api/v1/files/tags",
      body: {
        updated: 2,
        created: ["Trips/Iceland"],
        undo: { changes: [{ ids: [1, 2], remove: ["Trips/Iceland"] }] },
      },
    });

    fireEvent.contextMenu(tile(1), { clientX: 40, clientY: 40 });
    await userEvent.click(screen.getByRole("menuitem", { name: "Add tags (2 files)" }));
    expect(screen.getByText("Tag 2 files")).toHaveAttribute("data-tag-prompt-count");
    await userEvent.type(screen.getByLabelText("Tags to add"), "Trips/Iceland");
    await screen.findByRole("button", { name: /new tag “Trips\/Iceland”/i });
    await userEvent.click(screen.getByRole("button", { name: "Apply tags" }));

    await waitFor(() => expect(stub!.calls.find((call) => call.method === "POST" && call.url === "/api/v1/files/tags")?.body)
      .toEqual({ ids: [1, 2], add: ["Trips/Iceland"], remove: [] }));
  });

  it("should expose the untagged work queue as a mouse filter", async () => {
    const { onFilterChange } = renderGallery();
    await screen.findByLabelText("one.jpg");

    await userEvent.click(screen.getByRole("tab", { name: "Untagged" }));
    expect(onFilterChange).toHaveBeenCalledWith("untagged");
  });

  it("should offer both resolutions on a conflicted file and post the owner's choice", async () => {
    const conflicted = [aFile({ id: 9, filename: "recovered.jpg", status: "archived" })];
    renderGallery(conflicted, "conflicts");
    await screen.findByLabelText("recovered.jpg");

    stub!.route({ method: "POST", url: "/api/v1/files/9/resolve", body: conflicted[0] });

    await userEvent.click(screen.getByRole("button", { name: "Keep device" }));

    await waitFor(() =>
      expect(stub!.calls.find((c) => c.url.includes("/resolve"))?.body).toEqual({ keep: "device" }),
    );
  });

  it("should flag a file whose thumbnail the daemon has given up on", async () => {
    renderGallery([
      aFile({
        id: 5,
        filename: "screen_recording.mkv",
        extension: ".mkv",
        thumb_attempts: 3,
        thumb_error: "ffmpeg: Invalid data found when processing input",
      }),
    ]);

    const glyph = await screen.findByLabelText("thumbnail failed");
    expect(glyph).toHaveAttribute("title", "ffmpeg: Invalid data found when processing input");
  });

  it("should select every loaded tile on ctrl+A and clear on Escape", async () => {
    renderGallery();
    await screen.findByLabelText("one.jpg");

    fireEvent.keyDown(window, { key: "a", ctrlKey: true });
    expect(screen.getByText("4 selected")).toBeInTheDocument();

    fireEvent.keyDown(window, { key: "Escape" });
    await waitFor(() => expect(screen.queryByText("4 selected")).toBeNull());
  });

  it("should leave typing in an input alone", async () => {
    renderGallery();
    await screen.findByLabelText("one.jpg");

    const input = document.createElement("input");
    document.body.appendChild(input);
    input.focus();
    fireEvent.keyDown(input, { key: "a", ctrlKey: true });

    // Select-all inside a text box means select the text, not the photos.
    expect(screen.queryByText("4 selected")).toBeNull();
    input.remove();
  });

  it("should open the description editor with i and keep j as caption text", async () => {
    renderGallery();
    await screen.findByLabelText("one.jpg");
    stub!.route({
      method: "PATCH",
      url: "/api/v1/files/1",
      body: { ...PAGE[0], description: "j" },
    });

    fireEvent.keyDown(window, { key: "i" });
    const textarea = await screen.findByRole("textbox", { name: "Description for one.jpg" });
    await userEvent.type(textarea, "j");

    expect(textarea).toHaveValue("j");
    expect(tile(1).dataset.cursor).toBe("true");
    expect(tile(2).dataset.cursor).toBe("false");

    fireEvent.keyDown(textarea, { key: "Escape" });
    await waitFor(() =>
      expect(stub!.calls.find((call) => call.method === "PATCH")?.body).toEqual({ description: "j" }),
    );
  });

  it("should group capture-sorted files by day and preserve raw date headings", async () => {
    renderGallery([
      aFile({ id: 1, filename: "one.jpg", captured_local: "2024-03-02 10:00:00", captured_offset_minutes: null }),
      aFile({ id: 2, filename: "two.jpg", captured_local: "2024-03-02 09:00:00" }),
      aFile({ id: 3, filename: "three.jpg", captured_local: "2024-03-01 23:00:00" }),
      aFile({ id: 4, filename: "four.jpg", captured_local: "", meta_scanned_at: 100 }),
    ]);
    await screen.findByLabelText("one.jpg");

    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Sort gallery" }), "captured");

    expect(await screen.findByRole("heading", { name: "March 2, 2024" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "March 1, 2024" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "No capture date" })).toBeInTheDocument();
    expect(document.querySelector('[data-capture-day="2024-03-02"]')).not.toBeNull();
    expect(document.querySelector('[data-capture-date="2024-03-02 10:00:00"]')).not.toBeNull();
    expect(screen.getAllByText("Timezone not recorded").length).toBeGreaterThan(0);
    expect(stub!.urls().some((url) => url.includes("sort=captured"))).toBe(true);
  });

  it("should navigate capture days with braces and counted jumps", async () => {
    renderGallery([
      aFile({ id: 1, filename: "one.jpg", captured_local: "2024-03-02 10:00:00" }),
      aFile({ id: 2, filename: "two.jpg", captured_local: "2024-03-02 09:00:00" }),
      aFile({ id: 3, filename: "three.jpg", captured_local: "2024-03-01 23:00:00" }),
      aFile({ id: 4, filename: "four.jpg", captured_local: "", meta_scanned_at: 100 }),
    ]);
    await screen.findByLabelText("one.jpg");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Sort gallery" }), "captured");
    await screen.findByRole("heading", { name: "March 2, 2024" });

    fireEvent.keyDown(window, { key: "}", shiftKey: true });
    await waitFor(() => expect(tile(3).dataset.cursor).toBe("true"));

    fireEvent.keyDown(window, { key: "{", shiftKey: true });
    await waitFor(() => expect(tile(1).dataset.cursor).toBe("true"));

    fireEvent.keyDown(window, { key: "2" });
    fireEvent.keyDown(window, { key: "}", shiftKey: true });
    await waitFor(() => expect(tile(4).dataset.cursor).toBe("true"));
  });

  it("should finish a capture-day jump after paging past a day boundary", async () => {
    const firstPage = Array.from({ length: 60 }, (_, index) =>
      aFile({
        id: index + 1,
        filename: `same-day-${index + 1}.jpg`,
        captured_local: `2024-03-02 10:00:${String(59 - index).padStart(2, "0")}`,
      }),
    );
    const nextPage = [
      aFile({ id: 61, filename: "older-day.jpg", captured_local: "2024-03-01 23:00:00" }),
    ];
    stub = stubFetch([
      { url: /\/api\/v1\/files\?.*offset=0/, body: firstPage },
      { url: /\/api\/v1\/files\?.*offset=60/, body: nextPage },
    ]);
    renderWithToasts(
      <KeymapProvider>
        <Gallery
          folder="CameraRoll"
          filterKey="all"
          onFilterChange={() => {}}
          onFolderChanged={() => {}}
        />
      </KeymapProvider>,
    );
    await screen.findByLabelText("same-day-1.jpg");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Sort gallery" }), "captured");
    await screen.findByRole("heading", { name: "March 2, 2024" });

    fireEvent.keyDown(window, { key: "}", shiftKey: true });

    await screen.findByLabelText("older-day.jpg");
    await waitFor(() => expect(tile(61).dataset.cursor).toBe("true"));
    expect(stub.urls().some((url) => url.includes("offset=60"))).toBe(true);
  });

  it("should expose location editing, display, and nearby filtering to pointer users", async () => {
    const located = aFile({ id: 1, filename: "zero.jpg", gps_lat: 0, gps_lon: 0, gps_source: "manual" });
    renderGallery([located]);
    await screen.findByLabelText("zero.jpg");

    fireEvent.contextMenu(tile(1), { clientX: 24, clientY: 48 });
    await userEvent.click(screen.getByRole("menuitem", { name: "Location and nearby files" }));
    expect(screen.getByRole("textbox", { name: "Latitude" })).toHaveValue("0");
    expect(screen.getByRole("textbox", { name: "Longitude" })).toHaveValue("0");

    const radius = screen.getByRole("textbox", { name: "Nearby radius in kilometres" });
    await userEvent.clear(radius);
    await userEvent.type(radius, "10");
    await userEvent.click(screen.getByRole("button", { name: "Show nearby" }));

    await waitFor(() => expect(stub!.urls().some((url) => url.includes("near=0%2C0%2C10"))).toBe(true));
    expect(screen.getByRole("button", { name: "Clear nearby filter within 10 kilometres" })).toBeInTheDocument();

    await userEvent.click(tile(1));
    await userEvent.click(screen.getByRole("button", { name: "Info" }));
    expect(screen.getByText("0.000000, 0.000000", { exact: false })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Location" })).toBeInTheDocument();
  });
});
