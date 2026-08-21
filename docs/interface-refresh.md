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

- [?] B1. Pick the LAN access mechanism. (research: `access-research`)
- [?] B2. Pick the remote access mechanism.
- [?] B3. Decide whether HTTPS is required, and how the cert is obtained without a domain.
- [ ] B4. Make the bind address configurable and document the firewall posture.
- [?] B5. Token / auth design. Must work with `<img src>` thumbnail requests and range
      requests -- that constraint rules out header-only bearer tokens for the media path.
- [ ] B6. Update `docs/deployment.md` + the deploy checklist with the chosen story.

## C. Design refresh

Run `/frontend-design` over the existing Tailwind v4 dark-only UI. Current state: default
Tailwind neutral palette, `system-ui` type, no design tokens, no light mode.

- [ ] C1. Aesthetic direction: palette, type scale, spacing, elevation, motion.
- [ ] C2. Selection affordance -- bigger, prettier checkboxes, tuned for photo tiles.
      Current: native `<input type="checkbox" class="accent-blue-500">` at `top-1 left-1`
      inside a `bg-black/50` chip (`web/src/components/Gallery.tsx:229`). Too small to hit on
      a phone, and it disappears until hover.
- [ ] C3. Restyle the tile: badges, pin toggle, archived/pinned states, hover/focus rings.
- [ ] C4. Restyle the file browser table, filter bar, selection bar, status bar, config panel.
- [ ] C5. Empty states, loading states, error states.
- [ ] C6. Light mode? (needs a decision)

## D. Keyboard -- vim-flavoured navigation

Leader is **space**. Today only the lightbox and a few inputs handle keys
(`Gallery.tsx:67`, `Lightbox.tsx:103`).

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

- [ ] F1. `F10` **and** `<leader>fe` (space, f, e) open a fuzzy file finder.
- [ ] F2. Decide client-side vs server-side matching -- depends on catalog size at scale.
      A folder with 100k files cannot ship its filename list to the browser.
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

---

## Open questions for the interview

1. Access: LAN-only, or genuinely remote (off-network) too? Which devices?
2. Is a browser cert warning acceptable, or must the padlock be clean?
3. Single shared secret, or per-device tokens that can be revoked?
4. Light mode: worth building, or is dark-only the intent?
5. Aesthetic direction -- reference points the owner actually likes.
6. Vim fidelity: how far? Operator-pending grammar (`d3j`), or just single-key bindings?
7. Fuzzy finder scope: filenames only, or paths and metadata too?
8. Preview: is "restore to preview" acceptable for archived files, or must cold files stream?
9. Is adding an apt dependency (poppler-utils) acceptable, or must the binary stay standalone?
10. Priority order across A-H, and what ships first.
