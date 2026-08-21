import { useState } from "react";
import { errorMessage } from "../lib/api";
import { formatAgo, formatCount, formatSize, parseSize } from "../lib/format";
import { useEvents, useFolders, usePersistentPaths } from "../hooks/useConfig";
import { useToast } from "../lib/toast";
import type { FolderConfig, FolderStatus, StatusResponse, UpdateFolderRequest } from "../types/api";
import { TagManager } from "./TagManager";

interface ConfigPanelProps {
  status: StatusResponse | null;
  selectedFolder: string;
  onSelectFolder(label: string): void;
  /** Refresh the status poll's figures after a policy change. */
  onChanged(): void;
}

export function ConfigPanel({
  status,
  selectedFolder,
  onSelectFolder,
  onChanged,
}: ConfigPanelProps) {
  const { folders, error, updateFolder } = useFolders();
  const toast = useToast();

  const save = async (label: string, patch: UpdateFolderRequest) => {
    try {
      await updateFolder(label, patch);
      onChanged();
      toast.show("success", `Saved ${label}`);
    } catch (err) {
      toast.show("error", `${label}: ${errorMessage(err)}`);
    }
  };

  return (
    <div className="p-4 md:p-6 max-w-3xl space-y-8">
      <div>
        {/* ml-11 clears the fixed sidebar toggle, which is md:hidden. */}
        <h2 className="text-title font-medium tracking-tight ml-11 md:ml-0">Settings</h2>
        <p className="text-meta text-ink-muted mt-1.5">
          Budgets, grace periods and persistent patterns live in the catalog and take effect on the
          next enforcement tick -- no restart. The path and the Syncthing id come from
          coldstore.toml.
        </p>
      </div>

      {error && (
        <p className="text-base text-alarm" role="alert">
          Cannot load folder settings &mdash; {error}
        </p>
      )}

      <section className="space-y-3">
        {folders.map((folder) => (
          <FolderCard
            key={folder.folder_label}
            folder={folder}
            live={status?.folders.find((f) => f.label === folder.folder_label)}
            selected={selectedFolder === folder.folder_label}
            onSelect={() => onSelectFolder(folder.folder_label)}
            onSave={(patch) => save(folder.folder_label, patch)}
          />
        ))}
        {folders.length === 0 && !error && (
          <p className="text-ink-muted text-base">
            No folders yet. Declare them in coldstore.toml and restart the daemon; coldstore seeds
            each one into the catalog the first time it sees it.
          </p>
        )}
      </section>

      {selectedFolder && (
        <>
          <PersistentPathsEditor folder={selectedFolder} />
          <TagManager folder={selectedFolder} />
          <ActivityList folder={selectedFolder} />
        </>
      )}
    </div>
  );
}

interface FolderCardProps {
  folder: FolderConfig;
  live: FolderStatus | undefined;
  selected: boolean;
  onSelect(): void;
  onSave(patch: UpdateFolderRequest): Promise<void>;
}

