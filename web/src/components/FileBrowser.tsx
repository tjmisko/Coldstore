import { useCallback, useEffect, useRef } from "react";
import { formatCount, formatDate, formatSize } from "../lib/format";
import { useFileWorkspace, type FilterKey } from "../hooks/useFileWorkspace";
import type { FileRecord } from "../types/api";
import { FilterBar } from "./FilterBar";
import { PinToggle } from "./PinToggle";
import { SelectionBar } from "./SelectionBar";

interface FileBrowserProps {
  folder: string;
  filterKey: FilterKey;
  onFilterChange(key: FilterKey): void;
  onFolderChanged(): void;
}

/** Only a departure from normal gets a hue.
 *
 *  "synced" is the majority state on nearly every screen, so colouring it made
 *  the column a wall of green that said nothing. It is now plain ink, which is
 *  what makes the handful of cold and alarm rows findable at a glance. */
function statusColor(status: string): string {
  switch (status) {
    case "archived":
      return "text-cold";
    case "missing":
      return "text-alarm";
    case "restoring":
    case "archiving":
      // Full brightness reads as "happening right now".
      return "text-ink";
    default:
      return "text-ink-muted";
  }
}

export function FileBrowser({
  folder,
  filterKey,
  onFilterChange,
  onFolderChanged,
}: FileBrowserProps) {
  const workspace = useFileWorkspace({ folder, filterKey, sort: "name", onFolderChanged });
  const { files: list, selection, actions, preset, filters } = workspace;
  const { files, loading, error, hasMore, loadMore, refetchIds } = list;

  const observer = useRef<IntersectionObserver | null>(null);
  const headerCheckbox = useRef<HTMLInputElement>(null);

  const sentinelRef = useCallback(
    (node: HTMLDivElement | null) => {
      observer.current?.disconnect();
      if (!node) return;
      observer.current = new IntersectionObserver(
        (entries) => {
          if (entries[0].isIntersecting && hasMore && !loading) loadMore();
        },
        { rootMargin: "200px" },
      );
      observer.current.observe(node);
    },
    [hasMore, loading, loadMore],
  );

  const loadedSelected = files.filter((f) => selection.isSelected(f.id)).length;
  const allLoadedSelected = files.length > 0 && loadedSelected === files.length;

  // indeterminate has no HTML attribute; it is a DOM property, so a
  // partially-selected header checkbox can only be set through a ref.
  useEffect(() => {
    if (headerCheckbox.current) {
      headerCheckbox.current.indeterminate = loadedSelected > 0 && !allLoadedSelected;
    }
  }, [loadedSelected, allLoadedSelected]);

  const onFinished = useCallback(
    async (ids: number[]) => {
      await refetchIds(ids);
      onFolderChanged();
    },
    [refetchIds, onFolderChanged],
  );

  if (!folder) {
    return (
      <div className="flex items-center justify-center h-full text-ink-muted">
        Select a folder from the sidebar
      </div>
    );
  }

  return (
    <div className="p-4 md:p-6 pb-24">
      {/* ml-11 clears the fixed sidebar toggle, which is md:hidden and sat on
          top of the first few characters of the folder name at 390px. */}
      <h2 className="text-title font-medium tracking-tight mb-4 ml-11 md:ml-0">{folder}</h2>
      <FilterBar active={filterKey} onChange={onFilterChange} />

      {error && (
        <p className="mb-3 text-meta text-alarm" role="alert">
          {error}
        </p>
      )}

      <div className="overflow-x-auto">
        <table className="w-full text-base">
          <thead>
            <tr className="text-left text-ink-faint border-b border-line-strong text-micro uppercase tracking-wider">
              <th className="pb-2 pr-2 w-8">
                <RowCheckbox
                  indeterminateRef={headerCheckbox}
                  checked={allLoadedSelected}
                  label="Select all loaded files"
                  dataAttrs={{ "data-select-all-loaded": true }}
                  onToggle={() => {
                    if (allLoadedSelected) selection.clear();
                    else selection.selectMany(files.map((f) => f.id));
                  }}
                />
              </th>
              <th className="pb-2 pr-2 w-8" title="Pinned files are never archived">
                Pin
              </th>
              <th className="pb-2 pr-4 font-medium">Name</th>
              <th className="pb-2 pr-4 font-medium hidden md:table-cell">Path</th>
              <th className="pb-2 pr-4 font-medium">Size</th>
              <th className="pb-2 pr-4 font-medium hidden sm:table-cell">Modified</th>
              <th className="pb-2 pr-4 font-medium">Status</th>
              <th className="pb-2 font-medium">Actions</th>
            </tr>
          </thead>
          <tbody>
            {files.map((file) => (
              <Row
                key={file.id}
                file={file}
                selected={selection.isSelected(file.id)}
                conflictMode={preset.key === "conflicts"}
                onToggle={() => selection.toggle(file.id)}
                onToggleRange={() => selection.toggleRange(file.id)}
                onPin={(pinned) => void actions.setPinned(file, pinned)}
                onRestore={() => void actions.restore(file)}
                onArchive={() => void actions.archive(file)}
                onResolve={(keep) => void actions.resolve(file, keep)}
                onDownload={() => actions.download(file)}
              />
            ))}
          </tbody>
        </table>
      </div>

      <div ref={sentinelRef} className="h-8" />

      {loading && <div className="text-center text-ink-faint py-4 text-meta">Loading...</div>}

      {!loading && files.length === 0 && (
        <div className="text-center text-ink-muted py-12 text-meta">
          No files match this filter.
        </div>
      )}

      {!hasMore && files.length > 0 && (
        <div className="text-center text-ink-faint py-4 text-meta tabular">
          {formatCount(files.length)} files
        </div>
      )}

      <SelectionBar
        selection={selection}
        filters={filters}
        files={files}
        onFinished={onFinished}
      />
    </div>
  );
}

