# Coldstore Deployment Runbook

## Prerequisites

- **Go 1.22+** (the module requires 1.25.7; any 1.22+ toolchain will work)
- **Node 20+** and npm (for building the React frontend)
- **ffmpeg** -- required for HEIF/HEIC decoding and video thumbnail extraction
- **Syncthing** -- running and configured with the folders you want to manage
- **SQLite 3** -- linked via `go-sqlite3` (CGo; a C compiler is required at build time)
- **ZFS** (optional) -- recommended for the cold storage volume; see the ZFS section below

## Building

```bash
# Build frontend + Go binary in one step
make build

# Or install directly to /usr/local/bin
make install        # runs make build, then installs the binary
```

`make build` does two things in sequence:

1. Builds the React app (`cd web && npm run build`) and copies the output into `internal/api/dist/`.
2. Compiles the Go binary from `cmd/coldstore/`.

The resulting `coldstore` binary embeds the frontend -- no separate static file serving is needed.

### Development

```bash
make dev            # go run with local coldstore.toml
make dev-web        # React dev server with hot reload (port 5173)
make test           # go test ./...
```

## Configuration

Coldstore reads a TOML file (default: `coldstore.toml` in the working directory, overridden with `--config`).

Copy the example to get started:

```bash
cp coldstore.toml.example /etc/coldstore/coldstore.toml
```

### Full field reference

#### `[storage]`

| Field | Type | Default | Description |
|---|---|---|---|
| `cold_storage_path` | string | `"/srv/coldstore"` | Root directory for the archive, catalog database, and thumbnails. |

#### `[daemon]`

| Field | Type | Default | Description |
|---|---|---|---|
| `scan_interval_minutes` | int | `15` | How often quota enforcement runs. Each tick checks every folder against its budget and offloads the oldest files that exceed it. |
| `catalog_sync_interval_minutes` | int | `30` | How often a full filesystem scan runs to reconcile the catalog with what is actually on disk. Also triggers thumbnail generation. |
| `thumbnail_workers` | int | `2` | Number of concurrent goroutines for thumbnail generation. Increase on machines with fast storage and many CPU cores; keep low if ffmpeg contention is a concern. |
| `grace_period_minutes` | int | `30` | Files whose most recent activity (the later of mtime and atime) is within this window are never offloaded, even if the folder is over budget. Protects files that are actively being synced or viewed. |

#### `[server]`

| Field | Type | Default | Description |
|---|---|---|---|
| `listen_addr` | string | `"0.0.0.0:8090"` | Address and port for the web UI and REST API. |
| `username` | string | `""` | If non-empty, enables HTTP Basic Auth on all API routes. Both `username` and `password` must be set. |
| `password` | string | `""` | Password for HTTP Basic Auth. |

#### `[syncthing]` -- reserved, currently unused

> **Neither key does anything today.** `api_url` and `api_key` are parsed into
> `config.SyncthingConfig` and then never read: there is no Syncthing API client
> anywhere in the codebase (`rg -n 'Syncthing\.API' internal/` returns nothing outside
> `config.go`). Coldstore integrates with Syncthing purely through the filesystem -- it
> moves files in and out of the folder path and lets Syncthing's own watcher propagate
> the change. Setting these keys will not make coldstore talk to Syncthing, and leaving
> them unset breaks nothing.
>
> They are kept in the schema because a future version may query Syncthing for folder
> IDs, pause/resume state, or scan triggers. Treat them as reserved.

