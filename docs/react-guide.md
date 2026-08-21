# Frontend Architecture Guide

React 19 + Vite 7 + Tailwind 4, embedded into the Go binary. No router, no state
library, no data-fetching library. Everything below stays inside that.

The UI is a **control plane**, not a viewer. Anything it can do, it can do to the
owner's files: a pin changes what quota enforcement may touch, an archive is a
Syncthing deletion on every synced device, and a bulk restore can push a folder
over budget and cause an eviction the owner never asked for. That is why so much
of what follows is about saying what an action will do before it does it, and
about never showing a state that is not true.

## Component tree

```
main.tsx
  StrictMode
    App                                   (web/src/App.tsx)
      ToastProvider                       (components/ToastProvider.tsx)
       KeymapProvider                     (components/KeymapProvider.tsx)
        Shell
        |
        |-- useKeymapLayer("global") ----> views, folders, palette, finder, ?
        |-- CommandPalette / FuzzyFinder / Cheatsheet   (portalled overlays)
        |
        |-- useStatus() -----------------> GET /api/v1/status, polled every 10 s
        |
        +-- <aside> Sidebar
        |   |-- Folder list (from status.folders; disabled and orphaned folders
        |   |   are greyed and labelled, never hidden)
        |   |-- View switcher (gallery | files | config)
        |   +-- StatusBar                 (components/StatusBar.tsx)
        |       |-- FolderMeter per folder: two-segment budget bar, wedge banner
        |       |-- BulkEvictionStrip --> GET /api/v1/events?type=bulk_eviction
        |       +-- rescan --------------> POST /api/v1/jobs {action:"scan"}
        |
        +-- <main>
            |-- ErrorBanner               (components/ErrorBanner.tsx)
            |
            |-- Gallery                   (components/Gallery.tsx)
            |   |-- useFileWorkspace({folder, filterKey, sort:"mtime"})
            |   |-- FilterBar             (components/FilterBar.tsx)
            |   |-- Tile grid: checkbox, PinToggle, status badges
            |   |-- Lightbox              (components/Lightbox.tsx)
            |   +-- SelectionBar          (components/SelectionBar.tsx)
            |
            |-- FileBrowser               (components/FileBrowser.tsx)
            |   |-- useFileWorkspace({folder, filterKey, sort:"name"})
            |   |-- FilterBar
            |   |-- <table>: checkbox column, pin column, per-row actions
            |   +-- SelectionBar
            |
            +-- ConfigPanel               (components/ConfigPanel.tsx)
                |-- useFolders()          budget / grace / enabled editors
                |-- PersistentPathsEditor with per-pattern match counts
                +-- ActivityList -------> GET /api/v1/events?class=warning
```

State is local; data flows down through props. `Shell` owns the selected folder,
the current view and the active filter, and passes them down. Gallery and
FileBrowser are keyed by folder (`key={"gallery-" + folder}`), so switching
folders remounts them and the selection resets without an effect to clear it.

`Shell` auto-selects the first folder the moment status arrives, using React's
"adjusting state during render" pattern rather than an effect. The `label !== ""`
guard is what makes it terminate.

## Layers

```
components/   what the owner sees
hooks/        what the components run on
keymap/       the input engine: no React, no DOM, unit-tested as pure functions
lib/          what the hooks are built from: api.ts, format.ts, toast.ts,
              keymapContext.ts, zLayers.ts
types/api.ts  the mirrored API contract (design section 6.13)
```

### `lib/api.ts`

The only place in the app that calls `fetch`.

`apiFetch<T>` parses the error body inside a `try` before throwing, so a
plain-text 405 or an HTML page produces the status line rather than a
`SyntaxError` about markup -- which is what the blank-page failure mode used to
be. It throws `ApiRequestError`, carrying the **status and the decoded body**:
the 409 from the restore budget pre-flight names the folder, the overage and the
number of files coldstore would evict, and a dialog that cannot see those
numbers can only ask "are you sure?".

`api.*` are thin typed wrappers, one per endpoint. `fileFilterParams` is shared
by `GET /files` and `GET /files/ids` on purpose: "select all in folder" must
resolve the same set the grid is showing, or a job acts on files nobody saw.

### `lib/format.ts`

One `formatSize`, **SI (1000-based)**, mirroring `catalog.HumanSI` exactly, so a
budget rendered here and a wedge warning written by the daemon are worded
identically. There used to be four copies with two rounding schemes, all binary,
so a TOML budget of `20GB` rendered as "18.6 GB".

`parseSize` is its inverse and accepts what `config.ParseBudget` accepts, so
"20 GB" typed in Settings is the same 20000000000 the owner wrote in the file.
It returns `NaN` for anything that is not a size, which is how the budget editor
refuses one inline instead of PUTting a guess.

