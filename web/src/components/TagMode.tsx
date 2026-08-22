import { useCallback, useState } from "react";
import type { TagOperation, FileActions } from "../hooks/useFileWorkspace";
import { useKeymapLayer } from "../hooks/useKeymapLayer";
import { useQuickSlots } from "../hooks/useQuickSlots";
import { useScrollLock } from "../hooks/useScrollLock";
import type { Command } from "../keymap/types";
import { formatDateTime, formatSize } from "../lib/format";
import { resolvePreview } from "../lib/preview";
import { Z } from "../lib/zLayers";
import type { FileRecord } from "../types/api";
import { CaptureMeta } from "./CaptureMeta";
import { LocationMeta } from "./LocationMeta";
import { Portal } from "./Portal";
import { PreviewSurface } from "./PreviewSurface";
import { TagChips } from "./TagChips";

interface TagModeProps {
  files: FileRecord[];
  index: number;
  actions: FileActions;
  lastOperation: TagOperation | null;
  onClose(): void;
}

function pairIDs(files: FileRecord[], file: FileRecord): number[] {
  if (!file.pair_key) return [file.id];
  return files.filter((candidate) => candidate.pair_key === file.pair_key).map((candidate) => candidate.id);
}

/** A focused review surface layered over, but deliberately not replacing, the
 * grid keymap. Motions and operators fall through to the grid; this layer only
 * owns Tag-mode state (close, carry toggle, and accepting a carried write). */
export function TagMode({ files, index, actions, lastOperation, onClose }: TagModeProps) {
  const file = files[index];
  const [carry, setCarry] = useState(false);
  const [seenIndex, setSeenIndex] = useState(index);
  const [seenOperation, setSeenOperation] = useState(lastOperation);
  const [arrivedFromID, setArrivedFromID] = useState<number | null>(null);
  const [pending, setPending] = useState<TagOperation | null>(null);
  const quick = useQuickSlots(file?.folder_label ?? "");
  useScrollLock(true);

  if (seenOperation !== lastOperation) {
    setSeenOperation(lastOperation);
    if (carry && lastOperation && arrivedFromID !== null && lastOperation.ids.includes(arrivedFromID)) {
      setPending(lastOperation);
    }
  }

  // React's supported "adjust state while rendering" pattern avoids painting
  // the next file once without the carry prompt. It also avoids an effect that
  // would synchronously set state after every motion.
  if (seenIndex !== index) {
    const departedID = files[seenIndex]?.id ?? null;
    setSeenIndex(index);
    setArrivedFromID(departedID);
    setPending(carry && lastOperation && departedID !== null && lastOperation.ids.includes(departedID) ? lastOperation : null);
  }

  const handle = useCallback((command: Command): boolean => {
    if (command.kind === "action" && command.action === "accept-tag-carry") {
      if (pending && file) {
        const ids = pairIDs(files, file);
        void actions.tagFiles(
          ids,
          pending.mode === "add" ? pending.tags : [],
          pending.mode === "remove" ? pending.tags : [],
        );
        setPending(null);
      }
      return true;
    }

    // The carry offer is intentionally ephemeral: any key other than Enter
    // discards it before that command continues through the scope chain.
    if (pending) setPending(null);
    if (command.kind !== "action") return false;
    if (command.action === "toggle-tag-carry") {
      setCarry((enabled) => !enabled);
      return true;
    }
    if (command.action === "cancel" || command.action === "close") {
      onClose();
      return true;
    }
    return false;
  }, [actions, file, files, onClose, pending]);
  const observeRawKey = useCallback((key: string) => {
    if (pending && key !== "<CR>") setPending(null);
  }, [pending]);
  useKeymapLayer("tagging", handle, true, observeRawKey);

  if (!file) return null;
  const preview = resolvePreview(file);
  const prefetch = [
    ...files.slice(Math.max(0, index - 2), index),
    ...files.slice(index + 1, index + 3),
  ].map(resolvePreview).filter((item) => item.kind === "image" && item.src);

  return (
    <Portal>
      <section
        role="dialog"
        aria-label={`Tag mode for ${file.filename}`}
        data-tag-mode
        data-tag-scope="on"
        data-keyscope="tagging"
        data-file-id={file.id}
        style={{ zIndex: Z.lightbox }}
        className="fixed inset-0 flex flex-col bg-ground/95 text-ink"
      >
        {prefetch.map((item) => <link key={item.src} rel="prefetch" href={item.src} />)}
        <header className="flex items-center justify-between gap-4 border-b border-line px-4 py-3">
          <div className="min-w-0">
            <h2 className="truncate font-medium">{file.filename}</h2>
            <p className="text-meta text-ink-faint tabular">{index + 1} / {files.length}</p>
          </div>
          <div className="flex items-center gap-3 text-meta">
            <span data-tag-carry={carry ? "on" : "off"} className={carry ? "text-held" : "text-ink-faint"}>
              Carry {carry ? "on" : "off"} (,)
            </span>
            <button
              type="button"
              onClick={onClose}
              className="rounded-md px-3 py-1.5 ring-1 ring-line-strong hover:bg-raised-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
            >
              Close
            </button>
          </div>
        </header>

        <div className="grid min-h-0 flex-1 gap-4 p-4 md:grid-cols-[minmax(0,2fr)_minmax(18rem,1fr)]">
          <div className="flex min-h-0 items-center justify-center overflow-hidden rounded-xl bg-surface" data-preview-kind={preview.kind}>
            <PreviewSurface file={file} preview={preview} />
          </div>
          <aside className="overflow-y-auto rounded-xl border border-line bg-surface p-4 text-meta">
            <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-2">
              <dt className="text-ink-faint">Tags</dt>
              <dd>{file.tags.length ? <TagChips tags={file.tags} max={file.tags.length} /> : <span className="text-ink-muted">None</span>}</dd>
              <dt className="text-ink-faint">Captured</dt>
              <dd><CaptureMeta file={file} /></dd>
              <dt className="text-ink-faint">Location</dt>
              <dd><LocationMeta file={file} /></dd>
              <dt className="text-ink-faint">Size</dt>
              <dd>{formatSize(file.size_bytes)}</dd>
              <dt className="text-ink-faint">Modified</dt>
              <dd>{formatDateTime(file.file_mtime)}</dd>
              <dt className="text-ink-faint">Status</dt>
              <dd>{file.status}</dd>
            </dl>
            <div className="mt-4 border-t border-line pt-4">
              <h3 className="text-ink-faint">Description</h3>
              <p className="mt-1 whitespace-pre-wrap text-ink-muted">{file.description || "None"}</p>
              <p className="mt-2 text-ink-faint">Press i to edit</p>
            </div>
            <div className="mt-4 border-t border-line pt-4" data-quick-slots>
              <h3 className="mb-2 text-ink-faint">Quick tags</h3>
              <div className="grid grid-cols-3 gap-1">
                {quick.slots.map((name, slot) => (
                  <span key={slot} data-quick-slot={slot + 1} className="truncate rounded bg-raised px-2 py-1 text-ink-muted">
                    {slot + 1} {name || "—"}
                  </span>
                ))}
              </div>
            </div>
          </aside>
        </div>

        <footer className="border-t border-line px-4 py-2 text-meta text-ink-faint" data-tag-mode-status>
          {pending
            ? `Enter repeats ${pending.mode === "add" ? "tag" : "untag"} ${pending.tags.join(", ")} · any other key discards`
            : "t/T tag · . repeat · U undo · Ctrl-R redo · Space 1–9 quick tags · , carry · J/K untagged · Esc close"}
        </footer>
      </section>
    </Portal>
  );
}
