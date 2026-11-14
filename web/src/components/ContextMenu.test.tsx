import { screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { BulkActions } from "../hooks/useBulkActions";
import type { FileActions } from "../hooks/useFileWorkspace";
import { aCollectionFile, aFile } from "../test/fixtures";
import { renderWithToasts, stubFetch } from "../test/harness";
import { ContextMenu } from "./ContextMenu";
import type { FileRecord } from "../types/api";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
});

function renderMenu(file: FileRecord, ids = [file.id]) {
  stub = stubFetch([]);
  return renderWithToasts(
    <ContextMenu
      target={{ file, ids, at: null }}
      conflictMode={false}
      actions={{} as FileActions}
      bulk={{ perform: vi.fn(), download: vi.fn() } as unknown as BulkActions}
      onOpen={() => {}}
      onRename={() => {}}
      onEditDescription={() => {}}
      onEditLocation={() => {}}
      onEditTags={() => {}}
      onShowInFiles={() => {}}
      onClose={() => {}}
    />,
  );
}

const item = (id: string) => document.querySelector(`[data-menu-item="${id}"]`);

describe("ContextMenu", () => {
  it("should not offer archive, restore, pin or rename when the selection is collection files", () => {
    const a = aCollectionFile();
    const b = aCollectionFile();
    renderMenu(a, [a.id, b.id]);

    for (const id of ["archive", "restore", "pin", "unpin", "rename", "description", "location"]) {
      expect(item(id)).toBeNull();
    }
  });

  it("should not offer archive, restore, pin or rename for one live collection file", () => {
    renderMenu(aCollectionFile());

    for (const id of ["archive", "restore", "pin", "unpin", "rename", "description", "location"]) {
      expect(item(id)).toBeNull();
    }
  });

  it("should offer download, tag and delete-to-trash when the selection is collection files", () => {
    renderMenu(aCollectionFile());

    expect(item("download")).not.toBeNull();
    expect(item("add-tags")).not.toBeNull();
    expect(item("trash")).not.toBeNull();
  });

  it("should offer restore from trash when a trashed collection file is selected", () => {
    renderMenu(aCollectionFile({ status: "trashed", trashed_at: "2026-06-02T00:00:00Z", trashed_from: "archived" }));

    expect(item("untrash")).not.toBeNull();
    expect(item("delete-permanently")).toBeNull();
    expect(screen.getByText("Restore from Trash")).toBeInTheDocument();
  });

  it("should still offer archive and pin when the file is a synced legacy file", () => {
    renderMenu(aFile({ status: "synced" }));

    expect(item("archive")).not.toBeNull();
    expect(item("pin")).not.toBeNull();
    expect(item("rename")).not.toBeNull();
  });
});
