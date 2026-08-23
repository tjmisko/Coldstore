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
    filterKey?: "all" | "trash";
  } = {},
) {
  stub = stubFetch([{ url: "/api/v1/files", body: rows }]);
  return renderWithToasts(
    <FileBrowser
      folder="Documents"
      filterKey={options.filterKey ?? "all"}
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
  it("should offer state-specific actions and recoverable Delete on active rows", async () => {
    renderBrowser();
    await screen.findByText("a.txt");

    expect(within(row(1)).getByRole("button", { name: "Archive" })).toBeInTheDocument();
    expect(within(row(1)).getByRole("button", { name: "Delete" })).toBeInTheDocument();
    expect(within(row(1)).queryByRole("button", { name: "Restore" })).toBeNull();
    expect(within(row(2)).getByRole("button", { name: "Restore" })).toBeInTheDocument();
    expect(within(row(2)).getByRole("button", { name: "Delete" })).toBeInTheDocument();
    expect(within(row(2)).queryByRole("button", { name: "Archive" })).toBeNull();
  });

  it("should offer restore and confirmed permanent deletion in the trash view", async () => {
    const trashed = aFile({
      id: 9,
      filename: "deleted.txt",
      relative_path: "notes/deleted.txt",
      extension: ".txt",
      status: "trashed",
      trashed_from: "synced",
    });
    renderBrowser([trashed], { filterKey: "trash" });
    await screen.findByText("deleted.txt");

    expect(within(row(9)).getByRole("button", { name: "Restore" })).toBeInTheDocument();
    expect(within(row(9)).getByRole("button", { name: "Delete permanently" })).toBeInTheDocument();
    expect(within(row(9)).queryByRole("button", { name: "Archive" })).toBeNull();
    expect(within(row(9)).queryByRole("button", { name: /Pin deleted\.txt/ })).toBeNull();

    await userEvent.click(within(row(9)).getByRole("button", { name: "Delete permanently" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("Permanently delete 1 file?");
    expect(dialog).toHaveTextContent("It cannot be undone");
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

  it("should inset the row cursor and leave space before the first checkbox", async () => {
    renderBrowser();
    await screen.findByText("a.txt");

    expect(screen.getByRole("table")).toHaveClass("mx-1", "w-[calc(100%-0.5rem)]");
    expect(within(row(1)).getByRole("checkbox").closest("td")).toHaveClass("pl-2");
  });

  it("should show only Pin for one unpinned file and give every menu action an icon", async () => {
    renderBrowser();
    await screen.findByText("a.txt");

    fireEvent.contextMenu(row(1), { clientX: 24, clientY: 48 });
    const menu = screen.getByRole("menu");
    expect(within(menu).getByRole("menuitem", { name: "Pin" })).toBeInTheDocument();
    expect(within(menu).queryByRole("menuitem", { name: "Unpin" })).toBeNull();
    expect(menu.querySelectorAll("[data-menu-icon]")).toHaveLength(within(menu).getAllByRole("menuitem").length);
  });

  it("should show only Unpin for one pinned file", async () => {
    const pinned = aFile({
      id: 10,
      folder_label: "Documents",
      filename: "kept.txt",
      relative_path: "notes/kept.txt",
      extension: ".txt",
      pinned: true,
      pin_source: "user",
    });
    renderBrowser([pinned]);
    await screen.findByText("kept.txt");

    fireEvent.contextMenu(row(10), { clientX: 24, clientY: 48 });
    const menu = screen.getByRole("menu");
    expect(within(menu).getByRole("menuitem", { name: "Unpin" })).toBeInTheDocument();
    expect(within(menu).queryByRole("menuitem", { name: "Pin" })).toBeNull();
  });

  it("should rename a file in place from its context menu", async () => {
    const original = { ...ROWS[0], folder_label: "Documents" };
    const { toasts } = renderBrowser([original]);
    await screen.findByText("a.txt");
    const renamed = { ...original, filename: "renamed.txt", relative_path: "notes/renamed.txt" };
    stub!.route({
      method: "POST",
      url: "/api/v1/folders/Documents/rename",
      body: { job_id: 91, action: "rename", state: "pending", total: 1, warnings: [] },
    });
    stub!.route({
      url: "/api/v1/jobs/91",
      body: {
        id: 91,
        action: "rename",
        folder_label: "Documents",
        state: "done",
        total: 1,
        done: 1,
        failed_count: 0,
        error: "",
        created_at: 1,
        started_at: 1,
        finished_at: 2,
        failed: [],
      },
    });
    stub!.route({ url: "/api/v1/files", body: [renamed] });

    fireEvent.contextMenu(row(1), { clientX: 24, clientY: 48 });
    await userEvent.click(screen.getByRole("menuitem", { name: "Rename" }));
    const input = screen.getByRole("textbox", { name: "New name for a.txt" });
    await userEvent.clear(input);
    await userEvent.type(input, "renamed.txt");
    await userEvent.click(screen.getByRole("button", { name: "Rename" }));

    await waitFor(() => expect(stub!.calls).toContainEqual({
      url: "/api/v1/folders/Documents/rename",
      method: "POST",
      body: { from: "notes/a.txt", to: "notes/renamed.txt" },
    }));
    expect(await screen.findByText("renamed.txt")).toBeInTheDocument();
    expect(toasts.messages()).toContain("Renamed a.txt to renamed.txt");
  });
});