### `lib/toast.ts` + `components/ToastProvider.tsx`

The context and the hook live in `lib/toast.ts`; the provider component lives in
`components/`. That split is not stylistic: `react-refresh/only-export-components`
fails a module that exports both a component and a hook.

Errors stay on screen for 20 seconds and are dismissible; before this existed,
every per-file failure went to `console.error`.

## Hooks

### The compliant shape

`eslint-plugin-react-hooks` v7 flags `useEffect(() => { load(); }, [load])` as
`react-hooks/set-state-in-effect` -- it inlines the callee and sees the setState
as synchronous. Every hook here uses:

```ts
useEffect(() => {
  const controller = new AbortController();
  void (async () => {
    await load(controller.signal);
  })();
  return () => controller.abort();
}, [load]);
```

The `await` is not decorative: behind it, the state write is what it always
really was, a continuation that runs after the request returns. **No
`eslint-disable` anywhere in `web/src`.** `npm run lint` reports zero errors and
zero warnings; keep it that way, because the first suppression makes the next
one free.

Every `refetch` a hook returns is wrapped in `useCallback`. They are effect
dependencies in `StatusBar` and `ConfigPanel`, and a fresh closure per render
re-runs those effects forever.

### `useStatus` (`hooks/useConfig.ts`)

`GET /api/v1/status` on mount and every 10 seconds, because `over_budget`,
`wedged` and the budget bars have to move while a bulk job runs. It exposes
`errorStatus` as well as `error`, so `Shell` can tell a rejected password (401)
from a daemon that is not running -- they need different sentences.

### `useFolders`, `usePersistentPaths`, `useEvents` (`hooks/useConfig.ts`)

`updateFolder` is narrowed to `{budget_bytes?, grace_minutes?, enabled?}`. With
`Partial<FolderConfig>` the first budget editor written against it would have
sent `syncthing_path: ""`, `grace_minutes: 0` and `enabled: false` alongside the
budget, disabling the folder on its first save.

`useEvents` defaults to the warning class. One enforcement pass writes hundreds
of `archive` rows, so an unfiltered "last ten events" would never once show the
wedge warning, the cold-storage warning or a conflict.

### `useFiles` (`hooks/useFiles.ts`)

```ts
const { files, loading, error, hasMore, loadMore, refetch, refetchIds,
        patchFile, removeFile } = useFiles({ folder, status, pinned,
                                             pinSource, conflict, sort, limit });
```

**One page object, keyed by its query.** State is `{query, files, hasMore}`, and
the rendered list is `page.query === query ? page.files : []`. Two consequences
fall out of that: `loading` is *derived*, so nothing is set before an `await` and
the lint rule is satisfied without ceremony; and a filter change empties the grid
immediately rather than showing the previous filter's rows under the new
heading.

**A generation counter, not a `loading` guard.** The old `fetchPage` early-returned
while a page was in flight and listed `loading` as a dependency, so a filter reset
arriving mid-page was dropped and the grid kept showing the old filter. Now every
request takes a generation number and an `AbortController`; a response whose
generation is stale is discarded.

**`refetchIds(ids)`** repaints exactly the rows a job touched, using the `ids`
parameter of `GET /files` (server cap: 500 per request, so it chunks; above 2000
it reloads instead). Without a server-side id filter this would degrade to
fetching the unfiltered first page -- patching 50 arbitrary rows with
stale-but-plausible data while the files the job actually restored went on
rendering as archived.

**`offsetRef`** stays a ref, not state: an offset in state re-renders, which
recreates the fetch callback, which re-fires the reset effect.

### `useSelection` (`hooks/useSelection.ts`)

Holds **ids, never indices**. `orderedIds` is the order currently on screen, so a
shift-click range means what the owner sees, and the anchor is the last id passed
to `toggle` (held in a ref). Ids scrolled out of the loaded window stay selected,
which is what lets "select all in folder" hand 20,000 ids to a job when only
sixty rows have ever been rendered.

`toggleRange` applies **the anchor's state** to the range, so shift-clicking after
unticking a photo deselects the range. Selecting it instead would turn a
correction into a re-selection.

`selectExactly` replaces the whole set, and exists for visual mode: every motion
redraws the highlighted span, and a span that could only ever grow would leave
the files behind the cursor selected when the owner reversed direction.

### `useJob` / `useJobRunner` (`hooks/useJob.ts`)

`useJob` polls `GET /jobs/{id}` on a chained `setTimeout` -- not `setInterval`, so
a slow response cannot stack a second poll on the first -- and stops when the job
reaches `done` or `failed`.

