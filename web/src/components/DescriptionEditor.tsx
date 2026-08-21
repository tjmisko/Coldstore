import { useCallback, useEffect, useRef, useState } from "react";
import type { FileActions } from "../hooks/useFileWorkspace";
import { useKeymapLayer } from "../hooks/useKeymapLayer";
import { useToast } from "../lib/toast";
import type { FileRecord } from "../types/api";
import { Overlay } from "./Overlay";

interface DescriptionEditorProps {
  files: FileRecord[];
  initialIndex: number;
  actions: FileActions;
  onClose(): void;
  onIndexChange?(index: number): void;
}

const descriptionLimit = 2000;
const idleSaveDelay = 1500;

/** A caption editor with an exclusive keymap scope. Its textarea owns typed
 * keys, while explicit control chords save, discard, or move between files. */
export function DescriptionEditor({
  files,
  initialIndex,
  actions,
  onClose,
  onIndexChange = () => {},
}: DescriptionEditorProps) {
  const [index, setIndex] = useState(initialIndex);
  const file = files[index];
  const [draft, setDraft] = useState(file?.description ?? "");
  const [saveState, setSaveState] = useState<"saved" | "saving" | "unsaved">("saved");
  const saved = useRef(file?.description ?? "");
  const currentDraft = useRef(draft);
  const saveQueue = useRef<Promise<boolean>>(Promise.resolve(true));
  const pending = useRef<{ key: string; promise: Promise<boolean> } | null>(null);
  const discard = useRef(false);
  const textarea = useRef<HTMLTextAreaElement>(null);
  const toast = useToast();

  // Registering the scope is load-bearing even though ordinary textarea keys
  // are filtered before the key machine: if focus moves unexpectedly, the
  // exclusive scope still prevents a caption keystroke reaching the grid.
  const ignoreProgrammaticCommands = useCallback(() => false, []);
  useKeymapLayer("description", ignoreProgrammaticCommands);

  useEffect(() => {
    const node = textarea.current;
    node?.focus();
    if (node) node.setSelectionRange(node.value.length, node.value.length);
  }, [index]);

  const commit = useCallback(async (): Promise<boolean> => {
    if (!file || draft === saved.current) {
      setSaveState("saved");
      return true;
    }
    const key = `${file.id}\u0000${draft}`;
    if (pending.current?.key === key) return pending.current.promise;

    const text = draft;
    const promise = saveQueue.current.then(async () => {
      setSaveState("saving");
      const ok = await actions.setDescription(file, text);
      if (ok) saved.current = text;
      setSaveState(ok && currentDraft.current === text ? "saved" : "unsaved");
      return ok;
    });
    saveQueue.current = promise;
    pending.current = { key, promise };
    void promise.finally(() => {
      if (pending.current?.promise === promise) pending.current = null;
    });
    return promise;
  }, [actions, draft, file]);

  useEffect(() => {
    if (!file || draft === saved.current) return;
    const timer = window.setTimeout(() => void commit(), idleSaveDelay);
    return () => window.clearTimeout(timer);
  }, [commit, draft, file]);

  const closeAndSave = useCallback(async () => {
    if (await commit()) onClose();
  }, [commit, onClose]);

  const move = useCallback(
    async (delta: number) => {
      if (!(await commit())) return;
      const next = Math.max(0, Math.min(index + delta, files.length - 1));
      if (next === index) return;
      const nextDescription = files[next]?.description ?? "";
      saved.current = nextDescription;
      currentDraft.current = nextDescription;
      setDraft(nextDescription);
      setSaveState("saved");
      setIndex(next);
      onIndexChange(next);
    },
    [commit, files, index, onIndexChange],
  );

  if (!file) return null;

  return (
    <Overlay
      label={`Description for ${file.filename}`}
      onClose={() => void closeAndSave()}
      className="max-w-2xl"
      testAttribute="description"
    >
      <section className="rounded-xl border border-line-strong bg-surface p-5 shadow-2xl">
        <header className="mb-3 flex items-start justify-between gap-4">
          <div className="min-w-0">
            <h2 className="truncate text-base font-semibold text-ink">{file.filename}</h2>
            <p className="truncate text-meta text-ink-faint">
              {file.folder_label}/{file.relative_path}
            </p>
          </div>
          <span className="shrink-0 text-meta text-ink-faint" aria-live="polite">
            {saveState === "saving" ? "Saving…" : saveState === "saved" ? "Saved" : "Unsaved"}
          </span>
        </header>

        <textarea
          ref={textarea}
          aria-label={`Description for ${file.filename}`}
          data-description-editor
          data-keyscope="description"
          rows={8}
          value={draft}
          onChange={(event) => {
            const value = event.target.value;
            if ([...value].length <= descriptionLimit) {
              currentDraft.current = value;
              setDraft(value);
              setSaveState(value === saved.current ? "saved" : "unsaved");
            }
          }}
          onBlur={() => {
            if (!discard.current) void commit();
          }}
          onKeyDown={(event) => {
            if (event.key === "Escape") {
              event.preventDefault();
              void closeAndSave();
              return;
            }
            if (!event.ctrlKey && !event.metaKey) return;
            if (event.key.toLowerCase() === "c") {
              event.preventDefault();
              discard.current = true;
              toast.show("info", `Discarded uncommitted description for ${file.filename}`);
              onClose();
            } else if (event.key.toLowerCase() === "j") {
              event.preventDefault();
              void move(1);
            } else if (event.key.toLowerCase() === "k") {
              event.preventDefault();
              void move(-1);
            }
          }}
          className="w-full resize-y rounded-lg border border-line-strong bg-ground px-3 py-2 text-base text-ink outline-none placeholder:text-ink-faint focus:border-cursor focus:ring-2 focus:ring-cursor/40"
          placeholder="Add a description…"
        />

        <footer className="mt-3 flex flex-wrap items-center justify-between gap-3 text-meta text-ink-faint">
          <span>{[...draft].length} / {descriptionLimit}</span>
          <span className="hidden sm:inline">Esc saves · Ctrl-C discards · Ctrl-J/K saves and moves</span>
          <div className="flex gap-2">
            <button
              type="button"
              onClick={() => void move(-1)}
              disabled={index === 0}
              className="rounded-md px-3 py-1.5 text-ink-muted ring-1 ring-line-strong hover:bg-raised-hover disabled:opacity-40"
            >
              Previous
            </button>
            <button
              type="button"
              onClick={() => void move(1)}
              disabled={index === files.length - 1}
              className="rounded-md px-3 py-1.5 text-ink-muted ring-1 ring-line-strong hover:bg-raised-hover disabled:opacity-40"
            >
              Next
            </button>
            <button
              type="button"
              onClick={() => void closeAndSave()}
              className="rounded-md bg-ink px-3 py-1.5 font-medium text-ground hover:brightness-95"
            >
              Save
            </button>
          </div>
        </footer>
      </section>
    </Overlay>
  );
}
