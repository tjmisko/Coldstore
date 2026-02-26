import { useState } from "react";
import { useFolders, usePersistentPaths } from "../hooks/useConfig";

function formatSize(bytes: number): string {
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(0)} MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)} GB`;
}

export function ConfigPanel() {
  const { folders } = useFolders();
  const [selectedFolder, setSelectedFolder] = useState<string>("");

  return (
    <div className="p-4 md:p-6 max-w-3xl">
      <h2 className="text-xl font-semibold mb-6">Settings</h2>

      <section className="mb-8">
        <h3 className="text-sm font-medium text-neutral-400 uppercase mb-3">Folder Budgets</h3>
        <div className="space-y-3">
          {folders.map((fc) => (
            <div
              key={fc.folder_label}
              className="bg-neutral-900 rounded-lg p-4 border border-neutral-800"
            >
              <div className="flex items-center justify-between">
                <div>
                  <p className="font-medium">{fc.folder_label}</p>
                  <p className="text-xs text-neutral-500 mt-1">{fc.syncthing_path}</p>
                </div>
                <div className="text-right">
                  <p className="text-sm">{formatSize(fc.budget_bytes)}</p>
                  <p className="text-xs text-neutral-500">{fc.grace_minutes} min grace</p>
                </div>
              </div>
              <button
                onClick={() => setSelectedFolder(fc.folder_label)}
                className="mt-2 text-xs text-blue-400 hover:text-blue-300"
              >
                Manage persistent paths
              </button>
            </div>
          ))}
          {folders.length === 0 && (
            <p className="text-neutral-500 text-sm">
              No folder configs in database. Folders are configured via coldstore.toml.
            </p>
          )}
        </div>
      </section>

      {selectedFolder && <PersistentPathsEditor folder={selectedFolder} />}
    </div>
  );
}

function PersistentPathsEditor({ folder }: { folder: string }) {
  const { paths, addPath, deletePath } = usePersistentPaths(folder);
  const [newPattern, setNewPattern] = useState("");

  const handleAdd = async () => {
    if (!newPattern.trim()) return;
    await addPath(newPattern.trim());
    setNewPattern("");
  };

  return (
    <section>
      <h3 className="text-sm font-medium text-neutral-400 uppercase mb-3">
        Persistent Paths: {folder}
      </h3>
      <p className="text-xs text-neutral-500 mb-3">
        Files matching these glob patterns will never be offloaded.
      </p>

      <div className="space-y-2 mb-4">
        {paths.map((p) => (
          <div
            key={p.id}
            className="flex items-center justify-between bg-neutral-900 rounded-md px-3 py-2 border border-neutral-800"
          >
            <code className="text-sm text-neutral-300">{p.pattern}</code>
            <button
              onClick={() => deletePath(p.id)}
              className="text-red-400 hover:text-red-300 text-xs"
            >
              remove
            </button>
          </div>
        ))}
        {paths.length === 0 && (
          <p className="text-neutral-600 text-sm">No persistent paths configured.</p>
        )}
      </div>

      <div className="flex gap-2">
        <input
          type="text"
          value={newPattern}
          onChange={(e) => setNewPattern(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && handleAdd()}
          placeholder="e.g. Albums/Starred/*"
          className="flex-1 bg-neutral-800 border border-neutral-700 rounded-md px-3 py-2 text-sm focus:outline-none focus:border-blue-500"
        />
        <button
          onClick={handleAdd}
          className="px-4 py-2 bg-blue-600 hover:bg-blue-500 rounded-md text-sm transition-colors"
        >
          Add
        </button>
      </div>
    </section>
  );
}
