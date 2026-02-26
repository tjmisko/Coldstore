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

## Phase 2: Daemon + Offloading [DONE]

- [x] Offloader: move files from syncthing dir to cold storage archive
  - Cross-filesystem fallback (copy + SHA-256 verify + delete)
  - Preserve relative path structure in archive
- [x] Quota enforcement loop in daemon
  - Sort by mtime ascending (oldest first)
  - Skip persistent paths and grace-period files
  - Re-check mtime on disk before each move
  - Move files until folder is under budget
- [x] Restorer: move files from cold storage back to syncthing dir
  - Status transitions: archived -> restoring -> synced
  - Clean up empty parent directories in archive after restore
- [x] Disk space checks before offloading (pause if < 5% free)
- [x] Signal handling, graceful shutdown
- [x] Tests: offloader/restorer round-trip, cross-fs copy, quota ordering, grace period, persistent paths

## Phase 3: API Server [DONE]

- [x] HTTP server with `net/http` (Go 1.22+ routing)
- [x] File listing with filtering (folder, status, extension, search), sorting, pagination
- [x] Download endpoint (serves from syncthing dir or cold storage based on status)
- [x] Restore endpoint (triggers move back to syncthing)
- [x] Folder config CRUD endpoints
- [x] Persistent path management (list, add, delete)
- [x] Status/dashboard endpoint (file counts, folder sizes, cold storage free space)
- [x] API server wired into daemon main.go (runs in background goroutine)
- [x] Tests: httptest-based handler tests for all endpoints

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