function FolderCard({ folder, live, selected, onSelect, onSave }: FolderCardProps) {
  // A free-text size rather than the "number of GB" box design section 7.9
  // sketches: budgets here span kilobytes (the demo) to terabytes, and a
  // GB-only input renders a 200 kB budget as 0.0002. parseSize accepts exactly
  // what coldstore.toml accepts, so "20 GB" typed here is the same 20000000000
  // the owner wrote in the file.
  const [budget, setBudget] = useState(formatSize(folder.budget_bytes));
  const [grace, setGrace] = useState(String(folder.grace_minutes));
  const [budgetError, setBudgetError] = useState<string | null>(null);
  const [graceError, setGraceError] = useState<string | null>(null);

  const commitBudget = async () => {
    const bytes = parseSize(budget);
    if (!Number.isFinite(bytes) || bytes <= 0) {
      setBudgetError("Enter a positive size, e.g. 20 GB");
      return;
    }
    setBudgetError(null);
    if (bytes === folder.budget_bytes) return;
    await onSave({ budget_bytes: bytes });
  };

  const commitGrace = async () => {
    const minutes = Number.parseInt(grace, 10);
    if (!Number.isFinite(minutes) || minutes < 0) {
      setGraceError("Enter 0 or more minutes");
      return;
    }
    setGraceError(null);
    if (minutes === folder.grace_minutes) return;
    await onSave({ grace_minutes: minutes });
  };

  return (
    <div
      className={`bg-surface rounded-lg p-4 border ${
        selected ? "border-line-strong" : "border-line"
      }`}
      data-folder-card={folder.folder_label}
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="font-medium flex items-center gap-2 flex-wrap">
            {folder.folder_label}
            {!folder.present_in_toml && <Chip tone="neutral">not in coldstore.toml</Chip>}
            {!folder.enabled && <Chip tone="neutral">disabled</Chip>}
            {live?.wedged && <Chip tone="danger">wedged</Chip>}
            {live?.over_budget && !live.wedged && <Chip tone="warn">over budget</Chip>}
            {live && live.syncthing_type !== "" && live.syncthing_type !== "sendreceive" && (
              <Chip tone="danger">{live.syncthing_type}</Chip>
            )}
          </p>
          <p className="text-meta text-ink-muted mt-1.5 truncate font-mono" title={folder.syncthing_path}>
            {folder.syncthing_path}
          </p>
          <p className="text-meta text-ink-faint">
            syncthing id {folder.syncthing_id || "-"} &middot; path and id come from coldstore.toml
          </p>
        </div>

        <label className="flex items-center gap-2 text-meta text-ink-muted whitespace-nowrap">
          <input
            type="checkbox"
            checked={folder.enabled}
            data-folder-enabled={folder.folder_label}
            onChange={(e) => void onSave({ enabled: e.target.checked })}
            className="size-4 accent-ink"
          />
          enabled
        </label>
      </div>

      <div className="mt-3 grid grid-cols-1 sm:grid-cols-2 gap-3">
        <div>
          <label className="block text-meta text-ink-muted mb-1.5" htmlFor={`budget-${folder.folder_label}`}>
            Budget
          </label>
          <input
            id={`budget-${folder.folder_label}`}
            type="text"
            value={budget}
            data-budget-input={folder.folder_label}
            onChange={(e) => setBudget(e.target.value)}
            onBlur={() => void commitBudget()}
            onKeyDown={(e) => {
              if (e.key === "Enter") void commitBudget();
            }}
            className="w-full bg-raised border border-line-strong rounded-md px-2.5 py-1.5 text-base text-ink focus:outline-none focus-visible:ring-2 focus-visible:ring-cursor font-mono"
          />
          {budgetError ? (
            <p className="text-meta text-alarm mt-1.5">{budgetError}</p>
          ) : (
            live && (
              <p className="text-meta text-ink-muted mt-1.5 tabular">
                {formatSize(live.synced_size_bytes)} on device
                {live.pinned_bytes > 0 && `, ${formatSize(live.pinned_bytes)} pinned`}
              </p>
            )
          )}
        </div>

        <div>
          <label className="block text-meta text-ink-muted mb-1.5" htmlFor={`grace-${folder.folder_label}`}>
            Grace period (minutes)
          </label>
          <input
            id={`grace-${folder.folder_label}`}
            type="number"
            min={0}
            value={grace}
            data-grace-input={folder.folder_label}
            onChange={(e) => setGrace(e.target.value)}
            onBlur={() => void commitGrace()}
            onKeyDown={(e) => {
              if (e.key === "Enter") void commitGrace();
            }}
            className="w-full bg-raised border border-line-strong rounded-md px-2.5 py-1.5 text-base text-ink focus:outline-none focus-visible:ring-2 focus-visible:ring-cursor font-mono"
          />
          {graceError ? (
            <p className="text-meta text-alarm mt-1.5">{graceError}</p>
          ) : (
            <p className="text-meta text-ink-muted mt-1.5">
              0 means no grace. Last checked {formatAgo(folder.last_enforced_at)}.
            </p>
          )}
        </div>
      </div>

      <button
        onClick={onSelect}
        className="mt-3 text-meta text-ink-muted hover:text-ink underline underline-offset-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor rounded"
        data-manage-folder={folder.folder_label}
      >
        {selected ? "Editing patterns and activity below" : "Manage patterns and activity"}
      </button>
    </div>
  );
}

function Chip({ children, tone }: { children: React.ReactNode; tone: "neutral" | "warn" | "danger" }) {
  const palette = {
    neutral: "bg-raised text-ink-muted border-line-strong",
    warn: "bg-cold-dim text-cold border-cold/40",
    danger: "bg-alarm-dim text-alarm border-alarm/45",
  }[tone];
  return (
    <span className={`text-[10px] uppercase tracking-wide px-1.5 py-0.5 rounded border ${palette}`}>
      {children}
    </span>
  );
}

