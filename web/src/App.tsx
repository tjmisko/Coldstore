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
import { FolderNavItem, StatusBar } from "./components/StatusBar";
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
type BrowsingView = Exclude<View, "config">;

const VIEWS: View[] = ["gallery", "files", "config"];
const BROWSING_VIEWS: BrowsingView[] = ["gallery", "files"];
const WORKSPACE_STORAGE_KEY = "coldstore.workspace.v1";
const ALL_FOLDERS_VIEW_KEY = "*";

interface SavedWorkspace {
  folder: string;
  allFolders: boolean;
  view: View;
  folderViews: Record<string, BrowsingView>;
  filterKey: FilterKey;
  tags: string[];
}

const DEFAULT_WORKSPACE: SavedWorkspace = {
  folder: "",
  allFolders: false,
  view: "gallery",
  folderViews: {},
  filterKey: "all",
  tags: [],
};

function readWorkspace(): SavedWorkspace {
  if (typeof localStorage === "undefined") return DEFAULT_WORKSPACE;
  try {
    const value = JSON.parse(localStorage.getItem(WORKSPACE_STORAGE_KEY) ?? "null") as Partial<SavedWorkspace> | null;
    const folder = typeof value?.folder === "string" ? value.folder : "";
    const allFolders = value?.allFolders === true;
    const savedView = VIEWS.includes(value?.view as View) ? (value?.view as View) : "gallery";
    const rawFolderViews = value?.folderViews;
    const folderViews = rawFolderViews && typeof rawFolderViews === "object" && !Array.isArray(rawFolderViews)
      ? Object.fromEntries(
          Object.entries(rawFolderViews).filter(
            (entry): entry is [string, BrowsingView] =>
              entry[0] !== "" && BROWSING_VIEWS.includes(entry[1] as BrowsingView),
          ),
        )
      : {};
    const locationKey = allFolders ? ALL_FOLDERS_VIEW_KEY : folder;

    // A v1 workspace saved only one global view. Seed the per-folder map from
    // it so existing browsers keep their current choice when they upgrade.
    if (locationKey && BROWSING_VIEWS.includes(savedView as BrowsingView) && !folderViews[locationKey]) {
      folderViews[locationKey] = savedView as BrowsingView;
    }

    return {
      folder,
      allFolders,
      view: savedView === "config" ? savedView : folderViews[locationKey] ?? savedView,
      folderViews,
      filterKey: FILTER_PRESETS.some((preset) => preset.key === value?.filterKey)
        ? (value?.filterKey as FilterKey)
        : "all",
      tags: Array.isArray(value?.tags)
        ? value.tags.filter((tag): tag is string => typeof tag === "string" && tag.trim() !== "")
        : [],
    };
  } catch {
    return DEFAULT_WORKSPACE;
  }
}

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
  const [initialWorkspace] = useState(readWorkspace);
  const [selectedFolder, setSelectedFolder] = useState<string>(initialWorkspace.folder);
  const [allFolders, setAllFolders] = useState(initialWorkspace.allFolders);
  const [view, setView] = useState<View>(initialWorkspace.view);
  const [folderViews, setFolderViews] = useState<Record<string, BrowsingView>>(initialWorkspace.folderViews);
  const [filterKey, setFilterKey] = useState<FilterKey>(initialWorkspace.filterKey);
  const [tagFilters, setTagFilters] = useState<string[]>(initialWorkspace.tags);
  const [sidebarOpen, setSidebarOpen] = useState(true);
  const [overlay, setOverlay] = useState<OverlayState | null>(null);
  const [reveal, setReveal] = useState<FileReveal | null>(null);
  const selectedFolderStatus = folders.find((folder) => folder.label === selectedFolder);
  const selectedDisplayName = allFolders
    ? "All folders"
    : selectedFolderStatus?.display_name || selectedFolder;
  const viewLocationKey = allFolders ? ALL_FOLDERS_VIEW_KEY : selectedFolder;

  const keymap = useKeymap();
  const toast = useToast();
  const { updateFolder } = useFolders();

  // Auto-select the first folder the moment status arrives. This is React's
  // documented "adjusting state during render" pattern rather than a
  // useEffect: the guard flips false as soon as it runs, React re-renders
  // before painting, and there is no cascading render.
  // The `label !== ""` guard is what makes it terminate: a folder with an
  // empty label would leave the condition true forever.
  if (!allFolders && folders.length > 0 && !folders.some((folder) => folder.label === selectedFolder)) {
    const first = folders[0];
    setSelectedFolder(first.label);
    setView(folderViews[first.label] ?? (isImageFolder(first.label) ? "gallery" : "files"));
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

  useEffect(() => {
    if ((!selectedFolder && !allFolders) || typeof localStorage === "undefined") return;
    try {
      localStorage.setItem(
        WORKSPACE_STORAGE_KEY,
        JSON.stringify({ folder: selectedFolder, allFolders, view, folderViews, filterKey, tags: tagFilters }),
      );
    } catch {
      // Browsing must keep working when private-mode storage is unavailable.
    }
  }, [allFolders, filterKey, folderViews, selectedFolder, tagFilters, view]);

  const changeView = useCallback((next: View) => {
    setView(next);
    if (!viewLocationKey || !BROWSING_VIEWS.includes(next as BrowsingView)) return;
    setFolderViews((current) => current[viewLocationKey] === next
      ? current
      : { ...current, [viewLocationKey]: next as BrowsingView });
  }, [viewLocationKey]);

  const selectFolder = useCallback((label: string, filter: FilterKey = "all") => {
    setSelectedFolder(label);
    setAllFolders(false);
    setFilterKey(filter);
    if (filter !== "all") setTagFilters([]);
    setView(folderViews[label] ?? (isImageFolder(label) ? "gallery" : "files"));
  }, [folderViews]);

  const selectAllFolders = useCallback(() => {
    setAllFolders(true);
    setView(folderViews[ALL_FOLDERS_VIEW_KEY] ?? (view === "config" ? "files" : view));
  }, [folderViews, view]);

  const onFolderChanged = useCallback(() => {
    void refetch();
  }, [refetch]);

  const stepFolder = useCallback(
    (delta: number) => {
      if (folders.length === 0) return;
      const at = folders.findIndex((folder) => folder.label === selectedFolder);
      const nextIndex = allFolders
        ? delta > 0 ? 0 : folders.length - 1
        : (Math.max(0, at) + delta + folders.length) % folders.length;
      const next = folders[nextIndex];
      selectFolder(next.label);
    },
    [allFolders, folders, selectedFolder, selectFolder],
  );

  const stepView = useCallback((delta: number) => {
    changeView(VIEWS[(VIEWS.indexOf(view) + delta + VIEWS.length) % VIEWS.length]);
  }, [changeView, view]);

  const rescan = useCallback(async () => {
    try {
      await api.createJob({ action: "scan", ...(allFolders ? {} : { folder: selectedFolder }) });
      toast.show("success", `Rescanning ${allFolders ? "every folder" : selectedFolder}`);
    } catch (err) {
      toast.show("error", errorMessage(err));
    }
  }, [allFolders, selectedFolder, toast]);

  /** The global layer: everything that is about the app rather than about a
   *  file. File commands fall through to whichever grid is mounted. */
  const handle = useCallback(
    (command: Command): boolean => {
      if (command.kind !== "action") return false;
      switch (command.action) {
        case "view-gallery":
          changeView("gallery");
          return true;
        case "view-files":
          changeView("files");
          return true;
        case "view-settings":
          changeView("config");
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
        case "toggle-sidebar":
          setSidebarOpen((open) => !open);
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
    [changeView, rescan, stepFolder, stepView],
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
          changeView(action.view as View);
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
    [changeView, keymap, onFolderChanged, rescan, selectFolder, selectedFolder, toast, updateFolder],
  );

  const showInFiles = useCallback(
    (file: FileRecord) => {
      setSelectedFolder(file.folder_label);
      setAllFolders(false);
      setFilterKey("all");
      setTagFilters([]);
      setView("files");
      setFolderViews((current) => current[file.folder_label] === "files"
        ? current
        : { ...current, [file.folder_label]: "files" });
      setReveal((current) => ({ file, token: (current?.token ?? 0) + 1 }));
      setOverlay(null);
    },
    [],
  );

  return (
    <div className="flex h-screen overflow-hidden">
      <button
        style={{ zIndex: Z.sidebarToggle }}
        className={`fixed top-3 left-3 bg-raised ring-1 ring-line-strong text-ink p-2 rounded-lg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor ${
          sidebarOpen ? "md:hidden" : ""
        }`}
        onClick={() => setSidebarOpen((open) => !open)}
        aria-label="Toggle sidebar"
        aria-controls="folder-sidebar"
        aria-expanded={sidebarOpen}
        title="Toggle folders (F1)"
      >
        <svg className="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M4 6h16M4 12h16M4 18h16" />
        </svg>
      </button>

      <aside
        id="folder-sidebar"
        data-folder-sidebar
        style={{ zIndex: Z.sidebar }}
        className={`${
          sidebarOpen ? "translate-x-0 md:flex" : "-translate-x-full md:hidden"
        } fixed md:static w-72 h-full bg-surface border-r border-line flex flex-col transition-transform`}
      >
        <div className="flex min-h-16 items-center gap-2.5 border-b border-line px-4 pl-14 md:pl-4">
          <img
            src="/favicon.svg"
            alt=""
            aria-hidden="true"
            data-brand-logo
            className="size-8 shrink-0"
          />
          <h1 className="text-lg font-semibold tracking-tight">Coldstore</h1>
        </div>

        <nav className="min-h-0 flex-1 overflow-y-auto p-2 space-y-1">
          <p className="text-micro uppercase tracking-wider text-ink-faint px-2 pt-2 pb-1.5">Folders</p>
          <button
            type="button"
            data-folder-nav="all"
            aria-current={allFolders ? "page" : undefined}
            onClick={() => {
              selectAllFolders();
              if (window.innerWidth < 768) setSidebarOpen(false);
            }}
            className={`w-full rounded-lg px-3 py-2 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor ${
              allFolders ? "bg-raised-hover text-ink" : "text-ink-muted hover:bg-raised hover:text-ink"
            }`}
          >
            <span className="font-medium">All folders</span>
            <span className="mt-0.5 block text-meta text-ink-faint">
              Filter and compare tags across the catalog
            </span>
          </button>
          {folders.map((folder) => (
            <FolderNavItem
              key={folder.label}
              folder={folder}
              selected={!allFolders && selectedFolder === folder.label}
              onSelect={() => {
                selectFolder(folder.label);
                if (window.innerWidth < 768) setSidebarOpen(false);
              }}
              onReview={() => selectFolder(folder.label, "pinned-auto")}
            />
          ))}
          {folders.length === 0 && !error && (
            <p className="px-3 py-2 text-base text-ink-faint">No folders yet.</p>
          )}
        </nav>

        <StatusBar
          status={status}
          error={error}
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

      <main className={`min-w-0 flex-1 flex flex-col ${sidebarOpen ? "" : "md:pl-12"}`}>
        <header data-app-toolbar className="flex min-h-16 shrink-0 items-center justify-end px-3">
          <ViewToggle
            view={view}
            onChange={changeView}
            onHelp={() => setOverlay({ kind: "cheatsheet" })}
          />
        </header>

        <div data-content-scroll className="min-h-0 flex-1 overflow-y-auto">
          {error && <ErrorBanner message={error} status={errorStatus} />}

          <div className="min-h-full">
            {view === "gallery" && (
              <Gallery
                key={`gallery-${allFolders ? "all" : selectedFolder}`}
                folder={selectedFolder}
                displayName={selectedDisplayName}
                allFolders={allFolders}
                filterKey={filterKey}
                onFilterChange={setFilterKey}
                tagFilters={tagFilters}
                onTagFiltersChange={setTagFilters}
                onFolderChanged={onFolderChanged}
                onShowInFiles={showInFiles}
              />
            )}
            {view === "files" && (
              <FileBrowser
                key={`files-${allFolders ? "all" : selectedFolder}`}
                folder={selectedFolder}
                displayName={selectedDisplayName}
                allFolders={allFolders}
                filterKey={filterKey}
                onFilterChange={setFilterKey}
                tagFilters={tagFilters}
                onTagFiltersChange={setTagFilters}
                onFolderChanged={onFolderChanged}
                reveal={reveal}
                onShowInFiles={showInFiles}
              />
            )}
            {view === "config" && (
              <ConfigPanel
                status={status}
                selectedFolder={selectedFolder}
                onSelectFolder={(label) => {
                  setSelectedFolder(label);
                  setAllFolders(false);
                }}
                onChanged={onFolderChanged}
              />
            )}
          </div>
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
          filters={allFolders ? {} : { folder: selectedFolder }}
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

function ViewToggle({
  view,
  onChange,
  onHelp,
}: {
  view: View;
  onChange(view: View): void;
  onHelp(): void;
}) {
  const labels: Record<View, string> = {
    gallery: "Gallery",
    files: "Files",
    config: "Settings",
  };

  return (
    <div className="flex items-center gap-2">
      <nav
        aria-label="Views"
        className="flex rounded-lg border border-line-strong bg-surface p-1 shadow-lg"
      >
        {VIEWS.map((option) => (
          <button
            key={option}
            type="button"
            data-view-nav={option}
            aria-label={`${labels[option]} view`}
            aria-current={view === option ? "page" : undefined}
            title={labels[option]}
            onClick={() => onChange(option)}
            className={`rounded-md p-2 transition-[background-color,color,transform] duration-150 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor ${
              view === option
                ? "bg-raised-hover text-ink"
                : "text-ink-faint hover:bg-raised hover:text-ink"
            }`}
          >
            <ViewIcon view={option} />
          </button>
        ))}
      </nav>
      <button
        type="button"
        aria-label="Keyboard shortcuts"
        title="Keyboard shortcuts (?)"
        onClick={onHelp}
        className="flex size-10 items-center justify-center rounded-lg border border-line-strong bg-surface font-mono text-base text-ink-faint shadow-lg transition-colors hover:bg-raised hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
      >
        ?
      </button>
    </div>
  );
}

function ViewIcon({ view }: { view: View }) {
  if (view === "gallery") {
    return (
      <svg className="size-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" aria-hidden="true">
        <rect x="3" y="3" width="7" height="7" rx="1" />
        <rect x="14" y="3" width="7" height="7" rx="1" />
        <rect x="3" y="14" width="7" height="7" rx="1" />
        <rect x="14" y="14" width="7" height="7" rx="1" />
      </svg>
    );
  }
  if (view === "files") {
    return (
      <svg className="size-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" aria-hidden="true">
        <path strokeLinecap="round" d="M8 6h13M8 12h13M8 18h13" />
        <circle cx="4" cy="6" r="1" fill="currentColor" stroke="none" />
        <circle cx="4" cy="12" r="1" fill="currentColor" stroke="none" />
        <circle cx="4" cy="18" r="1" fill="currentColor" stroke="none" />
      </svg>
    );
  }
  return (
    <svg className="size-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" aria-hidden="true">
      <circle cx="12" cy="12" r="3" />
      <path strokeLinecap="round" strokeLinejoin="round" d="M19.4 15a1.7 1.7 0 00.34 1.88l.06.06-2.83 2.83-.06-.06A1.7 1.7 0 0015 19.4a1.7 1.7 0 00-1 .6 1.7 1.7 0 00-.4 1.1V21h-4v-.09A1.7 1.7 0 008.6 19.4a1.7 1.7 0 00-1.88.34l-.06.06-2.83-2.83.06-.06A1.7 1.7 0 004.6 15a1.7 1.7 0 00-.6-1 1.7 1.7 0 00-1.1-.4H3v-4h.09A1.7 1.7 0 004.6 8.6a1.7 1.7 0 00-.34-1.88l-.06-.06 2.83-2.83.06.06A1.7 1.7 0 009 4.6a1.7 1.7 0 001-.6 1.7 1.7 0 00.4-1.1V3h4v.09A1.7 1.7 0 0015.4 4.6a1.7 1.7 0 001.88-.34l.06-.06 2.83 2.83-.06.06A1.7 1.7 0 0019.4 9c.12.38.33.72.6 1 .3.27.68.4 1.1.4H21v4h-.09A1.7 1.7 0 0019.4 15z" />
    </svg>
  );
}
