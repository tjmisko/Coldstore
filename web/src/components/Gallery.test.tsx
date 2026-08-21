import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { aFile } from "../test/fixtures";
import { renderWithToasts, stubFetch } from "../test/harness";
import { Gallery } from "./Gallery";

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
  const view = renderWithToasts(
    <Gallery
      folder="CameraRoll"
      filterKey={filterKey}
      onFilterChange={onFilterChange}
      onFolderChanged={() => {}}
    />,
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
});