`useJobRunner` wraps enqueue plus poll, and turns a 409 into a `preflight` object
for the confirm dialog instead of an error. `start()` is only ever called from an
event handler: creating a job from an effect would post it twice under StrictMode,
and a duplicated archive job is a duplicated set of Syncthing deletions.

### `useFileWorkspace` (`hooks/useFileWorkspace.ts`)

Everything Gallery and FileBrowser share: the six filter presets, `useFiles`,
`useSelection`, and the per-file actions (`setPinned`, `restore`, `archive`,
`resolve`, `download`). Every action goes through `apiFetch`, writes the returned
record back with `patchFile`, and reports failures as toasts. Two implementations
of "restore this row" is how one of them ends up without the `res.ok` check --
which is exactly what happened before: a successful restore left the row reading
"archived" with its Restore button showing, and a failed one looked identical.

The six filters (design section 7.4):

| Filter | Query |
|---|---|
| All | -- |
| Synced | `status=synced` |
| Archived | `status=archived` |
| Pinned by me | `pinned=1&pin_source=user` |
| Auto-pinned on restore | `pinned=1&pin_source=restore&sort=pinned_at` (oldest first) |
| Conflicts | `status=archived&conflict=1` |

The two pin filters exist because a restore pins silently and permanently and
nothing releases that pin, so a folder's long-run destination under ordinary use
is wedged. `pin_source` is what keeps "files I chose to protect" apart from
"files a restore pinned months ago", so the remedy for a wedge is not the click
that unpins the owner's favourites.

## The budget bar

Two **nested** segments, never two additive ones:

```
pinnedPct   = min(pinned_bytes / budget, 1) * 100
totalPct    = min(synced_size_bytes / budget, 1) * 100
unpinnedPct = max(totalPct - pinnedPct, 0)
```

`pinned_bytes` is a subset of `synced_size_bytes`, so stacking them would draw a
fully pinned folder at 200% of its budget. Both clamp at 100%, so an over-budget
folder is distinguished by a red overflow cap at the right edge rather than by
width.

## Infinite scroll

Gallery and FileBrowser share the `IntersectionObserver` sentinel pattern: a ref
callback assigned to an empty `<div>` at the bottom of the content, which
disconnects the previous observer on every invocation, uses `rootMargin: "200px"`
to fire early, and calls `loadMore()` when the sentinel intersects and there is
more to load. The callback ref handles cleanup naturally -- React calls it with
`null` on unmount.

Every sort ends in `id ASC` server-side. Without a total order, two rows that tie
on the sort key can come back in either order from page to page, and an
OFFSET-paged infinite scroll then shows one twice and drops the other. A camera
roll copied off a card shares one mtime across hundreds of files, so ties are the
norm.

## The keyboard

`docs/keymap-design.md` is the design; this is where it touches the React tree.

`web/src/keymap/` is a vim input engine with **no React import and no DOM
access**: token normalisation, motion arithmetic, a binding trie, the state
machine, the binding table, the fuzzy scorer and the ex-command parser. The
grammar is where the bugs are, and `step(state, input, bindings)` tested as a
fold over a token list costs a millisecond per case instead of a jsdom render.
Counts multiply (`2d3j` is `d6j`), `0` is a motion until a count buffer is
non-empty, an operator waits for a range and cancels on anything else, and an
ambiguous prefix returns its timeout as a **number** so nothing here reads a
clock.

`KeymapProvider` owns the app's **only** `keydown` listener. There used to be
three -- `Gallery`, `Lightbox` and `ConfirmDialog` -- of which one had an "am I
typing?" guard, inline. Scope decides who gets a key now; document order used
to, which is why Escape closed the lightbox and the dialog on top of it in one
press.

Two things in the provider look odd and are deliberate:

- The machine lives in a **ref**, and the pending indicator is written straight
  to the DOM. A keystroke must not re-render five hundred tiles to redraw four
  characters in a corner, and the machine is not React state -- it is a value a
  listener folds over.
- Layers are ordered by an explicit `SCOPE_PRIORITY`, not by registration order.
  React mounts children before parents, so "last registered is on top" is true
  for an overlay opened later and false on first paint.

`useGridKeymap` is the whole keyboard behaviour of a file list, shared by Gallery
and FileBrowser. A table is a grid one column wide, so `columns` is the only
difference between them and `d3j` has one implementation. The cursor is one
index, and the effect that follows it writes DOM focus from it -- never reads it
back -- so the drawn cursor and `document.activeElement` cannot disagree.
FileBrowser rows are `tabIndex={-1}`: five hundred rows in the tab order is its
own accessibility failure.

`useBulkActions` came out of `SelectionBar` so the keyboard could reuse it. An
operator queues the **same** job a button does, with the same budget pre-flight,
the same progress readout and the same post-job repaint. Two implementations of
"archive these ids" is how one of them ends up without the `res.ok` check.
Operators over more than `CONFIRM_ABOVE` files confirm first; the bar's buttons
do not, because they sit beside a visible count and `dG` does not.

