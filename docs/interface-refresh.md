# Interface Refresh -- Tasklist

Post-deployment feedback from the first real install on the microserver. Source: owner's
notes, 2026-08-21. Each item below is either **decided**, **needs a decision** (goes to the
interview), or **needs research** (agent reports land in this doc as they resolve).

Status legend: `[ ]` not started `[~]` in progress `[x]` done `[?]` blocked on a decision

---

## A. Identity -- title and favicon

The page still ships Vite's scaffold identity: `web/index.html` has `<title>web</title>` and
`<link rel="icon" href="/vite.svg">`. Every tab in the owner's browser says "web".

- [ ] A1. Real `<title>` -- static, plus per-view suffix (folder name / "Gallery" / "Config").
- [ ] A2. Real favicon. SVG mark, dark/light aware, embedded (no CDN).
- [ ] A3. `apple-touch-icon` + `manifest.webmanifest` so an iOS home-screen bookmark looks right.
- [ ] A4. `<meta name="theme-color">` and viewport-fit for mobile.
- [ ] A5. Verify the embedded build serves all of the above from `internal/api/dist/`.

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

- [?] B1. Adopt Tailscale + `serve` as primary. Needs the owner's yes -- it is the one
      decision that changes what every device has to run.
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
- [?] B6. Token auth: **probably do not build it.** Basic auth
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

Also missing and needed by E and G: **no React portal anywhere** (`createPortal` appears
zero times -- every overlay is an inline `fixed inset-0` div), **no focus trap**, **no
scroll lock**, and **no z-index scale** (the ladder is hand-rolled: sidebar 40, backdrop
30, hamburger 50, lightbox 50, toasts 60, dialog 70). `ConfirmDialog.tsx` is the only
dialog primitive and it is 69 lines.

Constraint for every task in this section: the `data-*` attributes are the selector
contract both test tiers are written against (`data-file-id`, `data-selected`,
`data-folder-nav`, `data-view-nav`, `data-filter`, `data-badge`, `data-toast`, ~28 in all).
Restyling must not rename or drop them.

- [ ] C1. Aesthetic direction: palette, type scale, spacing, elevation, motion.
- [ ] C2. Selection affordance -- bigger, prettier checkboxes, tuned for photo tiles.
      Current: native `<input type="checkbox" class="accent-blue-500">` at `top-1 left-1`
      inside a `bg-black/50` chip (`web/src/components/Gallery.tsx:229`). Too small to hit on
      a phone, and it disappears until hover. See BUG-1.
- [ ] C3. Restyle the tile: badges, pin toggle, archived/pinned states, hover/focus rings.
- [ ] C4. Restyle the file browser table, filter bar, selection bar, status bar, config panel.
- [ ] C5. Empty states, loading states, error states.
- [ ] C6. Light mode? (needs a decision)

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
- [ ] F3. Preview pane in the finder (telescope-style), reusing the preview work in H.
- [ ] F4. Open / select / act on the result without leaving the keyboard.

## G. Right-click context menu

- [ ] G1. Native-feeling context menu on tiles and table rows.
- [ ] G2. Actions: open, preview, download, archive, restore, pin, copy path, reveal folder.
- [ ] G3. Multi-select aware -- right-clicking inside a selection acts on the whole selection.
- [ ] G4. Keyboard-reachable (context-menu key, and a leader binding).

## H. Thumbnails and previews for documents

Today `internal/thumbnail` covers images (imaging), HEIF/HEIC and video (ffmpeg). PDFs, EPUBs
and extension-less bookmark files render as a grey tile with the filename.

- [ ] H1. PDF thumbnail from the first page **with content** (blank-page skip heuristic).
- [ ] H2. EPUB thumbnail: cover image from the OPF manifest, falling back to first content page.
- [?] H3. Pick the rendering dependency -- pure Go vs an apt package (poppler / mupdf). This
      decides whether the deploy runbook grows a dependency. (research: `preview-research`)
- [ ] H4. Full in-browser PDF preview before download.
- [ ] H5. Full in-browser EPUB preview (paginated reader).
- [ ] H6. Plaintext preview, including files with **no extension** (bookmarks) -- needs
      server-side text-vs-binary sniffing, since the extension carries no signal.
