import { act, fireEvent, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { FileActions } from "../hooks/useFileWorkspace";
import { aFile } from "../test/fixtures";
import { renderWithToasts } from "../test/harness";
import { KeymapProvider } from "./KeymapProvider";
import { DescriptionEditor } from "./DescriptionEditor";

function fileActions(): FileActions {
  return {
    setPinned: vi.fn(async () => {}),
    restore: vi.fn(async () => {}),
    archive: vi.fn(async () => {}),
    resolve: vi.fn(async () => {}),
    download: vi.fn(),
    setDescription: vi.fn(async () => true),
    setCaptureDate: vi.fn(async () => true),
    setLocation: vi.fn(async () => true),
    tagFiles: vi.fn(async () => true),
    repeatTags: vi.fn(async () => true),
    undoTags: vi.fn(async () => true),
    redoTags: vi.fn(async () => true),
    applyQuickSlot: vi.fn(async () => true),
  };
}

function renderEditor(actions = fileActions()) {
  const files = [
    aFile({ id: 1, filename: "one.jpg", description: "first" }),
    aFile({ id: 2, filename: "two.jpg", description: "second" }),
  ];
  const onClose = vi.fn();
  const onIndexChange = vi.fn();
  const view = renderWithToasts(
    <KeymapProvider>
      <DescriptionEditor
        files={files}
        initialIndex={0}
        actions={actions}
        onClose={onClose}
        onIndexChange={onIndexChange}
      />
    </KeymapProvider>,
  );
  return { ...view, actions, onClose, onIndexChange, files };
}

afterEach(() => {
  vi.useRealTimers();
});

describe("DescriptionEditor", () => {
  it("should save on Escape while letting navigation letters remain text", async () => {
    const { actions, onClose, onIndexChange, files } = renderEditor();
    const textarea = screen.getByRole("textbox", { name: "Description for one.jpg" });

    fireEvent.change(textarea, { target: { value: "jellyfish" } });
    fireEvent.keyDown(textarea, { key: "j" });
    expect(textarea).toHaveValue("jellyfish");
    expect(onIndexChange).not.toHaveBeenCalled();

    fireEvent.keyDown(textarea, { key: "Escape" });
    await act(async () => {});
    expect(actions.setDescription).toHaveBeenCalledWith(files[0], "jellyfish");
    expect(onClose).toHaveBeenCalledOnce();
  });

  it("opens the tag prompt with Ctrl-T and restores the description cursor", async () => {
    renderEditor();
    const textarea = screen.getByRole("textbox", { name: "Description for one.jpg" }) as HTMLTextAreaElement;
    textarea.focus();
    textarea.setSelectionRange(1, 3);

    fireEvent.keyDown(textarea, { key: "t", ctrlKey: true });
    const prompt = await screen.findByLabelText("Tags to add");
    expect(prompt).toHaveFocus();
    fireEvent.keyDown(prompt, { key: "Escape" });

    await act(async () => {});
    expect(textarea).toHaveFocus();
    expect(textarea.selectionStart).toBe(1);
    expect(textarea.selectionEnd).toBe(3);
  });

  it("should discard only the uncommitted draft on Ctrl-C", () => {
    const { actions, onClose, toasts } = renderEditor();
    const textarea = screen.getByRole("textbox", { name: "Description for one.jpg" });
    fireEvent.change(textarea, { target: { value: "do not keep" } });
    fireEvent.keyDown(textarea, { key: "c", ctrlKey: true });

    expect(actions.setDescription).not.toHaveBeenCalled();
    expect(onClose).toHaveBeenCalledOnce();
    expect(toasts.messages().join(" ")).toContain("Discarded uncommitted description");
  });

  it("should save and move without leaving the editor on Ctrl-J", async () => {
    const { actions, onIndexChange, files } = renderEditor();
    const textarea = screen.getByRole("textbox", { name: "Description for one.jpg" });
    fireEvent.change(textarea, { target: { value: "changed first" } });
    fireEvent.keyDown(textarea, { key: "j", ctrlKey: true });

    await screen.findByRole("textbox", { name: "Description for two.jpg" });
    expect(actions.setDescription).toHaveBeenCalledWith(files[0], "changed first");
    expect(onIndexChange).toHaveBeenCalledWith(1);
  });

  it("should autosave after 1.5 seconds idle", async () => {
    vi.useFakeTimers();
    const { actions, files } = renderEditor();
    const textarea = screen.getByRole("textbox", { name: "Description for one.jpg" });
    fireEvent.change(textarea, { target: { value: "idle caption" } });

    await act(async () => {
      vi.advanceTimersByTime(1500);
      await Promise.resolve();
    });
    expect(actions.setDescription).toHaveBeenCalledWith(files[0], "idle caption");
  });

  it("should save a valid manual capture date through the existing file PATCH action", async () => {
    const { actions, files, onClose } = renderEditor();
    const input = screen.getByRole("textbox", { name: "Capture date for one.jpg" });
    fireEvent.change(input, { target: { value: "2024-02-29 03:04:05.006" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await act(async () => {});
    expect(actions.setCaptureDate).toHaveBeenCalledWith(files[0], "2024-02-29 03:04:05.006");
    expect(onClose).toHaveBeenCalledOnce();
  });

  it("should keep an impossible manual capture date local and explain the format", async () => {
    const { actions, onClose, toasts } = renderEditor();
    const input = screen.getByRole("textbox", { name: "Capture date for one.jpg" });
    fireEvent.change(input, { target: { value: "2024-02-30 03:04:05" } });

    expect(screen.getByRole("alert")).toHaveTextContent("Enter a real local date");
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await act(async () => {});

    expect(actions.setCaptureDate).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
    expect(toasts.messages().join(" ")).toContain("Capture date must be a real local date");
  });
});
