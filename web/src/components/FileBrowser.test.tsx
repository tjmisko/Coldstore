import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { aFile } from "../test/fixtures";
import { renderWithToasts, stubFetch } from "../test/harness";
import { FileBrowser } from "./FileBrowser";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
});

// Distinct paths and filenames: a row renders both, and a fixture where they
// are equal makes every getByText ambiguous.
const ROWS = [
  aFile({ id: 1, filename: "a.txt", relative_path: "notes/a.txt", extension: ".txt", status: "synced" }),
  aFile({ id: 2, filename: "b.txt", relative_path: "notes/b.txt", extension: ".txt", status: "archived" }),
  aFile({ id: 3, filename: "c.txt", relative_path: "notes/c.txt", extension: ".txt", status: "synced" }),
];

function renderBrowser(
  rows = ROWS,
  options: {
    reveal?: { file: (typeof ROWS)[number]; token: number };
    onShowInFiles?: (file: (typeof ROWS)[number]) => void;
  } = {},
) {
  stub = stubFetch([{ url: "/api/v1/files", body: rows }]);
  return renderWithToasts(
    <FileBrowser
      folder="Documents"
      filterKey="all"
      onFilterChange={vi.fn()}
      onFolderChanged={() => {}}
      reveal={options.reveal}
      onShowInFiles={options.onShowInFiles}
    />,
  );
}

function row(id: number): HTMLElement {
  return document.querySelector(`tr[data-file-id="${id}"]`) as HTMLElement;
}

describe("FileBrowser", () => {
  it("should offer Restore only for archived rows and Archive only for synced ones", async () => {
    renderBrowser();
    await screen.findByText("a.txt");

    expect(within(row(1)).getByRole("button", { name: "Archive" })).toBeInTheDocument();
    expect(within(row(1)).queryByRole("button", { name: "Restore" })).toBeNull();
    expect(within(row(2)).getByRole("button", { name: "Restore" })).toBeInTheDocument();
    expect(within(row(2)).queryByRole("button", { name: "Archive" })).toBeNull();
  });

  it("should repaint the row after a restore instead of leaving it reading archived", async () => {
    const { toasts } = renderBrowser();
    await screen.findByText("b.txt");

    stub!.route({
      method: "POST",
      url: "/api/v1/files/2/restore",
      body: { ...ROWS[1], status: "synced", pinned: true, pin_source: "restore" },
    });

    await userEvent.click(within(row(2)).getByRole("button", { name: "Restore" }));

    // The old handler awaited fetch() with no res.ok check and no repaint, so
    // the row went on reading "archived" with its Restore button showing until
    // something else reloaded the folder.
    await waitFor(() => expect(within(row(2)).getByText("synced")).toBeInTheDocument());
    expect(within(row(2)).queryByRole("button", { name: "Restore" })).toBeNull();
    expect(toasts.messages().join(" ")).toContain("Restored b.txt");
  });

  it("should report a refused restore rather than looking as if it worked", async () => {
    const { toasts } = renderBrowser();
    await screen.findByText("b.txt");

    stub!.route({
      method: "POST",
      url: "/api/v1/files/2/restore",
      status: 409,
      body: { error: "folder is not currently available" },
    });

    await userEvent.click(within(row(2)).getByRole("button", { name: "Restore" }));

    await waitFor(() =>
      expect(toasts.messages().join(" ")).toContain("folder is not currently available"),
    );
    expect(within(row(2)).getByText("archived")).toBeInTheDocument();
  });

  it("should shift-click a range of rows", async () => {
    renderBrowser();
    await screen.findByText("a.txt");

    await userEvent.click(within(row(1)).getByRole("checkbox"));
    fireEvent.click(within(row(3)).getByRole("checkbox"), { shiftKey: true });

    expect(screen.getByText("3 selected")).toBeInTheDocument();
  });

  it("should show the header checkbox as indeterminate when only some rows are selected", async () => {
    renderBrowser();
    await screen.findByText("a.txt");
    const header = screen.getByLabelText("Select all loaded files") as HTMLInputElement;

    expect(header.indeterminate).toBe(false);
    await userEvent.click(within(row(1)).getByRole("checkbox"));
    expect(header.indeterminate).toBe(true);
    expect(header.checked).toBe(false);

    await userEvent.click(header);
    expect(header.checked).toBe(true);
    expect(header.indeterminate).toBe(false);
    expect(screen.getByText("3 selected")).toBeInTheDocument();
  });

  it("should archive a synced row on demand", async () => {
    renderBrowser();
    await screen.findByText("a.txt");

    stub!.route({
      method: "POST",
      url: "/api/v1/files/1/archive",
      body: { ...ROWS[0], status: "archived", pinned: false },
    });

    await userEvent.click(within(row(1)).getByRole("button", { name: "Archive" }));

    await waitFor(() => expect(within(row(1)).getByText("archived")).toBeInTheDocument());
  });

  it("should render and focus a complete reveal target outside the initial page", async () => {
    const target = aFile({
      id: 99,
      folder_label: "Documents",
      filename: "z-last.txt",
      relative_path: "archive/z-last.txt",
      extension: ".txt",
    });
    const page = Array.from({ length: 60 }, (_, index) =>
      aFile({
        id: index + 1,
        folder_label: "Documents",
        filename: `page-${String(index + 1).padStart(2, "0")}.txt`,
        relative_path: `page/page-${String(index + 1).padStart(2, "0")}.txt`,
        extension: ".txt",
      }),
    );
    renderBrowser(page, { reveal: { file: target, token: 1 } });

    await waitFor(() => expect(row(99)).toBe(document.activeElement));
    expect(row(99)).toHaveAttribute("data-cursor", "true");
    expect(within(row(99)).getByText("archive/z-last.txt")).toBeInTheDocument();
  });

  it("should offer the precise in-app Files action from the context menu", async () => {
    const onShowInFiles = vi.fn();
    renderBrowser(ROWS, { onShowInFiles });
    await screen.findByText("a.txt");

    fireEvent.contextMenu(row(1), { clientX: 24, clientY: 48 });
    await userEvent.click(screen.getByRole("menuitem", { name: "Show in Files view" }));

    expect(onShowInFiles).toHaveBeenCalledWith(ROWS[0]);
  });
});
