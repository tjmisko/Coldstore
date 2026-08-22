# Status — coldstore closeout

Snapshot date: 2026-08-21 (America/Los_Angeles). Code state: `main` at merge commit
`5e1dc67`; PR #16 is merged. Production is now running the integrated release. The Git tree
and the owner-reported deployment evidence below are authoritative for handoff.

## Executive state

- Production was built from merged `main` at `5e1dc67`, installed, preflighted and restarted
  successfully. Startup migrated the catalog from schema 5 to schema 8.
- Immediately before migration, the owner stopped Coldstore, created a fresh SQLite
  `.backup` outside cold storage, verified `PRAGMA quick_check` and confirmed the backup's
  schema was 5. The private backup location is intentionally not tracked here.
- Production now uses private Tailscale Serve HTTPS plus Coldstore's exact-login Tailscale
  authentication. The backend remains on `127.0.0.1:8090`, the temporary Basic credentials
  were removed from the service configuration/environment, and Funnel remains disabled.
- Owner verification passed: the service stayed active, the schema marker reported 8, the
  private URL loaded without a Basic prompt, and direct loopback access failed closed without
  a Serve identity.
- The integrated release includes keymap, document-preview and tagging work. Document
  preview owns migration 6, metadata/tagging owns migration 7, and durable rename jobs own
  migration 8; production and the current binary now agree on schema 8.
- The folder-label identity guard, Live Photo atomicity, metadata/tagging/search surfaces and
  durable rename jobs are deployed after passing the full PR verification matrix.
- PR #16 merged with all six GitHub Actions jobs green. Issue-body migration-number
  bookkeeping remains separate follow-up work.

## Integrated repository state

The merged `main` branch carries:

- identity/title/icon/PWA assets and the dark token system;
- BUG-1 touch selection and BUG-2 mobile heading repairs;
- one scoped keymap dispatcher, operator-pending grammar, palette, finder, context menu and
  generated cheatsheet;
- the keyboard handler publication fix and centralized overlay/toast layering;
- schema migration 6, content sniffing, PDF/EPUB/text previews and PDF/EPUB thumbnails;
- finder-owned preview/open/select/download/reveal workflows;
- permanent folder-label/path ownership validation before reconciliation writes;
- `coldstore --status [--json]`, using immutable/query-only catalog inspection;
- migration 7 metadata extraction, tags, descriptions, capture/location fields and cosmetic
  folder display names;
- extended literal search/filtering, mouse and keyboard tagging, exact bulk undo/redo,
  per-folder quick slots and the dedicated culling scope;
- content-ID Live Photo grouping, globally ordered pair-aware quota selection, compensating
  archive/restore rollback and split warnings; and
- migration 8 durable file/directory rename jobs with group expansion, restart recovery,
  scanner exclusion, descriptor-relative path safety and atomic no-replace commits.

Key integration commits are listed in `HANDOFF.md`. The document and tagging-design merges
remain true two-parent merge commits (`752c210` and `9b5be84`).

## Verified test evidence

Passed on the integrated branch:

- `make lint` with zero warnings;
- `make test-web`: 31 files, 334 tests;
- 20 consecutive runs of the eight keyboard/tagging adversarial files, 147 tests each
  (2,940 test executions);
- 20 consecutive runs of the Gallery pending-context-motion regression after repairing the
  page-replay lifecycle;
- `go vet ./...`;
- `go test ./...`;
- `go test -race ./internal/...`;
- `make test-e2e`;
- `make build` after the final code integration;
- the earlier detached fresh-checkout build with no preparatory dependency install;
- GitHub Actions embedded-UI Playwright against the real daemon; and
- GitHub Actions live E2E with two Syncthing devices and the containerized daemon.

The final integrated build produced:

- `web/dist`: 3,624 KiB;
- `coldstore`: 21,483,024 bytes.

The earlier fresh-checkout target installed its own 370 frontend packages. This sandbox
denies esbuild's npm postinstall self-exec even with scoped escalation; setting
`npm_config_ignore_scripts=true` let `npm ci` install the correct platform package, after
which the normal TypeScript/Vite and Go build completed.

The local sandbox still cannot bind the listeners those last two tiers require. GitHub
Actions supplied the missing listener-capable environment: both jobs and the corrected
frontend tier passed on PR #16 at `16a7bf6`, so their tasklist boxes are closed.

## Production environment