function PersistentPathsEditor({ folder }: { folder: string }) {
  const { paths, error, addPath, deletePath } = usePersistentPaths(folder);
  const [pattern, setPattern] = useState("");
  const [busy, setBusy] = useState(false);
  const toast = useToast();

  const add = async () => {
    const trimmed = pattern.trim();
    if (!trimmed) return;
    setBusy(true);
    try {
      await addPath(trimmed);
      setPattern("");
    } catch (err) {
      toast.show("error", errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section>
      <h3 className="text-micro font-medium text-ink-faint uppercase tracking-wider mb-2">
        Persistent patterns: {folder}
      </h3>
      <p className="text-meta text-ink-muted mb-3">
        Files matching these patterns are never archived, whatever the budget says. A trailing{" "}
        <code>/</code> or <code>/*</code> protects a whole subtree; a pattern containing{" "}
        <code>/</code> matches the full path; a pattern without one also matches bare filenames.
      </p>

      {error && (
        <p className="text-base text-alarm mb-2" role="alert">
          {error}
        </p>
      )}

      <div className="space-y-2 mb-4">
        {paths.map((path) => (
          <div
            key={path.id}
            className="flex items-center justify-between bg-surface rounded-md px-3 py-2.5 border border-line"
            data-pattern={path.pattern}
          >
            <div>
              <code className="text-base text-ink font-mono">{path.pattern}</code>
              <p
                className={`text-meta ${path.match_count === 0 ? "text-alarm" : "text-ink-muted"}`}
                data-match-count={path.match_count}
              >
                {/* A pattern that protects nothing looks exactly like one that
                    protects everything until it says so. */}
                protects {formatCount(path.match_count)} file
                {path.match_count === 1 ? "" : "s"} in this folder
                {path.match_count === 0 && " -- check the pattern"}
              </p>
            </div>
            <button
              onClick={() => void deletePath(path.id)}
              className="text-ink-faint hover:text-alarm text-meta focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor rounded"
            >
              remove
            </button>
          </div>
        ))}
        {paths.length === 0 && !error && (
          <p className="text-ink-faint text-base">No persistent patterns configured.</p>
        )}
      </div>

      <div className="flex gap-2">
        <input
          type="text"
          value={pattern}
          onChange={(e) => setPattern(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") void add();
          }}
          placeholder="e.g. Albums/Starred/"
          data-new-pattern
          className="flex-1 bg-raised border border-line-strong rounded-md px-2.5 py-1.5 text-base text-ink focus:outline-none focus-visible:ring-2 focus-visible:ring-cursor font-mono"
        />
        <button
          onClick={() => void add()}
          disabled={busy}
          className="px-4 py-2 bg-ink text-ground font-medium hover:brightness-95 disabled:opacity-50 rounded-md text-base focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor focus-visible:ring-offset-2 focus-visible:ring-offset-ground"
        >
          Add
        </button>
      </div>
    </section>
  );
}

/** Warning-class events only.
 *
 *  Two panels design section 7.9 also asks for are deliberately absent: the
 *  stray-archive line and the .stignore chip. Sections 5.11 and 3.8 are marked
 *  NOT IMPLEMENTED, so nothing writes the events they would read, and a panel
 *  that can only ever render "nothing to report" is a claim that cold storage
 *  has been swept when it has not. */
function ActivityList({ folder }: { folder: string }) {
  const { events, error } = useEvents(folder, { warningsOnly: true, limit: 10 });

  return (
    <section>
      <h3 className="text-micro font-medium text-ink-faint uppercase tracking-wider mb-2">Activity: {folder}</h3>
      {error && (
        <p className="text-base text-alarm" role="alert">
          {error}
        </p>
      )}
      {events.length === 0 && !error && (
        <p className="text-ink-faint text-base">Nothing to report for this folder.</p>
      )}
      <ul className="space-y-1.5">
        {events.map((event) => (
          <li key={event.id} className="text-meta border-l-2 border-line-strong pl-2.5">
            <span className="text-ink font-mono">{event.event_type}</span>
            {event.path && <span className="text-ink-muted font-mono"> &middot; {event.path}</span>}
            <p className="text-ink-muted">{event.details}</p>
            <p className="text-ink-faint tabular">{new Date(event.timestamp).toLocaleString()}</p>
          </li>
        ))}
      </ul>
    </section>
  );
}
