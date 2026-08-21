# Handoff — coldstore interface refresh

Repo `github.com/tjmisko/Microserver`. Owner ran a deployment, gave interface feedback,
asked for parallel agent work. Spec: `docs/interface-refresh.md` (tasklist + decisions).

## Branches

| Branch | State |
|---|---|
| `feat/interface-refresh` | **merge of `feat/keymap` IN PROGRESS, UNCOMMITTED** — see below |
| `feat/keymap` | done, verified (= `worktree-agent-accbd99e410721eda`) |
| `feat/tagging-design` | done, docs only (= `worktree-agent-a5c6d024a429c00fc`, `2455afa`) |
| `feat/documents` | **done, green**, 12 commits, branched from `feat/interface-refresh`@`5cab271` |

Worktrees: `.worktrees/feat/interface-refresh`, `.claude/worktrees/agent-*`.

## FIRST THING TO DO

`.worktrees/feat/interface-refresh` has an **uncommitted, resolved merge** of `feat/keymap`.
Conflicts (App, ConfirmDialog, FileBrowser, Gallery, SelectionBar) resolved by taking the
keymap structure and re-applying the token classes. Verified green:
`make lint`, `go test ./...`, playwright **27/27**.

`make test-web` shows **2 intermittent failures in `web/src/components/keyboard.test.tsx`**
(visual mode / context menu). **This flakiness is pre-existing on `feat/keymap` itself** —
confirmed 2-of-3 clean runs on the unmerged branch, so it is NOT a merge artifact. Was
mid-diagnosis when handed off. Suspect module-level state or a leaked pending-key timeout
bleeding between tests. Fix it, then commit the merge.

Playwright needs an isolated root — both agents share `/tmp/coldstore-e2e`:
`COLDSTORE_E2E_ROOT=/tmp/coldstore-e2e-design COLDSTORE_E2E_ADDR=127.0.0.1:8093 make test-playwright`

## Done

- **A. Identity** — real `<title>`, per-view suffix, SVG favicon, apple-touch-icon, manifest,
  `.webmanifest` mime registered in Go (Debian lacks it → iOS install silently fails).
- **C. Design refresh** — Tailwind v4 `@theme` in `web/src/index.css`. Rule: **colour means
  where a file lives, never what a button does.** cold=archived, held=pinned, alarm=broken,
  and one reserved hue (`cursor`, teal) for the keyboard cursor only. IBM Plex Sans+Mono,
  latin, committed under `web/public/fonts/` (no egress on the target host).
  `Lightbox.tsx` deliberately UNSWEPT — documents agent owns it.
- **BUG-1** gallery selection dead on touch (hover-only checkbox) — fixed + 2 browser tests.
- **BUG-2** mobile toggle over folder heading — fixed + test.
- **D–G keyboard** — full operator-pending vim engine, palette, fuzzy find, context menu,
  cheatsheet. `docs/keymap-design.md`.
- **Tagging** — `docs/tagging-design.md` (1811 lines) + **GitHub issues #1–#15**.
- **H. Documents (`feat/documents`)** — all of H0a–H8, H0g, **BUG-3**. `pdftocairo` PDF
  thumbs + ink-coverage first-page-with-content, hand-rolled EPUB cover extraction
  (`internal/epub`), content sniffing (`internal/filetype`, migration 6, as a queue not a
  walk step), inline `GET /files/{id}/preview`, pdf.js + server-extracted EPUB reader behind
  one `React.lazy`. Green: `make test`, `test-web` 107, `lint`, `test-playwright` 22,
  `test-e2e`, and a **fresh-clone `make build` with no `npm ci`**. `test-e2e-live` NOT run.
  Deviations worth knowing: dist **3.4 MB not 2.6**, binary **18 MB not ~17** (pdf.js 5 needs
  a `wasm/` dir the research predated — required for scanned PDFs on a no-egress box); the
  **Files view gained a lightbox**, because `App` only picks the gallery for
  photographic-sounding folder names so documents were unreachable; `seed-demo` had to sniff
  its own files (Documents is seeded disabled → never scanned).

