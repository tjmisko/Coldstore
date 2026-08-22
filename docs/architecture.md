# Coldstore Architecture

## Overview

Coldstore is a daemon that enforces per-folder size budgets on syncthing directories by moving old/unused files to local cold storage, freeing space across all synced devices. A web UI provides a unified view of all files (synced + archived) with gallery browsing and restore capabilities.

## How It Works

1. Syncthing folders sync normally across devices
2. Each folder has a size budget (e.g., Camera Roll: 20GB)
3. When a folder exceeds budget, the daemon moves oldest/least-accessed files to cold storage (`/srv/coldstore/archive/`)
4. Syncthing detects the deletions and propagates them -- files disappear from phone/laptop
5. Web UI shows ALL files. User can "restore to sync" (re-syncs to all devices) or "download now" (direct to browser)

**Offload priority:** `score = max(mtime, atime)` -- lowest score offloaded first. Configurable persistent subfolder patterns are exempt.

**Syncthing safety:** Only move files on the filesystem; syncthing's watcher detects changes naturally. Grace period (30 min default) prevents racing with active transfers.

## Stack

- **Go** -- single binary, daemon + HTTP server, embedded React build
- **React + TypeScript + Vite** -- gallery UI, file browser, config panel
- **SQLite** -- file catalog (synced vs. archived state, metadata, thumbnail paths)

## Project Structure

```
coldstore/
  cmd/coldstore/main.go          # Entry point
  internal/
    config/config.go             # TOML config parsing
    daemon/
      daemon.go                  # Main loop: quota enforcement + catalog sync
      scanner.go                 # Walk syncthing folders, gather metadata
      mover.go                   # THE serialisation point: every status, pin and move
      move.go                    # moveFile / crossDeviceMove -- the move primitive
      folder.go                  # FolderReady: the .stfolder guard before any move
      offloader.go               # Pure filesystem: archive paths + move out
      restorer.go                # Pure filesystem: move in + prune empty archive dirs
    catalog/
      catalog.go                 # SQLite operations
      migrations.go              # Schema
      models.go                  # FileRecord, FolderConfig structs
      readonly.go                # Immutable/query-only diagnostic open; never migrates
      diagnostics.go             # Read-only derived-data health aggregates
    diagnostic/
      report.go                  # Offline health model and severity/exit policy
      render.go                  # Human and redacted JSON renderers
      storage.go                 # Best-effort redacted btrfs membership probe
    thumbnail/thumbnail.go       # Image/video thumbnail generation
    api/
      server.go                  # HTTP server, middleware, static serving
      static.go                  # Embedded frontend serving
      routes.go                  # Route registration
      handlers_files.go          # File listing, search, filtering
      handlers_moves.go          # Restore + archive
      handlers_pins.go           # Pin, unpin, bulk pin
      handlers_config.go         # Folder config CRUD
      handlers_status.go         # Dashboard stats
      handlers_test.go           # Unit tests for API handlers
      dist/                      # Embedded React build output
  tests/
    e2e/                         # End-to-end tests (Docker-based)
    fixtures/                    # Test fixture files (images, video)
  docker/
    compose.test.yml             # Docker Compose for e2e test environment
    Dockerfile.test              # Test container image
  web/                           # React frontend (Vite + TS)
  docs/                          # Documentation
  coldstore.toml.example         # Example config
  coldstore.service              # systemd unit
  Makefile                       # build, dev, install, test
```

`coldstore --status` branches before normal environment preflight. It uses an
inspection-only config load (no service credential environment and no write probes),
checks the on-disk schema before schema-dependent queries, and opens compatible catalogs
immutable/query-only. A non-empty WAL is refused rather than ignored or checkpointed.
Diagnostic wire types intentionally omit credentials, Syncthing IDs, event details/paths
and job error strings.

## Storage Infrastructure

**Hardware:** 4x 1.8TB drives in the Proliant Microserver. `sda` is the OS drive (Debian). `sdb`, `sdc`, `sdd` form a ZFS raidz1 pool.

**ZFS pool** (`coldpool`, mounted at `/srv/coldstore`):
- ~3.6TB usable with single-drive fault tolerance
- Checksumming (detects bit rot), LZ4 compression, snapshots

