# Interface Refresh -- Tasklist

Post-deployment feedback from the first real install on the microserver. Source: owner's
notes, 2026-08-21, refined by an interview the same day. Decisions are recorded at the end
of this document; one question (domain or no domain) is still open and blocks section B.

Status legend: `[ ]` not started `[~]` in progress `[x]` done `[?]` blocked on a decision

---

## A. Identity -- title and favicon

The page still ships Vite's scaffold identity: `web/index.html` has `<title>web</title>` and
`<link rel="icon" href="/vite.svg">`. Every tab in the owner's browser says "web".

- [x] A1. Real `<title>` -- static, plus per-view suffix (folder name / "Gallery" / "Config").
- [x] A2. Real favicon. SVG mark, dark/light aware, embedded (no CDN).
- [x] A3. `apple-touch-icon` + `manifest.webmanifest` so an iOS home-screen bookmark looks right.
- [x] A4. `<meta name="theme-color">` and viewport-fit for mobile.
- [x] A5. Verify the embedded build serves all of the above from `internal/api/dist/`.

## B. Access -- stop tunnelling over SSH

Today the UI is reached through an SSH tunnel. Wanted: LAN access without a tunnel, no
purchased domain, and a story for remote access. Related: token-based access to the site.

**Research conclusion: the answer is Tailscale, and it needs no code.** `tailscale serve
--bg --https=443 localhost:8090` publishes the loopback-bound daemon at
`https://microserver.<tailnet>.ts.net` with a **genuinely browser-trusted Let's Encrypt
certificate**, because Tailscale owns `ts.net` and does the DNS-01 challenge for you. No
domain purchase, free tier is 6 users / unlimited devices, first-class iOS and Android
apps, and it answers LAN **and** remote in one mechanism. Every alternative is worse here:

| Option | LAN | Remote | Trusted cert | Per-device setup | Repo change |
|---|---|---|---|---|---|
| **Tailscale + serve** | yes (VPN on) | **yes** | **yes** | install app, sign in | **none** |
| LAN bind + basic auth + avahi | **yes** | no | no | none | none |
| DuckDNS + Caddy DNS-01 | yes | no | yes | none | adds Caddy |
| WireGuard by hand | yes | needs port-forward | no | key per device | none |
| mkcert / private CA | yes | no | only where installed | **painful, forever** | needs TLS support |
| Cloudflare Tunnel | via internet | yes | yes | none | **needs a domain** |

- [?] B1. Adopt Tailscale + `serve` as primary. **Still open** -- the owner wants guest
      access, which Tailscale cannot serve without the guest installing a VPN. See
      "Still open" at the end of this document.
- [ ] B2. Also do the LAN bind, as the path that works when a phone's VPN toggle is off.
      This is already **step 11 of `docs/deploy-checklist.md`** (`listen_addr =
      "192.168.1.50:8090"`); the daemon refuses to start on a non-loopback bind without
      credentials (`internal/config/config.go:254-297`), which is correct and stays.
- [ ] B3. Install avahi so it is `microserver.local:8090` rather than an IP. Caveat: Android
      only resolves `.local` on 12+, needs Chrome's Local network permission, and Private
      DNS breaks it. The IP always works as fallback.
- [ ] B4. **Do not use Funnel, and do not use Cloudflare Tunnel.** Both put a control plane
      whose delete button propagates through Syncthing to every device the owner has, on the
      public internet behind one password.
- [ ] B5. Note for whoever does B1: `coldstore.service:215` sets `IPAddressDeny=any` with an
      `IPAddressAllow` list that **omits Tailscale's CGNAT range `100.64.0.0/10`**. Binding
      `listen_addr` to the Tailscale IP would be silently unreachable. Pointing `serve` at
      loopback avoids it entirely -- `tailscaled` is a separate cgroup.
