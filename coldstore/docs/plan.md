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

## Phase 4: Thumbnails [DONE]

- [x] Image thumbnails (JPEG/PNG/WebP via imaging library, 400px, JPEG q80)
- [x] HEIF/HEIC support via ffmpeg decoding
- [x] Video frame extraction via ffmpeg
- [x] Worker pool integrated with daemon catalog sync (configurable worker count)
- [x] Thumbnail serving endpoint
- [x] Generates for both synced and archived files
- [x] Tests: CanThumbnail, JPEG/PNG generation, ProcessQueue worker pool

## Phase 5: React Frontend [DONE]

- [x] Vite + React + TypeScript + Tailwind CSS (dark mode only)
- [x] App shell with responsive sidebar (folders + view modes)
- [x] Gallery view with infinite scroll, thumbnail grid
- [x] Full-screen lightbox with prev/next, metadata panel, restore/download
- [x] Keyboard nav: arrows, Escape, Space (toggle info), PageUp/Down, Home/End
- [x] Touch/swipe support: horizontal swipe prev/next, swipe down to close
- [x] File browser table view (auto-selected for non-image folders)
- [x] Config panel with budget display and persistent path editor
- [x] Status bar with per-folder usage bars, cold storage free space
- [x] Mobile responsive with sidebar overlay on small screens
- [x] Dev proxy to Go server on :8090

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