### Cold Storage Layout

```
/srv/coldstore/
  archive/
    CameraRoll/2024/01/IMG_1234.jpg    # Mirrors syncthing folder structure
    Books/subfolder/book.pdf
  thumbnails/
    CameraRoll/2024/01/IMG_1234.jpg.thumb.jpg
  catalog.db
```

Original relative paths are preserved exactly, so restore = move back to `{syncthing_path}/{relative_path}`.

## Database Schema

**`files`** -- one row per known file (synced or archived):
- `folder_label`, `relative_path` (unique together), `filename`, `extension`, `mime_type`
- `size_bytes`, `file_mtime`, `first_seen`, `archived_at`
- `status`: `synced` | `archiving` | `archived` | `restoring` | `missing`
- `thumbnail_path`, `content_hash` (SHA-256, computed lazily)
- `pinned`, `pinned_at`, `pin_source` (`""` | `user` | `restore`) -- a pinned file is
  never an offload candidate, whatever its age (D2). A restore pins the file it brought
  back, which is what stops the next quota tick taking it straight away again

**`folder_configs`** -- per-folder settings, and the runtime source of truth for
both folder policy and folder paths:
- `folder_label` -- permanent identity used by file rows, policy and archive paths.
  Reconciliation rejects a cleaned path or non-empty Syncthing ID already catalogued
  under a different label, including rows no longer present in TOML
- `syncthing_id`, `syncthing_path` -- refreshed from `coldstore.toml` by startup
  reconciliation; the path may change while `folder_label` stays unchanged. Non-empty
  Syncthing IDs must be unique across declarations
- `budget_bytes`, `grace_minutes`, `enabled` -- owned by the UI; TOML only seeds them
  when the row is created
- `present_in_toml` -- 0 once a folder stops being declared. The row is kept, its files
  stay browsable, and it drops out of scanning and enforcement
- `last_enforced_at`, `wedge_warned_at` -- enforcement bookkeeping

`folder_label` is deliberately not a cosmetic name. The metadata schema's
`display_name` is the cosmetic alternative once that field is available; versions that
do not expose it have no supported label-rename operation.

**`persistent_paths`** -- glob patterns for never-offload rules per folder

**`events`** -- audit log, readable through `GET /api/v1/events`. `class=warning`
excludes routine `archive`, `restore`, `folder_seeded` and `scan_completed` rows;
failed and incomplete scans remain warnings.

## API Endpoints

```
GET    /api/v1/files?folder=X&status=Y&ext=Z&q=search&pinned=1&pin_source=user
                     &ids=1,2,3&sort=mtime&offset=0&limit=50
GET    /api/v1/files/ids?folder=X&status=Y&...   # {ids, total, truncated}, capped at 20000
GET    /api/v1/files/{id}/download
GET    /api/v1/files/{id}/preview                # inline PDF/image/video/text; Range capable
GET    /api/v1/files/{id}/epub                   # ordered title/spine metadata
GET    /api/v1/files/{id}/epub/{resource...}     # allowlisted, size-capped book resources
POST   /api/v1/files/{id}/restore
POST   /api/v1/files/{id}/archive
POST   /api/v1/files/{id}/pin
DELETE /api/v1/files/{id}/pin
POST   /api/v1/files/pin                      # bulk: {"ids": [...], "pinned": bool}
POST   /api/v1/files/{id}/resolve             # {"keep": "device"|"archive"} -- section 4.1b conflicts
GET    /api/v1/thumbnails/{folder}/{path}.thumb.jpg
GET    /api/v1/folders
PUT    /api/v1/folders/{label}
GET    /api/v1/folders/{label}/persistent
POST   /api/v1/folders/{label}/persistent
DELETE /api/v1/folders/{label}/persistent/{id}
POST   /api/v1/jobs                           # {action, ids?, folder?, force?} -> 202
GET    /api/v1/jobs?limit=20                  # newest first, without the failure array
GET    /api/v1/jobs/{id}                      # job detail + failed[] (first 100)
GET    /api/v1/events?folder=X&type=Y&class=warning&limit=50
GET    /api/v1/status
POST   /api/v1/scan                           # 202 {job_id, action, state, total}
```

