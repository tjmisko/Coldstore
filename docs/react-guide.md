# Frontend Architecture Guide

React 19 + Vite 7 + Tailwind 4 single-page app embedded into the Go binary.

## Component tree

```
main.tsx
  StrictMode
    App                                  (web/src/App.tsx)
    |
    |-- useStatus() ---------------------> GET /api/v1/status
    |
    +-- <aside> Sidebar
    |   |-- Folder list (from status.folders)
    |   |-- View switcher (gallery | files | config)
    |   +-- StatusBar                    (web/src/components/StatusBar.tsx)
    |       props: status, onRefresh, onScan
    |
    +-- <main>
        |-- Gallery                      (web/src/components/Gallery.tsx)
        |   |-- useFiles({ folder, sort:"mtime" })
        |   |-- thumbnail grid
        |   |-- IntersectionObserver sentinel
        |   +-- Lightbox                 (web/src/components/Lightbox.tsx)
        |       props: files[], initialIndex, onClose, onLoadMore, hasMore
        |
        |-- FileBrowser                  (web/src/components/FileBrowser.tsx)
        |   |-- useFiles({ folder, sort:"name" })
        |   |-- <table> file listing
        |   +-- IntersectionObserver sentinel
        |
        +-- ConfigPanel                  (web/src/components/ConfigPanel.tsx)
            |-- useFolders()
            +-- PersistentPathsEditor (local component)
                +-- usePersistentPaths(folderLabel)
```

Data flows down through props; state is local to each view. There is no
global store. `App` owns the selected folder and current view, passing
`folder` as a prop to `Gallery`, `FileBrowser`, and `ConfigPanel`. The
`useStatus` hook lives in `App` and feeds `StatusBar` via props.

`App` auto-selects the first folder on initial load (`App.tsx:21-25`)
and picks a default view based on folder name -- folders containing
"camera" or "photo" open in gallery mode, others in file browser mode
(`App.tsx:10-12`).

## Custom hooks

All hooks in this project follow the same `useState` + `useCallback` +
`useEffect` fetch pattern:

1. Declare state with `useState` (e.g. `status`, `folders`, `paths`).
2. Wrap the fetch call in `useCallback` (memoized as `refetch`).
3. Trigger the initial fetch with `useEffect(() => { refetch() }, [refetch])`.

This gives each consumer a stable `refetch` function it can call after
mutations, while the `useEffect` handles the initial load.

### useStatus (`web/src/hooks/useConfig.ts:4-29`)

```ts
const { status, refetch, triggerScan } = useStatus();
```

Fetches `GET /api/v1/status` once on mount. Returns `StatusResponse | null`.
Also exposes `triggerScan` which POSTs to `/api/v1/scan`. That endpoint no longer walks
the tree inline: it answers `202 {job_id, action, state, total}` and the scan runs on the
job queue, so a refresh returns immediately and can be followed with
`GET /api/v1/jobs/{job_id}`. The current hook ignores the response body, which is why the
change did not break it.

### useFolders (`web/src/hooks/useConfig.ts:31-60`)

```ts
const { folders, updateFolder, refetch } = useFolders();
```

Fetches `GET /api/v1/folders`. The `updateFolder(label, config)` helper
PUTs partial config to `/api/v1/folders/:label` and auto-refetches.

### usePersistentPaths (`web/src/hooks/useConfig.ts:62-102`)

```ts
const { paths, addPath, deletePath, refetch } = usePersistentPaths(folderLabel);
```

Scoped to a single folder label. Provides `addPath(pattern)` and
`deletePath(id)` which POST/DELETE and auto-refetch. The `refetch`
callback skips the request when `folderLabel` is empty
(`useConfig.ts:66`).

### useFiles (`web/src/hooks/useFiles.ts`)

```ts
const { files, loading, hasMore, loadMore, refetch } = useFiles({ folder, sort, ... });
```

The most complex hook -- handles paginated fetching with infinite scroll.

**Why `useRef` for offset:** The current page offset is stored in
`offsetRef` (`useFiles.ts:17`) rather than `useState`. This is
deliberate. If offset were in state, every call to `setOffset` would
trigger a re-render, which would recreate the `fetchPage` callback,
which would fire the filter-reset `useEffect` again. Using a ref lets
`fetchPage` read and update the offset without causing additional renders
or stale-closure bugs in the `loadMore` -> `fetchPage` chain.

The hook builds query parameters from its options object
(`useFiles.ts:20-33`), passing `folder`, `status`, `ext`, `q`, `sort`,
`offset`, and `limit` to `GET /api/v1/files`.