## Open

1. **ACCESS (blocks section B) — needs the owner.** Tailscale + `tailscale serve` gives
   LAN+remote+real cert, free, no domain, zero code change. But owner wants **guest access**,
   which Tailscale can't do. He asked "is a real domain the way to go?" — unanswered. A domain
   makes session auth (B7) mandatory, not cosmetic. `coldstore.service:215` `IPAddressAllow`
   omits Tailscale's `100.64.0.0/10` — only matters if binding to the TS IP.
2. **Merge `feat/documents`** into `feat/interface-refresh` AFTER committing the keymap merge.
   Author's guidance: its `Lightbox.tsx` / `FileBrowser.tsx` edits are small and localised
   (one `resolvePreview` call + `<PreviewSurface>`; a filename `<button>` + a `<Lightbox>`
   mount) — **merge documents ON TOP of the design/keymap side, not the reverse.**
   It branched from `5cab271`; `feat/interface-refresh` is now at `13fe402`+.
   Its migration 6 bumps catalog schema — older binaries refuse the upgraded DB (this is what
   broke the shared `/tmp/coldstore-e2e` root).
   Its `Lightbox.tsx` is still UNSWEPT for design tokens — sweep after merging.
4. **Sweep new keymap components onto tokens** — palette/finder/menu/cheatsheet were written
   against raw Tailwind (that branch predates the tokens). Check:
   `grep -rn "neutral-\|amber-\|emerald-\|indigo-\|red-[0-9]\|blue-\|sky-" web/src --include=*.tsx | grep -v '\.test\.'`

## Metadata research outcome (affects issues #1, #6)

- **GPS `0,0` blocker RETRACTED** — did not reproduce on a real iPhone 12 Pro HEIC; only on
  synthetic minimal maker notes. #1 should unblock.
- **Apple maker note 0x0011 unreadable from pure Go — confirmed on real data.** Pattern:
  embedded (≤4 byte) values parse, **all offset-based values fail** (ContentIdentifier is a
  string → always broken). So Live Photo pairing must use `exiftool -stay_open` for the still
  half; MOV half is free from ffprobe. **Invoke exiftool as a SUBPROCESS, never import
  `barasher/go-exiftool` — it is GPL-3.0** and would make the binary derivative. Precedent:
  `internal/thumbnail/thumbnail.go:162` shells out to ffmpeg.
- `evanoberholster/imagemeta` **silently drops all EXIF when HEIC `iloc` precedes `iinf`**
  (libheif/Nokia files; Apple/Canon fine). Cross-check with `bep/imagemeta`, which is
  unaffected (returns unsigned GPS — apply the ref sign yourself).
- ffprobe `creationdate` offset has no colon → use `"2006-01-02T15:04:05-0700"`, not RFC3339.
- ffprobe surfaces **zero** EXIF from stills. Separate path required.

## Pre-existing bugs found (not in the owner's notes)

- **#13** editing a folder label in `coldstore.toml` **silently duplicates the catalog** —
  `ReconcileFolderFromTOML` (`internal/catalog/catalog.go:857`) upserts
  `ON CONFLICT(folder_label)`. Old row stays `present_in_toml=0`; scanner re-inserts every
  file under the new label; archived files survive only as old-label rows. Verified. Live risk
  — owner is actively editing that file.
- **#6** quota enforcement can archive half a Live Photo pair (mtime ordering splits them).

## Conventions

Conventional commits, no emoji, `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.
Tests named "should [behavior] when [condition]". Zero eslint warnings, no `eslint-disable`
in `web/src`. Never remove a `data-*` attribute (28 pre-existing = test contract). Never add
`mkdir -p internal/api/dist` to a Makefile target. `noclobber` is set — use `>|`.
Demo: `:8099` (`:8090` is the owner's live SSH tunnel — do not bind it).