Every other path under `/api/` answers `404 {"error": "no such endpoint"}` as JSON. The
embedded SPA is served only from paths that are not API paths, so a mistyped or
unimplemented endpoint can never come back as `200 text/html` for a client to choke on.

`GET /files` pages at 1-500 records (`limit` outside that range is a `400`, above it is
clamped); the `ids` parameter is not paginated at all and returns exactly the requested
existing rows in id order, capped at 500 ids per request.

### The job queue

Bulk restore and bulk archive go through `jobs` / `job_items` rather than the request
path, so a selection of hundreds of files does not hold an HTTP connection open for the
length of the moves. `POST /api/v1/jobs` validates, commits the job, pokes the worker's
wake channel and returns `202` with a job id; the UI polls `GET /api/v1/jobs/{id}`.

- One worker goroutine, started by the daemon after its initial scan and stopped by the
  same context that stops everything else. Items are processed serially: parallelism
  would only contend for the `Mover` mutex.
- The worker uses the SAME `Mover` as the quota tick. That mutex, the compare-and-swap
  in each mover entry point, `pinned = 1` on restore and `enforceFolder` re-querying
  `FolderSizes` per candidate are the four mechanisms that stop a bulk restore and
  automatic enforcement fighting over one file.
- `state`: `pending -> running -> done | failed`. `failed` means **partial success with
  at least one failure**, not "nothing happened" -- `done` and `failed_count` tell the
  whole story. On any terminal job `done == total` and `failed_count <= done`.
- A job left `running` by a kill is re-queued at startup (`ResumeInterruptedJobs`, after
  the file-level recovery). A resumed item cannot double-move anything: `ArchiveByID`
  and `RestoreByID` are no-op successes when the file is already where the item wants it.
- `POST /api/v1/scan` enqueues a `scan` job (`total = 1`, no items). Scans take the
  scanner's mutex with `TryLock`, so the daemon's catalog tick and a scan job can never
  walk the same tree at once.
- Terminal jobs are purged after seven days, at startup and on each catalog-sync tick.

A restore that would push a folder past its budget is refused at enqueue with `409` and
the numbers the UI needs to say what would be evicted; `"force": true` skips the check.
Separately, any enforcement pass that archives more than 100 files or more than 10% of a
folder writes one `bulk_eviction` event, whatever caused the pressure.

## Daemon Loops

**Quota enforcement** (every 15 min):
0. Read policy from `folder_configs` (`enabled = 1 AND present_in_toml = 1`), never from
   `coldstore.toml`, so a budget edited in the UI applies on the next tick with no restart
1. Walk each syncthing folder, sum sizes
2. If over budget: sort by `max(mtime, atime)` ascending, skip pinned files, persistent
   paths and grace-period files
3. Move files one by one through the `Mover` until under budget (cross-filesystem
   fallback: temp sibling + verify + rename + delete). The automatic path claims each
   row with `ClaimUnpinnedFile`, so a pin that lands after the candidate query still
   wins
4. Whatever the pass did or did not do, the epilogue runs: it recomputes the folder's
   budget picture, stamps `last_enforced_at`, and latches or clears the wedge warning

**Catalog sync** (every 30 min):
1. Walk syncthing folders, reconcile with DB (new files -> `synced`, missing files -> `missing`, re-appeared archived files -> `synced`)
2. Enqueue thumbnail generation for new image/video files (2-worker pool)

## Thumbnail Pipeline

Categories are registered in ONE place, `extCategories` in
`internal/thumbnail/thumbnail.go`. `CanThumbnail`, `SupportedExtensions` and
`Generate`'s dispatch switch all read from it, and the daemon asks
`SupportedExtensions()` for the list the queue selects on rather than keeping a
copy -- a second hand-maintained list is how a format ends up supported by the
generator and never selected by the queue.

