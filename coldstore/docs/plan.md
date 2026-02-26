# Coldstore Implementation Plan

See [architecture.md](architecture.md) for the full system design.

## Phase 1: Skeleton + Catalog [DONE]

- [x] Go module, project structure, Makefile
- [x] TOML config parsing with human-readable budget strings
- [x] SQLite catalog with schema (files, folder_configs, persistent_paths, events)
- [x] Full CRUD operations on all catalog tables
- [x] Filesystem scanner: walks syncthing folders, populates catalog, detects missing files
- [x] CLI one-shot scan mode (`--scan` flag)
- [x] Tests: config parsing, catalog CRUD, scanner filtering/detection
- [x] systemd unit file, example config

## Phase 2: Daemon + Offloading

- [ ] Offloader: move files from syncthing dir to cold storage archive
  - Cross-filesystem fallback (copy + verify hash + delete)
  - Preserve relative path structure in archive
- [ ] Quota enforcement loop in daemon
  - Sort by `max(mtime, atime)` ascending
  - Skip persistent paths and grace-period files
  - Move files until folder is under budget
- [ ] Restorer: move files from cold storage back to syncthing dir
- [ ] Catalog reconciliation: detect re-appeared files, update statuses
- [ ] Disk space checks before offloading (pause if < 5% free)
- [ ] Signal handling improvements, graceful shutdown
- [ ] Tests: offloader cross-fs logic, quota enforcement ordering, reconciliation

## Phase 3: API Server

- [ ] HTTP server with `net/http` (Go 1.22+ routing)
- [ ] File listing with filtering, sorting, pagination
- [ ] Download endpoint (serve from syncthing dir or cold storage)
- [ ] Restore endpoint (trigger move back to syncthing)
- [ ] Folder config CRUD endpoints
- [ ] Status/dashboard endpoint (storage usage, file counts)
- [ ] Tests: handler tests with httptest

## Phase 4: Thumbnails

- [ ] Image thumbnails (imaging library, 400px, JPEG q80)
- [ ] Video frame extraction (ffmpeg)
- [ ] PDF first-page rendering (pdftoppm)
- [ ] Worker pool integrated with catalog sync
- [ ] Thumbnail serving endpoint

## Phase 5: React Frontend

- [ ] Vite + React + TypeScript project setup
- [ ] Gallery view with infinite scroll (Camera Roll)
- [ ] File browser tree/list view (Books/Documents)
- [ ] File detail modal with restore/download actions
- [ ] Config panel for budgets and persistent paths
- [ ] Status bar with storage usage

## Phase 6: Polish + Deploy

- [ ] Embed React build in Go binary (`//go:embed`)
- [ ] Structured logging with `slog`
- [ ] Makefile install target
- [ ] Deploy to microserver, enable systemd unit

## Storage Setup (Manual, One-Time)

Requires sudo on the microserver:

```bash
apt install zfsutils-linux
zpool create coldpool raidz /dev/sdb /dev/sdc /dev/sdd
zfs set mountpoint=/srv/coldstore coldpool
zfs set compression=lz4 coldpool
chown tjmisko:tjmisko /srv/coldstore
```
