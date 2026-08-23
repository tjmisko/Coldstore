import { useCallback, useState } from "react";
import { useFocusTrap } from "../hooks/useFocusTrap";
import { useKeymapLayer } from "../hooks/useKeymapLayer";
import type { BulkActions } from "../hooks/useBulkActions";
import type { FileActions } from "../hooks/useFileWorkspace";
import type { Command } from "../keymap/types";
import { formatCount } from "../lib/format";
import { useToast } from "../lib/toast";
import { Z } from "../lib/zLayers";
import type { FileRecord } from "../types/api";
import { Portal } from "./Portal";
import type { TagPromptMode } from "./TagPrompt";
import { ContextMenuIcon, type ContextMenuIconName } from "./ContextMenuIcon";

export interface ContextMenuTarget {
  file: FileRecord;
  /** The ids the menu acts on. Right-clicking INSIDE a selection acts on the
   *  whole selection; right-clicking outside one acts on the file clicked. */
  ids: number[];
  /** Null when the menu was opened from the keyboard, in which case it is
   *  positioned against the cursor's element instead. */
  at: { x: number; y: number } | null;
}

interface ContextMenuProps {
  target: ContextMenuTarget;
  conflictMode: boolean;
  actions: FileActions;
  bulk: BulkActions;
  onOpen(): void;
  onRename(): void;
  onEditDescription(): void;
  onEditLocation(): void;
  onEditTags(mode: TagPromptMode): void;
  onShowInFiles(): void;
  onClose(): void;
}

interface Item {
  id: string;
  icon: ContextMenuIconName;
  label: string;
  run(): void;
  danger?: boolean;
}