- **Images** (`.jpg`, `.jpeg`, `.png`, `.webp`, `.gif`): `imaging` library, 400px longest edge, JPEG q80. Pure Go, no external binary.
- **HEIF/HEIC** (`.heif`, `.heic`): `ffmpeg` decodes to PNG on stdout, then the same Go resize path. Requires `ffmpeg` on `PATH`.
- **Videos** (`.mp4`, `.mov`, `.avi`, `.mkv`, `.webm`): `ffmpeg -vf "thumbnail,scale=400:-1" -frames:v 1`.
- **PDFs** (`.pdf`): `pdftocairo -jpeg -singlefile -scale-to 400 -f N -l N in.pdf -`, then the same Go resize path. Requires `poppler-utils` on `PATH`. **Not `pdftoppm`** -- it cannot write to stdout: given `-` it creates a file called `-.jpg`.
- **EPUBs** (`.epub`): the declared cover, extracted over `archive/zip` and `encoding/xml` in `internal/epub`. Pure Go, no external binary.
- **Everything else**: no thumbnail. The UI shows a file-type icon.

**First page with content.** The PDF path renders page 1, measures ink coverage
on a stride with a 3% border margin ignored, and tries pages 2 and 3 if the page
is effectively blank -- then keeps page 1 regardless. All three renders share one
deadline. A document of blank pages is a legitimate document and never fails over
this. Coverage rather than standard deviation: the two disagree on gradient
covers, and coverage is the one that behaves on scans.

**Content sniffing.** `extension` and `mime_type` both come from the filename, so
neither can classify a file that has no extension. `files.detected_type` (schema
6) holds what a bounded magic-number read of the file's leading bytes says it is,
filled in by the scanner on insert and whenever mtime or size changes. The
thumbnail queue matches on `extension IN (...) OR detected_type IN (...)`, which
is the only way a bookmark export called `reading` is ever reached.

## Key Edge Cases

- **Cross-filesystem moves:** `os.Rename` fails with `EXDEV` -> copy into a `.coldstore-tmp` sibling, fsync, re-read and hash it from disk, restore mode/mtime/atime, rename into place, then re-stat and delete the source. Nothing partial is ever visible at the destination path, and an occupied destination is refused rather than overwritten.
- **Race with active sync:** 30-min grace period + re-check mtime before moving
- **Re-sync after offload:** Another device re-syncs a file before deletion propagates -> catalog sync detects it, updates status to `synced`
- **Cold storage disk full:** Check available space before each offload, pause and write
  one deduped `cold_storage_low` event per folder per hour if < 5%
- **Pinned bytes exceed the budget (D3):** the budget is a hard cap on the on-device
  footprint and pins count against it, so a folder can be pinned past its budget. The
  daemon offloads every unpinned eligible file, then writes ONE `folder_wedged` event
  latched in `folder_configs.wedge_warned_at` -- not one per tick, and the latch survives
  restarts. It refreshes daily while the condition holds and clears with a
  `folder_unwedged` event when it resolves. `GET /api/v1/status` reports `pinned_bytes`
  (a subset of `synced_size_bytes`, never an addend), `over_budget` and `wedged` per
  folder
- **Unmounted folder root:** an unmounted mountpoint is an empty directory that
  `os.Stat` accepts. `FolderReady` requires Syncthing's `.stfolder` marker before any
  move, so a restore cannot materialise 400 files onto the root filesystem
- **First startup:** Catalog all existing files, generate thumbnails. No offloading until first full scan completes.

## Configuration

See [coldstore.toml.example](../coldstore.toml.example) for the full config format.

Key settings:
- `[storage].cold_storage_path` -- where archived files go
- `[daemon].grace_period_minutes` -- don't offload files modified within this window
- `[[folders]]` -- one entry per syncthing folder with label, path, budget, and persistent patterns

## Implementation History

The project was built in six phases:

1. **Skeleton + Catalog** -- Go module, config parsing, SQLite catalog, filesystem scanner
2. **Daemon + Offloading** -- Quota enforcement, file offloader/restorer, catalog reconciliation
3. **API Server** -- HTTP server with file listing, download, restore, config CRUD
4. **Thumbnails** -- Image/video thumbnail generation pipeline
5. **React Frontend** -- Gallery view, file browser, config panel, status bar
6. **Polish + Deploy** -- Embed React in Go binary, structured logging, systemd deployment
