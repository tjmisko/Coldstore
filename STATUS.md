# Status — coldstore closeout

Snapshot date: 2026-08-21. Integrated code/document state: `feat/interface-refresh`
through `4f029d1`. The Git tree is authoritative for changes after this snapshot.

## Executive state

- Production is live behind Basic Auth on a loopback listener reached by SSH tunnel.
- Production remains at repository commit `37ad497` and catalog schema 2. Nothing from this
  integration was deployed or run against the host.
- Local `main` is `9bcb7e8`, nine commits ahead of the deployed baseline, including the real
  `require_mountpoint` guard and build/runbook corrections.
- `feat/interface-refresh` now integrates keymap, document-preview and tagging-design work.
  Document preview owns shipped migration 6; planned tagging/rename migrations are 7/8.
- The folder-label identity guard for GitHub #13 is implemented locally and tested but not
  deployed. Live Photo members remain non-atomic; #6 is the outstanding P0 data bug.
- All GitHub issues were still open at the last reachable snapshot. Issue-body migration
  edits could not be made because `api.github.com` is unreachable from this environment.

## Integrated repository state

The active branch carries:

- identity/title/icon/PWA assets and the dark token system;
- BUG-1 touch selection and BUG-2 mobile heading repairs;
- one scoped keymap dispatcher, operator-pending grammar, palette, finder, context menu and
  generated cheatsheet;
- the keyboard handler publication fix and centralized overlay/toast layering;
- schema migration 6, content sniffing, PDF/EPUB/text previews and PDF/EPUB thumbnails;
- finder-owned preview/open/select/download/reveal workflows;
- permanent folder-label/path ownership validation before reconciliation writes;
- `coldstore --status [--json]`, using immutable/query-only catalog inspection; and
- the verified tagging/metadata/Live Photo/rename design with planned migrations 7 and 8.

Key integration commits are listed in `HANDOFF.md`. Both feature merges are true two-parent
merge commits (`752c210` for documents and `9b5be84` for tagging design).

## Verified test evidence

Passed on the integrated branch:

- `make lint` with zero warnings;
- `make test-web`: 22 files, 260 tests;
- `web/src/components/keyboard.test.tsx`: 20 consecutive runs, 34 tests each;
- `go vet ./...`;
- `go test ./...`;
- `go test -race ./internal/...`;
- `make test-e2e`; and
- detached fresh-checkout `make build` with no preparatory dependency install.

The fresh build installed its own 370 frontend packages and produced:

- `web/dist`: 3,568 KiB;
- `coldstore`: 18,187,688 bytes.

This sandbox denies esbuild's npm postinstall self-exec even with scoped escalation. Setting
`npm_config_ignore_scripts=true` allowed the target's own `npm ci` to install the correct
platform package, after which the normal TypeScript/Vite and Go build completed.

Two tiers remain unverified here:

- Playwright was attempted against isolated roots and unused ports normally and with scoped
  escalation; the daemon could not bind loopback.
- `make test-e2e-live` was attempted with three alternate ports and scoped escalation; its
  bind preflight failed before Docker startup.

These are execution-environment blockers, not observed product failures. Keep their tasklist
boxes open until the exact tiers run on a host that permits local listeners.

## Production environment

The host-specific ignored documents contain private identifiers and are not reproduced here.
The operational facts needed for planning are:

- Debian 12 on an HP N40L with two AMD K10 cores, 3.8 GiB RAM and configured swap;
- default `GOAMD64=v1`; v2/v3 binaries are not supported by this CPU;
- Go 1.27 and Node 24 are installed, so the host can run `make build` directly;
- hot folders live on the root filesystem; cold storage is a dedicated btrfs RAID1;
- `thumbnail_workers=1` is intentional on this memory-constrained host;
- ffmpeg, SQLite, poppler-utils and ExifTool are installed;
- managed Syncthing folders were healthy, Send & Receive and protected by the required
  coldstore temp/superseded ignore patterns at the last owner-provided check;
- Syncthing file versioning is disabled and the installed Syncthing release still needs an
  owner-approved maintenance upgrade; and
- snapshot scheduling is unresolved. Do not claim it is configured or absent without owner
  confirmation.

A fresh SQLite `.backup` is a mandatory gate immediately before production schema changes.
The catalog cannot be reconstructed from hot folders because scans do not rediscover
already-offloaded archive rows.

## Access and authority

- Current access is the safe interim state: loopback + Basic Auth + SSH tunnel.
- The desired permanent path is browser-native HTTPS, supports guests and does not require a
  VPN client.
- Public ingress requires a selected domain/proxy trust boundary plus session authentication,
  CSRF defense, rate limiting and a private backend listener.
- The owner has not selected or authorized a topology. Do not implement provider-specific
  deployment or expose the raw port by assumption.
- Agents have no production-host authority. Ask for output and require separate approval for
  deployment, restarts, migrations, budget changes and archive/restore actions.

## Remaining engineering work

### Load-bearing

- Live Photo identification, archive/restore expansion, mover backstop and whole-group quota
  accounting are complete. The future rename job in #14 must preserve that group contract.
- Metadata package #1, schema #2 (migration 7) and extraction pass #3 are complete.

### Product and operations

- Select and threat-model permanent HTTPS ingress, then implement session auth only against
  that explicit topology.
- Run Playwright and the live Syncthing tier in a listener-capable environment.
- Benchmark PDF thumbnail generation on the actual two-core host from owner-provided output.
- Complete the owner-mediated backup, deployment, throwaway quota, snapshot and Syncthing
  upgrade gates in `TASKLIST.md`.
- Update GitHub issue bodies from planned migration 6/7 to 7/8 when network access exists.

## Metadata decisions already settled

- `internal/metadata` now implements the pure-Go bulk reader, HEIC cross-check, QuickTime
  reader and resident candidate-only ExifTool probe; repeated package tests, its race test,
  vet and the repository-wide Go test suite pass.
- `evanoberholster/imagemeta` is the primary still-photo reader. Real iPhone HEIC GPS,
  capture time, offset and subseconds matched ExifTool.
- Re-probe suspicious/empty HEIC results with `bep/imagemeta`; it covers the verified
  `iloc`-before-`iinf` failure mode. Apply GPS reference signs explicitly.
- No pure-Go reader exposes Apple's offset-based maker-note ContentIdentifier. Use a
  resident `exiftool -stay_open` subprocess only for candidate Live Photo stills.
- Do not import `barasher/go-exiftool`; its GPL-3.0 license would change the binary's
  licensing posture.
- Use `ffprobe` for QuickTime/video metadata, not still-photo EXIF.
- Preserve owner-entered capture/location values across rescans and extraction retries.
- Issue #9 must benchmark `LIKE` at representative scale before accepting the no-FTS5 claim.