interface RowProps {
  file: FileRecord;
  selected: boolean;
  conflictMode: boolean;
  onToggle(): void;
  onToggleRange(): void;
  onPin(pinned: boolean): void;
  onRestore(): void;
  onArchive(): void;
  onResolve(keep: "device" | "archive"): void;
  onDownload(): void;
}

function Row({
  file,
  selected,
  conflictMode,
  onToggle,
  onToggleRange,
  onPin,
  onRestore,
  onArchive,
  onResolve,
  onDownload,
}: RowProps) {
  return (
    <tr
      className={`group border-b border-line/70 hover:bg-raised/50 ${
        selected ? "bg-ink/[0.055]" : ""
      }`}
      data-file-id={file.id}
      data-selected={selected}
    >
      <td className="py-2.5 pr-2">
        <RowCheckbox
          checked={selected}
          label={`Select ${file.filename}`}
          onToggle={onToggle}
          onToggleRange={onToggleRange}
        />
      </td>
      <td className="py-2.5 pr-2">
        <PinToggle file={file} onPin={onPin} />
      </td>
      <td className="py-2.5 pr-4">
        {/* Names are read, so they stay in the sans. Everything to the right of
            here is a measurement or an identifier and goes to the mono, where
            it aligns down the column. */}
        <span className="text-ink">{file.filename}</span>
        {file.thumb_attempts >= 3 && (
          <span
            className="ml-1.5 text-alarm"
            title={file.thumb_error || "thumbnail generation failed"}
          >
            &#9888;
          </span>
        )}
      </td>
      <td className="py-2.5 pr-4 text-ink-faint hidden md:table-cell text-meta font-mono">
        {file.relative_path}
      </td>
      <td className="py-2.5 pr-4 text-ink-muted whitespace-nowrap font-mono text-meta tabular">
        {formatSize(file.size_bytes)}
      </td>
      <td className="py-2.5 pr-4 text-ink-muted hidden sm:table-cell whitespace-nowrap font-mono text-meta tabular">
        {formatDate(file.file_mtime)}
      </td>
      <td className={`py-2.5 pr-4 text-meta ${statusColor(file.status)}`}>{file.status}</td>
      <td className="py-2.5">
        {/* Monochrome on purpose. These three buttons are identical on every
            row, so a colour here repeats down the page without ever
            distinguishing anything -- it was the loudest thing on screen and
            the first thing the owner asked to lose. The palette is spent on
            the status column instead, where it varies and therefore informs. */}
        <div className="flex gap-1 flex-wrap">
          <RowAction onClick={onDownload}>Download</RowAction>
          {file.status === "archived" && !conflictMode && (
            <RowAction onClick={onRestore}>Restore</RowAction>
          )}
          {file.status === "synced" && <RowAction onClick={onArchive}>Archive</RowAction>}
          {conflictMode && (
            <>
              <RowAction onClick={() => onResolve("device")}>Keep device copy</RowAction>
              <RowAction onClick={() => onResolve("archive")} tone="cold">
                Keep archived copy
              </RowAction>
            </>
          )}
        </div>
      </td>
    </tr>
  );
}

