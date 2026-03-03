import { useState, useEffect } from "react";
import { Gallery } from "./components/Gallery";
import { FileBrowser } from "./components/FileBrowser";
import { ConfigPanel } from "./components/ConfigPanel";
import { StatusBar } from "./components/StatusBar";
import { useStatus } from "./hooks/useConfig";

type View = "gallery" | "files" | "config";

function isImageFolder(label: string): boolean {
  return label.toLowerCase().includes("camera") || label.toLowerCase().includes("photo");
}

export default function App() {
  const { status, refetch: refetchStatus, triggerScan } = useStatus();
  const folders = status?.folders ?? [];
  const [selectedFolder, setSelectedFolder] = useState<string>("");
  const [view, setView] = useState<View>("gallery");
  const [sidebarOpen, setSidebarOpen] = useState(true);

  useEffect(() => {
    if (folders.length > 0 && selectedFolder === "") {
      handleFolderSelect(folders[0].label);
    }
  }, [folders]);

  const handleFolderSelect = (label: string) => {
    setSelectedFolder(label);
    setView(isImageFolder(label) ? "gallery" : "files");
  };

  return (
    <div className="flex h-screen overflow-hidden">
      {/* Mobile sidebar toggle */}
      <button
        className="fixed top-3 left-3 z-50 md:hidden bg-neutral-800 p-2 rounded-lg"
        onClick={() => setSidebarOpen(!sidebarOpen)}
      >
        <svg className="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M4 6h16M4 12h16M4 18h16" />
        </svg>
      </button>

      {/* Sidebar */}
      <aside
        className={`${
          sidebarOpen ? "translate-x-0" : "-translate-x-full"
        } fixed md:static md:translate-x-0 z-40 w-56 h-full bg-neutral-900 border-r border-neutral-800 flex flex-col transition-transform`}
      >
        <div className="p-4 border-b border-neutral-800">
          <h1 className="text-lg font-semibold tracking-tight">Coldstore</h1>
        </div>

        <nav className="flex-1 overflow-y-auto p-2 space-y-1">
          <p className="text-xs uppercase text-neutral-500 px-2 pt-2 pb-1">Folders</p>
          {folders.map((f) => (
            <button
              key={f.label}
              onClick={() => {
                handleFolderSelect(f.label);
                if (window.innerWidth < 768) setSidebarOpen(false);
              }}
              className={`w-full text-left px-3 py-2 rounded-md text-sm transition-colors ${
                selectedFolder === f.label
                  ? "bg-neutral-700 text-white"
                  : "text-neutral-400 hover:bg-neutral-800 hover:text-neutral-200"
              }`}
            >
              {f.label}
            </button>
          ))}

          <p className="text-xs uppercase text-neutral-500 px-2 pt-4 pb-1">Views</p>
          {(["gallery", "files", "config"] as View[]).map((v) => (
            <button
              key={v}
              onClick={() => {
                setView(v);
                if (window.innerWidth < 768) setSidebarOpen(false);
              }}
              className={`w-full text-left px-3 py-2 rounded-md text-sm capitalize transition-colors ${
                view === v
                  ? "bg-neutral-700 text-white"
                  : "text-neutral-400 hover:bg-neutral-800 hover:text-neutral-200"
              }`}
            >
              {view === v && <span className="mr-1.5">&#10003;</span>}
              {v === "config" ? "Settings" : v}
            </button>
          ))}
        </nav>

        <StatusBar status={status} onRefresh={refetchStatus} onScan={triggerScan} />
      </aside>

      {/* Overlay for mobile sidebar */}
      {sidebarOpen && (
        <div
          className="fixed inset-0 bg-black/50 z-30 md:hidden"
          onClick={() => setSidebarOpen(false)}
        />
      )}

      {/* Main content */}
      <main className="flex-1 overflow-y-auto">
        {view === "gallery" && <Gallery folder={selectedFolder} />}
        {view === "files" && <FileBrowser folder={selectedFolder} />}
        {view === "config" && <ConfigPanel />}
      </main>
    </div>
  );
}
