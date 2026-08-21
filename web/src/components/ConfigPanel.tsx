import { useState } from "react";
import { errorMessage } from "../lib/api";
import { formatAgo, formatCount, formatSize, parseSize } from "../lib/format";
import { useEvents, useFolders, usePersistentPaths } from "../hooks/useConfig";
import { useToast } from "../lib/toast";
import type { FolderConfig, FolderStatus, StatusResponse, UpdateFolderRequest } from "../types/api";

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
        <h2 className="text-xl font-semibold ml-11 md:ml-0">Settings</h2>
        <p className="text-xs text-neutral-500 mt-1">
          Budgets, grace periods and persistent patterns live in the catalog and take effect on the
          next enforcement tick -- no restart. The path and the Syncthing id come from
          coldstore.toml.
        </p>
      </div>

      {error && (
        <p className="text-sm text-red-400" role="alert">
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
          <p className="text-neutral-500 text-sm">
            No folders yet. Declare them in coldstore.toml and restart the daemon; coldstore seeds
            each one into the catalog the first time it sees it.
          </p>
        )}
      </section>

      {selectedFolder && (
        <>
          <PersistentPathsEditor folder={selectedFolder} />
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
      className={`bg-neutral-900 rounded-lg p-4 border ${
        selected ? "border-blue-700" : "border-neutral-800"
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
          <p className="text-xs text-neutral-500 mt-1 truncate" title={folder.syncthing_path}>
            {folder.syncthing_path}
          </p>
          <p className="text-xs text-neutral-600">
            syncthing id {folder.syncthing_id || "-"} &middot; path and id come from coldstore.toml
          </p>
        </div>

        <label className="flex items-center gap-2 text-xs text-neutral-400 whitespace-nowrap">
          <input
            type="checkbox"
            checked={folder.enabled}
            data-folder-enabled={folder.folder_label}
            onChange={(e) => void onSave({ enabled: e.target.checked })}
            className="accent-blue-500"
          />
          enabled
        </label>
      </div>

      <div className="mt-3 grid grid-cols-1 sm:grid-cols-2 gap-3">
        <div>
          <label className="block text-xs text-neutral-400 mb-1" htmlFor={`budget-${folder.folder_label}`}>
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
            className="w-full bg-neutral-800 border border-neutral-700 rounded-md px-2 py-1 text-sm focus:outline-none focus:border-blue-500"
          />
          {budgetError ? (
            <p className="text-xs text-red-400 mt-1">{budgetError}</p>
          ) : (
            live && (
              <p className="text-xs text-neutral-500 mt-1">
                {formatSize(live.synced_size_bytes)} on device
                {live.pinned_bytes > 0 && `, ${formatSize(live.pinned_bytes)} pinned`}
              </p>
            )
          )}
        </div>

        <div>
          <label className="block text-xs text-neutral-400 mb-1" htmlFor={`grace-${folder.folder_label}`}>
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
            className="w-full bg-neutral-800 border border-neutral-700 rounded-md px-2 py-1 text-sm focus:outline-none focus:border-blue-500"
          />
          {graceError ? (
            <p className="text-xs text-red-400 mt-1">{graceError}</p>
          ) : (
            <p className="text-xs text-neutral-500 mt-1">
              0 means no grace. Last checked {formatAgo(folder.last_enforced_at)}.
            </p>
          )}
        </div>
      </div>

      <button
        onClick={onSelect}
        className="mt-3 text-xs text-blue-400 hover:text-blue-300"
        data-manage-folder={folder.folder_label}
      >
        {selected ? "Editing patterns and activity below" : "Manage patterns and activity"}
      </button>
    </div>
  );
}

function Chip({ children, tone }: { children: React.ReactNode; tone: "neutral" | "warn" | "danger" }) {
  const palette = {
    neutral: "bg-neutral-800 text-neutral-400 border-neutral-700",
    warn: "bg-amber-950 text-amber-300 border-amber-800",
    danger: "bg-red-950 text-red-300 border-red-800",
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
      <h3 className="text-sm font-medium text-neutral-400 uppercase mb-2">
        Persistent patterns: {folder}
      </h3>
      <p className="text-xs text-neutral-500 mb-3">
        Files matching these patterns are never archived, whatever the budget says. A trailing{" "}
        <code>/</code> or <code>/*</code> protects a whole subtree; a pattern containing{" "}
        <code>/</code> matches the full path; a pattern without one also matches bare filenames.
      </p>

      {error && (
        <p className="text-sm text-red-400 mb-2" role="alert">
          {error}
        </p>
      )}

      <div className="space-y-2 mb-4">
        {paths.map((path) => (
          <div
            key={path.id}
            className="flex items-center justify-between bg-neutral-900 rounded-md px-3 py-2 border border-neutral-800"
            data-pattern={path.pattern}
          >
            <div>
              <code className="text-sm text-neutral-300">{path.pattern}</code>
              <p
                className={`text-xs ${path.match_count === 0 ? "text-amber-400" : "text-neutral-500"}`}
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
              className="text-red-400 hover:text-red-300 text-xs"
            >
              remove
            </button>
          </div>
        ))}
        {paths.length === 0 && !error && (
          <p className="text-neutral-600 text-sm">No persistent patterns configured.</p>
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
          className="flex-1 bg-neutral-800 border border-neutral-700 rounded-md px-3 py-2 text-sm focus:outline-none focus:border-blue-500"
        />
        <button
          onClick={() => void add()}
          disabled={busy}
          className="px-4 py-2 bg-blue-600 hover:bg-blue-500 disabled:opacity-50 rounded-md text-sm"
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
      <h3 className="text-sm font-medium text-neutral-400 uppercase mb-2">Activity: {folder}</h3>
      {error && (
        <p className="text-sm text-red-400" role="alert">
          {error}
        </p>
      )}
      {events.length === 0 && !error && (
        <p className="text-neutral-600 text-sm">Nothing to report for this folder.</p>
      )}
      <ul className="space-y-1.5">
        {events.map((event) => (
          <li key={event.id} className="text-xs border-l-2 border-neutral-700 pl-2">
            <span className="text-neutral-300">{event.event_type}</span>
            {event.path && <span className="text-neutral-500"> &middot; {event.path}</span>}
            <p className="text-neutral-500">{event.details}</p>
            <p className="text-neutral-600">{new Date(event.timestamp).toLocaleString()}</p>
          </li>
        ))}
      </ul>
    </section>
  );
}