export function ContextMenu({
  target,
  conflictMode,
  actions,
  bulk,
  onOpen,
  onRename,
  onEditDescription,
  onEditLocation,
  onEditTags,
  onShowInFiles,
  onClose,
}: ContextMenuProps) {
  const containerRef = useFocusTrap(true);
  const toast = useToast();
  const [highlight, setHighlight] = useState(0);
  const { file, ids } = target;
  const many = ids.length > 1;
  const inTrash = file.status === "trashed";
  const suffix = many ? ` (${formatCount(ids.length)} files)` : "";

  const close = useCallback(() => onClose(), [onClose]);

  const copyPath = useCallback(() => {
    const text = `${file.folder_label}/${file.relative_path}`;
    const clipboard = navigator.clipboard;
    if (!clipboard?.writeText) {
      toast.show("error", "This browser will not let the page write to the clipboard.");
      return;
    }
    void clipboard.writeText(text).then(
      () => toast.show("success", `Copied ${text}`),
      () => toast.show("error", "Could not write to the clipboard."),
    );
  }, [file, toast]);

  const items: Item[] = [];
  if (!many) {
    items.push({ id: "open", icon: "open", label: "Open", run: onOpen });
    items.push({ id: "rename", icon: "rename", label: "Rename", run: onRename });
    items.push({ id: "description", icon: "description", label: "Edit description", run: onEditDescription });
    items.push({ id: "location", icon: "location", label: "Location and nearby files", run: onEditLocation });
    items.push({ id: "show-in-files", icon: "show-in-files", label: "Show in Files view", run: onShowInFiles });
  }
  if (!inTrash) {
    items.push({ id: "add-tags", icon: "add-tags", label: `Add tags${suffix}`, run: () => onEditTags("add") });
    items.push({ id: "remove-tags", icon: "remove-tags", label: `Remove tags${suffix}`, run: () => onEditTags("remove") });
  }
  items.push({ id: "download", icon: "download", label: `Download${suffix}`, run: () => bulk.download(ids) });
  if (inTrash) {
    items.push({ id: "untrash", icon: "restore", label: `Restore from Trash${suffix}`, run: () => bulk.perform("untrash", ids) });
    items.push({
      id: "delete-permanently",
      icon: "delete-permanently",
      label: `Delete permanently${suffix}`,
      run: () => bulk.perform("delete", ids),
      danger: true,
    });
  } else if (file.status === "archived" || many) {
    items.push({ id: "restore", icon: "restore", label: `Restore${suffix}`, run: () => bulk.perform("restore", ids) });
  }
  if (!inTrash && (file.status === "synced" || many)) {
    items.push({
      id: "archive",
      icon: "archive",
      label: `Archive${suffix}`,
      run: () => bulk.perform("archive", ids),
      danger: true,
    });
  }
  if (!inTrash) {
    items.push({ id: "trash", icon: "trash", label: `Delete${suffix}`, run: () => bulk.perform("trash", ids), danger: true });
    if (many || !file.pinned) {
      items.push({ id: "pin", icon: "pin", label: `Pin${suffix}`, run: () => bulk.perform("pin", ids) });
    }
    if (many || file.pinned) {
      items.push({ id: "unpin", icon: "unpin", label: `Unpin${suffix}`, run: () => bulk.perform("unpin", ids) });
    }
  }
  if (!inTrash && conflictMode && !many) {
    items.push({
      id: "resolve-device",
      icon: "resolve-device",
      label: "Keep the copy on your device",
      run: () => void actions.resolve(file, "device"),
    });
    items.push({
      id: "resolve-archive",
      icon: "resolve-archive",
      label: "Keep the archived copy",
      run: () => void actions.resolve(file, "archive"),
    });
  }
  items.push({ id: "copy-path", icon: "copy-path", label: "Copy path", run: copyPath });

  const handle = useCallback(
    (command: Command): boolean => {
      if (command.kind === "action" && command.action === "cancel") {
        close();
        return true;
      }
      return false;
    },
    [close],
  );
  useKeymapLayer("overlay", handle);

  const run = (item: Item) => {
    item.run();
    close();
  };

  const onKeyDown = (event: React.KeyboardEvent) => {
    const down = event.key === "ArrowDown" || event.key === "j";
    const up = event.key === "ArrowUp" || event.key === "k";
    if (down || up) {
      event.preventDefault();
      setHighlight((current) => (current + (down ? 1 : items.length - 1)) % items.length);
      return;
    }
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      run(items[highlight]);
    }
  };

  // Positioned by the pointer when there was one, and pinned near the top-left
  // of the viewport when the menu was opened from the keyboard, where it cannot
  // land off-screen.
  const style = target.at
    ? { left: Math.min(target.at.x, window.innerWidth - 272), top: Math.min(target.at.y, window.innerHeight - 260) }
    : { left: 24, top: 96 };

  return (
    <Portal>
      <div
        style={{ zIndex: Z.contextMenu }}
        className="fixed inset-0"
        onMouseDown={(event) => {
          if (event.target === event.currentTarget) close();
        }}
        onContextMenu={(event) => {
          event.preventDefault();
          close();
        }}
      >
        <div
          ref={(node) => {
            containerRef.current = node;
          }}
          role="menu"
          aria-label={`Actions for ${file.filename}`}
          data-context-menu={file.id}
          tabIndex={-1}
          onKeyDown={onKeyDown}
          style={{ position: "absolute", ...style }}
          className="w-64 rounded-lg border border-line-strong bg-surface py-1 shadow-2xl outline-none"
        >
          <p className="truncate px-3 py-1 text-meta text-ink-faint">
            {many ? `${formatCount(ids.length)} files selected` : file.filename}
          </p>
          {items.map((item, index) => (
            <button
              key={item.id}
              type="button"
              role="menuitem"
              data-menu-item={item.id}
              onMouseEnter={() => setHighlight(index)}
              onClick={() => run(item)}
              className={`flex w-full items-center gap-2.5 px-3 py-1.5 text-left text-sm ${
                index === highlight ? "bg-raised-hover" : ""
              } ${item.danger ? "text-alarm" : "text-ink"}`}
            >
              <ContextMenuIcon name={item.icon} />
              <span className="min-w-0 truncate">{item.label}</span>
            </button>
          ))}
        </div>
      </div>
    </Portal>
  );
}