| Field | Type | Default | Status | Description |
|---|---|---|---|---|
| `api_url` | string | `""` | Reserved -- not read | Intended as the Syncthing REST API base URL (e.g., `http://127.0.0.1:8384`). |
| `api_key` | string | `""` | Reserved -- not read | Intended as the Syncthing API key (found in Syncthing's settings). |

#### `[[folders]]` (array of tables)

Each `[[folders]]` block defines a Syncthing folder to manage.

| Field | Type | Default | Description |
|---|---|---|---|
| `label` | string | required | Human-readable folder name. Used as the top-level directory name inside the archive and thumbnails trees. Must be unique across all folders. |
| `syncthing_id` | string | required | The Syncthing folder ID (e.g., `xxxxx-xxxxx`). |
| `path` | string | required | Absolute path to the Syncthing folder on disk. |
| `budget` | string | required | Maximum size of synced files before offloading begins. |
| `persistent` | string[] | `[]` | Glob patterns for files that must never be offloaded. |

### Zero means default, not zero

`config.setDefaults()` fills in a value for every `int` in `[daemon]` that comes out of
TOML parsing as `0`. Go cannot distinguish "the user wrote `0`" from "the key was
absent", so both take the default:

| Setting | Written as `0` | Actually used |
|---|---|---|
| `scan_interval_minutes` | `0` | `15` |
| `catalog_sync_interval_minutes` | `0` | `30` |
| `thumbnail_workers` | `0` | `2` |
| `grace_period_minutes` | `0` | `30` |

The one most likely to bite: **`grace_period_minutes = 0` does not disable the grace
period.** It silently becomes 30 minutes, so files touched in the last half hour stay
put and a folder that looks over budget will appear not to offload. If you want the
shortest possible grace period, write `grace_period_minutes = 1`. There is currently no
way to express "no grace period at all" through the config file.

The same rule applies to the string fields, though the consequences are milder:
`cold_storage_path = ""` becomes `/srv/coldstore` and `listen_addr = ""` becomes
`0.0.0.0:8090`.

`[[folders]].budget` is the exception -- it is **not** defaulted. A missing or empty
`budget` fails config loading outright (`folder "X": empty budget string`), as does an
unparseable one (`no recognized unit (B, KB, MB, GB, TB)`); either way the daemon exits
1 before binding a port. That is deliberate: a silently-defaulted budget would decide
how much of the user's data gets moved.

### Budget string format

Budgets are human-readable size strings parsed case-insensitively. Supported units:

| Unit | Multiplier |
|---|---|
| `B` | 1 |
| `KB` | 1,000 |
| `MB` | 1,000,000 |
| `GB` | 1,000,000,000 |
| `TB` | 1,000,000,000,000 |

The numeric part can be a float: `1.5TB`, `500MB`, `20GB` are all valid. Units use SI (base-10) multipliers, not binary.

### Persistent glob semantics

Persistent patterns use Go's `filepath.Match` semantics:

- `*` matches any sequence of non-separator characters. It does **not** cross directory boundaries.
- `?` matches any single non-separator character.
- `[abc]` matches any character in the set.
- Patterns are matched against the file's **relative path** within the folder.

Examples:

```toml
persistent = [
  "Albums/Starred/*",      # all files directly inside Albums/Starred/
  "Albums/Starred/*/*",    # one level deeper
  "important.txt",         # a specific file at the folder root
  "Docs/*.pdf",            # PDFs in the Docs/ directory only
]
```

To protect an entire subtree, you need a pattern for each depth level -- `*` alone will not recurse. Persistent patterns can also be added at runtime via the API; those are stored in the `persistent_paths` table in the catalog database.

## systemd setup

A unit file is provided at `coldstore.service`. Install it:

```bash
sudo cp coldstore.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now coldstore.service
```

The unit file expects:

- Binary at `/usr/local/bin/coldstore` (produced by `make install`)
- Config at `/etc/coldstore/coldstore.toml`
- `WorkingDirectory` set to `/srv/coldstore`
- Runs as user `tjmisko` (edit `User=` to match your system)
- Starts after `network.target` and `syncthing.service`
- Restarts on failure automatically

### Checking logs

```bash
# Follow live logs
journalctl -u coldstore -f

# Last 100 lines
journalctl -u coldstore -n 100

# Since last boot
journalctl -u coldstore -b
```

## Cold storage layout

The `cold_storage_path` directory has this structure:

```
/srv/coldstore/
  catalog.db              # SQLite database (WAL mode)
  catalog.db-wal          # WAL file (may exist during operation)
  catalog.db-shm          # shared memory file (may exist during operation)
  archive/
    CameraRoll/           # mirrors the folder label
      DCIM/
        IMG_0001.jpg      # offloaded files, preserving relative paths
        IMG_0002.jpg
    Books/
      SomeBook.pdf
  thumbnails/
    CameraRoll/
      DCIM/
        IMG_0001.jpg.thumb.jpg   # 400px max-edge JPEG thumbnails
    Books/
      SomeBook.pdf.thumb.jpg
```

- **`catalog.db`** is opened with `?_journal_mode=WAL&_busy_timeout=5000`.
- **`archive/<label>/`** mirrors the relative path structure of the Syncthing folder.
- **`thumbnails/<label>/`** also mirrors the relative path, with `.thumb.jpg` appended.
- Empty parent directories inside `archive/` are cleaned up automatically after restores.

## ZFS considerations

ZFS is recommended for the cold storage volume. Pool creation and management are out of scope, but some relevant settings:

**Checksumming.** ZFS checksums every block by default (`checksum=on`). This complements the SHA-256 verification that coldstore performs during cross-device moves. Together they provide end-to-end integrity from source to archive.

**Compression.** Enable LZ4 compression on the dataset:

```bash
zfs set compression=lz4 tank/coldstore
```

LZ4 has negligible CPU overhead and compresses well for many media metadata blocks, SQLite WAL pages, and thumbnails. Raw photos and videos are already compressed and will pass through at near-zero cost.

**Snapshots.** A simple cron-based snapshot strategy works well:

```bash
# Daily snapshot, keep 30
zfs snapshot tank/coldstore@$(date +%Y-%m-%d)

# Prune snapshots older than 30 days
zfs list -t snapshot -o name -H tank/coldstore | head -n -30 | xargs -r -n1 zfs destroy
```

Snapshots protect against accidental deletion and catalog corruption. Since the archive can always be reconstructed from a rescan (see Backup), snapshots are primarily valuable for recovering the catalog database quickly without a full rescan.

## Monitoring

### Key log messages to watch

| Level | Message | Meaning |
|---|---|---|
| `INFO` | `coldstore daemon starting` | Daemon started successfully. |
| `INFO` | `running initial catalog scan` | Startup scan in progress. |
| `WARN` | `initial scan failed after all retries, starting without offload capability` | The daemon is running but will not offload files until a catalog sync tick succeeds. Investigate the scan error. |
| `WARN` | `skipping quota enforcement: initial scan incomplete` | Quota enforcement is gated on a successful initial scan. Not an error, but persistent appearances mean scans keep failing. |
| `WARN` | `cold storage below 5% free, pausing offload` | The cold storage filesystem is nearly full. Free space or expand the volume. An event is also logged to the `events` table. |
| `ERROR` | `panic in quota enforcement` | A bug triggered a panic during offloading. The daemon recovered and will retry on the next tick. File an issue with the panic value. |
| `ERROR` | `scan failed for folder` | A folder scan failed (permissions, missing directory, etc.). Other folders still scan normally. |
| `ERROR` | `offload failed` | A single file could not be moved to cold storage. The file's status is reverted to `synced` and it will be retried on the next tick. |

### Disk space

The daemon checks available space before every offload. If the cold storage filesystem drops below 5% free, offloading pauses and a warning is logged. Monitor the `events` table for `warning` type events to catch this programmatically.

## Backup

**The SQLite catalog is the only state that matters.** Back it up regularly:

```bash
sqlite3 /srv/coldstore/catalog.db ".backup /path/to/backup/catalog.db"
```

Use the `.backup` command (not a file copy) to get a consistent snapshot even while the daemon is running with WAL mode.

**The archive directory** contains the offloaded files themselves. If the archive is lost, files are gone unless you have ZFS snapshots or another backup of the cold storage volume.

**Thumbnails** are fully regenerable. If lost, the daemon will regenerate them on its next catalog sync tick for any files with an empty `thumbnail_path`.

**The catalog is reconstructible.** If the catalog database is lost or corrupt, you can rebuild it from the files on disk:

```bash
coldstore --config /etc/coldstore/coldstore.toml --scan
```

This walks all configured Syncthing folders and populates the catalog. However, it will not know about archived files -- those will need to be manually reconciled or restored.

## Troubleshooting

### Files stuck in `archiving` state

The `archiving` status is a transient state set just before a file is moved to cold storage. If the daemon crashes mid-move, files can get stuck in this state.

**Automatic recovery:** On every scan (startup and periodic), the scanner calls `recoverArchiving()`. For each file in `archiving` state:

- If the file exists in the archive directory, its status is updated to `archived`.
- If the file does not exist in the archive directory, its status is reverted to `synced`.

**Manual recovery:** If automatic recovery is not triggering (e.g., the daemon won't start), query the catalog directly:

```sql
-- Find stuck files
SELECT id, folder_label, relative_path FROM files WHERE status = 'archiving';

-- If the file is in the archive, mark it archived
UPDATE files SET status = 'archived', archived_at = datetime('now') WHERE id = ?;

-- If the file is still in the syncthing folder, mark it synced
UPDATE files SET status = 'synced', archived_at = NULL WHERE id = ?;
```

### Catalog corruption recovery

If the catalog database is corrupt:

1. Stop the daemon: `sudo systemctl stop coldstore`
2. Move the corrupt database aside: `mv /srv/coldstore/catalog.db /srv/coldstore/catalog.db.corrupt`
3. Also remove WAL/SHM files if present: `rm -f /srv/coldstore/catalog.db-wal /srv/coldstore/catalog.db-shm`
4. Run a one-shot scan to rebuild: `coldstore --config /etc/coldstore/coldstore.toml --scan`
5. Restart the daemon: `sudo systemctl start coldstore`

The rebuilt catalog will contain all files currently present in the Syncthing folders (marked `synced`). Files in the `archive/` directory will **not** be automatically re-cataloged by `--scan` -- they must be restored manually or you can insert records directly:

```sql
-- Example: re-catalog an archived file
INSERT INTO files (folder_label, relative_path, filename, extension, size_bytes, file_mtime, status)
VALUES ('CameraRoll', 'DCIM/IMG_0001.jpg', 'IMG_0001.jpg', '.jpg', 4194304, '2024-06-15 10:30:00', 'archived');
```

### Daemon won't start

- **Config parse error:** Check that all `[[folders]]` entries have a valid `budget` string (e.g., `"20GB"`, not `"20 G"`).
- **Database locked:** Ensure no other process has the catalog database open. Check for stale WAL/SHM files.
- **Permission denied:** The daemon user needs read/write access to `cold_storage_path` and all configured Syncthing folder paths.

### Files not being offloaded

1. Verify the folder is over budget: check `FolderSyncedSize` in the logs.
2. Check if files match a persistent pattern (config `persistent` array or `persistent_paths` table).
3. Check the grace period: files with recent mtime or atime (within `grace_period_minutes`) are skipped.
4. Check cold storage free space: offloading pauses below 5% free.
5. Confirm the initial scan completed successfully (look for `initial scan completed` or the absence of `skipping quota enforcement` warnings).