- [?] B6. Token auth: **not now, but mandatory if the domain path wins.** Basic auth
      (`internal/api/server.go:80-103`) is already constant-time on both fields and already
      solves the hard part -- `<img src>`, `window.open` downloads and Range requests carry
      no custom header, and the browser reattaches the realm credential to them for free.
      Its three real weaknesses are cleartext (**fixed by Tailscale's TLS**), no logout and
      an ugly native dialog on iOS (cosmetic), and no brute-force lockout (real once the
      port is LAN-reachable). Build only if the iOS dialog is the actual complaint; the
      design is in B7.
- [ ] B7. If B6 is a yes: stateless HMAC session cookie, no user table.
      `POST /api/v1/session` against the existing password, `HttpOnly; SameSite=Lax; Path=/`
      so thumbnails and downloads keep working **with zero frontend change**. CSRF covered
      by Lax + a custom header on non-GET (the server sends no `Access-Control-*` at all, so
      the preflight fails and a cross-origin POST never lands). Keep basic auth accepted in
      the same middleware so `curl -u` and the runbook checks stay valid. Gotcha: behind
      `tailscale serve` **`r.TLS` is nil** -- TLS is terminated at the proxy -- so the
      `Secure` flag must be an explicit `[server] cookie_secure` key, not sniffed from
      `X-Forwarded-Proto`, which this codebase has deliberately never trusted.
- [ ] B8. Update `docs/deployment.md` (the posture table at `:141` says sessions are
      deliberately absent) and the deploy checklist with whichever story wins. Also
      `docs/deployment.md:1377` reads the "starting without authentication" WARN as "fine
      for an SSH tunnel" -- still benign under `serve`, but for a different reason.

## C. Design refresh

Run `/frontend-design` over the existing Tailwind v4 dark-only UI.

**The starting point is barer than it looks.** `web/src/index.css` is **ten lines** -- one
`@import "tailwindcss"`, a `body` rule and `box-sizing`. There is no `tailwind.config.js`,
no `@theme` block, no CSS custom property, no `:root`, and the `dark:` variant appears zero
times. **There are no design tokens at all**: every colour in the app is a raw Tailwind
default utility (`bg-neutral-900`, `bg-blue-600`, `ring-blue-500`) hardcoded into a JSX
class string, across ~12 component files. A refresh therefore means introducing a Tailwind
v4 `@theme` block *and* sweeping every one of those files -- there is no intermediate
abstraction to change instead. That is the bulk of the work in C, and it should happen
before D/E/G, which will otherwise be written against colours that are about to move.

Also missing at the start and needed by E and G: a React portal, focus trap, scroll
lock and central z-index scale. Those now live in `components/Portal.tsx`,
`hooks/useFocusTrap.ts`, `hooks/useScrollLock.ts` and `lib/zLayers.ts`. Components use
inline `Z` values from that single source of truth; toasts are explicitly above every
overlay so action failures cannot be hidden by a palette, preview or dialog.

Constraint for every task in this section: the `data-*` attributes are the selector
contract both test tiers are written against (`data-file-id`, `data-selected`,
`data-folder-nav`, `data-view-nav`, `data-filter`, `data-badge`, `data-toast`, ~28 in all).
Restyling must not rename or drop them.

- [x] C1. Aesthetic direction. **One rule: colour means where a file lives, never what a
      button does.** The axis is thermal -- cold for archived, held (warm) for pinned, alarm
      for missing/conflicted/over-budget -- and one hue, a pale teal, is reserved for the
      keyboard's position and nothing else may use it. Type is IBM Plex Sans + Mono, latin
      subset, three woff2 files committed under `web/public/fonts/` because the daemon has no
      egress. Every ink step lighter and every size a step larger than before.
- [x] C2. Selection affordance -- bigger, prettier checkboxes, tuned for photo tiles.
      Current: native `<input type="checkbox" class="accent-blue-500">` at `top-1 left-1`
      inside a `bg-black/50` chip (`web/src/components/Gallery.tsx:229`). Too small to hit on
      a phone, and it disappears until hover. See BUG-1.
- [x] C3. Restyle the tile: badges, pin toggle, archived/pinned states, hover/focus rings.
- [x] C4. Restyle the file browser table, filter bar, selection bar, status bar, config panel.
- [x] C5. Empty states, loading states, error states.
- [ ] C7. **`Lightbox.tsx` is deliberately not swept** -- BUG-3 is being fixed in it in
      another worktree and a parallel restyle would collide. It still carries raw Tailwind
      defaults; sweep it when that branch merges.
- [x] C6. Light mode: **no**. Dark only, decided in the interview.

## D. Keyboard -- vim-flavoured navigation

Leader is **space**. Today there are exactly three `window` keydown listeners and no
registry: `Gallery.tsx:44-69` (Ctrl/Cmd+A, Escape, Enter), `Lightbox.tsx:67-105` (arrows,
PageUp/Down, Home/End, Space, Escape) and `ConfirmDialog.tsx:30-37` (Escape). **The
FileBrowser table has zero keyboard shortcuts and its rows are not even focusable** -- the
only `tabIndex` in the whole app is on the gallery tile. There are no vim keys anywhere.
Only `Gallery.tsx:47-55` has an "am I typing?" guard, and it is local rather than shared.

- [ ] D1. A real keymap layer: a single dispatcher, scoped modes (gallery / browser /
      lightbox / palette / preview), and an escape hatch when focus is in a text field.
- [ ] D2. Motions: `h j k l`, `g g`, `G`, `Ctrl-d`/`Ctrl-u`, `0`/`$` in the grid.
- [ ] D3. Selection: `x`/`Space` toggle, `v` visual-range select, `V` select all, `Esc` clear.
- [ ] D4. Actions on selection: archive / restore / pin / download, each with a mnemonic.
- [ ] D5. `?` opens a keymap cheatsheet overlay.
- [ ] D6. Focus management so the visible cursor and the DOM focus never disagree.

## E. Command palette

- [ ] E1. `:` opens a command palette (vim ex-line flavour).
- [ ] E2. Command set: navigation, filters, selection ops, folder switch, config actions.
- [ ] E3. Argument parsing + completion (`:filter archived`, `:folder Camera`).
- [ ] E4. Recently-used ordering, fuzzy matching over command names.

## F. Fuzzy finder

**Half the plumbing is already there and wired to nothing.** `FileFilters.search` maps to
`?q=` and `FileFilters.extension` maps to `?ext=` (`web/src/lib/api.ts:97-102`), and
`useFiles` destructures both (`useFiles.ts:43`) -- but **no component ever sets either**,
and there is no search box anywhere in the app. Server side `q` is
`AND filename LIKE '%q%'` (`internal/catalog/catalog.go:230-232`): filename only, not path,
no ranking, no FTS. So "fuzzy" is the part that does not exist yet, at either end.

- [ ] F1. `F10` **and** `<leader>fe` (space, f, e) open a fuzzy file finder.
- [ ] F2. Decide client-side vs server-side matching -- depends on catalog size at scale.
      A folder with 100k files cannot ship its filename list to the browser. Note
      `GET /files/ids` already returns bare ids capped at 20000 (`api.ts:135`), which is the
      natural seam for a client-side scorer over a bounded set. SQLite FTS5 is the
      server-side alternative and would be a catalog migration.
- [x] F3. Preview pane in the finder (telescope-style), reusing the preview work in H.
- [x] F4. Open / select / act on the result without leaving the keyboard.

## G. Right-click context menu

- [ ] G1. Native-feeling context menu on tiles and table rows.
- [x] G2. Actions: open preview, show in Files view, download, archive, restore, pin,
      and copy path. A browser page cannot invoke the operating system's native
      “reveal in folder” action without a trusted native bridge. “Show in Files
      view” is the precise web equivalent: it selects the record's folder,
      clears restrictive filters, switches to the Files view, and focuses the
      record (including finder matches beyond the first loaded page).
- [ ] G3. Multi-select aware -- right-clicking inside a selection acts on the whole selection.
- [ ] G4. Keyboard-reachable (context-menu key, and a leader binding).

## H. Thumbnails and previews for documents

Today `internal/thumbnail` covers images (imaging), HEIF/HEIC and video (ffmpeg). PDFs, EPUBs
and extension-less bookmark files render as a grey tile with the filename. Four docs already
say so on purpose (`docs/deployment.md:27, :239, :1414-1423`, `coldstore.toml.example:64-65`)
-- all four need updating in the same change as the code.

**Research conclusions:**

| Need | Answer | Cost |
|---|---|---|
| PDF thumbnail | shell out to **`pdftocairo`** (poppler-utils) | 1 apt package, 716 kB |
| EPUB thumbnail | **hand-rolled ~150-line extractor**, `archive/zip` + `encoding/xml` | zero deps |
| first page *with content* | render up to 3 pages, **ink-coverage test** in Go | zero deps |
| PDF preview | **pdf.js via react-pdf**, lazy-loaded | ~1.5 MB into the binary |
| EPUB preview | **server-extracted chapters + sandboxed iframe** | 0 KB |
| text preview | server-side sniff + `<pre>` behind a new inline endpoint | zero deps |

Notes that decided those:
- **`pdftoppm` cannot write to stdout** -- `pdftoppm ... -` creates a file named `-.jpg`.
  Every snippet online that pipes it is wrong. `pdftocairo -jpeg -singlefile -scale-to 400
  -f 1 -l 1 in.pdf -` does, and Debian bookworm's manpage documents exactly that form.
- **Pure Go is not available.** pdfcpu cannot rasterize; ledongthuc/rsc.io are text-only;
  UniPDF is commercial; go-fitz is AGPL. `go-pdfium` in wasm is the one real candidate and
  is **rejected on this box**: a 5.6 MB `pdfium.wasm` embedded in the binary, "a lot slower
  than native" on a 1.5 GHz K10, and wazero **never returns linear memory to the OS** --
  the wrong shape for a long-lived daemon on 3.8 GB that `docs/deployment.md:1426` already
  flags for OOM risk. A subprocess is bounded by process exit instead.
- **ImageMagick is a non-starter**: Debian ships a policy denying the PDF coder over the
  Ghostscript CVEs, so `convert` fails "not authorized" until someone hand-edits a system
  security policy on a hardened box.
- Preview rendering belongs **in the browser**, not on the N40L. Server-rendered preview
  pages would put PDF rasterization on a 1.5 GHz 2011 core in the request path, competing
  with Syncthing and the thumbnail pass. With pdf.js the daemon's job is `http.ServeFile`
  with range requests, which it already does. Archived files are served straight from cold
  storage, so **previewing does not trigger a restore** -- browsing the archive without
  un-archiving it comes free.

### Blockers that must be cleared first

- [x] H0a. **`thumbnailExts` (`daemon.go:73-78`) is a second hand-maintained copy of
      `CanThumbnail` (`thumbnail.go:52`).** Nothing ties them together and nothing tests that
      they agree; adding a format to one and not the other means the generator supports it
      while the queue never selects it. Collapse into one exported
      `thumbnail.SupportedExtensions()` **before** touching anything else here.
- [x] H0b. **Extension-less files cannot be reached at all by an extension-keyed change.**
      `mime_type` comes from `mime.TypeByExtension` (`scanner.go:174-175`) and there is **no
      content sniffing anywhere in the repo**; `FilesNeedingThumbnails` filters
      `extension IN (...)` (`catalog.go:803`), which is `''` for exactly the bookmark files
      this is for. Needs migration 6 adding `detected_type`, a 512-byte magic-number read in
      the scanner gated on insert-or-mtime-change, and an `OR detected_type IN (...)` in the
      queue predicate.
- [x] H0c. **`scripts/seed-demo` writes fake PDFs** -- `seedDocuments`
      (`scripts/seed-demo/main.go:475-497`) fills six `.pdf` files with filler bytes. The
      moment `.pdf` joins the allowlist all six fail three times and Documents fills with
      failure glyphs; the Playwright suite asserts on gallery and glyph state, so
      `make test-playwright` and CI go red. Fix the fixture **in the same commit**: a
      minimal valid one-page PDF is ~700 bytes of literal text, a minimal EPUB is a dozen
      lines of `archive/zip`. Keep one deliberately corrupt file so the failure path stays
      covered.
- [x] H0d. **BUG-3 must be fixed before H1 ships, not after.** `Lightbox.tsx:128` treats
      "has a thumbnail" as "is an image", so the first PDF thumbnail turns every PDF into a
      broken `<img>` pointing at 20 MB of PDF bytes.
- [x] H0e. **`Content-Disposition: attachment` is unconditional**
      (`handlers_files.go:229`). Preview needs a new inline route, and an explicit
      `Content-Type` -- `ServeFile` sniffs from the extension and `.epub` is not in Go's
      default MIME table.
- [x] H0f. **`db.SetMaxOpenConns(1)`** (`catalog.go:70`) -- every handler serialises against
      the quota tick and the job worker. A synchronous render inside a request handler would
      block the daemon; slow work goes on the job queue, which is the lesson
      `handlers_status.go:38-45` already records from the old inline scan.
- [x] H0g. CI needs `poppler-utils` in the `e2e` and `playwright` jobs alongside ffmpeg, or
      the new thumbnails silently do not generate there. `fresh-clone-build` does not need it.

- [x] H1. PDF thumbnail via `pdftocairo` to stdout, decoded and resized through the existing
      `saveThumb` path so the 400px/q80 contract comes free. Mirror `generateHEIF`
      (`thumbnail.go:199-231`) line for line, and reuse the subprocess harness verbatim --
      `Setpgid`, process-group SIGKILL on cancel, `WaitDelay = 2s`, a 30s deadline
      (`thumbnail.go:161-197`) -- plus a `SetPDFRenderer(binary, timeout)` seam mirroring
      `SetFFmpeg` so the fake-binary test pattern in `ffmpeg_test.go` extends to it.
- [x] H2. EPUB thumbnail: five-branch cover lookup (EPUB3 `properties="cover-image"`; EPUB2
      `<meta name="cover">` by id; the same by filename; a manifest id of
      `cover`/`cover-image`/`coverimage`; `<guide type="cover">` then its first `<img>`),
      falling back to a spine walk for the first document containing an image. Resolve every
      href **relative to the OPF's own directory, not the zip root** -- that is the bug every
      naive implementation ships. Cap the OPF read and the cover read, reject `..` in
      resolved paths and absurd declared sizes. Also: **register `image/gif`**
      (`thumbnail.go:12-20` does not), since some older covers are GIF. Return a clean
      `thumb_error` for SVG covers rather than pulling in a rasterizer.
- [x] H3. "First page with content": render page 1 at thumbnail size, compute **ink
      coverage** on a stride with a ~3% border margin ignored; below ~0.5% try page 2 then 3,
      then keep page 1 regardless -- never fail a file over this. Cap at 3 renders on one
      shared deadline. Coverage, not standard deviation: they disagree on gradient covers and
      coverage is the one that behaves on scans.
- [x] H4. PDF preview via `react-pdf` (MIT, pins `pdfjs-dist` exactly). **Not** the native
      `<embed>`/`<iframe>`: iOS Safari renders only the first page until tapped to full
      screen, Samsung Internet and Opera Mobile hand off or download outright, and the
      unconditional `Content-Disposition: attachment` turns a native embed into a download.
      Wire the worker with `import workerSrc from 'pdfjs-dist/build/pdf.worker.min.mjs?url'`
      so Vite emits it as a hashed asset the embedded FS serves -- no CDN. Ship
      `standard_fonts/` (+780 kB, needed by older PDFs that rely on the standard-14 without
      embedding); **skip `cmaps/`** (+1.2 MB, CJK only).
- [x] H5. EPUB preview: extend the H2 zip reader into
      `GET /api/v1/files/{id}/epub/{resource...}` plus a spine/TOC endpoint, rendered in a
      sandboxed iframe with rewritten resource URLs. **0 KB of bundle**, no unzipping 20 MB
      in a phone browser, and it reuses a parser that will already be tested. Gives up
      epub.js's paginated columns and CFI memory, which "look before downloading" does not
      need. epub.js (224 kB, BSD-2) drops in behind the same lazy boundary later if wanted --
      note it must be handed an `arrayBuffer` with `"binary"`, because it infers type from
      the URL extension and the download route has none.
      **New traversal surface**: the untrusted component is a zip entry name, so validate
      resolved paths against the archive root explicitly rather than relying on mux cleaning.
- [x] H6. Plaintext preview, including files with **no extension** (bookmarks). Detect in Go,
      never in the browser: magic numbers first, then git's heuristic -- a NUL byte in the
      first 8000 bytes, or invalid UTF-8, means binary. `http.DetectContentType` is not
      enough on its own; it returns `application/octet-stream` for anything without a magic
      number. Serve from the new inline endpoint with an explicit
      `text/plain; charset=utf-8`, `nosniff`, and a **hard byte cap** (256 KB-1 MB with a
      `truncated` flag) so a 2 GB log cannot stream the box to death for a preview nobody
      reads past screen one. Client side is `<pre class="whitespace-pre-wrap font-mono">`.
      **Skip syntax highlighting** -- highlight.js core plus grammars is 80-150 kB for
      content that is bookmarks and notes.
- [x] H7. Bundle budget. Note the counterintuitive part: **`React.lazy` does not shrink the
      Go binary** -- `//go:embed all:dist` embeds every file whether split or not. Splitting
      buys first paint, not size. Budget: dist 278 KB today -> ~2.6 MB, binary 14.4 -> ~17 MB.
      One `React.lazy` boundary holding pdf.js, imported **on lightbox open for a previewable
      file**, not on app mount, so the gallery grid never touches it. Embedded assets are
      demand-paged from the executable's read-only segment, so they cost evictable page cache
      rather than daemon RSS -- which is exactly why +2.6 MB of frontend and +5.6 MB of
      `pdfium.wasm` are not comparable numbers.
- [x] H8. Preview must work for **archived** files too, or explicitly say "restore to preview".
- [ ] H9. **Thumbnails exist at exactly one size** -- `maxEdge = 400`, JPEG q80,
      `.thumb.jpg` (`internal/thumbnail/thumbnail.go:25-28`). There is no larger derivative,
      which is why the lightbox loads the full original instead (see BUG-3). A preview tier
      probably wants a second, larger derivative rather than either extreme.
- [ ] H10. Host reality check for H3: the microserver is a **Debian 12 HP N40L, 3.8 GB RAM,
      2 x 1.5 GHz**. Whatever renders PDF pages has to be cheap, and the thumbnail worker
      pool is already sharing those two cores with the scanner.

### What shipped, and where it left the plan

H0a-H8 and BUG-3 are in. Four things differ from the research above, all
discovered against the real code:

1. **The EPUB reader is server-extracted, not epub.js.** H5's own analysis
   preferred it and it turned out to be the cheaper build too, because the
   container parsing had to exist for H2 anyway. It lives in `internal/epub`,
   shared by the cover thumbnailer and the API, so the traversal check exists
   once. epub.js still drops in behind the same lazy boundary if paginated
   columns and CFI memory turn out to matter.
2. **pdf.js 5 needs a `wasm/` directory the research did not know about** --
   the JPEG 2000 and colour-management decoders, +800 kB. It is shipped, because
   without it scanned PDFs fail on a box with no egress. `cmaps/` is still
   skipped. Built `dist` is 3.4 MB rather than the budgeted 2.6 MB.
3. **The Files view needed a lightbox.** Documents open there, not in the
   gallery -- `App` picks the gallery only for folders whose name looks
   photographic -- so the readers would have been unreachable from the one place
   documents actually are. The filename is the trigger, not the row, which
   leaves the checkbox and shift-range selection untouched.
4. **`seed-demo` has to sniff its own files.** The Documents folder is seeded
   DISABLED, so no scan ever reaches it, so its extension-less `bookmarks` file
   had an empty `detected_type` and the frontend could not classify it. A
   fixture whose catalog is missing a column the real one always has hides the
   feature it exists to exercise.

H9 (a second, larger thumbnail derivative) is **not** done and is no longer
urgent: the lightbox no longer loads a full original to render an image it
cannot decode. It stays open as a quality improvement -- a 400px thumbnail
standing in for a HEIC is visibly soft at full screen.

## I. Operational status dump

The first production rollout showed that answering “is the archive healthy?” currently
means assembling output from `systemctl`, `journalctl`, `findmnt`, SQLite, btrfs and the HTTP
API by hand. Add one CLI surface that produces a concise report suitable for routine checks
and for pasting into a support handoff.

- [x] I1. Add a read-only status-dump command to the `coldstore` binary. It must work when
      the daemon or HTTP listener is unavailable and must never require the Basic Auth
      password in argv, shell history or output.
- [x] I2. Report the running/build version when available, supported and on-disk catalog
      schema versions, cold-storage mount status, configured folder readiness, budget and
      pinned/wedged state, file counts by status, active/failed jobs, thumbnail/metadata
      failures, recent warning events, and the most recent successful scan and offload.
- [x] I3. Opening the catalog for this command must not run migrations, reconcile TOML,
      write probes, start a scan or otherwise mutate production state. A newer catalog must
      be reported rather than “fixed”.
- [x] I4. Default output is human-readable and explicitly redacts credentials, Syncthing
      device IDs and other secrets; add structured JSON for automation. Exit non-zero when
      a load-bearing health check fails, while distinguishing warnings from failures.
- [x] I5. Cover the failure modes that matter on the deployed host: archive mount absent,
      degraded btrfs membership, schema mismatch, folder missing its `.stfolder`, stale or
      failed scan, wedged folder, failed/interrupted job, derived-data failures and recent
      conflict/data-safety events. Document what the command can and cannot verify without
      root privileges or a Syncthing API client.

Implemented command contract: `coldstore --status` emits the human report and
`coldstore --status --json` emits the same redacted model for automation. Exit `0` is
healthy, `1` means warnings and `2` means a load-bearing failure. Status dispatches before
environment write probes and uses immutable/query-only SQLite access; it refuses an
uncheckpointed WAL rather than ignoring or changing it. See `docs/deployment.md`, “Offline
archive health report,” for the field list, redaction boundary and limitations.

---

## Bugs found while driving the seeded demo

Not in the owner's notes -- found by screenshotting the running UI at 1440x900 and 390x844.

- [x] **BUG-1. Gallery selection is unreachable on a touch device.** Fixed in 93affd0;
      two browser tests at a 390px viewport hold it. The pin toggle had the same defect and
      was fixed alongside.

  Original diagnosis: The tile checkbox is
      `opacity-0 group-hover:opacity-100` (`web/src/components/Gallery.tsx:227`). A phone has
      no hover, so the checkbox never appears, and the only other way in --
      `anySelected` -- cannot be reached because nothing can be selected first. The
      gallery's entire multi-select feature is dead on the device the owner browses photos
      from. Long-press to enter selection mode is the conventional fix; it lands with C2.
- [x] **BUG-2. The mobile sidebar toggle sits on top of the folder heading.** The button is
      `fixed top-3 left-3` (`web/src/App.tsx:58`) and the heading has no left inset at that
      breakpoint, so "CameraRoll" renders as "meraRoll" at 390px. Fixed in 7978fd5.
- [x] **BUG-3. The lightbox puts raw video and HEIC bytes inside an `<img>` tag.**
      `Lightbox.tsx:128` picks its source as "has a thumbnail and is not missing, therefore
      renderable as an image" -- but `internal/thumbnail` also thumbnails `.heif .heic` and
      `.mp4 .mov .avi .mkv .webm` (`thumbnail.go:36-50`). So opening a video hands the
      browser the **full original file** as an `<img src>`: it downloads every byte over the
      LAN and then renders a broken-image icon. HEIC does the same everywhere except Safari.
      Needs a `<video>` branch and a decode-capability check, and it lands with H4-H6 since
      that is the same "what can this browser actually render" decision.

## Decisions (owner interview, 2026-08-21)

1. **Access -- still open.** The owner had not met Tailscale, and asked back: *"Is it possible
   to setup full web routing with a domain name? Might that be the way to go?"* They also
   want **guests** to be able to reach it ("other people's devices -- a guest showing photos,
   a family member"), which is the one requirement Tailscale genuinely cannot serve: a guest
   would have to install a VPN and be invited to the tailnet. That pushes toward a real
   domain, which changes B6 from optional to required. Not resolved; see "Still open" below.
2. **Auth: keep basic auth for now**, with the note *"we may revisit this if we do full web"* --
   i.e. B6/B7 becomes mandatory if the domain path wins.
3. **Dark only.** No light mode. Design for dark properly.
4. **Aesthetic: restrained and utilitarian** -- `htop`/`lazygit`/Prometheus register, dense
   and high-information, colour only where it means something. But explicitly: *"The existing
   theme isn't actually that bad -- the right mix of photo forward and utilitarian."* So this
   is a refinement, not a rebuild.
5. **Specific dislikes**: the saturated primary-colour buttons (blue Download / green Restore
   / orange Archive on every row) -- *"This!"*; wants a settled typeface across systems though
   `system-ui` "isn't bad"; density slightly cramped; **text a bit small to read**, wanting a
   balance of readability against tool density.
6. **Keyboard: full operator-pending grammar.** *"That's how I use the rest of my tools, and
   would really appreciate to be able to do this well."* And: *"I want to use this without
   touching my mouse at all, ever."*
7. **Command palette: everything** -- navigation, selection actions, config changes, and vim
   meta commands.
8. **Fuzzy finder scope**: filenames, paths, folder, extension and status. Not full text
   inside documents.
9. **Archived preview: stream from cold storage**, no state change. *"Fine if somewhat slow,
   should be fairly rare, but want the option to do it."*
10. **poppler-utils approved.** *"Yes, please, don't reinvent the wheel here."*
11. **Ship full readers**, lazily and in the background; a bigger binary is fine.
12. **New major feature: tagging.** Photo tags, descriptions, multiselect tagging, a
    keyboard-driven culling flow like a professional photo vault, folder renaming, and
    capture-date + location metadata preserved from iPhone. Being designed and split into
    GitHub issues in its own worktree; see `docs/tagging-design.md`.

## Still open

Only one thing, and it blocks section B: **domain or no domain.**

- Tailscale covers the owner's own phone and laptops, on and off the LAN, free, with a real
  certificate and no code change. It cannot serve a guest without that guest installing a VPN.
- A purchased domain (~$15/yr) plus Cloudflare Tunnel + Cloudflare Access, or a reverse proxy
  on a forwarded port, serves guests -- at the cost of putting a control plane whose archive
  button propagates deletions through Syncthing to every device the owner owns on the public
  internet. That raises the stakes on B6/B7 from cosmetic to load-bearing, and Cloudflare's
  free-tier terms are a poor fit for serving full-resolution photo and video.
- These are not exclusive. Tailscale now costs nothing to adopt and forecloses nothing.


---

## Integration notes

Four branches are in flight. They did not all fork from the same commit, and two of
them will need a follow-up pass rather than a clean merge.

| Branch | Forked at | Carries |
|---|---|---|
| `feat/interface-refresh` | `9bcb7e8` (main tip) | A, C, BUG-1, BUG-2 |
| `worktree-agent-a57eb908…` | `9bcb7e8` (main tip) | H, BUG-3 |
| `worktree-agent-accbd99e…` | `37ad497` (6 behind) | D, E, F, G |
| `feat/tagging-design` | `37ad497` (6 behind) | `docs/tagging-design.md` only |

1. **The keyboard branch predates the design tokens.** It forked before
   `feat/interface-refresh` existed, and it was deliberately told not to restyle
   anything so that a parallel restyle could not collide with it. So every component it
   adds -- command palette, context menu, fuzzy finder, `?` cheatsheet -- is written
   against raw Tailwind defaults (`bg-neutral-900`, `text-blue-400`). Those classes
   still exist, so the merge compiles and renders; it just renders off-system. **Sweep
   the new components onto the tokens as the first commit after that merge**, and
   re-run the `SWEEP CLEAN` grep:
   `grep -rn "neutral-\|amber-\|emerald-\|indigo-\|red-[0-9]\|blue-\|sky-" web/src --include=*.tsx | grep -v '\.test\.'`
2. **`Lightbox.tsx` is unswept on purpose** (C7). The documents branch is rewriting its
   render path for BUG-3 and the document previews. Sweep it once that lands, not before.
3. **Both `37ad497` branches are missing six commits of main** -- the deploy runbook
   rewrite, `[storage] require_mountpoint`, the node-lockfile build fix. All docs and
   config; rebase or merge main into them before review rather than after.
4. **The z ladder is centralized in `web/src/lib/zLayers.ts`.** Tailwind v4 has no
   `--z-*` theme namespace, so `z-sidebar` and friends silently produced class names
   that did not exist. New layers must add or reuse a `Z` value there and apply it as
   an inline `style`; do not introduce a numeric z-index class at the call site.
5. **The table's native checkbox is `sr-only`** with a styled span over it, so anything
   that clicks `input[type=checkbox]` directly will hang on Playwright's actionability
   check. Use the `selectRow` helper in `web/e2e/helpers.ts`, or click the label.