- [ ] H7. Bundle budget: the frontend is embedded in the binary. Preview libraries must be
      lazy-loaded chunks, not part of the initial payload.
- [ ] H8. Preview must work for **archived** files too, or explicitly say "restore to preview".
- [ ] H9. **Thumbnails exist at exactly one size** -- `maxEdge = 400`, JPEG q80,
      `.thumb.jpg` (`internal/thumbnail/thumbnail.go:25-28`). There is no larger derivative,
      which is why the lightbox loads the full original instead (see BUG-3). A preview tier
      probably wants a second, larger derivative rather than either extreme.
- [ ] H10. Host reality check for H3: the microserver is a **Debian 12 HP N40L, 3.8 GB RAM,
      2 x 1.5 GHz**. Whatever renders PDF pages has to be cheap, and the thumbnail worker
      pool is already sharing those two cores with the scanner.

---

## Bugs found while driving the seeded demo

Not in the owner's notes -- found by screenshotting the running UI at 1440x900 and 390x844.

- [ ] **BUG-1. Gallery selection is unreachable on a touch device.** The tile checkbox is
      `opacity-0 group-hover:opacity-100` (`web/src/components/Gallery.tsx:227`). A phone has
      no hover, so the checkbox never appears, and the only other way in --
      `anySelected` -- cannot be reached because nothing can be selected first. The
      gallery's entire multi-select feature is dead on the device the owner browses photos
      from. Long-press to enter selection mode is the conventional fix; it lands with C2.
- [x] **BUG-2. The mobile sidebar toggle sits on top of the folder heading.** The button is
      `fixed top-3 left-3` (`web/src/App.tsx:58`) and the heading has no left inset at that
      breakpoint, so "CameraRoll" renders as "meraRoll" at 390px. Fixed in 7978fd5.
- [ ] **BUG-3. The lightbox puts raw video and HEIC bytes inside an `<img>` tag.**
      `Lightbox.tsx:128` picks its source as "has a thumbnail and is not missing, therefore
      renderable as an image" -- but `internal/thumbnail` also thumbnails `.heif .heic` and
      `.mp4 .mov .avi .mkv .webm` (`thumbnail.go:36-50`). So opening a video hands the
      browser the **full original file** as an `<img src>`: it downloads every byte over the
      LAN and then renders a broken-image icon. HEIC does the same everywhere except Safari.
      Needs a `<video>` branch and a decode-capability check, and it lands with H4-H6 since
      that is the same "what can this browser actually render" decision.

## Open questions for the interview

Research settled more than expected. What is left is genuinely the owner's call.

1. **Access.** Adopt Tailscale + `serve` as the primary path? It costs installing an app on
   each phone and laptop, and gives a real padlock plus off-network access for nothing else.
   The alternative is LAN-bind-only, which means no remote access and a permanent
   "Not secure" chip.
2. **Auth.** Keep basic auth (recommended once TLS is in front of it), or build the login
   screen + logout button? The honest framing: it buys cosmetics and brute-force lockout,
   not security. Is the iOS native credential dialog actually annoying enough?
3. **Light mode.** Build it, or commit to dark-only? This roughly doubles the token work in
   C1 and every restyle after it, so it wants deciding before the sweep, not after.
4. **Aesthetic direction.** Reference points the owner actually likes. Right now the app is
   raw Tailwind defaults, which read as unstyled rather than as a choice.
5. **Vim fidelity.** Single-key bindings and a leader, or a real operator-pending grammar
   (`d3j`, counts, `v` visual mode with motions)? The second is a much larger build and
   changes the architecture of the keymap layer.
6. **Fuzzy finder scope.** Filenames only (matches what the backend can do today), or paths
   and metadata too (needs a catalog change)?
7. **Preview of archived files.** Is "restore to preview" acceptable, or should a cold file
   stream out of cold storage for preview without changing its state?
8. **The apt dependency.** May the deploy runbook grow `poppler-utils` for PDF rendering, or
   must the binary stay standalone? This is the single biggest constraint on H, and it is a
   deployment-philosophy question, not a technical one.
9. **Priority.** What ships first? A is done; BUG-1 and BUG-3 are real defects sitting
   underneath the feature list.
