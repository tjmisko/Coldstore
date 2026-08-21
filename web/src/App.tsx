import { useCallback, useEffect, useMemo, useState } from "react";
import { Cheatsheet } from "./components/Cheatsheet";
import { CommandPalette } from "./components/CommandPalette";
import { prefetchReaders } from "./components/readers/prefetch";
import { ConfigPanel } from "./components/ConfigPanel";
import { ErrorBanner } from "./components/ErrorBanner";
import { FileBrowser } from "./components/FileBrowser";
import { FilePreviewDialog } from "./components/FilePreviewDialog";
import { FuzzyFinder } from "./components/FuzzyFinder";
import { Gallery } from "./components/Gallery";
import { KeymapProvider } from "./components/KeymapProvider";
import { StatusBar } from "./components/StatusBar";
import { ToastProvider } from "./components/ToastProvider";
import { useDocumentTitle } from "./hooks/useDocumentTitle";
import { useKeymapLayer } from "./hooks/useKeymapLayer";
import { useFolders, useStatus } from "./hooks/useConfig";
import { FILTER_PRESETS, type FilterKey } from "./hooks/useFileWorkspace";
import type { PaletteAction } from "./keymap/palette";
import type { Command } from "./keymap/types";
import { api, errorMessage } from "./lib/api";
import { parseSize } from "./lib/format";
import { useKeymap } from "./lib/keymapContext";
import { useToast } from "./lib/toast";
import { Z } from "./lib/zLayers";
import type { FileRecord } from "./types/api";
import type { FileReveal } from "./types/navigation";

type View = "gallery" | "files" | "config";

const VIEWS: View[] = ["gallery", "files", "config"];

type OverlayState =
  | { kind: "palette"; initial: string }
  | { kind: "finder"; query: string }
  | { kind: "preview"; file: FileRecord }
  | { kind: "cheatsheet" };

function isImageFolder(label: string): boolean {
  const lower = label.toLowerCase();
  return lower.includes("camera") || lower.includes("photo") || lower.includes("picture");
}

export default function App() {
  return (
    <ToastProvider>
      <KeymapProvider>
        <Shell />
      </KeymapProvider>
    </ToastProvider>
  );
}

