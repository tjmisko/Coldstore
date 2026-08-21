import { useCallback, useState } from "react";
import { ConfigPanel } from "./components/ConfigPanel";
import { ErrorBanner } from "./components/ErrorBanner";
import { FileBrowser } from "./components/FileBrowser";
import { Gallery } from "./components/Gallery";
import { StatusBar } from "./components/StatusBar";
import { ToastProvider } from "./components/ToastProvider";
import { useStatus } from "./hooks/useConfig";
import { useDocumentTitle } from "./hooks/useDocumentTitle";
import type { FilterKey } from "./hooks/useFileWorkspace";

type View = "gallery" | "files" | "config";

function isImageFolder(label: string): boolean {
  const lower = label.toLowerCase();
  return lower.includes("camera") || lower.includes("photo") || lower.includes("picture");
}

export default function App() {
  return (
    <ToastProvider>
      <Shell />
    </ToastProvider>
  );
}

function Shell() {
  const { status, error, errorStatus, refetch } = useStatus();
  const folders = status?.folders ?? [];
  const [selectedFolder, setSelectedFolder] = useState<string>("");
  const [view, setView] = useState<View>("gallery");
  const [filterKey, setFilterKey] = useState<FilterKey>("all");
  const [sidebarOpen, setSidebarOpen] = useState(true);

  // Auto-select the first folder the moment status arrives. This is React's
  // documented "adjusting state during render" pattern rather than a
  // useEffect: the guard flips false as soon as it runs, React re-renders
  // before painting, and there is no cascading render.
  // The `label !== ""` guard is what makes it terminate: a folder with an
  // empty label would leave the condition true forever.
  if (selectedFolder === "" && folders.length > 0 && folders[0].label !== "") {
    setSelectedFolder(folders[0].label);
    setView(isImageFolder(folders[0].label) ? "gallery" : "files");
  }

  useDocumentTitle(view === "config" ? "Settings" : selectedFolder);

  const selectFolder = useCallback((label: string, filter: FilterKey = "all") => {
    setSelectedFolder(label);
    setFilterKey(filter);
    setView(isImageFolder(label) ? "gallery" : "files");
  }, []);

  const onFolderChanged = useCallback(() => {
    void refetch();
  }, [refetch]);

  return (
    <div className="flex h-screen overflow-hidden">
      <button
        className="fixed top-3 left-3 z-50 md:hidden bg-neutral-800 p-2 rounded-lg"
        onClick={() => setSidebarOpen(!sidebarOpen)}
        aria-label="Toggle sidebar"
      >
        <svg className="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M4 6h16M4 12h16M4 18h16" />
        </svg>
      </button>

      <aside
        className={`${
          sidebarOpen ? "translate-x-0" : "-translate-x-full"
        } fixed md:static md:translate-x-0 z-40 w-72 h-full bg-neutral-900 border-r border-neutral-800 flex flex-col transition-transform`}
      >
        <div className="p-4 border-b border-neutral-800">
          <h1 className="text-lg font-semibold tracking-tight">Coldstore</h1>
        </div>

        {/* Navigation keeps its natural height and the status panel below
            takes the slack, so the folder list is never clipped mid-item by a
            wedge banner appearing underneath it. The cap is for an install
            with more folders than fit; then this scrolls too. */}
        <nav className="shrink-0 max-h-[45%] overflow-y-auto p-2 space-y-1">
          <p className="text-xs uppercase text-neutral-500 px-2 pt-2 pb-1">Folders</p>
          {folders.map((folder) => (
            <button
              key={folder.label}
              data-folder-nav={folder.label}
              onClick={() => {
                selectFolder(folder.label);
                if (window.innerWidth < 768) setSidebarOpen(false);
              }}
              className={`w-full text-left px-3 py-2 rounded-md text-sm transition-colors ${
                selectedFolder === folder.label
                  ? "bg-neutral-700 text-white"
                  : "text-neutral-400 hover:bg-neutral-800 hover:text-neutral-200"
              }`}
            >
              <span className={folder.enabled && folder.present_in_toml ? "" : "opacity-60"}>
                {folder.label}
              </span>
              {!folder.enabled && <span className="ml-1 text-xs text-neutral-500">disabled</span>}
              {!folder.present_in_toml && (
                <span className="ml-1 text-xs text-neutral-500">not in coldstore.toml</span>
              )}
            </button>
          ))}
          {folders.length === 0 && !error && (
            <p className="px-3 py-2 text-sm text-neutral-600">No folders yet.</p>
          )}

          <p className="text-xs uppercase text-neutral-500 px-2 pt-4 pb-1">Views</p>
          {(["gallery", "files", "config"] as View[]).map((option) => (
            <button
              key={option}
              data-view-nav={option}
              onClick={() => {
                setView(option);
                if (window.innerWidth < 768) setSidebarOpen(false);
              }}
              className={`w-full text-left px-3 py-2 rounded-md text-sm capitalize transition-colors ${
                view === option
                  ? "bg-neutral-700 text-white"
                  : "text-neutral-400 hover:bg-neutral-800 hover:text-neutral-200"
              }`}
            >
              {view === option && <span className="mr-1.5">&#10003;</span>}
              {option === "config" ? "Settings" : option}
            </button>
          ))}
        </nav>

        <StatusBar
          status={status}
          error={error}
          onReview={(label, filter) => selectFolder(label, filter)}
          onRefresh={refetch}
        />
      </aside>

      {sidebarOpen && (
        <div
          className="fixed inset-0 bg-black/50 z-30 md:hidden"
          onClick={() => setSidebarOpen(false)}
        />
      )}

      <main className="flex-1 overflow-y-auto flex flex-col">
        {error && <ErrorBanner message={error} status={errorStatus} />}

        <div className="flex-1">
          {view === "gallery" && (
            <Gallery
              key={`gallery-${selectedFolder}`}
              folder={selectedFolder}
              filterKey={filterKey}
              onFilterChange={setFilterKey}
              onFolderChanged={onFolderChanged}
            />
          )}
          {view === "files" && (
            <FileBrowser
              key={`files-${selectedFolder}`}
              folder={selectedFolder}
              filterKey={filterKey}
              onFilterChange={setFilterKey}
              onFolderChanged={onFolderChanged}
            />
          )}
          {view === "config" && (
            <ConfigPanel
              status={status}
              selectedFolder={selectedFolder}
              onSelectFolder={setSelectedFolder}
              onChanged={onFolderChanged}
            />
          )}
        </div>
      </main>
    </div>
  );
}
