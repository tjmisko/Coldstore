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
      offloader.go               # Move files syncthing -> cold storage
      restorer.go                # Move files cold storage -> syncthing
    catalog/
      catalog.go                 # SQLite operations
      migrations.go              # Schema
      models.go                  # FileRecord, FolderConfig structs
    thumbnail/thumbnail.go       # Image/video/PDF thumbnail generation
    syncthing/client.go          # Syncthing REST API client
    api/
      server.go                  # HTTP server, middleware, static serving
      routes.go                  # Route registration
      handlers_files.go          # File listing, search, filtering
      handlers_restore.go        # Restore + download
      handlers_config.go         # Folder config CRUD
      handlers_status.go         # Dashboard stats
  web/                           # React frontend (Vite + TS)
  docs/                          # Documentation
  coldstore.toml.example         # Example config
  coldstore.service              # systemd unit
  Makefile                       # build, dev, install, test
```

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
- `status`: `synced` | `archived` | `restoring` | `missing`
- `thumbnail_path`, `content_hash` (SHA-256, computed lazily)

**`folder_configs`** -- per-folder settings:
- `folder_label`, `syncthing_id`, `syncthing_path`, `budget_bytes`, `grace_minutes`, `enabled`

**`persistent_paths`** -- glob patterns for never-offload rules per folder

**`events`** -- audit log of archive/restore/error events

## API Endpoints

```
GET    /api/v1/files?folder=X&status=Y&ext=Z&q=search&sort=mtime&offset=0&limit=50
GET    /api/v1/files/{id}/download
POST   /api/v1/files/{id}/restore
GET    /api/v1/thumbnails/{folder}/{path}.thumb.jpg
GET    /api/v1/folders
PUT    /api/v1/folders/{label}
GET    /api/v1/folders/{label}/persistent
POST   /api/v1/folders/{label}/persistent
DELETE /api/v1/folders/{label}/persistent/{id}
GET    /api/v1/status
```

## Daemon Loops

**Quota enforcement** (every 15 min):
1. Walk each syncthing folder, sum sizes
2. If over budget: sort by `max(mtime, atime)` ascending, skip persistent paths and grace-period files
3. Move files one by one until under budget (cross-filesystem fallback: copy + verify + delete)
4. Update catalog after each move

**Catalog sync** (every 30 min):
1. Walk syncthing folders, reconcile with DB (new files -> `synced`, missing files -> `missing`, re-appeared archived files -> `synced`)
2. Enqueue thumbnail generation for new image/video/PDF files (2-worker pool)

## Thumbnail Pipeline

- **Images** (JPEG, PNG, WebP, HEIF): `imaging` library, 400px longest edge, JPEG q80
- **Videos** (MP4, MOV): `ffmpeg -vf "thumbnail,scale=400:-1" -frames:v 1`
- **PDFs**: `pdftoppm -jpeg -f 1 -singlefile -scale-to 400`
- Other types: no thumbnail, UI shows file-type icon

## Key Edge Cases

- **Cross-filesystem moves:** `os.Rename` fails with `EXDEV` -> copy + size verify + delete
- **Race with active sync:** 30-min grace period + re-check mtime before moving
- **Re-sync after offload:** Another device re-syncs a file before deletion propagates -> catalog sync detects it, updates status to `synced`
- **Cold storage disk full:** Check available space before each offload, pause + warn if < 5%
- **First startup:** Catalog all existing files, generate thumbnails. No offloading until first full scan completes.

## Configuration

See [coldstore.toml.example](../coldstore.toml.example) for the full config format.

Key settings:
- `[storage].cold_storage_path` -- where archived files go
- `[daemon].grace_period_minutes` -- don't offload files modified within this window
- `[[folders]]` -- one entry per syncthing folder with label, path, budget, and persistent patterns

## Implementation Phases

1. **Skeleton + Catalog** (Phase 1) -- Go module, config parsing, SQLite catalog, filesystem scanner
2. **Daemon + Offloading** (Phase 2) -- Quota enforcement, file offloader/restorer, catalog reconciliation
3. **API Server** (Phase 3) -- HTTP server with file listing, download, restore, config CRUD
4. **Thumbnails** (Phase 4) -- Image/video/PDF thumbnail generation pipeline
5. **React Frontend** (Phase 5) -- Gallery view, file browser, config panel, status bar
6. **Polish + Deploy** (Phase 6) -- Embed React in Go binary, structured logging, systemd deployment