function Shell() {
  const { status, error, errorStatus, refetch } = useStatus();
  // Memoised because it is a useCallback dependency: a fresh [] every render
  // recreates stepFolder, which re-registers the keymap layer forever.
  const folders = useMemo(() => status?.folders ?? [], [status]);
  const [selectedFolder, setSelectedFolder] = useState<string>("");
  const [view, setView] = useState<View>("gallery");
  const [filterKey, setFilterKey] = useState<FilterKey>("all");
  const [sidebarOpen, setSidebarOpen] = useState(true);
  const [overlay, setOverlay] = useState<OverlayState | null>(null);
  const [reveal, setReveal] = useState<FileReveal | null>(null);
  const selectedFolderStatus = folders.find((folder) => folder.label === selectedFolder);
  const selectedDisplayName = selectedFolderStatus?.display_name || selectedFolder;

  const keymap = useKeymap();
  const toast = useToast();
  const { updateFolder } = useFolders();

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

  useDocumentTitle(view === "config" ? "Settings" : selectedDisplayName);

  // Warm the document readers once the app is idle, so the first PDF opened
  // does not wait on a 1.5 MB fetch. Deliberately NOT part of the initial
  // render path: the gallery grid is what the app opens on, and it must not
  // pay for pdf.js. requestIdleCallback where it exists, a timeout where it
  // does not (Safari).
  useEffect(() => {
    const idle = window.requestIdleCallback;
    if (typeof idle === "function") {
      const handle = idle(() => prefetchReaders());
      return () => window.cancelIdleCallback?.(handle);
    }
    const timer = window.setTimeout(prefetchReaders, 2000);
    return () => window.clearTimeout(timer);
  }, []);

  const selectFolder = useCallback((label: string, filter: FilterKey = "all") => {
    setSelectedFolder(label);
    setFilterKey(filter);
    setView(isImageFolder(label) ? "gallery" : "files");
  }, []);

  const onFolderChanged = useCallback(() => {
    void refetch();
  }, [refetch]);

  const stepFolder = useCallback(
    (delta: number) => {
      if (folders.length === 0) return;
      const at = folders.findIndex((folder) => folder.label === selectedFolder);
      const next = folders[(Math.max(0, at) + delta + folders.length) % folders.length];
      selectFolder(next.label);
    },
    [folders, selectedFolder, selectFolder],
  );

  const stepView = useCallback((delta: number) => {
    setView((current) => VIEWS[(VIEWS.indexOf(current) + delta + VIEWS.length) % VIEWS.length]);
  }, []);

  const rescan = useCallback(async () => {
    try {
      await api.createJob({ action: "scan", folder: selectedFolder });
      toast.show("success", `Rescanning ${selectedFolder || "every folder"}`);
    } catch (err) {
      toast.show("error", errorMessage(err));
    }
  }, [selectedFolder, toast]);

  /** The global layer: everything that is about the app rather than about a
   *  file. File commands fall through to whichever grid is mounted. */
  const handle = useCallback(
    (command: Command): boolean => {
      if (command.kind !== "action") return false;
      switch (command.action) {
        case "view-gallery":
          setView("gallery");
          return true;
        case "view-files":
          setView("files");
          return true;
        case "view-settings":
          setView("config");
          return true;
        case "next-view":
          stepView(1);
          return true;
        case "prev-view":
          stepView(-1);
          return true;
        case "next-folder":
          stepFolder(1);
          return true;
        case "prev-folder":
          stepFolder(-1);
          return true;
        case "palette":
          setOverlay({ kind: "palette", initial: ":" });
          return true;
        case "palette-folder":
          setOverlay({ kind: "palette", initial: ":folder " });
          return true;
        case "palette-filter":
          setOverlay({ kind: "palette", initial: ":filter " });
          return true;
        case "cheatsheet":
          setOverlay({ kind: "cheatsheet" });
          return true;
        case "finder":
          setOverlay({ kind: "finder", query: "" });
          return true;
        case "rescan":
          void rescan();
          return true;
        default:
          return false;
      }
    },
    [rescan, stepFolder, stepView],
  );
  useKeymapLayer("global", handle);

  /** Close the palette WITHOUT clobbering an overlay it opened. `:h` sets the
   *  cheatsheet and then the palette closes itself; a plain setOverlay(null)
   *  would take the cheatsheet with it. */
  const closeIf = useCallback((kind: OverlayState["kind"]) => {
    setOverlay((current) => (current?.kind === kind ? null : current));
  }, []);

  const onPaletteAction = useCallback(
    (action: PaletteAction): string | null => {
      switch (action.kind) {
        case "folder":
          selectFolder(action.label);
          return null;
        case "filter":
          setFilterKey(action.key as FilterKey);
          return null;
        case "view":
          setView(action.view as View);
          return null;
        case "operator": {
          const ran = keymap.dispatch({
            kind: "operator",
            operator: action.operator,
            target: { kind: "selection" },
          });
          return ran ? null : "nothing is selected and there is no file under the cursor";
        }
        case "open":
          return keymap.dispatch({ kind: "action", action: "open", count: 0 })
            ? null
            : "no file under the cursor";
        case "select-all":
          return keymap.dispatch({ kind: "action", action: "select-all-folder", count: 0 })
            ? null
            : "no file list on screen";
        case "clear":
          keymap.dispatch({ kind: "action", action: "clear-selection", count: 0 });
          return null;
        case "budget": {
          if (!selectedFolder) return "no folder selected";
          const bytes = parseSize(action.text);
          // The SAME parseSize the budget editor uses, so a value the palette
          // accepts is byte for byte one the settings panel would accept.
          if (!Number.isFinite(bytes) || bytes <= 0) return `not a size: "${action.text}"`;
          void updateFolder(selectedFolder, { budget_bytes: bytes })
            .then(() => {
              toast.show("success", `${selectedFolder} budget set to ${action.text}`);
              onFolderChanged();
            })
            .catch((err: unknown) => toast.show("error", errorMessage(err)));
          return null;
        }
        case "grace": {
          if (!selectedFolder) return "no folder selected";
          void updateFolder(selectedFolder, { grace_minutes: action.minutes })
            .then(() => {
              toast.show("success", `${selectedFolder} grace period set to ${action.minutes} min`);
              onFolderChanged();
            })
            .catch((err: unknown) => toast.show("error", errorMessage(err)));
          return null;
        }
        case "enabled": {
          if (!selectedFolder) return "no folder selected";
          void updateFolder(selectedFolder, { enabled: action.enabled })
            .then(() => {
              toast.show(
                "success",
                `${action.enabled ? "Enabled" : "Disabled"} coldstore for ${selectedFolder}`,
              );
              onFolderChanged();
            })
            .catch((err: unknown) => toast.show("error", errorMessage(err)));
          return null;
        }
        case "rescan":
          void rescan();
          return null;
        case "finder":
          setOverlay({ kind: "finder", query: action.query });
          return null;
        case "quit":
          keymap.dispatch({ kind: "action", action: "close", count: 0 });
          return null;
        case "help":
          setOverlay({ kind: "cheatsheet" });
          return null;
      }
    },
    [keymap, onFolderChanged, rescan, selectFolder, selectedFolder, toast, updateFolder],
  );

  const showInFiles = useCallback(
    (file: FileRecord) => {
      setSelectedFolder(file.folder_label);
      setFilterKey("all");
      setView("files");
      setReveal((current) => ({ file, token: (current?.token ?? 0) + 1 }));
      setOverlay(null);
    },
    [],
  );

  return (
    <div className="flex h-screen overflow-hidden">
      <button
        style={{ zIndex: Z.sidebarToggle }}
        className="fixed top-3 left-3 md:hidden bg-raised ring-1 ring-line-strong text-ink p-2 rounded-lg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
        onClick={() => setSidebarOpen(!sidebarOpen)}
        aria-label="Toggle sidebar"
      >
        <svg className="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M4 6h16M4 12h16M4 18h16" />
        </svg>
      </button>

      <aside
        style={{ zIndex: Z.sidebar }}
        className={`${
          sidebarOpen ? "translate-x-0" : "-translate-x-full"
        } fixed md:static md:translate-x-0 w-72 h-full bg-surface border-r border-line flex flex-col transition-transform`}
      >
        <div className="p-4 border-b border-line">
          <h1 className="text-lg font-semibold tracking-tight">Coldstore</h1>
        </div>

        {/* Navigation keeps its natural height and the status panel below
            takes the slack, so the folder list is never clipped mid-item by a
            wedge banner appearing underneath it. The cap is for an install
            with more folders than fit; then this scrolls too. */}
        <nav className="shrink-0 max-h-[45%] overflow-y-auto p-2 space-y-1">
          <p className="text-micro uppercase tracking-wider text-ink-faint px-2 pt-2 pb-1.5">Folders</p>
          {folders.map((folder) => (
            <button
              key={folder.label}
              data-folder-nav={folder.label}
              onClick={() => {
                selectFolder(folder.label);
                if (window.innerWidth < 768) setSidebarOpen(false);
              }}
              className={`w-full text-left px-3 py-2 rounded-md text-base transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor ${
                selectedFolder === folder.label
                  ? "bg-raised-hover text-ink"
                  : "text-ink-muted hover:bg-raised hover:text-ink"
              }`}
            >
              <span className={folder.enabled && folder.present_in_toml ? "" : "opacity-60"}>
                {folder.display_name || folder.label}
              </span>
              {!folder.enabled && <span className="ml-1 text-meta text-ink-faint">disabled</span>}
              {!folder.present_in_toml && (
                <span className="ml-1 text-meta text-ink-faint">not in coldstore.toml</span>
              )}
            </button>
          ))}
          {folders.length === 0 && !error && (
            <p className="px-3 py-2 text-base text-ink-faint">No folders yet.</p>
          )}

          <p className="text-micro uppercase tracking-wider text-ink-faint px-2 pt-4 pb-1.5">Views</p>
          {VIEWS.map((option) => (
            <button
              key={option}
              data-view-nav={option}
              onClick={() => {
                setView(option);
                if (window.innerWidth < 768) setSidebarOpen(false);
              }}
              className={`w-full text-left px-3 py-2 rounded-md text-base capitalize transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor ${
                view === option
                  ? "bg-raised-hover text-ink"
                  : "text-ink-muted hover:bg-raised hover:text-ink"
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
          style={{ zIndex: Z.sidebarBackdrop }}
          className="fixed inset-0 bg-ground/60 md:hidden"
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
              displayName={selectedDisplayName}
              filterKey={filterKey}
              onFilterChange={setFilterKey}
              onFolderChanged={onFolderChanged}
              onShowInFiles={showInFiles}
            />
          )}
          {view === "files" && (
            <FileBrowser
              key={`files-${selectedFolder}`}
              folder={selectedFolder}
              displayName={selectedDisplayName}
              filterKey={filterKey}
              onFilterChange={setFilterKey}
              onFolderChanged={onFolderChanged}
              reveal={reveal}
              onShowInFiles={showInFiles}
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

      {overlay?.kind === "palette" && (
        <CommandPalette
          initial={overlay.initial}
          folders={folders.map((folder) => ({
            label: folder.label,
            name: folder.display_name || folder.label,
          }))}
          filters={FILTER_PRESETS.map((preset) => ({ key: preset.key, label: preset.label }))}
          onClose={() => closeIf("palette")}
          onAction={onPaletteAction}
        />
      )}

      {overlay?.kind === "finder" && (
        <FuzzyFinder
          initialQuery={overlay.query}
          filters={{ folder: selectedFolder }}
          onClose={() => closeIf("finder")}
          onOpen={(file) => setOverlay({ kind: "preview", file })}
          onShowInFiles={showInFiles}
        />
      )}

      {overlay?.kind === "preview" && (
        <FilePreviewDialog file={overlay.file} onClose={() => closeIf("preview")} />
      )}

      {overlay?.kind === "cheatsheet" && <Cheatsheet onClose={() => closeIf("cheatsheet")} />}
    </div>
  );
}
