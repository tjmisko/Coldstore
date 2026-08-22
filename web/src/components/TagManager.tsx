import { useMemo, useState } from "react";
import { useTagManager } from "../hooks/useTags";
import { deleteQuickSlotTag, QUICK_SLOT_COUNT, renameQuickSlotTag, useQuickSlots } from "../hooks/useQuickSlots";
import { errorMessage } from "../lib/api";
import { formatCount } from "../lib/format";
import { useToast } from "../lib/toast";
import type { Tag } from "../types/api";
import { ConfirmDialog } from "./ConfirmDialog";

export function TagManager({ folder }: { folder: string }) {
  const manager = useTagManager(folder);
  const quick = useQuickSlots(folder);
  const toast = useToast();
  const [editing, setEditing] = useState<Tag | null>(null);
  const [draft, setDraft] = useState("");
  const [deleting, setDeleting] = useState<Tag | null>(null);
  const [busy, setBusy] = useState(false);
  const tags = useMemo(
    () => [...manager.tags].sort((a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: "base" })),
    [manager.tags],
  );

  const beginRename = (tag: Tag) => {
    setEditing(tag);
    setDraft(tag.name);
  };

  const rename = async () => {
    if (!editing || !draft.trim() || busy) return;
    if (draft.trim() === editing.name) {
      setEditing(null);
      return;
    }
    setBusy(true);
    try {
      const result = await manager.rename(editing.name, draft);
      renameQuickSlotTag(editing.name, draft.trim());
      toast.show(
        "success",
        result.merged
          ? `Merged ${editing.name}; ${formatCount(result.files_affected)} file${result.files_affected === 1 ? " kept its" : "s kept their"} tag assignments`
          : `Renamed ${editing.name}`,
      );
      setEditing(null);
    } catch (err) {
      toast.show("error", `Cannot rename ${editing.name}: ${errorMessage(err)}`);
    } finally {
      setBusy(false);
    }
  };

  const subtree = deleting
    ? tags.filter((tag) => tag.name === deleting.name || tag.name.startsWith(`${deleting.name}/`))
    : [];
  const cascade = subtree.length > 1 || (!!deleting && deleting.descendant_count > deleting.count);

  const remove = async () => {
    if (!deleting || busy) return;
    const tag = deleting;
    setBusy(true);
    try {
      const result = await manager.remove(tag.name, cascade);
      deleteQuickSlotTag(tag.name, cascade);
      toast.show(
        "success",
        `Deleted ${formatCount(result.deleted)} tag${result.deleted === 1 ? "" : "s"}; removed assignments from ${formatCount(result.files_affected)} file${result.files_affected === 1 ? "" : "s"}`,
      );
      setDeleting(null);
    } catch (err) {
      toast.show("error", `Cannot delete ${tag.name}: ${errorMessage(err)}`);
    } finally {
      setBusy(false);
    }
  };

  const mergeTarget = tags.find(
    (tag) => editing && tag.id !== editing.id && tag.name.toLocaleLowerCase() === draft.trim().toLocaleLowerCase(),
  );

  return (
    <section data-tag-manager={folder}>
      <h3 className="text-micro font-medium uppercase tracking-wider text-ink-faint">
        Tag vocabulary (all folders)
      </h3>
      <p className="mb-3 mt-2 text-meta text-ink-muted">
        Counts are catalog-wide: exact assignments beside the tag and all assignments below it. Renaming onto an existing tag merges them without losing file assignments.
      </p>

      <div className="mb-5 rounded-lg border border-line bg-ground/35 p-3">
        <h4 className="text-meta font-medium text-ink">Quick tags for {folder}</h4>
        <p className="mt-1 text-meta text-ink-faint">
          Apply these with Space then 1–9. Bare digits remain motion counts.
        </p>
        <datalist id={`quick-tags-${folder}`}>
          {tags.map((tag) => <option key={tag.id} value={tag.name} />)}
        </datalist>
        <div className="mt-3 grid grid-cols-1 gap-2 sm:grid-cols-3">
          {Array.from({ length: QUICK_SLOT_COUNT }, (_, index) => {
            const slot = index + 1;
            return (
              <label key={slot} className="flex items-center gap-2 text-meta text-ink-muted">
                <span className="w-4 text-right font-mono">{slot}</span>
                <input
                  data-quick-slot={slot}
                  aria-label={`Quick tag ${slot}`}
                  list={`quick-tags-${folder}`}
                  value={quick.slots[index] ?? ""}
                  onChange={(event) => quick.setSlot(slot, event.target.value)}
                  placeholder="unassigned"
                  className="min-w-0 flex-1 rounded-md border border-line-strong bg-raised px-2 py-1 text-meta text-ink outline-none focus:ring-2 focus:ring-cursor"
                />
              </label>
            );
          })}
        </div>
      </div>

      {manager.error && <p className="mb-2 text-base text-alarm" role="alert">{manager.error}</p>}
      {manager.loading && tags.length === 0 && <p className="text-base text-ink-faint">Loading tags…</p>}
      {!manager.loading && !manager.error && tags.length === 0 && (
        <p className="text-base text-ink-faint">No tags exist in the catalog yet.</p>
      )}
      {tags.length === 500 && (
        <p className="mb-2 text-meta text-cold">Showing the first 500 tags. Search is available in the tagging prompt.</p>
      )}

      <div className="space-y-1.5">
        {tags.map((tag) => {
          const depth = tag.name.split("/").length - 1;
          const label = tag.name.split("/").at(-1) ?? tag.name;
          const isEditing = editing?.id === tag.id;
          return (
            <div
              key={tag.id}
              data-managed-tag={tag.name}
              className="rounded-md border border-line bg-surface px-3 py-2"
              style={{ marginLeft: `${Math.min(depth, 5) * 14}px` }}
            >
              {isEditing ? (
                <div className="space-y-2">
                  <label className="block text-meta text-ink-muted" htmlFor={`rename-tag-${tag.id}`}>
                    Rename or merge {tag.name}
                  </label>
                  <div className="flex gap-2">
                    <input
                      id={`rename-tag-${tag.id}`}
                      autoFocus
                      value={draft}
                      onChange={(event) => setDraft(event.target.value)}
                      className="min-w-0 flex-1 rounded-md border border-line-strong bg-raised px-2.5 py-1.5 text-base text-ink outline-none focus:ring-2 focus:ring-cursor"
                    />
                    <button
                      type="button"
                      onClick={() => void rename()}
                      disabled={busy || !draft.trim()}
                      className="rounded-md bg-ink px-3 py-1.5 text-meta font-medium text-ground disabled:opacity-50"
                    >
                      {mergeTarget ? "Merge" : "Save"}
                    </button>
                    <button
                      type="button"
                      onClick={() => setEditing(null)}
                      className="rounded-md px-3 py-1.5 text-meta text-ink-muted ring-1 ring-line-strong hover:bg-raised-hover"
                    >
                      Cancel
                    </button>
                  </div>
                  {mergeTarget && (
                    <p className="text-meta text-cold">This will merge into existing tag {mergeTarget.name}.</p>
                  )}
                </div>
              ) : (
                <div className="flex items-center justify-between gap-3">
                  <div className="min-w-0">
                    <p className="truncate text-base text-ink" title={tag.name}>{label}</p>
                    <p className="text-meta tabular text-ink-faint">
                      {formatCount(tag.count)} exact · {formatCount(tag.descendant_count)} in subtree
                    </p>
                  </div>
                  <div className="flex shrink-0 gap-1">
                    <button
                      type="button"
                      onClick={() => beginRename(tag)}
                      className="rounded px-2 py-1 text-meta text-ink-muted hover:bg-raised-hover hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
                    >
                      Rename
                    </button>
                    <button
                      type="button"
                      onClick={() => setDeleting(tag)}
                      className="rounded px-2 py-1 text-meta text-ink-faint hover:bg-alarm-dim hover:text-alarm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
                    >
                      Delete
                    </button>
                  </div>
                </div>
              )}
            </div>
          );
        })}
      </div>

      {deleting && (
        <ConfirmDialog
          title={cascade ? `Delete ${deleting.name} and its descendants?` : `Delete ${deleting.name}?`}
          confirmLabel={cascade ? "Delete entire subtree" : "Delete tag"}
          destructive
          onConfirm={() => void remove()}
          onCancel={() => setDeleting(null)}
        >
          <p>
            {cascade
              ? `This deletes this tag and every descendant and removes assignments from ${formatCount(deleting.descendant_count)} distinct file${deleting.descendant_count === 1 ? "" : "s"} in the subtree.`
              : `This removes the tag from ${formatCount(deleting.count)} file${deleting.count === 1 ? "" : "s"}.`}
          </p>
          <p>Files themselves are not deleted. This tag deletion cannot be undone from the interface.</p>
        </ConfirmDialog>
      )}
    </section>
  );
}
