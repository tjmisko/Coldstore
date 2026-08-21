# Status — coldstore closeout

Snapshot date: 2026-08-21. Snapshot base: `feat/interface-refresh` at `5a0de5b`.
The Git tree is the source of truth for any state that changes after this file.

## Executive state

- The production daemon is live and authenticated, reached through a loopback SSH tunnel.
- Initial cataloguing completed. A real UI archive propagated deletion through Syncthing,
  and restore works correctly. The isolated throwaway-folder quota test has not run.
- Production runs repository commit `37ad497` with catalog schema 2.
- Local `main` is `9bcb7e8`, nine commits ahead, including deployment/runbook corrections,
  the real `require_mountpoint` guard and the node-lockfile build fix.
- `feat/interface-refresh` already contains `feat/keymap`; the earlier handoff claiming an
  uncommitted merge was wrong.
- `feat/documents` and `feat/tagging-design` remain to be integrated.
- All GitHub issues #1–#15 are open. #15 is the tagging tracker; #13 is the first live bug.

## Repository and branches

### `main`

- Local head: `9bcb7e8`.
- Deployed/upstream head: `37ad497`.
- The nine local commits add scanner/config fixes, the hardware-correct deployment runbook,
  `[storage] require_mountpoint`, dependency-stamp build repair and ignored private deploy
  documents.

### `feat/interface-refresh`

Carries:

- identity/title/icon/PWA assets;
- BUG-1 touch selection and BUG-2 mobile heading fixes;
- Tailwind v4 design tokens and IBM Plex assets;
- the keymap engine, palette, fuzzy finder, context menu and cheatsheet;
- the operational status-dump task in `docs/interface-refresh.md` section I.

The keymap merge is commit `28ba626`, with parents `13fe402` and `e13fcd6`.

### `feat/documents`

Head: `63dfd06`; twelve commits after shared base `5cab271`.

Adds:

- schema migration 6, `files.detected_type` and content sniffing;
- one source of truth for thumbnailable extensions;
- real seeded PDFs/EPUBs;
- inline file preview routes and EPUB resource routes;
- PDF thumbnails through `pdftocairo` with content-page selection;
- pure-Go EPUB parsing/cover extraction;
- lazy PDF, EPUB and text readers; and
- BUG-3's correct per-file preview dispatch.

Historical isolated verification: Go tests, frontend tests, lint, Playwright, in-process E2E
and a fresh-clone build passed. The live Syncthing tier was not run. Measured output was
approximately 3.4 MB of frontend distribution and an 18 MB binary.

### `feat/tagging-design`

Head: `9bcbb3f`; six documentation commits on old base `37ad497`.

Adds the authoritative metadata/tagging/rename design and corresponds to GitHub issues
#1–#15. It contains no implementation. Its planned migration 6/7 numbers conflict with
documents' landed migration 6 and must become 7/8 before implementation.

## Verified test evidence

### Local `main`

Passed during the familiarization audit:

- `go vet ./...`
- `go test ./...`
- `go test -race ./internal/...`
- `go test -tags e2e -count=1 -timeout 120s ./tests/e2e/...`

Only `make test-e2e-live` proves real Syncthing deletion and restore propagation in CI.
Never combine the `e2e_live` and `e2e` build tags.

### Current interface/keymap branch

- ESLint passed with zero warnings.
- One full Vitest run passed 212/212.
- Isolated Playwright passed 27/27.
- Five repetitions of `web/src/components/keyboard.test.tsx` passed 3/5.

The intermittent failures included basic `j` motion and `2d3j`, not just visual/context-menu
tests. The best current diagnosis is a passive-effect race:

- tests wait until the fetched row is visible;
- `useKeymapLayer` updates `handlerRef.current` in `useEffect`;
- a row can commit before that effect replaces the handler closure that still sees an empty
  `files` array; and
- `useGridKeymap` then rejects the command at its empty-list guard.

Cleanup already unmounts every test and clears the provider timer, making the old leaked-timer
theory less likely. Fix the lifecycle, then prove it with repeated targeted runs.

## Production environment

The host-specific ignored documents contain private identifiers and are not reproduced here.
The operational shape other agents need is:

