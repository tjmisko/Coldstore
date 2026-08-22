import { fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { aFile } from "../test/fixtures";
import { renderWithToasts, stubFetch } from "../test/harness";
import { KeymapProvider } from "./KeymapProvider";
import { FuzzyFinder } from "./FuzzyFinder";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
  vi.restoreAllMocks();
});

const FILES = [
  aFile({ id: 21, filename: "alpha.jpg", relative_path: "photos/alpha.jpg" }),
  aFile({ id: 22, filename: "bravo.mp4", relative_path: "videos/bravo.mp4", extension: ".mp4" }),
];

function renderFinder(overrides: {
  onOpen?: (file: (typeof FILES)[number]) => void;
  onShowInFiles?: (file: (typeof FILES)[number]) => void;
} = {}) {
  stub = stubFetch([{ url: "/api/v1/files", body: FILES }]);
  const onOpen = overrides.onOpen ?? vi.fn();
  const onShowInFiles = overrides.onShowInFiles ?? vi.fn();
  const view = renderWithToasts(
    <KeymapProvider>
      <FuzzyFinder
        initialQuery=""
        filters={{ folder: "CameraRoll" }}
        onClose={() => {}}
        onOpen={onOpen}
        onShowInFiles={onShowInFiles}
      />
    </KeymapProvider>,
  );
  return { ...view, onOpen, onShowInFiles };
}

describe("FuzzyFinder", () => {
  it("should preview the active result and open its complete record with Enter", async () => {
    const onOpen = vi.fn();
    renderFinder({ onOpen });
    const input = await screen.findByRole("textbox", { name: "Find a file" });

    await waitFor(() => expect(document.querySelector('[data-finder-preview="21"]')).not.toBeNull());
    expect(document.querySelector('[data-finder-preview="21"] [data-preview-image]')).not.toBeNull();

    fireEvent.keyDown(input, { key: "Enter" });

    expect(onOpen).toHaveBeenCalledWith(FILES[0]);
  });

  it("should select results and run download without leaving the input", async () => {
    const open = vi.spyOn(window, "open").mockImplementation(() => null);
    renderFinder();
    const input = await screen.findByRole("textbox", { name: "Find a file" });
    await screen.findByRole("option", { name: /alpha.jpg/ });

    fireEvent.keyDown(input, { key: "x", ctrlKey: true });
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "x", ctrlKey: true });
    fireEvent.keyDown(input, { key: "y", ctrlKey: true });

    expect(document.querySelector('[data-finder-result="21"]')).toHaveAttribute(
      "data-finder-selected",
      "true",
    );
    expect(document.querySelector('[data-finder-result="22"]')).toHaveAttribute(
      "data-finder-selected",
      "true",
    );
    expect(open).toHaveBeenCalledTimes(2);
    expect(open.mock.calls.map(([url]) => url)).toEqual([
      "/api/v1/files/21/download",
      "/api/v1/files/22/download",
    ]);
    expect(document.activeElement).toBe(input);
  });

  it("should send the complete active record to Show in Files with Ctrl-g", async () => {
    const onShowInFiles = vi.fn();
    renderFinder({ onShowInFiles });
    const input = await screen.findByRole("textbox", { name: "Find a file" });
    await screen.findByRole("option", { name: /alpha.jpg/ });

    fireEvent.keyDown(input, { key: "g", ctrlKey: true });

    expect(onShowInFiles).toHaveBeenCalledWith(FILES[0]);
  });
});