**Filter reset:** When any filter option changes, the effect at
`useFiles.ts:63-69` resets `offsetRef` to 0, clears the file list, and
fetches the first page. The dependency array is intentionally narrowed to
the filter values (not `fetchPage`) to avoid double-fetching.

**Pagination signal:** `hasMore` is set to `true` when a response
returns exactly `limit` items (`useFiles.ts:52`). When a response is
shorter, the hook knows the server has no more data.

## Infinite scroll

Both `Gallery` and `FileBrowser` use the same `IntersectionObserver`
sentinel pattern for infinite scroll.

### The pattern

A ref callback (`sentinelRef`) is assigned to an empty `<div>` at the
bottom of the content area. On each render:

1. Disconnect any previous observer (`observerRef.current.disconnect()`).
2. If the node is `null` (unmounting), return early.
3. Create a new `IntersectionObserver` with `rootMargin: "200px"` so it
   fires 200px before the sentinel enters the viewport.
4. When the sentinel intersects and `hasMore && !loading`, call
   `loadMore()`.

Example from `Gallery.tsx:23-39`:

```ts
const sentinelRef = useCallback(
  (node: HTMLDivElement | null) => {
    if (observerRef.current) observerRef.current.disconnect();
    if (!node) return;
    observerRef.current = new IntersectionObserver(
      (entries) => {
        if (entries[0].isIntersecting && hasMore && !loading) {
          loadMore();
        }
      },
      { rootMargin: "200px" },
    );
    observerRef.current.observe(node);
  },
  [hasMore, loading, loadMore],
);
```

### Cleanup

The callback ref handles cleanup naturally: when React calls it with
`null` (on unmount or when dependencies change), the previous observer is
disconnected on the next call. There is no `useEffect` cleanup needed
because the `observerRef` is disconnected at the top of every invocation.

## Lightbox prefetch

`Lightbox` (`web/src/components/Lightbox.tsx:37-45`) triggers a prefetch
of the next page when the user is within 5 items of the end of the
current file list:

```ts
const goNext = useCallback(() => {
  if (index < files.length - 1) {
    setIndex(index + 1);
    // Prefetch more when near the end
    if (index >= files.length - 5 && hasMore) {
      onLoadMore();
    }
  }
}, [index, files.length, hasMore, onLoadMore]);
```

`Gallery` passes its `loadMore` as the `onLoadMore` prop
(`Gallery.tsx:118`), so the lightbox can extend the same file list
without the gallery needing to know about it. When the new page arrives,
`useFiles` appends the results to its `files` array, which flows into
`Lightbox` via props. The user sees no loading gap as long as they don't
swipe faster than the fetch resolves.

The lightbox also supports keyboard navigation (arrows, Page Up/Down,
Home/End, Space for metadata, Escape to close) and touch swipe gestures
(`Lightbox.tsx:111-127`).

## Dev proxy

`web/vite.config.ts` proxies all `/api` requests to the Go daemon:

```ts
server: {
  proxy: {
    '/api': 'http://127.0.0.1:8090',
  },
},
```

During development you run two processes:

| Command        | Port   | Purpose                 |
|----------------|--------|-------------------------|
| `make dev`     | `:8090`| Go daemon (API + data)  |
| `make dev-web` | `:5173`| Vite dev server (HMR)   |

The browser hits `:5173`. Vite serves React with hot module replacement
and forwards API calls to `:8090`. This means the frontend and backend
can be developed and restarted independently.

## Embedding

In production, the React build is embedded directly into the Go binary
so there is no separate web server.

### Build pipeline (`Makefile:6-9`)

```make
frontend:
	cd web && npm run build
	rm -rf internal/api/dist
	cp -r web/dist internal/api/dist
```

1. `npm run build` runs `tsc -b && vite build` (`web/package.json:8`),
   producing optimized assets in `web/dist/`.
2. The Makefile copies `web/dist/` to `internal/api/dist/`.
3. `make build` depends on `frontend`, so a full build always includes
   the latest frontend.

### Go embed (`internal/api/static.go`)

```go
//go:embed all:dist
var distFS embed.FS
```

The `all:` prefix includes dotfiles (Vite sometimes produces
`.vite/manifest.json`). At startup, `staticHandler()` creates an
`http.FileServer` rooted at the `dist` subdirectory of the embedded FS.

The handler (`static.go:21-40`) has SPA-aware routing:

- Requests matching a real file in `dist/` are served directly (JS, CSS,
  images).
- Everything else falls back to `index.html`, letting React Router (or
  in this case, client-side view switching) handle the path.
- `/api/` paths never reach this handler -- they are registered on the
  mux before the static fallback.

For testing without a frontend build, `make test` creates an empty
`internal/api/dist` directory (`Makefile:21`) so the `//go:embed`
directive does not fail at compile time.