interface RowActionProps {
  onClick(): void;
  children: React.ReactNode;
  tone?: "neutral" | "cold";
}

/** A quiet, always-present row action.
 *
 *  Ghost rather than filled: the whole point of the restyle is that these do not
 *  compete with the status column. `tone="cold"` is the single exception, for
 *  the conflict resolution that keeps the archived copy -- there the choice
 *  itself is a state choice, so the state hue is carrying real information. */
function RowAction({ onClick, children, tone = "neutral" }: RowActionProps) {
  // No border. Two of these on every row is thirty outlined rectangles down a
  // full page -- monochrome noise instead of coloured noise. They earn an
  // outline on hover, when one of them is the thing being aimed at.
  const tones = {
    neutral: "text-ink-muted hover:bg-raised-hover hover:text-ink",
    cold: "text-cold hover:bg-cold-dim",
  };
  return (
    <button
      onClick={onClick}
      className={`px-2 py-1 text-meta rounded-md bg-transparent transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor ${tones[tone]}`}
    >
      {children}
    </button>
  );
}

interface RowCheckboxProps {
  checked: boolean;
  label: string;
  onToggle(): void;
  /** Shift-click. Omitted where a range has no meaning, e.g. the header box. */
  onToggleRange?(): void;
  indeterminateRef?: React.Ref<HTMLInputElement>;
  /** Test-contract data attributes, spread onto the real input. */
  dataAttrs?: Record<string, string | boolean>;
}

/** The table's checkbox, drawn to match the gallery tile's.
 *
 *  The native box is kept in the DOM and merely hidden, so `getByRole`,
 *  `toBeChecked`, the tab order and the indeterminate property all keep
 *  working; only its appearance is ours. */
export function RowCheckbox({
  checked,
  label,
  onToggle,
  onToggleRange,
  indeterminateRef,
  dataAttrs,
}: RowCheckboxProps) {
  return (
    <label className="inline-flex p-1 cursor-pointer align-middle">
      <input
        ref={indeterminateRef}
        type="checkbox"
        checked={checked}
        aria-label={label}
        {...dataAttrs}
        onClick={(e) => {
          if (e.shiftKey && onToggleRange) {
            e.preventDefault();
            onToggleRange();
          }
        }}
        onChange={(e) => {
          if (e.nativeEvent instanceof MouseEvent && e.nativeEvent.shiftKey) return;
          onToggle();
        }}
        className="peer sr-only"
      />
      <span
        aria-hidden="true"
        className="block size-[18px] rounded border-2 border-ink-faint bg-transparent transition-colors peer-checked:border-ink peer-checked:bg-ink peer-checked:[&_svg]:opacity-100 peer-indeterminate:border-ink peer-indeterminate:bg-ink peer-indeterminate:[&_svg]:opacity-100 peer-focus-visible:ring-2 peer-focus-visible:ring-cursor peer-focus-visible:ring-offset-1 peer-focus-visible:ring-offset-ground"
      >
        <svg viewBox="0 0 20 20" className="size-full text-ground opacity-0">
          <path
            d={checked ? "M5 10.5l3.2 3.2L15 7" : "M5 10h10"}
            fill="none"
            stroke="currentColor"
            strokeWidth="2.75"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
      </span>
    </label>
  );
}