- Debian 12 on an HP N40L: two AMD K10 cores, 3.8 GiB RAM and a 4 GiB swapfile.
- Build with the default `GOAMD64=v1`; v2/v3 binaries are not supported by this CPU.
- Go 1.27 and Node 24 are installed from upstream tarballs. The old split-build procedure is
  retired; the host can run `make build` itself.
- Hot Syncthing folders live on the root filesystem. Cold storage is a dedicated btrfs RAID1
  subvolume on two devices. A separate unprotected btrfs device is explicitly out of scope.
- `thumbnail_workers=1` is intentional on this memory-constrained host.
- `ffmpeg`, SQLite, `poppler-utils` and `exiftool` are installed.
- Five managed Syncthing folders are healthy, fully synced, Send & Receive and protected by
  the required coldstore temp/superseded ignore patterns.
- Syncthing file versioning is disabled. The server still runs Syncthing v1.19.2 and should
  be upgraded as an operations task.
- The host uses a static wired address outside the DHCP allocation pool.
- The archive mount, swap and btrfs scrub are operational. Snapshot scheduling is awaiting
  explicit owner confirmation; do not claim it is either configured or absent until then.
- The owner considers existing backups sufficient. A fresh SQLite `.backup` remains a
  mandatory gate immediately before any production schema upgrade.

### Access and authority

- Current: Basic Auth on loopback, reached through an SSH tunnel.
- Desired: a permanent browser-native HTTPS route that does not require installing a VPN
  client and can accommodate guests.
- Consequence: Tailscale alone does not meet the primary requirement. A real public ingress
  makes session auth, rate limiting and a carefully private backend listener load-bearing.
- The owner does not authorize agents to SSH to or otherwise access the production host.
  Ask for specific output. Do not deploy/restart/migrate/change budgets without explicit
  approval.

## Known defects and integration hazards

### Data-safety bugs

- GitHub #13: editing a TOML folder label silently creates a second catalog identity for the
  same path. Labels are permanent until the guard is implemented.
- GitHub #6: Live Photo still/video/edit members have no pair group; automatic or manual
  movement can split them.

### Frontend/integration bugs

- Intermittent keyboard stale-handler race described above.
- `ToastProvider` still hardcodes `z-60`, although `zLayers.ts` requires toasts above
  overlays. The CSS/task docs describe an obsolete ladder.
- New keymap overlays and the deliberately untouched Lightbox still use raw Tailwind colors.
- Fuzzy-finder preview/select/general-action behavior is incomplete.
- Context-menu “reveal folder” is absent.
- FileBrowser's keyboard Open is currently a no-op and context-menu Open downloads; after the
  documents merge both must open the preview lightbox.

### Documentation drift

- `docs/interface-refresh.md` checkboxes for D–H do not describe actual implementation state.
- Deployment rollback prose incorrectly says any older binary accepts a newer catalog. The
  migration runner refuses newer schemas; only pre-migration binaries have the dangerous
  behavior the prose describes.
- Deployment prose says an archive can be rebuilt by scanning, then admits archived files
  are absent from that scan. The first claim is false.
- The tracked architecture API list omits the document preview/EPUB routes.
- The React guide omits the new readers.
- `docs/control-plane-design.md` contains historical build instructions that now contradict
  the tracked `.gitkeep` rule.
- The ignored deployment record/checklist need local-only updates after document support is
  integrated. Never commit their identifiers.

## Metadata decisions already settled

- `evanoberholster/imagemeta` is the primary still-photo reader. Real iPhone HEIC GPS,
  capture time, offset and subseconds matched ExifTool.
- Re-probe suspicious/empty HEIC results with `bep/imagemeta`; it handles the verified
  `iloc`-before-`iinf` failure mode. Apply GPS reference signs explicitly.
- No pure-Go reader can read Apple's offset-based maker-note ContentIdentifier. Use a
  resident `exiftool -stay_open` subprocess only for Live Photo still candidates.
- Never import `barasher/go-exiftool`; its GPL-3.0 license would change the Go binary's
  licensing posture.
- Use `ffprobe` only for QuickTime/video metadata. It exposes no useful EXIF from stills.
- Parse Apple's colon-less creation-date offset and all three ISO 6709 forms documented in
  the tagging design.
- The only unmeasured tagging claim is the FTS5 performance argument; issue #9 requires a
  benchmark before the search decision is final.