The host-specific ignored documents contain private identifiers and are not reproduced here.
The operational facts needed for planning are:

- Debian 12 on an HP N40L with two AMD K10 cores, 3.8 GiB RAM and configured swap;
- default `GOAMD64=v1`; v2/v3 binaries are not supported by this CPU;
- Go 1.27 and Node 24 are installed, so the host can run `make build` directly;
- hot folders live on the root filesystem; cold storage is a dedicated btrfs RAID1;
- the deployment preflight confirmed cold storage was an actual btrfs mount with roughly
  2 TB free, and the owner's privileged btrfs device check reported no errors;
- `thumbnail_workers=1` is intentional on this memory-constrained host;
- ffmpeg, SQLite, poppler-utils and ExifTool are installed;
- managed Syncthing folders were healthy, Send & Receive and protected by the required
  coldstore temp/superseded ignore patterns at the last owner-provided check;
- Syncthing file versioning is disabled and the installed Syncthing release still needs an
  owner-approved maintenance upgrade; and
- snapshot scheduling is unresolved. Do not claim it is configured or absent without owner
  confirmation.

A fresh SQLite `.backup` remains mandatory immediately before every future production schema
change. The 5-to-8 deployment satisfied that gate. The catalog cannot be reconstructed from
hot folders because scans do not rediscover already-offloaded archive rows.

The owner notes that the runtime `require_mountpoint` option had previously been removed.
This update does not claim that it or systemd `RequiresMountsFor=` was re-enabled; reconcile
and document those two distinct mount-safety controls separately before considering that
deployment task closed.

## Access and authority

- Current production application auth is an exact allowlist of Tailscale Serve login
  identities. The identity-aware build is deployed and the temporary Basic credentials are
  absent from the service configuration/environment.
- Tailscale Serve HTTPS is enabled on the server. The backend remains loopback-only, direct
  access without a Serve identity returns 401, and the owner successfully loaded the private
  URL without a Basic prompt.
- The v1 implementation refuses non-loopback binding, rejects missing/unlisted headers and
  cross-origin browser mutations, and attributes accepted mutations in the journal. Keep
  Funnel disabled and the raw listener private.
- An authenticated remote owner receives the same full web UI/API as at home, including all
  existing mutating operations. No read-only remote owner role is planned for v1.
- Two co-owners will use separate Tailscale identities, allowing per-person attribution and
  revocation. The old Basic credential may remain in the owner's password manager as rollback
  material, but it is not active and is not shared for the new onboarding.
- The second co-owner is not yet onboarded and will use a separate Tailscale account. The
  selected least-privilege path is a revocable share of the server machine, restricted to
  TCP 443 and the Coldstore Serve proxy. SSH, Syncthing, raw port 8090 and every other server
  service remain out of scope. `onboarding.md` is the runbook.
- Owner-provided preflight on 2026-08-21 confirmed Tailscale 1.102.3, the initial absence of
  Serve/Funnel configuration and the expected loopback Basic challenge. The owner then
  reviewed the restricted tailnet policy, enabled Serve and verified the private HTTPS URL.
- Public-domain, zero-install guest access is deferred. If selected later, it requires a
  domain/proxy trust boundary, session authentication, app-enforced authorization, CSRF
  defense and rate limiting.
- Agents have no production-host authority. Ask for output and require separate approval for
  deployment, restarts, migrations, budget changes and archive/restore actions.

## Remaining engineering work

### Load-bearing

- Implementation issues #1–#14 are deployed. The remaining tagging follow-up is XMP export
  into cold storage; it still needs a separate GitHub issue when connectivity exists.
- The 5-to-8 migration and identity rollout are complete. No remaining engineering item
  authorizes another service restart or filesystem operation against production.

### Product and operations

- Complete second-co-owner onboarding and cross-platform/off-LAN verification using
  `onboarding.md`. Public guest sessions remain deferred.
- Reconcile and record the intentionally removed mountpoint setting versus the separate
  systemd mount dependency; do not infer either from the successful one-time rollout.
- Benchmark PDF thumbnail generation on the actual two-core host from owner-provided output.
- Complete the owner-mediated throwaway quota, snapshot and Syncthing upgrade gates in
  `TASKLIST.md`.
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
- Issue #9's 50,000-row ARM64 benchmark measured 14.0–47.9 ms for extended `q`,
  tag/description filtering and ID resolution, below the documented 100 ms FTS5 threshold.