The `?` cheatsheet is generated from the binding table. A hand-written one drifts
within a week, and a cheatsheet that lies is worse than none.

### Overlays

`components/Portal.tsx`, `hooks/useFocusTrap.ts`, `hooks/useScrollLock.ts` and
`lib/zLayers.ts` did not exist before the palette needed them: `createPortal`
appeared zero times, every overlay was an inline `fixed inset-0` div inside an
`overflow-hidden` ancestor, and the z ladder was hand-rolled with the hamburger
and the lightbox already tied at 50.

`Portal` renders into `document.body` rather than a host div it creates, because
a host needs an effect to append it and a state write to render into it, and
`react-hooks/set-state-in-effect` is not negotiable here. `zLayers` is applied as
an inline `style`, so the design refresh moving every colour does not have to
grep for `z-[70]`.

The focus trap restores focus to whatever had it, which is what makes `:` then
Escape put the cursor back on the tile it was on.

## Lightbox

Opens on a plain click, on `<CR>` or on `o`; ctrl/cmd-click toggles selection and
shift-click extends the range instead.

It registers the `lightbox` scope, which is **not** exclusive: `q` and `i` are
its own, and `d`, `p`, `y`, `j` and `G` fall through to the same paths the grid
uses. That is the point of having a grammar rather than a switch statement per
component. `Space` no longer toggles the metadata panel -- it is the leader now
-- so `i` does. The tile is a `<div role="button">`, not a `<button>`: a
checkbox and a pin toggle inside a button are nested interactive elements, which
is invalid HTML and made a click on the checkbox also open the lightbox.

The lightbox prefetches the next page when the user is within five items of the
end, and routes Pin, Restore and Archive through the parent's actions, so a
change made here repaints the grid behind it without a refetch.

## Dev proxy

`web/vite.config.ts` proxies `/api` to the daemon:

| Command | Port | Purpose |
|---|---|---|
| `make dev` | `:8090` | Go daemon (API + data) |
| `make dev-web` | `:5173` | Vite dev server (HMR) |

Two environment variables matter here:

- `COLDSTORE_DEV_TARGET` points the proxy somewhere other than
  `http://127.0.0.1:8090` (a demo daemon on another port, say).
- `COLDSTORE_USERNAME` / `COLDSTORE_PASSWORD` make the proxy inject an
  `Authorization` header. Basic auth wraps the whole mux -- the API, the
  thumbnails and the SPA -- so against an authenticated daemon the browser
  answers a 401 with its own native dialog and the app's error banner never
  renders. Without this the 401 branch cannot be exercised through `make
  dev-web` at all.

## Testing

| Tier | Command | What it proves |
|---|---|---|
| Pure | `make test-web` | The keymap grammar: counts, operators, motions, visual mode, the trie, the scorer, the ex-line parser. No DOM at all |
| Unit / component | `make test-web` | Query building, selection semantics, bar arithmetic, repaint rules, dialog wording, the error banner. jsdom, stubbed `fetch`, no daemon |
| Browser | `make test-playwright` | The embedded bundle, driven in Chromium against the real daemon over a freshly seeded demo installation |

`web/src/test/harness.tsx` provides `stubFetch` (answers by URL and method,
records calls, **throws on an unstubbed request**) and `renderWithToasts`
(collects what an action reported, so tests assert on the sentence the owner
sees). Fixtures live in `web/src/test/fixtures.ts`.

`scripts/seed-demo` builds a complete installation covering every state this UI
renders -- pins of both kinds, a wedged folder, a disabled folder, one that is no
longer in `coldstore.toml`, a conflicted file, thumbnail failures. `make demo`
runs it and starts the daemon on it. Without such a fixture, half the UI in this
document is unreachable and untested.

## Embedding

In production the React build is embedded in the Go binary; there is no separate
web server.

```make
frontend: deps
	cd web && npm run build
	rm -rf internal/api/dist
	cp -r web/dist internal/api/dist
	touch internal/api/dist/.gitkeep
```

`deps` runs `npm ci` when `web/node_modules` is missing, so `make build` works on
a clone that has never been built. The `.gitkeep` is restored because
`//go:embed all:dist` fails to compile if the directory does not exist, and it is
tracked for exactly that reason.

`staticHandler()` (`internal/api/static.go`) serves real files from `dist/` and
falls back to `index.html` for everything else. `/api/` never reaches it: the mux
registers an explicit JSON 404 for unmatched API paths ahead of the SPA fallback,
so a mistyped endpoint returns `{"error": ...}` rather than an HTML page the
client's `JSON.parse` chokes on.
