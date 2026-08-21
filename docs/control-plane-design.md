# Coldstore Control Plane — Authoritative Technical Design

Status: **authoritative**. Branch `feat/control-plane`. This document is the contract every
implementation step codes against. Where it contradicts `docs/architecture.md`,
`docs/plan.md`, or any survey note, **this document wins**.

It builds to five settled owner decisions:

| ID | Decision |
|----|----------|
| D1 | TOML declares folders (label, path, syncthing id). SQLite `folder_configs` owns tunable policy (budget, grace, enabled, globs, pins) and the daemon reads policy from the DB every tick. |
| D2 | An explicit per-file `pinned` flag. Pinned files are never offload candidates. Restore sets `pinned=1`. |
| D3 | Pinned bytes count against the budget. `pinned_bytes` and `over_budget` are exposed; a wedged folder warns exactly once, loudly. |
| D4 | Bulk restore/archive run through an async, restart-durable job queue. |
| D5 | LAN-only HTTP with the existing basic auth. No TLS, no sessions, no new auth work. |

---

## 0. Global invariants

These hold everywhere. Violating one is a bug regardless of what any local code comment says.

1. **One SQLite writer.** `catalog.Open` sets `db.SetMaxOpenConns(1)`. Every statement in the
   process serializes through a single connection. `SQLITE_BUSY` becomes unreachable in-process.
2. **One mover.** Every filesystem move of a catalogued file — automatic offload, manual archive,
   single-file restore, bulk job item — goes through `daemon.Mover`, which holds a `sync.Mutex`
   for the duration of one file's move. There is exactly one `*Mover` in the process, constructed
   in `main` and injected into both the API server and the daemon. **Every write to `files.pinned`
   also goes through the `Mover`** (`Mover.SetPinned`, `Mover.SetPinnedBulk`), so a pin can never
   commit while a move of that file is in flight. Nothing else — no handler, no scanner, no
   recovery function — calls `moveFile`, `os.Rename`, `os.Remove` or `os.Create` against a
   catalogued path.
3. **Claim before you move.** No mover operation trusts a `FileRecord` handed to it. It re-reads
   the row and performs a compare-and-swap status transition (`ClaimFile`). A lost CAS is a clean
   no-op, never a filesystem action.
4. **Timestamps.** Existing `DATETIME` text columns keep their format. **Every new time-shaped
   column is `INTEGER` Unix epoch seconds.** No Go `time.Time` is ever bound against a text
   timestamp column in a comparison. Grace-period arithmetic stays in Go.
5. **Path authority.** `folder_configs.syncthing_path` is the single runtime source of a folder's
   on-disk location. Startup reconciliation guarantees it equals the TOML value. Nothing at
   runtime scans `cfg.Folders` for a path.
6. **Budget authority.** `folder_configs.budget_bytes` is the only budget enforcement reads.
   `cfg.Folders[i].BudgetBytes` is a seed value, used exactly once per folder, at first insert.
7. **A non-positive budget means "unconfigured — do not enforce."** Never "archive everything."
8. **Recovery is a startup-only phase.** `RecoverStartupState` runs once in `main`, after
   `catalog.Open` and **before** the `Mover`, the `Worker`, the API server or the daemon goroutine
   exist. No recovery function is reachable from any tick, any handler or any job. This is what
   makes it safe for recovery to touch the filesystem without the `Mover` mutex — there is nothing
   to race with.
9. **Prefer keeping bytes.** Wherever two interpretations of an ambiguous on-disk state are
   possible, choose the one that destroys nothing, keep the row pointing at the bytes that exist,
   and write a loud event. Never delete a verified copy to tidy up bookkeeping.
10. **Never overwrite, never truncate.** No move creates its destination with `os.Create` or
    renames onto a path that already exists. Destinations are written to a temp sibling and renamed
    into place after verification, and the real destination is checked with `os.Lstat` first. An
    unexpected occupant is a `conflict` event, never a silent overwrite.
11. **A CAS result is load-bearing.** Every `UPDATE ... WHERE status = ?` specified in this document
    returns a bool. A `false` is an error path that must be logged and propagated. No call site may
    discard it.
12. **Idempotent end states.** `Mover.ArchiveByID` and `Mover.RestoreByID` return success as a
    no-op when the re-read row is already in the state the operation was trying to reach.
    `ErrFileStateChanged` is reserved for a genuine conflict — a row in a state that is neither the
    expected start nor the intended end.

---

## 1. Migration plan

### 1.1 The problem being solved

`internal/catalog/migrations.go` today is one `const schemaSQL` of `CREATE TABLE IF NOT EXISTS`
statements, executed wholesale by `catalog.Open`. There is no version marker. Any column added
from here on reaches a fresh database and silently fails to reach an existing one — including the
demo database produced by `scripts/seed-demo` and the owner's first production `catalog.db`.

### 1.2 Version marker: `PRAGMA user_version`

We use SQLite's built-in `PRAGMA user_version`, **not** a `schema_version` table.

`user_version` is a 32-bit integer in the database header. It is written transactionally, so the
DDL of a migration and the version bump commit or roll back together, with no bootstrap row and no
chance of a half-applied migration recorded as complete.

A brand-new database reports `user_version = 0`. **So does every database created by the current
`schemaSQL` blob.** The runner cannot tell them apart, and does not need to — see 1.4.

**Rejected alternative:** a `schema_version(version INTEGER PRIMARY KEY, applied_at INTEGER)` table.
It buys an audit trail of when each migration ran, which nobody will read, at the cost of a
bootstrap problem (the table itself has to be created before it can record that it was created) and
a second source of truth that can disagree with `user_version`. Not worth it.

### 1.3 The runner

Replace `internal/catalog/migrations.go` entirely with the structure below, and replace
`catalog.go:22`'s `db.Exec(schemaSQL)` with `migrate(db)`.

```go
package catalog

import (
	"database/sql"
	"fmt"
)

type migration struct {
	version int
	sql     string
}

// migrations MUST be append-only and MUST stay in ascending version order.
// Never edit the SQL of a migration that has shipped; add a new one instead.
//
// A version number is ASSIGNED WHEN THE MIGRATION LANDS, never reserved in
// advance. The final state below is what the tree looks like after step 9 of
// the implementation order (section 8); each step appends exactly one entry
// and bumps schemaVersion in the SAME commit.
var migrations = []migration{
	{1, migration1InitialSchema},    // step 1
	{2, migration2Pins},             // step 3
	{3, migration3FolderState},      // step 4
	{4, migration4Jobs},             // step 8
	{5, migration5ThumbnailFailures},// step 9
}

// schemaVersion is the version a freshly migrated database reports.
// It equals the highest entry above and is bumped in the same commit as that
// entry. At the end of step 3 it is 2; at the end of step 4 it is 3; and so on.
const schemaVersion = 5

// migrate brings the database up to schemaVersion. It is safe to call on a
// fresh file, on a database created by the pre-migration schemaSQL blob
// (which reports user_version = 0), and on an already-current database.
func migrate(db *sql.DB) error {
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}
	if current > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d)",
			current, schemaVersion)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("migration %d: begin: %w", m.version, err)
		}
		if _, err := tx.Exec(m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", m.version, err)
		}
		// PRAGMA cannot take a bind parameter; m.version is an int constant
		// from this file, never user input.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: setting user_version: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d: commit: %w", m.version, err)
		}
	}
	return nil
}
```

Refusing to run against a *newer* database (`current > schemaVersion`) prevents this binary from
writing rows a newer binary's constraints would reject.

**Numbering rule, normative.** The version a migration gets is the next free integer at the moment
it lands on `main`. Nothing in this document reserves a number: §1.5–1.9 are written in landing
order and their headings must be kept in sync with §8 if the order ever changes. An implementer who
finds a mismatch between a §1.x heading and a §8 step must fix §1.x, never renumber a migration that
has shipped.

### 1.3a The downgrade hazard the runner cannot solve, and what we do instead

`current > schemaVersion` protects the **new** binary. It does nothing about the **old** one. The
pre-migration `catalog.Open` runs a blob of `CREATE TABLE IF NOT EXISTS` (`catalog.go:22`), which
no-ops cleanly against a v5 database and starts happily. Its `OffloadCandidates` has no
`AND pinned = 0`, its `enforceFolder` reads `cfg.Folders[i].BudgetBytes` instead of the DB, and its
`Restorer.Restore` finishes with `UpdateFileStatus(StatusSynced)` instead of `MarkRestored`. So
`make install` of a pre-control-plane binary onto a migrated database archives every pinned file on
the first quota tick and Syncthing propagates every one of those deletions. There is no schema
sentinel that can stop a binary whose DDL is entirely `IF NOT EXISTS`.

Three concrete mitigations, all required:

1. **`coldstore --schema-version`** — a subcommand on the new binary that prints `schemaVersion` and
   exits 0, and a `--db-schema-version <path>` that prints the `user_version` of an existing
   catalog and exits 0.
2. **A refusing `make install`.** The `install` target runs
   `coldstore --db-schema-version $(COLD_DB)` against the installed catalog (when it exists) and
   compares it to `./coldstore --schema-version` of the binary about to be installed. If the
   database is newer, `install` fails with the rollback warning and requires
   `make install FORCE_DOWNGRADE=1`.
3. **A marker file.** After a successful `migrate`, `catalog.Open` writes
   `<dir(catalog.db)>/.coldstore-schema` containing the decimal `schemaVersion`, `0644`. It is
   advisory — it exists so a human, a backup, or a future `install` check can see what wrote the
   database without opening SQLite.

`docs/deployment.md` gains a **Rolling back** section that states plainly: *rolling the binary back
across a schema migration will archive every pinned file in every folder on the first quota tick,
and Syncthing will delete them from every device. Restore the pre-upgrade `catalog.db` backup
alongside the older binary, or do not roll back.*

### 1.4 Migration 1 — how the existing schema becomes version 1 without breaking anything

**Rule: `migration1InitialSchema` is the current `schemaSQL` text, byte for byte, with every
`IF NOT EXISTS` clause intact.** Do not "clean it up" into bare `CREATE TABLE` because it is the
initial schema. That rewrite bricks the demo database and every already-created catalog.

Why this works, for both cases:

- **Fresh file** — `user_version = 0`, no tables. Migration 1 creates all four tables and their
  indexes. `user_version` becomes 1. Migrations 2–5 then apply normally.
- **Existing database created by the old blob** (demo DB, any dev DB, a production DB deployed
  before this change) — `user_version = 0`, all four tables already present with data. Migration 1
  runs and every statement is a no-op because of `IF NOT EXISTS`. No data is touched.
  `user_version` becomes 1. Migrations 2–5 then apply the `ALTER TABLE`s to the populated tables.

**Only migration 1 is idempotent. Migrations 2 and up are written bare** (`ALTER TABLE ...`,
`CREATE TABLE jobs ...` with no `IF NOT EXISTS`). If one of them ever runs twice, that is a runner
bug and it must fail loudly rather than silently succeed. Because each migration runs inside a
transaction, a failure leaves the database at the previous version with no partial DDL applied.

```go
const migration1InitialSchema = `
CREATE TABLE IF NOT EXISTS files (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	folder_label TEXT NOT NULL,
	relative_path TEXT NOT NULL,
	filename TEXT NOT NULL,
	extension TEXT NOT NULL DEFAULT '',
	mime_type TEXT NOT NULL DEFAULT '',
	size_bytes INTEGER NOT NULL DEFAULT 0,
	file_mtime DATETIME NOT NULL,
	file_atime DATETIME NOT NULL DEFAULT '1970-01-01 00:00:00',
	first_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	archived_at DATETIME,
	status TEXT NOT NULL DEFAULT 'synced',
	thumbnail_path TEXT DEFAULT '',
	content_hash TEXT DEFAULT '',
	UNIQUE(folder_label, relative_path)
);

CREATE INDEX IF NOT EXISTS idx_files_folder_status ON files(folder_label, status);
CREATE INDEX IF NOT EXISTS idx_files_status ON files(status);
CREATE INDEX IF NOT EXISTS idx_files_mtime ON files(file_mtime);

CREATE TABLE IF NOT EXISTS folder_configs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	folder_label TEXT NOT NULL UNIQUE,
	syncthing_id TEXT NOT NULL DEFAULT '',
	syncthing_path TEXT NOT NULL,
	budget_bytes INTEGER NOT NULL DEFAULT 0,
	grace_minutes INTEGER NOT NULL DEFAULT 30,
	enabled INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS persistent_paths (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	folder_label TEXT NOT NULL,
	pattern TEXT NOT NULL,
	UNIQUE(folder_label, pattern)
);

CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	timestamp DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	event_type TEXT NOT NULL,
	folder TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	details TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_events_timestamp ON events(timestamp);
CREATE INDEX IF NOT EXISTS idx_events_type ON events(event_type);
`
```

### 1.5 Migration 2 — pins (D2, D3) — lands in step 3

```go
const migration2Pins = `
ALTER TABLE files ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE files ADD COLUMN pinned_at INTEGER;
ALTER TABLE files ADD COLUMN pin_source TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_files_folder_status_pinned
	ON files(folder_label, status, pinned, size_bytes);
CREATE INDEX idx_files_pin_source
	ON files(folder_label, pinned, pin_source, pinned_at);
`
```

`DEFAULT 0` / `DEFAULT ''` are constants, which SQLite's `ALTER TABLE ADD COLUMN` requires. Every
pre-existing row becomes unpinned with an empty `pin_source`, which is the correct starting state.

**Why `pin_source` and `pinned_at` ship in the same migration as `pinned`, not later.** Under D2
every restore silently sets `pinned = 1` and nothing ever releases a pin, so over months of ordinary
use the pinned set is dominated by files the *Restore button* pinned, not files the owner chose to
protect. When the folder finally wedges, the remedy the banner offers ("unpin some files") is a
grid of thousands of identical pin badges with no way to tell the two apart, and the safest-looking
bulk action — rubber-band select, Unpin — is the one that releases the owner's real favourites.
Two extra columns turn that triage into one filter. Added later they would need another migration
and could never be back-filled with attribution for pins already set.

`pin_source` is `''` (never pinned), `'user'` (an explicit pin toggle), or `'restore'`
(auto-pinned by a restore). The CHECK constraint is enforced in Go rather than SQL because
`ALTER TABLE ADD COLUMN` cannot add a CHECK; `SetPinned`/`MarkRestored` are the only writers.

**`MarkRestored` must never downgrade a `'user'` pin to `'restore'`** — see §4.6.

### 1.6 Migration 3 — folder reconciliation, wedge state, scan generation (D1, D3) — lands in step 4

```go
const migration3FolderState = `
ALTER TABLE folder_configs ADD COLUMN present_in_toml INTEGER NOT NULL DEFAULT 1;
ALTER TABLE folder_configs ADD COLUMN wedge_warned_at INTEGER;
ALTER TABLE folder_configs ADD COLUMN last_enforced_at INTEGER;

ALTER TABLE files ADD COLUMN last_seen INTEGER NOT NULL DEFAULT 0;

CREATE INDEX idx_files_folder_last_seen ON files(folder_label, status, last_seen);
CREATE INDEX idx_events_folder_type_ts ON events(folder, event_type, timestamp);
`
```

`present_in_toml` defaults to 1 so that rows created before this migration (all of which came from
the old permissive `PUT`) are treated as declared until the first reconciliation pass corrects them.

`files.last_seen` is the **scan generation** column (§5.9). It holds the epoch second at which the
scan that last observed this file on disk *started*. `DEFAULT 0` means every pre-existing row looks
unseen, which is harmless: the missing-detection sweep only ever runs after a walk that completed
without error, and that walk stamps every file it finds.

**Rejected alternative:** persisting derived state (`synced_bytes`, `pinned_bytes`, `over_budget`,
`wedged`) on `folder_configs` so the API can read it without recomputing. Rejected because it
introduces a staleness window — the numbers would only refresh on an enforcement tick, so after a
bulk restore the UI would show pre-restore figures until the next tick, and the budget bar would
disagree with the `over_budget` flag driving the banner. Instead the API computes all four live from
one indexed aggregate per folder (§4.2), which is cheap and never drifts. Only `wedge_warned_at`
(a latch that must survive restarts) and `last_enforced_at` (an observability field the UI shows as
"last checked N minutes ago") are persisted.

### 1.7 Migration 4 — the job queue (D4) — lands in step 8

```go
const migration4Jobs = `
CREATE TABLE jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	action TEXT NOT NULL CHECK(action IN ('restore','archive','scan')),
	folder_label TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT 'pending'
		CHECK(state IN ('pending','running','done','failed')),
	total INTEGER NOT NULL DEFAULT 0,
	done INTEGER NOT NULL DEFAULT 0,
	failed INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	started_at INTEGER,
	finished_at INTEGER
);

CREATE TABLE job_items (
	job_id INTEGER NOT NULL,
	file_id INTEGER NOT NULL,
	state TEXT NOT NULL DEFAULT 'pending'
		CHECK(state IN ('pending','running','done','failed')),
	error TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (job_id, file_id)
) WITHOUT ROWID;

CREATE INDEX idx_jobs_state ON jobs(state, id);
CREATE INDEX idx_job_items_pending ON job_items(job_id, state, file_id);
`
```

`jobs.error` carries a job-level failure reason — the message from a `FailJob` (§5.5) when the
worker abandons a job for a reason that is not attributable to any single item. It is `''` on a
normal terminal job.

**No `REFERENCES` / `ON DELETE CASCADE` clause.** Foreign keys are off per-connection by default and
the DSN does not enable them, so the clause would parse, be stored, and do nothing. `job_items` is
deleted explicitly, in the same transaction as its `jobs` row, by the retention sweep (§5.7).

`WITHOUT ROWID` on `job_items`: the composite primary key is the entire useful content of the row
alongside two small columns, so the b-tree *is* the table and there is no rowid indirection. The
composite PK also gives free deduplication when the UI submits the same file id twice in one
selection.

### 1.8 Migration 5 — thumbnail failure tracking — lands in step 9

```go
const migration5ThumbnailFailures = `
ALTER TABLE files ADD COLUMN thumb_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE files ADD COLUMN thumb_last_attempt INTEGER;
ALTER TABLE files ADD COLUMN thumb_error TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_files_thumb_queue
	ON files(thumbnail_path, thumb_attempts, status);
`
```

`thumb_last_attempt` is a **nullable INTEGER epoch**, not a `DATETIME`. `ALTER TABLE ADD COLUMN`
rejects `NOT NULL DEFAULT CURRENT_TIMESTAMP`; the existing `files.first_seen` gets away with it only
because it is part of a `CREATE TABLE`.

### 1.9 `catalog.Open` after migrations

```go
func Open(dbPath string) (*Catalog, error) {
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating database directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// One connection for the whole process. The daemon tick, the job worker
	// and every HTTP handler share it, so SQLITE_BUSY and WAL snapshot
	// upgrade failures cannot occur in-process.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	// Advisory marker so install checks and humans can see what wrote this
	// catalog without opening SQLite (section 1.3a).
	writeSchemaMarker(filepath.Dir(dbPath), schemaVersion) // errors logged, never fatal
	return &Catalog{db: db}, nil
}
```

`_txlock=immediate` makes `db.Begin()` issue `BEGIN IMMEDIATE`, so a read-then-write transaction
takes its write lock up front rather than attempting a snapshot upgrade that WAL mode fails
instantly with `SQLITE_BUSY_SNAPSHOT` (which `busy_timeout` does not retry). With
`SetMaxOpenConns(1)` this is belt and braces, and it stays correct if the pool is ever widened.

**Rejected alternative:** a larger pool plus an explicit `sync.Mutex` inside `Catalog` guarding
writes. That requires auditing and fixing every existing call site that discards a write error
(`offloader.go:49`, `restorer.go:49`, `scanner.go:52`, `scanner.go:55`, `handlers_status.go:35-42`),
and buys read concurrency this workload — one owner, one LAN, one daemon — does not need.

---

## 2. Full schema after all migrations

### 2.1 `files`

| Column | Type | Default | Notes |
|---|---|---|---|
| `id` | INTEGER PK AUTOINCREMENT | | |
| `folder_label` | TEXT NOT NULL | | matches `folder_configs.folder_label` |
| `relative_path` | TEXT NOT NULL | | UNIQUE with `folder_label` |
| `filename` | TEXT NOT NULL | | |
| `extension` | TEXT NOT NULL | `''` | lowercased, includes the dot |
| `mime_type` | TEXT NOT NULL | `''` | |
| `size_bytes` | INTEGER NOT NULL | `0` | |
| `file_mtime` | DATETIME NOT NULL | | text; written by the driver from a Go `time.Time` |
| `file_atime` | DATETIME NOT NULL | `'1970-01-01 00:00:00'` | |
| `first_seen` | DATETIME NOT NULL | `CURRENT_TIMESTAMP` | |
| `archived_at` | DATETIME NULL | | |
| `status` | TEXT NOT NULL | `'synced'` | `synced`\|`archiving`\|`archived`\|`restoring`\|`missing` |
| `thumbnail_path` | TEXT | `''` | relative to `<cold>/thumbnails/` |
| `content_hash` | TEXT | `''` | unused today |
| **`pinned`** | INTEGER NOT NULL | `0` | **new (v2)** — 0/1 |
| **`pinned_at`** | INTEGER NULL | | **new (v2)** — epoch of the pin, NULL when unpinned |
| **`pin_source`** | TEXT NOT NULL | `''` | **new (v2)** — `''`\|`user`\|`restore`, enforced in Go |
| **`last_seen`** | INTEGER NOT NULL | `0` | **new (v3)** — scan generation; epoch at which the scan that last saw this file on disk started. Never serialised to the API (`json:"-"`) |
| **`thumb_attempts`** | INTEGER NOT NULL | `0` | **new (v5)** |
| **`thumb_last_attempt`** | INTEGER NULL | | **new (v5)** — epoch seconds |
| **`thumb_error`** | TEXT NOT NULL | `''` | **new (v5)** |

Indexes:

| Index | Columns | Serves |
|---|---|---|
| `idx_files_folder_status` | `(folder_label, status)` | pre-existing; folder+status counts (`MarkMissingSyncedFiles`, its old consumer, is deleted in step 4 — §5.9) |
| `idx_files_status` | `(status)` | `ArchivingFiles`, `RestoringFiles`, global `FileCount(status)` |
| `idx_files_mtime` | `(file_mtime)` | default `ListFiles` sort |
| **`idx_files_folder_status_pinned`** | `(folder_label, status, pinned, size_bytes)` | **`OffloadCandidates`** (folder + status + `pinned=0` seek) and **`FolderSizes`** (the `size_bytes` suffix makes both SUMs index-covered, so no table rows are read) |
| **`idx_files_pin_source`** | `(folder_label, pinned, pin_source, pinned_at)` | the Gallery's "Pinned by me" / "Auto-pinned on restore" filters and their oldest-first ordering |
| **`idx_files_folder_last_seen`** | `(folder_label, status, last_seen)` | the scan-generation missing sweep (§5.9) |
| **`idx_files_thumb_queue`** | `(thumbnail_path, thumb_attempts, status)` | **`FilesNeedingThumbnails`** — the `thumbnail_path = ''` equality plus the `thumb_attempts < 3` range are the leading columns |

### 2.2 `folder_configs`

| Column | Type | Default | Owner | Notes |
|---|---|---|---|---|
| `id` | INTEGER PK AUTOINCREMENT | | | |
| `folder_label` | TEXT NOT NULL UNIQUE | | TOML | |
| `syncthing_id` | TEXT NOT NULL | `''` | TOML | overwritten every startup |
| `syncthing_path` | TEXT NOT NULL | | TOML | overwritten every startup |
| `budget_bytes` | INTEGER NOT NULL | `0` | **UI** | seeded from TOML on first insert only |
| `grace_minutes` | INTEGER NOT NULL | `30` | **UI** | seeded from `[daemon].grace_period_minutes` on first insert only |
| `enabled` | INTEGER NOT NULL | `1` | **UI** | seeded to 1 on first insert only |
| **`present_in_toml`** | INTEGER NOT NULL | `1` | reconciler | **new (v5)** — 0 means the folder is no longer declared |
| **`wedge_warned_at`** | INTEGER NULL | | daemon | **new (v5)** — epoch of the last wedge warning; NULL means not wedged |
| **`last_enforced_at`** | INTEGER NULL | | daemon | **new (v5)** — epoch of the last completed enforcement pass |

No index beyond the `folder_label` UNIQUE constraint. This table has one row per folder; the owner
has a handful.

### 2.3 `jobs`

| Column | Type | Default | Notes |
|---|---|---|---|
| `id` | INTEGER PK AUTOINCREMENT | | the `job_id` returned by the API |
| `action` | TEXT NOT NULL | | CHECK `restore`\|`archive`\|`scan` |
| `folder_label` | TEXT NOT NULL | `''` | scan scope; `''` means all folders. Unused for restore/archive |
| `state` | TEXT NOT NULL | `'pending'` | CHECK `pending`\|`running`\|`done`\|`failed` |
| `total` | INTEGER NOT NULL | `0` | item count fixed at creation |
| `done` | INTEGER NOT NULL | `0` | items that reached a terminal state, successful or not |
| `failed` | INTEGER NOT NULL | `0` | subset of `done` that failed |
| `error` | TEXT NOT NULL | `''` | job-level failure reason from `FailJob`; `''` on a normal terminal job |
| `created_at` | INTEGER NOT NULL | | epoch seconds |
| `started_at` | INTEGER NULL | | epoch seconds, set on first claim |
| `finished_at` | INTEGER NULL | | epoch seconds, set on terminal transition |

**Counter invariant.** On a terminal job (`done`/`failed`), `jobs.done == jobs.total` and
`jobs.failed <= jobs.done` always hold. Every method that can end a job — `CompleteJobItem`,
`CompleteJobItemVirtual`, `FailRemainingItems`, `FailJob` — is specified in §5.4 to preserve it.

Index `idx_jobs_state (state, id)` serves `NextPendingJob` — `WHERE state IN ('pending','running')
ORDER BY id LIMIT 1` is a direct index range scan taking the first entry.

### 2.4 `job_items`

| Column | Type | Default | Notes |
|---|---|---|---|
| `job_id` | INTEGER NOT NULL | | PK part 1 |
| `file_id` | INTEGER NOT NULL | | PK part 2; `files.id` |
| `state` | TEXT NOT NULL | `'pending'` | CHECK `pending`\|`running`\|`done`\|`failed` |
| `error` | TEXT NOT NULL | `''` | populated only when `state='failed'` |

`WITHOUT ROWID`, primary key `(job_id, file_id)`.

Index `idx_job_items_pending (job_id, state, file_id)` serves `ClaimJobItem` — the next pending item
for a job is the first entry in an index range — and the failure listing for
`GET /api/v1/jobs/{id}` (`WHERE job_id = ? AND state = 'failed'`).

### 2.5 `persistent_paths` and `events`

Unchanged from migration 1, except `events` gains
`idx_events_folder_type_ts (folder, event_type, timestamp)`, which serves both
`LogEventDeduped`'s `NOT EXISTS` probe and `ListEvents`' `WHERE folder = ? ORDER BY timestamp DESC`.

### 2.6 The `fileColumns` refactor — **do this first, in step 2, before any new column**

The 14-column select list for `files` is currently duplicated verbatim at `catalog.go` lines 56, 66,
85, 154, 195 and 308, and consumed by two hand-maintained positional scanners (`scanFileRecord` at
344, `scanFileRecordRows` at 359). Adding `pinned` means eight coordinated edits, and a missed one
is not a compile error — it is a runtime `sql: expected 14 destination arguments in Scan, not 15`
from whichever query was missed, two of which (`OffloadCandidates`, `FilesNeedingThumbnails`) are
daemon-loop-only paths no HTTP smoke test touches.

Collapse them first, in a commit of its own that changes no behaviour. **In step 2 the constant
lists exactly the 14 columns that exist at schema v1 — no more.** Adding a column is then two edits
in one commit: the migration, and this constant plus `scanFile`.

```go
// step 2: the v1 columns, verbatim, nothing else.
const fileColumns = `id, folder_label, relative_path, filename, extension, mime_type,
	size_bytes, file_mtime, file_atime, first_seen, archived_at, status,
	thumbnail_path, content_hash`

type fileScanner interface{ Scan(dest ...any) error }

func scanFile(s fileScanner) (*FileRecord, error) {
	var f FileRecord
	var archivedAt sql.NullTime
	err := s.Scan(&f.ID, &f.FolderLabel, &f.RelativePath, &f.Filename, &f.Extension,
		&f.MimeType, &f.SizeBytes, &f.FileMtime, &f.FileAtime, &f.FirstSeen, &archivedAt,
		&f.Status, &f.ThumbnailPath, &f.ContentHash)
	if err != nil {
		return nil, err
	}
	if archivedAt.Valid {
		f.ArchivedAt = &archivedAt.Time
	}
	return &f, nil
}
```

`*sql.Row` and `*sql.Rows` both satisfy `fileScanner`, so the two scanners collapse into one. Every
`SELECT` on `files` becomes `"SELECT " + fileColumns + " FROM files WHERE ..."`.

`UpsertFile`'s INSERT column list (`catalog.go:37`) is a different list in a different order and
**does not** use this constant. Do not try to make it.

**Final state, after step 9.** Each of steps 3, 4 and 9 appends its own columns to the constant and
its own destinations to `scanFile`, in the same commit as its migration. The end state is:

```go
const fileColumns = `id, folder_label, relative_path, filename, extension, mime_type,
	size_bytes, file_mtime, file_atime, first_seen, archived_at, status,
	thumbnail_path, content_hash, pinned, pinned_at, pin_source, last_seen,
	thumb_attempts, thumb_last_attempt, thumb_error`

func scanFile(s fileScanner) (*FileRecord, error) {
	var f FileRecord
	var archivedAt sql.NullTime
	var pinnedAt, lastAttempt sql.NullInt64
	err := s.Scan(&f.ID, &f.FolderLabel, &f.RelativePath, &f.Filename, &f.Extension,
		&f.MimeType, &f.SizeBytes, &f.FileMtime, &f.FileAtime, &f.FirstSeen, &archivedAt,
		&f.Status, &f.ThumbnailPath, &f.ContentHash, &f.Pinned, &pinnedAt, &f.PinSource,
		&f.LastSeen, &f.ThumbAttempts, &lastAttempt, &f.ThumbError)
	if err != nil {
		return nil, err
	}
	if archivedAt.Valid {
		f.ArchivedAt = &archivedAt.Time
	}
	if pinnedAt.Valid {
		f.PinnedAt = &pinnedAt.Int64
	}
	if lastAttempt.Valid {
		f.ThumbLastAttempt = &lastAttempt.Int64
	}
	return &f, nil
}
```

### 2.7 `FileRecord`, final state after step 9

Steps 3, 4 and 9 each add their own fields in the same commit as their migration. At the end:

```go
type FileRecord struct {
	ID               int64      `json:"id"`
	FolderLabel      string     `json:"folder_label"`
	RelativePath     string     `json:"relative_path"`
	Filename         string     `json:"filename"`
	Extension        string     `json:"extension"`
	MimeType         string     `json:"mime_type"`
	SizeBytes        int64      `json:"size_bytes"`
	FileMtime        time.Time  `json:"file_mtime"`
	FileAtime        time.Time  `json:"file_atime"`
	FirstSeen        time.Time  `json:"first_seen"`
	ArchivedAt       *time.Time `json:"archived_at,omitempty"`
	Status           FileStatus `json:"status"`
	ThumbnailPath    string     `json:"thumbnail_path,omitempty"`
	ContentHash      string     `json:"content_hash,omitempty"`
	Pinned           bool       `json:"pinned"`
	PinnedAt         *int64     `json:"pinned_at"`      // null when unpinned; NOT omitempty
	PinSource        string     `json:"pin_source"`     // "" | "user" | "restore"
	LastSeen         int64      `json:"-"`              // internal scan generation
	ThumbAttempts    int        `json:"thumb_attempts"`
	ThumbLastAttempt *int64     `json:"thumb_last_attempt,omitempty"`
	ThumbError       string     `json:"thumb_error,omitempty"`
}
```

`Pinned bool` scans cleanly from an `INTEGER` 0/1 with go-sqlite3. `PinnedAt` follows the
nullability rule in §6.0: the TypeScript declares `pinned_at: number | null`, so the Go field is a
pointer with **no** `omitempty`.

---

## 3. Policy reconciliation (D1)

### 3.1 Catalog primitives

`UpsertFolderConfig` is **removed by the end of step 5**. It overwrites every column including the
three the user edits, so calling it from a startup reconciler would reset a UI-set budget to the
TOML value on every daemon restart — the exact clobber D1 forbids. It is replaced by the methods
below, which have disjoint authority.

**It is not deleted in step 4.** It has three live callers — `internal/api/handlers_config.go:52`,
`scripts/seed-demo/main.go:96` and `internal/catalog/catalog_test.go:268` — and step 4 rewrites none
of them. Step 4 leaves it in place, marked `// Deprecated: use ReconcileFolderFromTOML or
UpdateFolderPolicy.` Step 5 rewrites `handlers_config.go` and the test, step 12 rewrites
`seed-demo`, and the **deletion happens in step 12**, once the last caller is gone. Same rule for
`FolderSyncedSize` (§4.2).

```go
// ReconcileFolderFromTOML upserts the infrastructure fields of a folder
// declared in coldstore.toml. budget/grace/enabled are written ONLY when the
// row is created; on an existing row they are left exactly as the user set
// them in the UI. Returns true if a new row was inserted.
func (c *Catalog) ReconcileFolderFromTOML(label, syncthingID, syncthingPath string,
	seedBudget int64, seedGrace int) (inserted bool, err error) {

	res, err := c.db.Exec(`
		INSERT INTO folder_configs
			(folder_label, syncthing_id, syncthing_path, budget_bytes,
			 grace_minutes, enabled, present_in_toml)
		VALUES (?, ?, ?, ?, ?, 1, 1)
		ON CONFLICT(folder_label) DO NOTHING
	`, label, syncthingID, syncthingPath, seedBudget, seedGrace)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}

	// Always refresh the TOML-owned fields, never the policy fields.
	if _, err := c.db.Exec(`
		UPDATE folder_configs
		SET syncthing_id = ?, syncthing_path = ?, present_in_toml = 1
		WHERE folder_label = ?
	`, syncthingID, syncthingPath, label); err != nil {
		return false, err
	}
	return n == 1, nil
}

// MarkFoldersAbsentFromTOML sets present_in_toml = 0 for every row whose label
// is not in the given set. Rows are never deleted.
func (c *Catalog) MarkFoldersAbsentFromTOML(declared []string) error

// UpdateFolderPolicy writes ONLY the UI-tunable fields. It never inserts and
// never touches syncthing_id or syncthing_path. Returns false if no row with
// that label exists.
func (c *Catalog) UpdateFolderPolicy(label string, budgetBytes *int64,
	graceMinutes *int, enabled *bool) (found bool, err error)

// ActiveFolderConfigs returns folders the daemon should enforce and scan:
// enabled and still declared in TOML.
func (c *Catalog) ActiveFolderConfigs() ([]FolderConfigRecord, error)
//   SELECT ... FROM folder_configs
//   WHERE enabled = 1 AND present_in_toml = 1 ORDER BY folder_label

// ListFolderConfigs returns every row, including disabled and orphaned ones,
// for the API. ORDER BY folder_label.
func (c *Catalog) ListFolderConfigs() ([]FolderConfigRecord, error)

func (c *Catalog) FolderConfigByLabel(label string) (*FolderConfigRecord, error)
```

`UpdateFolderPolicy` builds its `SET` clause from the non-nil pointers only, so a request that omits
`grace_minutes` leaves `grace_minutes` alone. If all three pointers are nil it is a no-op that still
reports `found` correctly.

### 3.2 Startup algorithm

**`main` has one fixed order and every phase below depends on the ones above it.** Getting the order
wrong is not a style question: invariant 8 says recovery must complete before any concurrent actor
exists, and reconciliation must complete before recovery, because recovery resolves file rows using
folder paths that only reconciliation guarantees are current.

```
1. config.Load                          -> fatal on error
2. validateServerBinding(cfg)           -> fatal (D5, step 13): a non-loopback
                                           listen_addr with an empty username or
                                           password refuses to start
3. catalog.Open  (runs migrate)         -> fatal on error
4. reconcileFolders(cfg, cat, logger)   -> fatal on error          (below)
5. syncthing.AssertFolderTypes(...)     -> never fatal              (section 3.7)
6. checkStignore(...)                   -> never fatal              (section 3.8)
7. daemon.RecoverStartupState(...)      -> fatal on error           (section 5.6)
8. cat.ResumeInterruptedJobs()          -> fatal on error           (section 5.6)
   --- nothing above this line runs concurrently with anything else ---
9. construct Mover, Worker, Scanner, Daemon, api.NewServer(Deps{...})
10. go worker.Run(ctx); go srv.Serve(...); daemon.Run(ctx)
```

The `-scan` one-shot branch runs 1–8 and then a single `ScanAll`, and exits.

`reconcileFolders`, phase 4:

```
reconcileFolders(cfg, cat, logger):
  declared := []
  for each folder f in cfg.Folders:
      if f.Label == "" or f.Path == "":
          log error "folder declaration missing label or path"; exit(1)
      if f.BudgetBytes <= 0:
          log error "folder %q has a non-positive budget"; exit(1)

      seedGrace := cfg.Daemon.GracePeriodMinutes    // already defaulted to 30 by config.setDefaults
      inserted, err := cat.ReconcileFolderFromTOML(
          f.Label, f.SyncthingID, f.Path, f.BudgetBytes, seedGrace)
      if err != nil: log error; exit(1)

      if inserted:
          // First sighting of this folder: seed its TOML persistent globs too.
          for each pattern in f.Persistent:
              cat.AddPersistentPath(f.Label, pattern)   // INSERT OR IGNORE
          cat.LogEvent("folder_seeded", f.Label, "",
              fmt.Sprintf("budget %d bytes, grace %d min", f.BudgetBytes, seedGrace))

      declared = append(declared, f.Label)

  cat.MarkFoldersAbsentFromTOML(declared)
```

Reconciliation failure is fatal. A daemon that cannot establish its policy table must not start and
begin enforcing against whatever happens to be in the database.

**Config validation moves earlier and gets stricter.** `config.setDefaults` already errors on an
empty `budget` string. Add: a folder with a duplicate `label` is a load error, and a folder whose
`path` is not absolute is a load error.

### 3.3 The seeding rule, stated precisely

For a given folder label, on any startup:

- If no `folder_configs` row exists → one is inserted with `budget_bytes` = the TOML budget,
  `grace_minutes` = `[daemon].grace_period_minutes`, `enabled` = 1, `present_in_toml` = 1, and the
  TOML `persistent` globs are copied into `persistent_paths`.
- If a row already exists → `syncthing_id`, `syncthing_path` and `present_in_toml` are overwritten
  from TOML. `budget_bytes`, `grace_minutes`, `enabled` and `persistent_paths` are **not touched**,
  regardless of what TOML says. Changing `budget = "40GB"` in TOML after first run has no effect;
  the budget is edited in the UI from then on.

That last sentence is a documentation obligation: `docs/deployment.md` and
`coldstore.toml.example` must both say so, next to the `budget` key.

### 3.4 Folders that disappear from TOML

A `folder_configs` row whose label is no longer declared has `present_in_toml` set to 0 and is
**never deleted**. Consequences:

- It is excluded from `ActiveFolderConfigs`, so it is neither scanned nor enforced. A stale
  `syncthing_path` can never be walked or offloaded against.
- It is still returned by `ListFolderConfigs` and therefore by `GET /api/v1/folders` and
  `GET /api/v1/status`, with `"present_in_toml": false`. The UI renders it greyed out with an
  "orphaned — not in coldstore.toml" note.
- Its files stay in the catalog and remain **browsable and downloadable**, because every path
  lookup goes through `cat.FolderPath`, which resolves orphaned rows too (§3.5). Downloading a synced
  file whose folder path no longer exists on disk is a `409`, not a `404`.
- **Restore and archive of an orphaned folder's file are refused with a `409`.** `Mover` checks
  `present_in_toml` and then `FolderReady` (§3.6) before any status write. Two separate reasons: a
  no-longer-declared `syncthing_path` may point anywhere, and a folder coldstore has been told to
  stop managing should not have files pushed back into it.
- Re-declaring the folder in TOML flips `present_in_toml` back to 1 on the next startup with the
  user's budget, grace and pins intact.

**Rejected alternative:** deleting the row. It throws away the budget, the grace setting and (via a
cascade or a manual sweep) the persistent globs, so a typo in `coldstore.toml` followed by a restart
silently destroys the owner's tuning. Retaining the row costs one integer column.

### 3.5 Per-tick read path

`Daemon.EnforceQuotas` no longer touches `d.cfg.Folders`:

```go
func (d *Daemon) EnforceQuotas(ctx context.Context) {
	folders, err := d.cat.ActiveFolderConfigs()
	if err != nil {
		d.logger.Error("cannot read folder policy, skipping enforcement tick", "error", err)
		return // never fall back to cfg.Folders
	}
	for _, fc := range folders {
		if ctx.Err() != nil {
			return
		}
		if err := d.enforceFolder(ctx, fc); err != nil {
			d.logger.Error("quota enforcement failed", "folder", fc.FolderLabel, "error", err)
		}
	}
}
```

A read error skips the tick entirely. Falling back to `cfg.Folders` is forbidden: that path is
exactly where a stale or zero budget archives a folder.

`Scanner.ScanAll` sources its loop the same way, and `Scanner.ScanFolder`'s parameter changes from
`config.FolderConfig` to `catalog.FolderConfigRecord`. TOML `persistent` globs are already folded
into `persistent_paths` by reconciliation, so the scanner reads globs from the DB only and the
`folder.Persistent` merge disappears.

**Every path lookup, exhaustively.** Invariant 5 says nothing at runtime scans `cfg.Folders` for a
path. There are **five** such lookups in the tree today and all five change in step 4. Naming only
the first two would leave `§3.4`'s promise ("an orphaned folder's files stay browsable and
downloadable") false, because the download handler would 404 on every file in a renamed folder.

| Call site | Today | After |
|---|---|---|
| `internal/daemon/daemon.go:EnforceQuotas` | `range d.cfg.Folders` | `cat.ActiveFolderConfigs()` |
| `internal/daemon/scanner.go:ScanAll` | `range s.cfg.Folders` | `cat.ActiveFolderConfigs()` |
| `internal/api/handlers_files.go:100 folderPath` | loops `s.cfg.Folders`, returns `""` | `cat.FolderPath(label)` |
| `internal/daemon/restorer.go:72 folderPath` | loops `r.cfg.Folders`, errors | `cat.FolderPath(label)` |
| `internal/daemon/daemon.go:237 generateThumbnails` | builds a `map[label]path` from `cfg.Folders`; a miss yields `""` and `filepath.Join("", rel)` reads relative to the daemon's cwd | `cat.FolderPath(label)`, and a file whose folder has no row is skipped with a logged warning |

```go
// FolderPath returns the syncthing_path of any folder_configs row, including
// disabled and orphaned (present_in_toml = 0) ones. It is the single runtime
// source of a folder's on-disk location.
//   SELECT syncthing_path FROM folder_configs WHERE folder_label = ?
// Returns ErrNoSuchFolder if there is no row.
func (c *Catalog) FolderPath(label string) (string, error)
```

**What "downloadable" means for an orphaned folder.** `GET /files/{id}/download` uses
`cat.FolderPath`, which succeeds for an orphaned row, so a synced file downloads normally. If the
path no longer exists on disk, `http.ServeFile` would 404 with an HTML body; the handler therefore
`os.Stat`s the resolved path first and returns **`409 {"error":"folder \"X\" is not currently
available"}`** when it is absent. `404` stays reserved for "no such file id". Archived files
download from cold storage regardless of the folder's state.

The upshot: a budget edited in the UI takes effect on the next enforcement tick, with no restart.

### 3.6 Folder readiness — the guard that must run before any move

A folder's `syncthing_path` existing as a directory is **not** proof that the folder is available.
The single most dangerous case in this whole design is an unmounted mountpoint: the directory exists
and is empty, `os.Stat` succeeds, `Restorer.Restore`'s `os.MkdirAll(filepath.Dir(dst), 0755)`
(`restorer.go:42`) cheerfully materialises the whole tree on the **root** filesystem, the restore
writes 400 files there, `crossDeviceMove` deletes each cold-storage source, and when the drive is
finally mounted every one of those files is shadowed and invisible while the archive copies are
gone. A bare `os.Stat` on the root does not catch it.

```go
var ErrFolderUnavailable = errors.New("folder is not currently available")

// FolderReady reports whether a folder may be scanned, enforced, or moved
// into or out of. It is cheap (two stats) and is called on EVERY entry point
// that touches the folder's filesystem.
func FolderReady(syncthingPath string) error {
	info, err := os.Stat(syncthingPath)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrFolderUnavailable, syncthingPath)
	}
	// Syncthing creates .stfolder inside every folder precisely so that an
	// unmounted or wiped volume is distinguishable from an empty one. Use it
	// for exactly that. It is a directory in current Syncthing and was a file
	// in older versions; accept either.
	if _, err := os.Stat(filepath.Join(syncthingPath, ".stfolder")); err != nil {
		return fmt.Errorf("%w: %s has no .stfolder marker (unmounted, or not a syncthing folder)",
			ErrFolderUnavailable, syncthingPath)
	}
	return nil
}
```

**Every one of these calls it, before any status write and before any `MkdirAll`:**

- `Scanner.ScanFolder` — returns the error; the walk never starts, so the missing sweep never runs
  (§5.9).
- `Mover.RestoreByID` — returns `ErrFolderUnavailable` → `409`.
- `Mover.ArchiveByID` — returns `ErrFolderUnavailable` → `409`.
- The job worker — treats `ErrFolderUnavailable` like `ErrColdStorageFull`: it fails the **whole
  job** fast via `FailRemainingItems` rather than producing 400 identical failures (§5.5).
- `Daemon.enforceFolder` — skips the folder for this tick and writes a deduped
  `folder_unavailable` event.

This subsumes the `os.Stat` root check §5.9 used to specify on its own, and it is the reason this
document does **not** adopt the st_dev-vs-parent mountpoint comparison a reviewer proposed: comparing
device numbers only detects a mountpoint that is *declared* as one, needs a config key to declare
it, and still says nothing about a wiped volume. `.stfolder` is the marker Syncthing itself invented
for this exact question, costs one `stat`, and needs no configuration.

**Test-fixture obligation.** Every test that builds a folder on disk (`tests/e2e/helpers_test.go`,
`internal/daemon/*_test.go`, `scripts/seed-demo`) must `os.MkdirAll(path/".stfolder")`. Add a
`newSyncthingFolder(t, dir)` helper in step 4 and use it everywhere; a fixture that forgets will
fail loudly with `ErrFolderUnavailable` rather than silently skipping assertions.

### 3.7 Syncthing folder type — the assumption that silently voids the product (D5-adjacent)

Coldstore frees space on the owner's phone by **deleting** files from the server's Syncthing folder
and letting Syncthing propagate the deletion. That only works if the server's folder is
**Send & Receive**. Syncthing's own recommendation for a backup target — which is exactly what this
microserver is — is **Receive Only**, and on a Receive Only folder a local deletion is never
propagated. It is recorded as a "Local Deletion" and the folder shows "Out of Sync" with a
**Revert Local Changes** button.

The failure is silent and then explosive. Day one: coldstore archives 11 GB across 2,400 files, its
UI shows the folder comfortably under budget, and the phone still holds every byte — the entire
premise of the product defeated with nothing reporting it. Day two: the owner clicks Revert Local
Changes to clear the red banner in Syncthing, all 2,400 files come back, `UpsertFile` flips the rows,
and the next quota tick re-archives all of them. Every attempt to clear the out-of-sync state
restarts the loop.

**So §9 does not delete `[syncthing] api_url` / `api_key`. It makes them load-bearing.** A read-only
Syncthing client, one endpoint, in a new `internal/syncthing` package:

```go
type FolderInfo struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Path  string `json:"path"`
	Type  string `json:"type"` // "sendreceive" | "receiveonly" | "sendonly" | ...
}

// Folders does GET {apiURL}/rest/config/folders with X-API-Key, 5s timeout.
func (c *Client) Folders(ctx context.Context) ([]FolderInfo, error)
```

At startup, immediately after `reconcileFolders`:

- **`api_key` is set.** Fetch the folder list. Match each declared folder by `syncthing_id`, falling
  back to `path`. For a folder whose `Type != "sendreceive"`, or whose Syncthing `Path` disagrees
  with the TOML `path`, set `folder_configs.enabled = 0`, write a
  `syncthing_misconfigured` event naming the type, and log a warning. The UI renders the folder card
  with "CameraRoll is a **Receive Only** Syncthing folder — coldstore cannot free space on your
  devices. Change it to Send & Receive in Syncthing, then re-enable it here."
  A folder Syncthing does not know about at all gets the same treatment.
- **The Syncthing API is unreachable or returns an error.** Log a warning and continue. Do not
  disable anything: a Syncthing restart must not switch coldstore off.
- **`api_key` is empty.** Log one startup warning naming the requirement, and write an
  `syncthing_unverified` event once per startup. Enforcement proceeds. The ConfigPanel shows an
  "unverified" chip on every folder.

This is the only Syncthing API call in the codebase and it is read-only. `docs/deployment.md` and
`coldstore.toml.example` must both state, next to the folder table, that **coldstore requires
Send & Receive folders because offloading is implemented as a deletion**, and that setting `api_key`
lets coldstore check this for you.

### 3.8 `.stignore` and coldstore's temp files

Both directions of a move write to a temp sibling and rename into place (§5.2b). On the **restore**
side that sibling lands inside the Syncthing folder, named `<name>.coldstore-tmp`, and Syncthing
will happily start syncing it to the phone before it is renamed away. That wastes bandwidth and
leaves transient junk; it loses nothing.

Because the cost is a nuisance and not a hazard, this is a **warning, not a refusal**: at startup,
read `<syncthingPath>/.stignore`; if it does not contain a line matching `*.coldstore-tmp`, log a
warning, write a deduped `stignore_missing` event (24-hour window), and show a ConfigPanel chip with
the exact line to add. Coldstore never edits `.stignore` itself — it is the owner's Syncthing
configuration.

Contrast with §3.6, which **refuses**: a missing `.stfolder` can cost the owner 400 files, a missing
`.stignore` line costs some cellular data.

---

## 4. Pin and budget accounting (D2, D3)

### 4.1 How `OffloadCandidates` excludes pinned rows

```go
// OffloadCandidates returns unpinned synced files in a folder, oldest first by
// max(mtime, atime). Persistent-glob filtering happens in the caller, in Go.
func (c *Catalog) OffloadCandidates(folder string, limit int) ([]FileRecord, error) {
	if limit <= 0 {
		limit = 5000
	}
	rows, err := c.db.Query(`SELECT `+fileColumns+`
		FROM files
		WHERE folder_label = ? AND status = 'synced' AND pinned = 0
		ORDER BY MAX(file_mtime, file_atime) ASC
		LIMIT ?`, folder, limit)
	...
}
```

Three things about this query:

- `AND pinned = 0` is the SQL half of D2. It is served by `idx_files_folder_status_pinned`.
- The `LIMIT` is new and mandatory. Without it a 50,000-file camera roll materialises 50,000
  `FileRecord`s per tick. Candidates are consumed oldest-first and the loop stops at the budget, so
  5,000 is always more than one pass needs; if a pass exhausts the list while still over budget, the
  next tick picks up where it left off.
- The grace cutoff stays **out** of this SQL. `file_mtime`/`file_atime` are TEXT columns written by
  the driver in `2006-01-02 15:04:05.999999999-07:00` form; comparing a bound Go time against them is
  a string comparison with a timezone-dependent result. Grace is evaluated in Go, on real
  `time.Time` values, in `enforceFolder`.

`enforceFolder` additionally does `if file.Pinned { continue }` at the top of each iteration. That
is redundant with the SQL and deliberately so: it means a future caller that forgets the SQL filter
still cannot offload a pinned file. The third and authoritative layer is the CAS itself: the
**automatic** offload path claims with `WHERE id = ? AND status = 'synced' AND pinned = 0` (§4.6).

**A rescan must never clear a pin.** `UpsertFile`'s `ON CONFLICT DO UPDATE SET` list must not
mention `pinned`, `pinned_at`, `pin_source` or `thumb_attempts`. It is safe by omission today; add a
comment saying why, and a regression test (§8, step 3).

### 4.1a Persistent glob semantics — `filepath.Match` is the wrong matcher

> **Status: IMPLEMENTED** (`daemon.IsPersistent`, `internal/daemon/scanner.go`), with the three
> rules below, `match_count` on `GET /folders/{label}/persistent`, and the rules restated in
> `coldstore.toml.example`, `docs/deployment.md` and the ConfigPanel help text. The example
> patterns are now `Albums/Starred/` and `Favorites/`. The "logged once at load" half is not
> implemented: a malformed pattern matches nothing and shows `protects 0 files`, which is the
> feedback that matters, but nothing writes a log line about it.

`IsPersistent` (`scanner.go`) uses `filepath.Match`, whose `*` **does not cross a path separator**.
Both patterns shipped in `coldstore.toml.example` are of the form `Albums/Starred/*`, so today they
protect exactly the loose files directly inside `Albums/Starred/` and **nothing nested**. An owner
whose starred albums are organised by year watches the folder they explicitly marked never-archive
empty off their phone one year-subfolder at a time while two loose files at its root stay put. §3.5
folds these TOML globs into `persistent_paths` and §7.9 puts a UI editor in front of them for the
first time, so this latent bug is about to get a front door.

Replace it. No new dependency; the rules are short and total:

```go
// IsPersistent reports whether relPath is protected by any pattern.
// relPath is always slash-separated and relative to the folder root.
//
// Semantics, checked in this order for each pattern:
//   1. A pattern ending in "/" or "/*" protects the ENTIRE SUBTREE beneath it.
//      "Albums/Starred/" and "Albums/Starred/*" both protect
//      "Albums/Starred/2019/IMG_1.jpg".
//   2. Otherwise, filepath.Match against the full relative path.
//      "Albums/*.raw" matches "Albums/a.raw" and not "Albums/x/a.raw".
//   3. Otherwise, if the pattern contains no "/", filepath.Match against the
//      BASE NAME. "*.raw" protects "Albums/2019/a.raw".
// A pattern that filepath.Match rejects as malformed is skipped and logged
// once at load; it never silently matches everything.
func IsPersistent(relPath string, patterns []string) bool
```

`docs/deployment.md`, `coldstore.toml.example` and the ConfigPanel help text all state rules 1–3.
The two example patterns become `Albums/Starred/` and `Favorites/`, fixed in the same commit.

**The editor must prove a pattern does something.** A pattern that protects nothing looks identical
to one that protects everything. `GET /api/v1/folders/{label}/persistent` therefore returns a
`match_count` per pattern (`SELECT COUNT(*) FROM files WHERE folder_label = ?` filtered in Go by
`IsPersistent`, one pass over the folder's relative paths, capped at the folder's row count), and
the ConfigPanel renders "protects N of M files in this folder" under each entry. A freshly typed
pattern showing `0` is the feedback that closes this class of bug for good.

### 4.1b `UpsertFile` must not adopt an archived file's path

`UpsertFile`'s conflict clause is today:

```sql
status = CASE WHEN files.status IN ('missing','archived') THEN 'synced' ELSE files.status END
```

The `'archived'` arm is a silent data-loss engine. When a path under an `archived` row reappears in
the Syncthing folder — a photo recovered from the phone's Recently Deleted as a re-encode, a
laptop reconnecting with an offline edit that outranks coldstore's deletion, or a Receive Only
folder's "Revert Local Changes" — the row flips to `synced` and overwrites `size_bytes` and
`file_mtime`. The cold-storage copy is then referenced by **no row**: invisible to `ListFiles`, to
`/files/ids`, to every job, to any cleanup. Worse, the next time that relative path is offloaded the
move writes to the identical archive path and destroys the original.

**Narrow the arm and make the conflict loud:**

```sql
status = CASE WHEN files.status = 'missing' THEN 'synced' ELSE files.status END
```

`UpsertFile` gains a return value: `(adopted bool, conflict bool, err error)`. `conflict` is true
when the pre-existing row's status was `archived`. On a conflict the scanner:

- leaves the row `archived` and **does not** overwrite `size_bytes` / `file_mtime` (the conflict
  branch of the `ON CONFLICT DO UPDATE` writes only `last_seen`),
- writes one `conflict` event per path per scan, with both sizes in `details`,
- and the file appears in the UI under a **Conflicts** filter with a two-button resolution: *Keep the
  device copy* (delete the cold-storage file, set the row `synced` with the on-disk metadata) or
  *Keep the archived copy* (rename the device file aside as `<name>.coldstore-superseded` and leave
  the row `archived`).

An archived file's path reappearing is precisely the state a control plane should be loud about.
Both resolutions run through `Mover` (`Mover.ResolveConflict(id, keep string)`), take the mutex, and
are exposed as `POST /api/v1/files/{id}/resolve {"keep":"device"|"archive"}`.

**Second half of the same fix, in the mover:** `crossDeviceMove` never truncates (§5.2b). Its
destination is opened `O_WRONLY|O_CREATE|O_EXCL` and a pre-existing destination is an error, not an
overwrite.

### 4.2 `FolderSizes` — the one aggregate

```go
// FolderSizes returns the folder's on-device footprint and the pinned subset
// of it, in one pass.
//
// Both figures count status IN ('synced','archiving','restoring'):
//   - synced     : on device now
//   - archiving  : still on device, a move out is in flight
//   - restoring  : a move in is in flight; the bytes are about to land
// 'archived' and 'missing' are not on the device and are excluded from both.
//
// pinned is a SUBSET of total, never an addend. Per D3 the budget is a hard
// cap on the on-device footprint whether pinned or not, so pinned bytes are
// included in total and reported alongside it.
func (c *Catalog) FolderSizes(folder string) (total, pinned int64, err error) {
	err = c.db.QueryRow(`
		SELECT COALESCE(SUM(size_bytes), 0),
		       COALESCE(SUM(CASE WHEN pinned = 1 THEN size_bytes ELSE 0 END), 0)
		FROM files
		WHERE folder_label = ? AND status IN ('synced','archiving','restoring')
	`, folder).Scan(&total, &pinned)
	return
}
```

`FolderSyncedSize` is **not deleted in step 3.** Its callers are `internal/daemon/daemon.go:131`,
`internal/api/handlers_status.go:42`, `internal/catalog/catalog_test.go:197`
(`TestFolderSyncedSize`) and `tests/e2e/api_status_test.go:62` (`TestAPIStatusFolderSyncedSize`), and
step 3 rewrites none of them. Step 3 adds `FolderSizes` and leaves `FolderSyncedSize` as a
`// Deprecated:` one-line shim delegating to it. Step 4 converts `daemon.go`, step 6 converts
`handlers_status.go` and both tests, and **step 6 deletes the shim**. Same rule as
`UpsertFolderConfig` (§3.1): a method is removed in the step that removes its last caller, never
earlier, so every step really does leave `go build ./...` green over the whole module.

Widening the status set to the two transient states is what keeps accounting correct once the job
worker has moves in flight concurrently with a quota tick.

Because both columns in the aggregate come from `idx_files_folder_status_pinned`, the query never
touches the table heap.

Note the interaction with pinning an archived file: an archived-and-pinned row contributes to
neither figure, which is correct — its bytes are in cold storage, not on the device. `pinned` on an
archived row is a latent instruction that takes effect when the file is restored.

### 4.3 The precise `over_budget` and wedge rules

Let `budget = fc.BudgetBytes`, and `(total, pinned) = FolderSizes(label)` evaluated **after** an
enforcement pass has finished offloading whatever it could.

```
if budget <= 0:
    enforcement is skipped entirely
    over_budget = false
    wedged      = false

else:
    over_budget = total  > budget
    wedged      = pinned > budget
```

- `over_budget` is the honest "this folder does not fit" flag. It is legitimately true for one tick
  when every remaining candidate is inside its grace window or matches a persistent glob. It drives
  the budget bar's over-budget treatment, nothing louder.
- `wedged` is the D3 condition: pinned bytes **alone** exceed the budget, so even offloading every
  unpinned file in the folder cannot converge. This is what drives the banner and the warning event.

Warning on `over_budget` instead of `wedged` would fire "nothing left to offload" during ordinary
operation, every time a burst of new photos lands inside the grace window. Do not do it.

The same two expressions are evaluated in exactly two places, and must agree:

- `Daemon.enforceFolder`, at the end of a pass, to drive the warning latch.
- `Server.handleStatus`, live per request, to populate the API response.

Put them in one place so they cannot drift:

```go
// package catalog
type FolderBudget struct {
	BudgetBytes int64
	TotalBytes  int64
	PinnedBytes int64
}

func (b FolderBudget) OverBudget() bool { return b.BudgetBytes > 0 && b.TotalBytes > b.BudgetBytes }
func (b FolderBudget) Wedged() bool     { return b.BudgetBytes > 0 && b.PinnedBytes > b.BudgetBytes }
```

### 4.4 The enforcement epilogue, and wedge warning deduplication

**Structure first, because `enforceFolder` has four exits and the epilogue must run on all of them.**
Today the function returns early on the happy path (`if syncedSize <= budget { return nil }`), §4.3
skips it entirely for `budget <= 0`, and §5.2 abandons the folder on `ErrColdStorageFull`. If the
latch logic lives at the textual end of the function, an unwedged folder that is now within budget
takes the happy-path return and **never clears its latch**, and a `budget = 0` folder freezes
`last_enforced_at` so the ConfigPanel reads "last checked 6 days ago" — indistinguishable from a dead
daemon.

So: only the **offload loop** is skipped by those conditions. The epilogue is a `defer` and runs on
every path, including a panic recovered upstream.

```go
func (d *Daemon) enforceFolder(ctx context.Context, fc catalog.FolderConfigRecord) (err error) {
	defer d.enforceEpilogue(fc.FolderLabel, fc.BudgetBytes)   // ALWAYS runs

	if err := FolderReady(fc.SyncthingPath); err != nil {
		d.cat.LogEventDeduped("folder_unavailable", fc.FolderLabel, "", err.Error(), 60)
		return err                       // epilogue still stamps last_enforced_at
	}
	if fc.BudgetBytes <= 0 {
		return nil                       // "unconfigured", not "archive everything"
	}
	... the offload loop ...
}

// enforceEpilogue recomputes the budget picture from scratch and applies the
// latch. It never trusts a value the loop carried.
func (d *Daemon) enforceEpilogue(label string, budget int64) {
	now := time.Now().Unix()
	total, pinned, err := d.cat.FolderSizes(label)
	if err != nil {
		d.logger.Error("epilogue: folder sizes", "folder", label, "error", err)
		return
	}
	b := catalog.FolderBudget{BudgetBytes: budget, TotalBytes: total, PinnedBytes: pinned}

	fc, err := d.cat.FolderConfigByLabel(label)  // re-read: the UI may have changed it mid-pass
	if err != nil {
		d.logger.Error("epilogue: folder config", "folder", label, "error", err)
		return
	}

	switch {
	case b.Wedged():
		if fc.WedgeWarnedAt == nil || now-*fc.WedgeWarnedAt >= 86400 {
			d.cat.LogEvent("folder_wedged", label, "", fmt.Sprintf(
				"%s pinned exceeds %s budget - nothing left to offload",
				humanSI(pinned), humanSI(budget)))
			d.logger.Warn("folder wedged: pinned bytes exceed budget",
				"folder", label, "pinned", pinned, "budget", budget)
			d.cat.SetWedgeWarnedAt(label, &now)
		}
	default:
		if fc.WedgeWarnedAt != nil {
			d.cat.LogEvent("folder_unwedged", label, "", "folder no longer wedged")
			d.cat.SetWedgeWarnedAt(label, nil)
		}
	}

	d.cat.SetLastEnforcedAt(label, now)
}
```

Two consequences worth stating because they are easy to get wrong: `last_enforced_at` is stamped even
for a `budget = 0` folder and for an unavailable one — it means "the daemon looked at this folder",
which is what the UI claims it means. And the latch clears on the within-budget path, because the
epilogue does not care why the loop did or did not run.

Properties this gives:

- **One event per wedge episode**, not one per tick. With a 15-minute scan interval, the naive
  version would write 96 rows a day per wedged folder.
- **Survives restarts.** An in-memory `map[string]bool` on the `Daemon` struct resets on every
  restart, so a systemd restart loop or a routine deploy re-floods the table. The column does not.
- **Re-warns on a genuine re-wedge.** Clearing the latch when the condition resolves means the next
  wedge warns immediately, which is the behaviour the owner asked for.
- **Re-warns daily while wedged.** The 24-hour refresh means a folder that stays wedged for a week
  produces seven rows, not one from last Tuesday that scrolls off the activity list. It is a
  persistent problem and it should keep saying so.

#### Event types are specific, and `"warning"` never enters the dedup path

`LogEventDeduped` keys its suppression window on `(event_type, folder)` and nothing else. If two
unrelated conditions both log under the coarse type `"warning"` for the same folder, **they suppress
each other**: a wedge row written at 09:00 makes the 09:20 "cold storage at 4.2% free" row vanish,
and offloading stops for every folder with the only surface that would have said so — §7.9's
Activity list — showing nothing. The existing call at `daemon.go:207` is literally
`LogEvent("warning", folder.Label, ...)`.

**Normative event-type vocabulary.** Every event this design writes uses one of these; `"warning"`
and `"info"` are retired as event types.

| `event_type` | Written by | Deduped |
|---|---|---|
| `archive`, `restore` | `Mover`, per file | no |
| `folder_wedged` / `folder_unwedged` | enforcement epilogue | latch, §4.4 |
| `cold_storage_low` | `enforceFolder` / `Mover` | 60 min |
| `folder_unavailable` | `enforceFolder`, scanner | 60 min |
| `bulk_eviction` | enforcement loop, §5.10 | no |
| `conflict` | scanner, §4.1b | per path per scan |
| `restore_conflict` | `Mover.RestoreByID`, §5.2 | no |
| `source_vanished`, `source_changed` | `crossDeviceMove`, §5.2b | no |
| `archive_unconfirmed` | recovery, §5.6 | no |
| `stray_archive` | orphan sweep, §5.11 | 24 h |
| `syncthing_misconfigured`, `syncthing_unverified`, `stignore_missing` | startup, §3.7/§3.8 | 24 h |
| `folder_seeded` | reconciliation, §3.2 | no |
| `job_failed` | worker, §5.5 | no |

§7.9's Activity list filters to the **warning-class** types — everything above except `archive`,
`restore` and `folder_seeded` — so a burst of per-file `archive` rows can never push the wedge
warning out of the last ten.

For the *other* flooding warning — the cold-storage-below-5%-free event at `daemon.go:207`, which
today writes one row per folder per tick while the condition holds — use a windowed helper, because
that condition is global and transient rather than a per-folder state transition:

```go
// LogEventDeduped inserts an event only if no event with the same type and
// folder was recorded within the last windowMinutes. The window is evaluated
// entirely inside SQLite so that both sides of the comparison are
// CURRENT_TIMESTAMP-format text; never bind a Go time.Time here.
func (c *Catalog) LogEventDeduped(eventType, folder, path, details string, windowMinutes int) error {
	_, err := c.db.Exec(`
		INSERT INTO events (event_type, folder, path, details)
		SELECT ?, ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM events
			WHERE event_type = ? AND folder = ? AND timestamp > datetime('now', ?)
		)`,
		eventType, folder, path, details,
		eventType, folder, fmt.Sprintf("-%d minutes", windowMinutes))
	return err
}
```

Call it with a 60-minute window for `cold_storage_low` and `folder_unavailable`, and 24 hours for
the startup-configuration events. `idx_events_folder_type_ts` serves the `NOT EXISTS` probe. Because
every deduped type is now distinct from every other type, two conditions can never suppress each
other.

**Rejected alternative** for the wedge warning: implementing it with `LogEventDeduped` and a long
window. Rejected because a window cannot distinguish "still wedged" from "wedged again after being
fixed" — a folder unwedged and re-wedged inside the window would warn zero times, which is the one
case the owner most needs to hear about.

### 4.5 Pinning is visible, not silent

D3 requires that the consequences of pinning be loud. Three surfaces, all specified elsewhere in
this document:

1. `GET /api/v1/status` reports `pinned_bytes`, `over_budget` and `wedged` per folder (§6.3).
2. `POST /api/v1/files/{id}/pin` returns a non-null `warning` string **iff `pinned_bytes >
   budget_bytes` after the write** — a state test, not a transition test — and **still performs the
   pin** (§6.5). The user is allowed to wedge their own folder knowingly. The state reading is
   normative: with a transition reading, pinning one more 5 MB photo into an already-wedged folder
   would report `warning: null` and look routine, which is the opposite of D3's intent. §6.5 and
   this paragraph must not disagree; if they ever do, this one is a typo.
3. The StatusBar draws a distinct pinned segment inside the budget bar and shows the wedge banner
   (§7.6).

### 4.6 Pin primitives, and the rule that a pin is never lost to a race

```go
// setPinned is UNEXPORTED. Every pin write goes through Mover.SetPinned /
// Mover.SetPinnedBulk (invariant 2), which hold the mover mutex, so a pin can
// never commit while a move of that file is in flight.
//   pin:   UPDATE files SET pinned=1, pinned_at=?, pin_source=? WHERE id = ?
//   unpin: UPDATE files SET pinned=0, pinned_at=NULL, pin_source='' WHERE id = ?
func (c *Catalog) setPinned(id int64, pinned bool, source string, at int64) error

//   same, WHERE id IN (?,?,...)   -- chunked at 500 ids
func (c *Catalog) setPinnedBulk(ids []int64, pinned bool, source string, at int64) (int64, error)

// MarkRestored: status, archived_at and the pin in ONE statement. It sets
// pin_source='restore' ONLY if the file was not already pinned by the user —
// a user pin is never downgraded to an auto-pin.
//   UPDATE files SET status='synced', archived_at=NULL, pinned=1,
//                    pinned_at   = COALESCE(pinned_at, ?),
//                    pin_source  = CASE WHEN pin_source='user' THEN 'user'
//                                       ELSE 'restore' END
//   WHERE id = ? AND status='restoring'
func (c *Catalog) MarkRestored(id int64, at int64) (bool, error)

// MarkArchived does NOT touch pinned/pinned_at/pin_source. See below.
//   UPDATE files SET status='archived', archived_at=?
//   WHERE id = ? AND status='archiving'
func (c *Catalog) MarkArchived(id int64, at time.Time) (bool, error)

// ClaimFile is the generic CAS.
//   UPDATE files SET status = ? WHERE id = ? AND status = ?
func (c *Catalog) ClaimFile(id int64, from, to FileStatus) (bool, error)

// ClaimUnpinnedFile is the CAS the AUTOMATIC offload path uses, and the only
// one it may use.
//   UPDATE files SET status='archiving'
//   WHERE id = ? AND status='synced' AND pinned = 0
func (c *Catalog) ClaimUnpinnedFile(id int64) (bool, error)
```

`MarkRestored` sets status and pin in **one statement**, so a crash cannot leave a restored file
unpinned and immediately re-archivable. This single statement is the authoritative fix for the
reproduced bug (restored 10:07:20, re-offloaded 10:07:53).

**Why `MarkArchived` no longer clears the pin.** The old rule ("archiving is a later, explicit user
action than the pin") is true for the manual archive endpoint and the archive job item, and exactly
backwards for a **daemon-initiated** offload, where the pin is the later action. The concrete loss:
the quota tick starts a 45-second cross-device copy of a 2 GB video; ten seconds in the owner sees it
about to go and clicks the pin; the handler commits `pinned=1` and paints a filled badge; the move
finishes and `MarkArchived` sets `pinned=0`; Syncthing deletes the file from the phone and the
laptop. The owner watched the UI confirm the pin and the file left anyway.

The fix is three rules, all required:

1. **`MarkArchived` never writes `pinned`.** A pin survives an archive as a latent instruction that
   takes effect on restore, exactly as §4.2 already describes for pinning an archived file.
2. **Automatic offload claims with `ClaimUnpinnedFile`.** A pin that commits before the claim makes
   the claim fail, and `enforceFolder` moves to the next candidate. This is the third and
   authoritative pin layer from §4.1.
3. **Explicit archive clears the pin itself, inside the mover, before claiming.**
   `Mover.ArchiveByID` (the endpoint and the job item both reach it) does
   `setPinned(id, false, "", 0)` and then `ClaimFile(id, 'synced', 'archiving')`. The user who
   selects a pinned file and presses Archive means it.

Because `Mover.SetPinned` takes the mover mutex, a pin request that arrives mid-move **blocks** until
that move finishes and then commits against the settled row. If the file ended up `archived`, the pin
is recorded on the archived row and the `200` response carries the real post-write `FileRecord`
(`status: "archived"`) plus
`warning: "this file finished archiving while the pin was being applied; the pin will take effect
when you restore it"`. The owner is told the truth instead of being shown a lie. That exact wording
is reserved for this case — a row that was **not** archived when the request arrived; §6.5 has the
other wording, for the ordinary pin on a file that was already in cold storage.

**Rejected alternative:** letting the pin win by discarding a completed, byte-verified destination
and rewriting the source. Reverting a finished move is strictly more dangerous than the problem it
solves — it re-introduces a second write path with its own torn-move window — and it violates
invariant 9.

`UpdateFileStatus` stays for the transient and rollback transitions (`synced`→`archiving`,
`archived`→`restoring`, and the revert paths) and must **never** touch `pinned` — a pin flag written
on a transient flip would be silently set or cleared by every rollback.

---

## 5. Job queue (D4)

### 5.1 Placement

`internal/daemon/mover.go` and `internal/daemon/worker.go`. Not a new package: the worker needs
`Offloader` and `Restorer`, and `internal/api` already imports `internal/daemon`, so a separate
`internal/jobs` package would either duplicate the movers or create an import cycle.

### 5.2 `Mover` — the single serialization point

```go
type Mover struct {
	mu     sync.Mutex
	cat    *catalog.Catalog
	cfg    *config.Config
	logger *slog.Logger

	// Injectable seams. Tests replace them; production uses the defaults.
	spaceCheck func(string) (float64, error) // default AvailableSpacePercent
	moveFn     func(src, dst string) error   // default moveFile (section 5.2b)
}

var (
	ErrFileStateChanged  = errors.New("file state changed before the move could start")
	ErrColdStorageFull   = errors.New("cold storage below 5% free")
	ErrFolderUnavailable = errors.New("folder is not currently available")
	ErrDestinationExists = errors.New("a file already exists at the destination")
	ErrSourceChanged     = errors.New("the source file changed during the move")
)

// RestoreByID moves one archived file back to its syncthing folder and pins it.
func (m *Mover) RestoreByID(id int64) (*catalog.FileRecord, error)

// ArchiveByID moves one synced file to cold storage, clearing its pin first.
// It applies the cold-storage free-space guard. It deliberately does NOT apply
// the grace period or the budget check: a manual or job-driven archive is an
// explicit user instruction.
func (m *Mover) ArchiveByID(id int64) (*catalog.FileRecord, error)

// SetPinned / SetPinnedBulk are the ONLY writers of files.pinned (invariant 2).
// They take the same mutex, so a pin cannot commit mid-move.
func (m *Mover) SetPinned(id int64, pinned bool) (*catalog.FileRecord, error)
func (m *Mover) SetPinnedBulk(ids []int64, pinned bool) (int64, error)

// ResolveConflict implements the two-button resolution from section 4.1b.
func (m *Mover) ResolveConflict(id int64, keep string) (*catalog.FileRecord, error)
```

**`Offloader` and `Restorer` survive, stripped of every catalog write.** This is not optional and it
is not a style question: leaving `Offloader.Offload` intact would mean it still finishes with
`UpdateFileStatus(StatusArchived)` rather than `MarkArchived`, so a manual archive of a pinned file
would take a completely different transition than the one §4.6 specifies. Inlining the whole move
into `Mover` instead would drop the `os.MkdirAll(filepath.Dir(dst), 0755)` that both types do today —
the first restore of `2024/06/IMG_0001.jpg` into a folder where `2024/06/` no longer exists would
`ENOENT` — and drop `cleanEmptyParents`, so the archive tree would accumulate empty directories
forever.

So, after step 7:

```go
// Offloader and Restorer are PURE FILESYSTEM operations. No catalog access,
// no status writes, no events. They own path construction, MkdirAll and
// cleanEmptyParents; Mover owns every row transition.
func (o *Offloader) ArchivePath(folderLabel, relPath string) string
func (o *Offloader) MoveOut(src, dst string) error  // MkdirAll(dir(dst)) + moveFn(src, dst)
func (r *Restorer) MoveIn(src, dst string) error    // MkdirAll(dir(dst)) + moveFn(src, dst)
                                                    // + cleanEmptyParents(dir(src), archiveRoot)
```

`Offloader.Offload` and `Restorer.Restore` — the catalog-writing versions — are deleted in step 7
together with their last callers (`daemon.go:196`, `handlers_restore.go`). `Restorer.folderPath` is
deleted with them; path resolution is `cat.FolderPath` (§3.5).

#### `ArchiveByID`, in full

```
m.mu.Lock(); defer m.mu.Unlock()

rec, err := cat.GetFileByID(id)                  // re-read; never trust the caller
if err != nil: return nil, err                   // ErrNoSuchFile -> 404

if rec.Status == 'archived': return rec, nil     // invariant 12: already there, no-op success
if rec.Status != 'synced':   return nil, ErrFileStateChanged

fc, err := cat.FolderConfigByLabel(rec.FolderLabel)
if err != nil || !fc.PresentInTOML: return nil, ErrFolderUnavailable
if err := FolderReady(fc.SyncthingPath); err != nil: return nil, ErrFolderUnavailable

free, err := m.spaceCheck(cfg.Storage.ColdStoragePath)
if err != nil: return nil, err
if free < 5.0:
    cat.LogEventDeduped("cold_storage_low", rec.FolderLabel, "",
        fmt.Sprintf("offload paused: cold storage at %.1f%% free", free), 60)
    return nil, ErrColdStorageFull

src := filepath.Join(fc.SyncthingPath, rec.RelativePath)
dst := offloader.ArchivePath(rec.FolderLabel, rec.RelativePath)

// Explicit archive clears the pin itself (section 4.6 rule 3).
cat.setPinned(id, false, "", 0)

ok, err := cat.ClaimFile(id, 'synced', 'archiving')     // automatic path: ClaimUnpinnedFile(id)
if err != nil: return nil, err
if !ok: return nil, ErrFileStateChanged

if err := offloader.MoveOut(src, dst); err != nil {
    if _, rerr := cat.ClaimFile(id, 'archiving', 'synced'); rerr != nil { log loudly }
    return nil, err                                      // ErrDestinationExists, ErrSourceChanged, io
}

ok, err = cat.MarkArchived(id, time.Now())
if err != nil: return nil, err
if !ok {
    // Invariant 11. The bytes moved but the row says something else. This is
    // the loudest thing the daemon can say short of exiting.
    logger.Error("MarkArchived affected 0 rows after a completed move",
        "id", id, "path", rec.RelativePath, "dst", dst)
    cat.LogEvent("archive_unconfirmed", rec.FolderLabel, rec.RelativePath,
        "file moved to cold storage but the catalog row was not in 'archiving'")
    return nil, fmt.Errorf("archive of %d completed but could not be recorded", id)
}
cat.LogEvent("archive", rec.FolderLabel, rec.RelativePath, ...)
return cat.GetFileByID(id)
```

#### `RestoreByID`, in full

```
m.mu.Lock(); defer m.mu.Unlock()

rec, err := cat.GetFileByID(id)
if err != nil: return nil, err

if rec.Status == 'synced': return rec, nil       // invariant 12: no-op success
if rec.Status != 'archived': return nil, ErrFileStateChanged

fc, err := cat.FolderConfigByLabel(rec.FolderLabel)
if err != nil || !fc.PresentInTOML: return nil, ErrFolderUnavailable
if err := FolderReady(fc.SyncthingPath); err != nil: return nil, ErrFolderUnavailable

src := filepath.Join(cfg.Storage.ColdStoragePath, "archive", rec.FolderLabel, rec.RelativePath)
dst := filepath.Join(fc.SyncthingPath, rec.RelativePath)

// The destination-occupied check. See below — this one is not optional.
if _, err := os.Lstat(dst); err == nil {
    os.Rename(src, src+".coldstore-superseded")      // nothing is destroyed
    cat.ClaimFile(id, 'archived', 'synced')          // adopt the on-disk copy
    cat.LogEvent("restore_conflict", rec.FolderLabel, rec.RelativePath,
        "a copy already existed in the folder; coldstore adopted it and set the "+
        "archived copy aside as .coldstore-superseded")
    return nil, ErrDestinationExists                 // -> 409
}

ok, err := cat.ClaimFile(id, 'archived', 'restoring')
if err != nil: return nil, err
if !ok: return nil, ErrFileStateChanged

if err := restorer.MoveIn(src, dst); err != nil {
    if _, rerr := cat.ClaimFile(id, 'restoring', 'archived'); rerr != nil { log loudly }
    return nil, err
}

ok, err = cat.MarkRestored(id, time.Now().Unix())
if err != nil: return nil, err
if !ok { same loud path as MarkArchived }
cat.LogEvent("restore", rec.FolderLabel, rec.RelativePath, ...)
return cat.GetFileByID(id)
```

**Why the `os.Lstat(dst)` guard is load-bearing.** `moveFile` is `os.Rename`, which silently
replaces, falling back to a copy that (before §5.2b) used `os.Create`, which silently truncates. A
file can legitimately reappear at the destination while the row still says `archived` — that is
exactly what Syncthing does when a device that modified the file while offline reconnects and its
version vector outranks coldstore's deletion. Without the guard, a Restore click overwrites the
newer content with September's bytes, Syncthing sees an ordinary local edit rather than a conflict
(so it produces **no** conflict file) and pushes the stale version to every device. The catalog-sync
tick is up to 30 minutes away, so the UI happily offers Restore for that whole window.

**The free-space guard living inside `ArchiveByID`** is the fix for the survey's sharpest hazard:
today that check lives only in `Daemon.enforceFolder`, so a bulk archive of 800 files that called
`Offloader.Offload` directly would fill the ZFS pool with no brake at all.

`Daemon.enforceFolder` also goes through the mover — via the automatic variant that claims with
`ClaimUnpinnedFile` — so there is exactly one code path that moves a file out. When it returns
`ErrColdStorageFull` or `ErrFolderUnavailable`, `enforceFolder` abandons the folder for this tick;
the epilogue still runs (§4.4).

### 5.2b The move primitive — `moveFile` and `crossDeviceMove`

Syncthing dir → ZFS pool is cross-device, so `crossDeviceMove` is the **normal** path in production,
not the exotic one. It is rewritten completely. Ordering is the specification; do not reorder it.

```
moveFile(src, dst):
    if os.Lstat(dst) succeeds: return ErrDestinationExists    // invariant 10
    err := os.Rename(src, dst)
    if err == nil: return nil
    if !isCrossDeviceError(err): return err
    return crossDeviceMove(src, dst)

crossDeviceMove(src, dst):
     1. srcFile := os.Open(src);  st := fstat(srcFile)
        Record dev, ino, size, mtime, atime FROM THE DESCRIPTOR, not from a
        second path stat.
     2. tmp := dst + ".coldstore-tmp"
        os.Remove(tmp)                       // a leftover from a previous kill
        tmpFile := os.OpenFile(tmp, O_WRONLY|O_CREATE|O_EXCL, 0o644)
     3. h := sha256.New()
        n := io.Copy(io.MultiWriter(tmpFile, h), srcFile)
        if n != st.Size: cleanup; return size mismatch
     4. tmpFile.Sync(); tmpFile.Close()
        fsyncDir(filepath.Dir(tmp))
     5. RE-OPEN tmp and re-hash it from disk. Compare to h.
        (The old code fed both hashes from the same TeeReader/MultiWriter
        stream, so they could not differ by construction. It verified nothing.)
     6. os.Chmod(tmp, st.Mode())             // error is now FATAL, not swallowed
        os.Chtimes(tmp, st.Atime, st.Mtime)  // see below
     7. os.Rename(tmp, dst)                  // same device, atomic
        fsyncDir(filepath.Dir(dst))
        --- the destination is now COMMITTED and verified ---
     8. os.Lstat(src) and compare dev/ino/size/mtime against step 1.
        - unchanged            -> os.Remove(src); return nil
        - vanished (ENOENT)    -> LogEvent("source_vanished"); return nil
                                  The destination stays. Invariant 9: the bytes
                                  are kept and stay referenced by the row.
        - changed (different inode, size or mtime)
                               -> os.Rename(dst, dst+".coldstore-superseded")
                                  LogEvent("source_changed")
                                  return ErrSourceChanged
```

Four things this ordering buys, each of which was a live data-loss path:

**Destination committed before the source is touched.** Steps 2–7 never write to `dst` itself, so at
every instant `dst` either does not exist or is a complete, re-hashed copy. There is no state in
which both copies are partial, and recovery (§5.6) can decide by looking at `dst` alone.

**`os.Chtimes` — the finding that would otherwise flatten the whole library.** The old code called
`os.Chmod` and never `os.Chtimes`, so an archive reset the cold copy's mtime to the archive time and
a restore reset the live file's mtime to the restore time. Syncthing then propagated the new mtime to
the phone and the laptop, and the next scan wrote it over `files.file_mtime`, destroying the last
remaining record. A bulk restore of 400 photos spanning seven years lands them all dated today, at
the top of every gallery, in arbitrary order — and since `OffloadCandidates` orders by
`MAX(file_mtime, file_atime)`, every round-tripped file becomes permanently the newest thing in the
folder. Restoring **atime as well as mtime** matters for the same reason: atime is half the ordering
key.

Belt and braces on the catalog side: `UpsertFile` keeps `MIN(files.file_mtime, excluded.file_mtime)`
rather than blindly taking `excluded.file_mtime`, so no scan can ever move a file's recorded date
forward. (Both are TEXT in the same `2006-01-02 15:04:05` driver format, so `MIN` is a correct
lexicographic comparison for any timezone-consistent database; the daemon writes them all.)

**Step 8's re-stat closes the Syncthing rename-over window.** Syncthing updates a file by writing
`.syncthing.NAME.tmp` and renaming it over the target, so between step 1's `os.Open` and step 8 the
*path* can be rebound to a completely different inode. The old code opened by path once and then
deleted by path ninety seconds later with no check, so an edit made on the phone mid-copy was
unlinked from the server and the deletion propagated everywhere while cold storage kept the pre-edit
version. `enforceFolder`'s existing disk-mtime re-check (`daemon.go:186`) happens *before*
`ArchiveByID` is called and never repeats, so it does not help.

**`O_EXCL` on the temp file and `ErrDestinationExists` on the real one** mean no move can ever
truncate an existing file — invariant 10 — which is the second half of the §4.1b fix.

`fsyncDir(path)` is `f, _ := os.Open(path); f.Sync(); f.Close()`; its error is logged and not fatal,
since some filesystems reject it.

**A note on `.coldstore-tmp` inside the Syncthing folder.** On the restore side the temp sibling is
visible to Syncthing for the duration of the copy. That costs bandwidth, not data. §3.8 handles it
with an `.stignore` warning rather than a refusal.


### 5.3 Job state machines

**`jobs.state`**

```
pending ──claim first item──▶ running ──all items terminal, failed == 0──▶ done
                                 │
                                 ├─────all items terminal, failed  > 0──▶ failed
                                 │
                                 └─────worker gave up (FailJob)─────────▶ failed
running ──daemon restart──▶ pending      (recovery; see 5.6)
```

- `pending` — created, no item claimed yet. `started_at` is NULL.
- `running` — at least one item has been claimed. `started_at` set on the transition.
- `done` — every item reached a terminal state and none failed. `finished_at` set. Terminal.
- `failed` — every item reached a terminal state and at least one failed. `finished_at` set.
  Terminal. **"failed" means partial success with at least one failure**, not "nothing happened";
  `done` and `failed` counts tell the whole story and the UI shows both.

The **worker gave up** edge is not optional. Without it, a `ClaimJobItem` that errors leaves the job
in `running` forever, `NextPendingJob` (`WHERE state IN ('pending','running') ORDER BY id LIMIT 1`)
hands the same job back on the very next iteration of the worker's inner loop, and for a persistent
fault — a CHECK violation from a bad state value, a corrupt `job_items` page — that is an unthrottled
loop pinning a core, one error line per iteration in the journal, and every subsequent job starved
behind it with the UI showing "pending" indefinitely. Only a daemon restart clears it. `FailJob`
plus the backoff in §5.5 makes the state machine terminal-safe without one.

There is no `canceled` state and no cancel endpoint in v1. A bulk job over a few hundred files on a
LAN completes in seconds to minutes and the CHECK constraint would have to be relaxed later to add
one. If cancellation is wanted, it arrives as migration 6.

A `scan` job has `total = 1` and no `job_items` rows; it goes `pending → running → done|failed`
around a single `Scanner.ScanAll` (or `ScanFolder`) call.

**`job_items.state`**

```
pending ──ClaimJobItem──▶ running ──success──▶ done
                             │
                             └────failure────▶ failed  (error text recorded)
running ──daemon restart──▶ pending      (recovery; see 5.6)
```

Items only ever move forward or back to `pending` on recovery. `done` and `failed` are terminal.

### 5.4 Catalog job methods

```go
// CreateJob inserts the job and its items in ONE transaction, with
// total = len(fileIDs) (or 1 for a scan). Duplicate ids in fileIDs collapse
// via the (job_id, file_id) primary key and total is set from the count
// actually inserted.
func (c *Catalog) CreateJob(action, folderLabel string, fileIDs []int64) (int64, error)

// NextPendingJob returns the lowest-id job that still has work.
//   SELECT ... FROM jobs WHERE state IN ('pending','running') ORDER BY id LIMIT 1
func (c *Catalog) NextPendingJob() (*Job, error)

// ClaimJobItem atomically takes the next pending item of a job.
// Returns (0, nil) when the job has no pending items left.
//   UPDATE job_items SET state = 'running'
//   WHERE job_id = ? AND file_id = (
//       SELECT file_id FROM job_items
//       WHERE job_id = ? AND state = 'pending'
//       ORDER BY file_id LIMIT 1)
//   RETURNING file_id
// (go-sqlite3 v1.14.34 supports RETURNING.)
func (c *Catalog) ClaimJobItem(jobID int64) (fileID int64, err error)

// MarkJobRunning sets state='running' and started_at if not already set.
func (c *Catalog) MarkJobRunning(jobID int64, at int64) error

// CompleteJobItem writes the item's terminal state and bumps jobs.done
// (and jobs.failed when errMsg != "") in the SAME transaction, so progress
// can never drift from the items.
func (c *Catalog) CompleteJobItem(jobID, fileID int64, errMsg string) error

// FinishJob sets the terminal state from the counters and stamps finished_at.
//   UPDATE jobs SET state = CASE WHEN failed > 0 THEN 'failed' ELSE 'done' END,
//                   finished_at = ?
//   WHERE id = ?
func (c *Catalog) FinishJob(jobID int64, at int64) error

// GetJob returns the job row plus up to `limit` failed items.
func (c *Catalog) GetJob(jobID int64, failureLimit int) (*Job, []JobFailure, error)

// ListJobs returns the most recent jobs, newest first.
func (c *Catalog) ListJobs(limit int) ([]Job, error)

// ResumeInterruptedJobs resets in-flight work left behind by a kill.
//   UPDATE job_items SET state='pending', error='' WHERE state='running';
//   UPDATE jobs SET state='pending' WHERE state='running';
func (c *Catalog) ResumeInterruptedJobs() (items int64, jobs int64, err error)

// PurgeOldJobs deletes finished jobs older than the cutoff, items first, in
// one transaction (there is no ON DELETE CASCADE — foreign keys are off).
func (c *Catalog) PurgeOldJobs(before int64) (int64, error)

// FailRemainingItems flips every still-pending item of a job to 'failed' with
// the same message AND advances both job counters by the same count, in ONE
// transaction. Bumping the items without bumping the counters is the bug this
// signature exists to prevent: it produces a terminal job reporting
// {total: 800, done: 12, failed_count: 1} while 788 files actually failed.
//
//   tx:
//     n := UPDATE job_items SET state='failed', error=?
//          WHERE job_id=? AND state IN ('pending','running')   -- RowsAffected
//     UPDATE jobs SET done = done + n, failed = failed + n WHERE id=?
//
// Postcondition, always: jobs.done == jobs.total and jobs.failed <= jobs.done.
func (c *Catalog) FailRemainingItems(jobID int64, errMsg string) error

// CompleteJobItemVirtual records the single synthetic item of a 'scan' job,
// which has total = 1 and NO job_items rows. It does not stamp finished_at;
// FinishJob does that.
//
//   UPDATE jobs SET done = done + 1,
//                   failed = failed + CASE WHEN ? = '' THEN 0 ELSE 1 END,
//                   error = ?
//   WHERE id = ?
func (c *Catalog) CompleteJobItemVirtual(jobID int64, errMsg string) error

// FailJob terminates a job the worker has abandoned for a reason not
// attributable to one item (a catalog I/O error, an unreadable queue). It
// calls FailRemainingItems internally so the counter invariant holds, records
// the reason on jobs.error, and stamps state='failed' + finished_at.
func (c *Catalog) FailJob(jobID int64, at int64, reason string) error

// ListFileIDs backs GET /api/v1/files/ids. It applies the same filters as
// ListFiles, ignores sort/offset/limit, orders by id ASC, and returns at most
// `cap` ids together with the true total.
//   SELECT id FROM files WHERE <filters> ORDER BY id LIMIT ?
//   SELECT COUNT(*) FROM files WHERE <filters>
// ids is always non-nil; an empty result is an empty slice (section 6.0).
func (c *Catalog) ListFileIDs(q FileQuery, cap int) (ids []int64, total int64, err error)

// FolderSizesForIDs backs the restore pre-flight (section 5.10).
//   SELECT folder_label, COALESCE(SUM(size_bytes),0)
//   FROM files WHERE id IN (...) AND status='archived' GROUP BY folder_label
func (c *Catalog) FolderSizesForIDs(ids []int64) (map[string]int64, error)
```

**Counter invariant, restated because four methods must preserve it.** On a job in a terminal state,
`jobs.done == jobs.total` and `jobs.failed <= jobs.done`. `CompleteJobItem` and
`CompleteJobItemVirtual` each add exactly 1 to `done`; `FailRemainingItems` adds the number of items
it flipped; `FinishJob` adds nothing. Any implementation that leaves a terminal job with
`done < total` is wrong, and `TestJobCountersReachTotalOnEveryTerminalPath` (§8, step 8) proves it
for all four paths.

### 5.5 The worker loop

**One worker goroutine.** Items are processed serially. Parallelism would only contend for the
`Mover` mutex and thrash the disk head on a spinning pool.

```go
func (w *Worker) Run(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:      // buffered chan struct{}, cap 1; POST /api/v1/jobs pokes it
		case <-tick.C:      // safety net so a missed poke costs at most 2s
		}

		// consecutive is reset by any iteration that makes progress; it is the
		// brake that keeps a persistent catalog fault from becoming a spin.
		consecutive := 0
		for ctx.Err() == nil {
			job, err := w.cat.NextPendingJob()
			if err != nil {
				w.logger.Error("reading job queue", "error", err)
				break
			}
			if job == nil {
				break
			}
			if err := w.runJob(ctx, job); err != nil {
				consecutive++
				if consecutive >= 3 {
					w.logger.Error("job worker backing off", "job", job.ID, "error", err)
					break // fall back to the outer select; the 2s ticker retries
				}
				continue
			}
			consecutive = 0
		}
	}
}

// runJob returns a non-nil error ONLY when the job could not be driven to a
// terminal state for an infrastructural reason. Item failures are not errors:
// they are recorded and the job ends 'failed'.
func (w *Worker) runJob(ctx context.Context, job *catalog.Job) error {
	if err := w.cat.MarkJobRunning(job.ID, time.Now().Unix()); err != nil {
		return err
	}

	if job.Action == "scan" {
		err := w.scanner.ScanAll(ctx)     // or ScanFolder for a scoped job
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		w.cat.CompleteJobItemVirtual(job.ID, msg)   // done += 1, failed += (msg != "")
		return w.cat.FinishJob(job.ID, time.Now().Unix())
	}

	for {
		// Cancellation lands BETWEEN items, never inside a move.
		if ctx.Err() != nil {
			return nil // the item stays 'pending'; recovery picks the job back up
		}
		fileID, err := w.cat.ClaimJobItem(job.ID)
		if err != nil {
			// Terminal, not a silent return. Leaving the job 'running' makes
			// NextPendingJob hand it straight back (section 5.3).
			w.logger.Error("claiming job item", "job", job.ID, "error", err)
			w.cat.FailJob(job.ID, time.Now().Unix(), "could not read the job queue: "+err.Error())
			w.cat.LogEvent("job_failed", job.FolderLabel, "", fmt.Sprintf("job %d abandoned: %v", job.ID, err))
			return err
		}
		if fileID == 0 {
			break // no pending items left
		}

		var opErr error
		switch job.Action {
		case "restore":
			_, opErr = w.mover.RestoreByID(fileID)
		case "archive":
			_, opErr = w.mover.ArchiveByID(fileID)
		}

		msg := ""
		if opErr != nil {
			msg = opErr.Error()
		}
		if err := w.cat.CompleteJobItem(job.ID, fileID, msg); err != nil {
			w.logger.Error("recording job item result", "job", job.ID, "file", fileID, "error", err)
			w.cat.FailJob(job.ID, time.Now().Unix(), "could not record item results: "+err.Error())
			return err
		}

		// Two errors mean every remaining item would fail identically. Fail the
		// rest of the job fast rather than writing hundreds of identical rows.
		if errors.Is(opErr, ErrColdStorageFull) || errors.Is(opErr, ErrFolderUnavailable) {
			w.cat.FailRemainingItems(job.ID, opErr.Error())
			break
		}
	}
	return w.cat.FinishJob(job.ID, time.Now().Unix())
}
```

Cancellation is checked at the top of each iteration and nowhere else. A `ctx` check inside
`crossDeviceMove`'s `io.Copy`, or between the copy and the `os.Remove`, converts a graceful shutdown
into a torn move. **Moves are atomic units.**

### 5.6 Crash recovery — a startup-only phase

**Recovery does not live in `ScanAll` and is not reachable from any tick.** Today
`Scanner.ScanAll` opens with `s.recoverArchiving()` (`scanner.go:30`) and `ScanAll` runs on the
daemon's catalog-sync ticker, on a goroutine entirely independent of the job worker. Recovery
mutates the filesystem and blind-writes file rows without holding the `Mover` mutex, so leaving it
there means recovery and an in-flight move overlap. The result is total loss of a file on the normal
path:

> The worker is inside `ArchiveByID` for a 4 GB video. It has CAS'd the row to `archiving` and the
> cross-device copy is 300 MB in. `catalogTicker` fires; `ScanAll` calls `recoverArchiving`; it sees
> the row, stats the destination, finds 300 MB against a catalogued 4 GB, deletes the "partial" and
> writes `status='synced'`. Forty seconds later the copy finishes writing into an unlinked inode,
> the size and hash checks pass, `os.Remove(src)` deletes the original from the Syncthing folder, and
> `MarkArchived`'s `WHERE status='archiving'` affects zero rows. Nothing at the Syncthing path,
> nothing in the archive, a row that says `synced`. Syncthing propagates the deletion to the phone
> and the laptop.

So, normatively:

- `Scanner.recoverArchiving` and the new `Scanner.recoverRestoring` become
  **`daemon.RecoverStartupState(cfg, cat, logger) error`**, an ordinary function in
  `internal/daemon/recover.go`. They are removed from `Scanner` and from `ScanAll` entirely.
- `main` calls it **exactly once**, after `catalog.Open` and `reconcileFolders`, and **before** the
  `Mover`, the `Worker`, the API server or `daemon.New` are constructed. This is invariant 8. It is
  the whole reason recovery may touch the filesystem without the mover mutex: at that instant
  nothing else in the process exists.
- A recovery error is fatal. Starting a daemon that could not resolve its transient rows means
  enforcing quotas against a catalog whose truth value is unknown.
- The `-scan` one-shot path in `main` calls it too, before scanning.

`§5.2b`'s ordering — destination written to a temp sibling, verified, renamed into place, *then* the
source removed — is what makes recovery decidable by inspection. At every instant `dst` either does
not exist or is a complete, re-hashed copy.

#### `recoverArchiving` — rows stuck in `archiving`

For each row, with `dst` = the archive path and `src` = the syncthing path:

| `dst` | `src` | Action |
|---|---|---|
| absent | present | Delete any `dst.coldstore-tmp`. `status='synced'`. Nothing happened. |
| absent | absent | Delete any `dst.coldstore-tmp`. `status='missing'`, `LogEvent("source_vanished")`. The source can only be absent here because someone else deleted it — step 8 of `crossDeviceMove` runs strictly after the rename that creates `dst`. |
| present, size == `files.size_bytes` | absent | `status='archived'`, `archived_at = now`. The normal kill-between-unlink-and-commit case. |
| present, size == `files.size_bytes` | present, size and mtime match `files.size_bytes`/`file_mtime` | Same bytes in both places; the unlink simply never ran. `os.Remove(src)`, then `status='archived'`. |
| present, size == `files.size_bytes` | present, size or mtime **differ** | The device copy is a newer version. `os.Rename(dst, dst+".coldstore-superseded")`, `status='synced'`, `LogEvent("conflict")`. Nothing is destroyed. |
| present, size != `files.size_bytes` | present | Impossible under §5.2b (only a verified copy is ever renamed to `dst`) but reachable from a database written by an older binary. Rename `dst` aside as `.coldstore-superseded`, `status='synced'`, `LogEvent("conflict")`. |
| present, size != `files.size_bytes` | absent | Same, but there is no device copy: leave `dst` where it is, `status='archived'`, `LogEvent("archive_unconfirmed")` naming both sizes. Invariant 9 — keep the bytes and keep them referenced; do not delete a copy to make the bookkeeping tidy. |

**Deliberate deviation, recorded because a reviewer proposed the opposite.** In row 3 the archive
copy is complete and the source is gone, which is genuinely indistinguishable from "the owner deleted
this photo on their phone while the archive was in flight". A reviewer proposed treating it as a user
deletion — delete the stray copy and mark the row `missing`. This design promotes to `archived`
instead. The two error directions are not symmetric: guessing "archived" wrong leaves a photo the
owner meant to delete sitting restorable in cold storage, which they can see in the Activity list and
remove; guessing "deleted" wrong destroys the only remaining copy of a file, irreversibly. Invariant
9 decides it. `LogEvent("archive_unconfirmed")` fires in the ambiguous case so the owner is told.

#### `recoverRestoring` — rows stuck in `restoring`

`Restorer` sets `restoring` at `restorer.go:39` and **nothing has ever recovered that state**. A row
left in `restoring` is invisible to `OffloadCandidates`, excluded from `FolderSizes`, and
undownloadable (the handler 409s on any other status) — permanently stranded. A bulk restore of 500
files interrupted at file 200 strands one row today.

**The rule is not "does the destination exist".** It must not be, and the difference is the whole
finding. On the restore side `crossDeviceMove` (before §5.2b) created the destination at byte zero
and did not remove the cold-storage source until the copy verified, so after a kill the partial
destination *always* exists and the good source *always* still exists. An existence test picks the
partial every single time: `MarkRestored` sets `synced, pinned=1` over a 6 MB truncation of a 40 MB
photo, the intact archive copy is referenced by no row, the next scan writes `size_bytes = 6 MB` over
the correct value, Syncthing propagates the truncation to every device, and because `pinned=1`
nothing will ever re-archive it or look at the archive path again.

**The cold-storage source is authoritative while a restore is in flight.** With §5.2b's temp-then-
rename discipline the state is decidable:

| `src` (cold) | `dst` (syncthing) | Action |
|---|---|---|
| present | absent | Delete any `dst.coldstore-tmp`. `status='archived'`. The restore is simply retried. |
| present | present | The rename completed but the source removal did not (or the destination was already occupied). If `dst`'s size matches `files.size_bytes`, `os.Remove(src)` and `MarkRestored`. Otherwise `os.Rename(src, src+".coldstore-superseded")`, `status='synced'`, `LogEvent("conflict")`. |
| absent | present, size == `files.size_bytes` | The move completed; only the commit was lost. `MarkRestored`. |
| absent | present, size != `files.size_bytes` | `status='synced'`, `LogEvent("conflict")` naming both sizes. Do not trust the size. |
| absent | absent | `status='missing'`, `LogEvent("source_vanished")`. |

#### `ResumeInterruptedJobs`, and why a resumed item must be idempotent

Third, after both file-level recoveries:

```
UPDATE job_items SET state='pending', error='' WHERE state='running';
UPDATE jobs      SET state='pending'           WHERE state='running';
```

`started_at` is left as it was. `done` and `failed` are untouched and remain correct, because
`CompleteJobItem` only increments them inside the same transaction that makes an item terminal — an
item that was `running` was never counted.

Ordering matters: recover per-file state first, then re-queue the job, or the worker can re-pick an
item whose file is still in a transient state and immediately fail its CAS.

**The retry must be able to succeed.** `CompleteJobItem` is a separate transaction from
`MarkArchived`/`MarkRestored`, so a kill between them leaves the file **terminal** and the item
`running`. Without invariant 12 the resumed item re-claims, `ArchiveByID` re-reads a row that is
already `archived`, the CAS refuses, and the item is recorded `failed` — a job that reports
`state: "failed", failed_count: 1` against a file that was archived perfectly. §7.5 then invites the
owner to retry it, which fails identically, with no way to tell a bookkeeping artefact from a real
failure. That is precisely the question a control plane exists to answer.

Invariant 12 fixes it at the source: `ArchiveByID` returns success as a no-op when the re-read row is
already `archived`, `RestoreByID` when it is already `synced`. A resumed item that already ran
records `done`, and `ErrFileStateChanged` is reserved for a genuine conflict.

So: **a job that was mid-flight when the daemon was killed resumes automatically on the next start.
No item is lost and no item is double-moved.** The file the worker was moving is left consistent by
`recoverArchiving`/`recoverRestoring` before the retry happens, and the retry is a no-op if the move
had in fact completed.


### 5.7 Retention

After `ResumeInterruptedJobs`, and once per catalog-sync tick, call
`PurgeOldJobs(now - 7*24*3600)`, which deletes `job_items` then `jobs` for terminal jobs finished
more than seven days ago, in one transaction. Without this the tables grow without bound.

### 5.8 The rule that stops bulk restore and quota enforcement fighting

Four mechanisms, all required, none sufficient alone:

1. **`pinned = 1` on restore.** A restored file is not an offload candidate. Without this, restoring
   200 files pushes the folder far over budget, the next tick archives all 200, and each archive is a
   Syncthing deletion propagated to the owner's phone — hundreds of sync deletes of files the user
   just asked to get back. This is why D2 and D4 ship together or not at all.
2. **Mutual exclusion.** Both actors take the same `Mover` mutex. Two moves of the same file cannot
   overlap in time.
3. **Compare-and-swap.** Both actors re-read the row and CAS its status inside the lock. The loser
   gets `ErrFileStateChanged` and does nothing. Without the CAS, a job item holding a stale id whose
   file the daemon archived a moment earlier would `rename()` into `ENOENT`, hit the revert path, and
   write `status='synced'` onto a file whose bytes are in cold storage — a catalog entry that says
   synced for a file that is not there, over-counted in the budget and refused by restore as "not
   archived". Stranded, and invisible.
4. **Re-query, don't snapshot.** `enforceFolder` re-reads `FolderSizes` at the top of each candidate
   iteration instead of decrementing a Go-local running total (`daemon.go:217` today). The moment a
   job worker restores a file mid-pass, a decremented snapshot is wrong and the daemon over-offloads
   past the budget. The extra aggregate is index-covered and costs microseconds against a move that
   costs milliseconds to seconds.

These four keep the two actors from corrupting each other. They do **not** stop a bulk restore from
being *correct* and still catastrophic — a restore that legitimately blows the budget makes the next
tick evict a comparable volume of files the owner never selected, and D3's wedge banner is scoped so
that it never fires for that case. That is a separate problem with a separate fix; see §5.10.

### 5.9 Scanning — generation-based missing detection, concurrency, and temp files

#### The mark-then-restore sweep is replaced by a scan generation

`ScanFolder` today calls `MarkMissingSyncedFiles(label)` — one `UPDATE` that flips **every** synced
row in the folder to `missing` — and then relies on the walk to put them back. On a 50,000-file
camera roll on a cold ZFS pool that walk takes minutes, and for its entire duration:

- Every `Mover` CAS of the form `WHERE status='synced'` fails. A 500-item bulk archive that overlaps
  a catalog-sync tick reports "done 500, failed 380" against files that are sitting there perfectly
  synced, with 380 identical "file state changed" messages. The owner retries; if the timing repeats,
  it fails again. §5.8 sells the CAS as making the loser "a clean no-op", but here the loser is the
  owner's explicit instruction and the winner is a bookkeeping sweep that reverses itself a minute
  later.
- `FolderSizes` returns `(0, 0)`, so `GET /status` reports `synced_size_bytes: 0`,
  `over_budget: false`, `wedged: false`. With §7.10's 10-second `useStatus` poll the owner watches
  the budget bar collapse to empty and the wedge banner blink out and back on every tick.

Replace it with `files.last_seen` (migration 3):

```
ScanFolder(fc):
    if err := FolderReady(fc.SyncthingPath); err != nil { return err }   // section 3.6

    gen := time.Now().Unix()

    walkErr := filepath.WalkDir(fc.SyncthingPath, func(path, d, err) error {
        if err != nil {
            if path == fc.SyncthingPath { return err }   // RETURN it, do not swallow
            log warn; return nil
        }
        ... skip rules, below ...
        UpsertFile(record, gen)      // sets last_seen = gen on INSERT and on CONFLICT
        return nil
    })
    if walkErr != nil { return walkErr }                 // no sweep on a failed walk

    // Only now, and only after a walk that COMPLETED:
    UPDATE files SET status='missing'
    WHERE folder_label = ? AND status = 'synced' AND last_seen < ?      -- gen
```

`MarkMissingSyncedFiles` is deleted. Missing-detection becomes a single atomic statement at the end
of a successful walk. The concurrent-job window disappears, the status flicker disappears, and an
aborted, errored or unmounted walk simply never runs the final `UPDATE` — which subsumes the
folder-root guard more robustly than the guard alone ever could. §3.6's `FolderReady` stays as the
cheap up-front check that also protects the mover.

Note the `status = 'synced'` predicate: rows in `archiving` or `restoring` are never swept, so a move
in flight cannot be flipped to `missing` behind the mover's back.

`ScanAll` must not set `initialScanComplete` when any folder failed.

#### Skip rules — Syncthing's temp files must never be catalogued

The current filter is `strings.HasPrefix(relPath, ".st")`, applied to the **relative path**, so it
only ever fires at the folder root, and it does not match Syncthing's actual temp-file name at all:
`.syncthing.NAME.tmp` starts `.sy`. Consequences, both real:

- The catalog-sync tick catalogues an in-flight 3.2 GB upload as an ordinary file whose `size_bytes`
  is whatever partial bytes exist. Those bytes count in `FolderSizes().total`, so the folder reports
  over budget when it is not.
- "Select all in folder" returns the temp file's id, and because §5.2 says `ArchiveByID` deliberately
  skips the grace period, a bulk Archive renames an active download into cold storage. Syncthing's
  pull fails with `ENOENT` and restarts the transfer from block zero — the phone burns another
  3.2 GB of cellular data — and cold storage permanently holds a fragment that appears in the archive
  listing as a restorable file called `.syncthing.VID_0031.mp4.tmp`.
- The fully automatic variant needs no user: the phone goes out of range mid-upload, the temp file's
  mtime ages past the grace period, and `enforceFolder` offloads it. The existing disk-mtime re-check
  passes, because the mtime genuinely is stale.

**Filter on `d.Name()`, at every depth, for both files and directories:**

| Pattern | Why |
|---|---|
| `.syncthing.*.tmp` | Syncthing's in-flight temp file |
| `~syncthing~*` | Syncthing's alternate temp naming |
| `*.tmp` | conservative; a `.tmp` in a photo folder is not a photo |
| `*.coldstore-tmp` | coldstore's own temp sibling (§5.2b) |
| `*.coldstore-superseded` | coldstore's set-aside copies (§4.1b, §5.6) |
| `.stfolder`, `.stignore`, `.stversions`, `.stignore.*` | Syncthing metadata |
| any directory whose name begins with `.` | unchanged from today |

**Second guard, in the mover.** `Mover.ArchiveByID` `os.Stat`s the source inside the lock and refuses
with `ErrSourceChanged` if `size` or `mtime` differs from the catalog row. Once `enforceFolder` is
rerouted through the mover (§5.2), `daemon.go:186-194`'s disk-mtime re-check is gone, and this is the
only thing standing between a bulk Archive and an actively-written file.

#### Concurrency

`Scanner` gets `mu sync.Mutex`; `ScanAll` and `ScanFolder` acquire it with `TryLock` and return
`ErrScanInProgress` if it is held. Both the daemon's catalog tick and the scan job take it. The API
never runs a scan inline any more — `POST /api/v1/scan` enqueues a job and returns 202. This fixes
`handleScan`, which today runs a full filesystem walk inside the HTTP handler with no guard against
the daemon's own scan.

### 5.10 Bulk restore must not silently evict what the owner did not select

D2's pin-on-restore protects the files the owner asked for. It says nothing about what the quota tick
offloads **instead**, and the answer is: everything else in the folder.

> CameraRoll: budget 20 GB, currently 19.8 GB synced, 0 pinned, 2,000 archived files. The owner
> filters to Archived, clicks "Select all in folder", gets 300 ids totalling 15 GB, and clicks
> Restore. `POST /jobs` returns 202 with no warning of any kind. The worker restores serially; every
> `MarkRestored` sets synced + pinned. The 15-minute tick fires at 21.8 GB; the 40 restored files are
> pinned and skipped, so the daemon evicts 1.8 GB — about 350 of the owner's **oldest** photos — and
> Syncthing deletes all 350 from the phone and the laptop. Two ticks later the total has reached
> 34.8 GB and a further 13 GB is gone: effectively every unpinned file in the folder. Final state:
> total 20 GB, pinned 15 GB. `wedged = pinned(15) > budget(20)` is **false**, so §7.6's banner never
> renders and §4.4's warning never fires. The job says `state: "done", done: 300, failed_count: 0`.
> Green. The owner asked for 300 photos back, got them, and roughly 2,500 completely different photos
> vanished from their devices overnight. The only signal anywhere is a red budget bar.

Two independent fixes; both are required, because the first only covers the restore-shaped cause.

**1. Pre-flight the restore at enqueue.** In the `POST /api/v1/jobs` handler, for
`action == "restore"`:

```
sizes := cat.FolderSizesForIDs(ids)              // SUM(size_bytes) grouped by folder_label
for label, add := range sizes:
    fc := cat.FolderConfigByLabel(label)
    if fc.BudgetBytes <= 0 { continue }
    total, _ := cat.FolderSizes(label)
    if total + add > fc.BudgetBytes && !req.Force:
        overage := total + add - fc.BudgetBytes
        evictions := cat.CountEvictionsUpTo(label, overage, IsPersistent(fc))  // how many files that is
        return 409 {
          "error": "restoring these files would exceed the folder budget",
          "folder": label,
          "overage_bytes": overage,
          "estimated_evictions": evictions
        }
```

`JobRequest` gains `"force": true`, which skips the check. The SelectionBar turns the 409 into a
confirm dialog that names the number:

> Restoring these 300 files will exceed CameraRoll's 20 GB budget by 15 GB. Coldstore will archive
> about **2,500 other files** to make room, and Syncthing will delete them from your devices.
> [Cancel] [Restore anyway]

`CountEvictionsUpTo(label, bytes, protected)` is a single ordered scan of
`idx_files_folder_status_pinned` counting rows until the running sum reaches `bytes`; it is capped at
50,000 rows and reports the cap. `protected` is the caller's predicate for "the offloader would never
evict this", and the handler passes `IsPersistent` against the folder's persistent globs: a starred
file is not an eviction, and counting it promises the owner a deletion that cannot happen. The glob
semantics stay in `internal/daemon`, which is why this is a predicate and not another `WHERE` clause.
The grace period is deliberately **not** applied — grace delays an eviction, it does not prevent one,
so filtering on it would under-report a mass deletion that still happens an hour later.

**2. Make mass eviction loud regardless of cause.** The pre-flight cannot catch a folder that goes
over budget because the owner shot a wedding, or lowered the budget in the UI. So: when a single
`enforceFolder` pass archives **more than 100 files, or more than 10% of the folder's file count
when the pass took at least 10 files**, write one `bulk_eviction` event with the count and the byte
total, log it at `Warn`, and surface it in the StatusBar as a dismissible strip — not only in a
table the ConfigPanel shows ten rows of.

The 10-file floor under the percentage arm is load-bearing: one file out of six is 16%, so without
it a folder of a dozen holiday videos writes a mass-eviction warning every time an ordinary tick
sheds a single file, and a warning that fires on the normal case is not a warning. The absolute
100-file arm still fires whatever the folder's size.

### 5.11 The stray-archive sweep

> **Status: NOT IMPLEMENTED.** Nothing in the tree walks cold storage for unreferenced files and no
> `stray_archive` event is ever written. The scanner skips both `*.coldstore-tmp` and
> `*.coldstore-superseded` by name, so such a file is never catalogued, never counted and never
> downloadable, and a `Warn` log line at the moment it is created is the whole of the owner's
> notification. Do not write a code comment that promises this sweep until it exists — two did, and
> a reviewer had to chase them down. On the restore path the aside also lands *inside* the Syncthing
> folder, which is why the runbook's `.stignore` guidance is two lines (`*.coldstore-tmp` **and**
> `*.coldstore-superseded*`) rather than one.

Several paths in this document deliberately leave bytes in cold storage that no `files` row points
at: `.coldstore-superseded` copies from §4.1b, §5.2b and §5.6, and any pre-existing orphan created by
the old `UpsertFile` behaviour before §4.1b landed. Invariant 9 says keep them; it does not say hide
them.

Once per catalog-sync tick, after the scan, walk `<cold>/archive/<label>/` and compare each path
against `status='archived'` rows for that folder. For every file with no matching row, accumulate
size and count, and write one deduped `stray_archive` event per folder per 24 hours:

```
CameraRoll: 47 files (2.3 GB) in cold storage are not referenced by the catalog
```

The ConfigPanel shows the figure with the archive path so the owner can reclaim the space by hand.
**The sweep never deletes anything.** There is no delete-from-archive endpoint in v1 and that is
deliberate: a control plane whose first destructive feature is "delete things I could not account
for" is not one this owner should be handed on day one.



---

## 6. API contract

Base path `/api/v1`. Every response is JSON. Every error response is
`{"error": "<human readable message>"}` with a 4xx/5xx status. All routes sit behind the existing
basic-auth middleware (D5).

**Go ↔ TypeScript.** Every field name below is normative. The Go struct tag and the
`web/src/types/api.ts` declaration must both match it exactly. There is no codegen and no test that
compares them, so the reviewer of any change to one must check the other.

### 6.0 Serialisation rules and the Go types

Two rules and one block of structs, because §6.13's TypeScript is only half a contract and the other
half was never written down.

**Rule 1 — nullability.** Every field the TypeScript declares as `T | null` is a Go **pointer with no
`omitempty`**, so the key is always present and carries `null`. Every field the TypeScript declares
as `T?` gets `omitempty`. Nothing else. Following §2.7's `ThumbLastAttempt *int64
\`json:"thumb_last_attempt,omitempty"\`` pattern for `last_enforced_at` would omit the key entirely
on a fresh install, and §7.9's `=== null` guard does not catch `undefined`, so the ConfigPanel card
renders "last checked NaN min ago" on exactly the first-run experience D1 exists to fix.

**Rule 2 — arrays are never `null`.** Go marshals a nil slice as `null`. Every array-valued field in
every response is normalised to `[]` before marshalling: `ids`, `failed`, `folders`, the `/events`
array, the `/files` array, `/jobs`, and `persistent`. `handleListFiles` already does this
(`if files == nil { files = []catalog.FileRecord{} }`); the rule now applies everywhere. Without it,
a bulk job that succeeds completely returns `"failed": null`, §7.5's `job.failed.map(...)` throws a
TypeError, and React unmounts the SelectionBar at the exact moment the owner is watching a successful
job finish.

```go
// package catalog

type FolderConfigRecord struct {
	ID            int64  `json:"id"`
	FolderLabel   string `json:"folder_label"`
	SyncthingID   string `json:"syncthing_id"`
	SyncthingPath string `json:"syncthing_path"`
	BudgetBytes   int64  `json:"budget_bytes"`
	GraceMinutes  int    `json:"grace_minutes"`
	Enabled       bool   `json:"enabled"`
	PresentInTOML bool   `json:"present_in_toml"`
	LastEnforcedAt *int64 `json:"last_enforced_at"`  // null, never omitted
	WedgeWarnedAt  *int64 `json:"-"`                 // internal latch; not exposed
}

// Job is the jobs row. It is NOT the wire type: the row's counter column is
// `failed` and the wire field `failed` is the detail array, so they cannot
// share a struct.
type Job struct {
	ID          int64  `json:"id"`
	Action      string `json:"action"`
	FolderLabel string `json:"folder_label"`
	State       string `json:"state"`
	Total       int    `json:"total"`
	Done        int    `json:"done"`
	FailedCount int    `json:"failed_count"`   // column `failed`
	Error       string `json:"error"`          // job-level reason, "" normally
	CreatedAt   int64  `json:"created_at"`
	StartedAt   *int64 `json:"started_at"`     // null, never omitted
	FinishedAt  *int64 `json:"finished_at"`    // null, never omitted
}

type JobFailure struct {
	ID    int64  `json:"id"`
	Error string `json:"error"`
}

type FolderCounts struct {
	Total    int64 `json:"total"`
	Synced   int64 `json:"synced"`
	Archived int64 `json:"archived"`
	Missing  int64 `json:"missing"`
	Pinned   int64 `json:"pinned"`
}

// package api

// jobResponse is what GET /jobs/{id} returns. GET /jobs returns []catalog.Job.
type jobResponse struct {
	catalog.Job
	Failed []catalog.JobFailure `json:"failed"`   // always [], never null
}

type folderStatus struct {
	Label           string       `json:"label"`
	BudgetBytes     int64        `json:"budget_bytes"`
	SyncedSizeBytes int64        `json:"synced_size_bytes"`
	PinnedBytes     int64        `json:"pinned_bytes"`
	OverBudget      bool         `json:"over_budget"`
	Wedged          bool         `json:"wedged"`
	GraceMinutes    int          `json:"grace_minutes"`
	Enabled         bool         `json:"enabled"`
	PresentInTOML   bool         `json:"present_in_toml"`
	LastEnforcedAt  *int64       `json:"last_enforced_at"`
	SyncthingType   string       `json:"syncthing_type"`   // "" when unverified (section 3.7)
	Counts          catalog.FolderCounts `json:"counts"`
}

type pinResponse struct {
	File    catalog.FileRecord `json:"file"`
	Folder  folderStatus       `json:"folder"`
	Warning *string            `json:"warning"`          // null, never omitted
}
```

`GET /api/v1/folders` marshals `FolderConfigRecord` directly, so `WedgeWarnedAt` is tagged `json:"-"`
deliberately: it is an internal latch and the UI has no use for it. `FolderConfig` in §6.13 therefore
does not declare it, and that is not an omission.

### 6.1 `GET /api/v1/files` — changed

Query parameters:

| Param | Type | Notes |
|---|---|---|
| `folder` | string | |
| `status` | string | one of the five statuses |
| `ext` | string | |
| `q` | string | filename substring |
| `pinned` | `"1"` \| `"0"` | **new**; absent means no pin filter |
| `pin_source` | `user`\|`restore` | **new**; only meaningful with `pinned=1` |
| `ids` | comma-separated ints | **new**; at most 500, else 400. When present, all other filters are ignored and the response is exactly those ids that exist, in id order. Backs `useFiles.refetchIds` (§7.3) |
| `sort` | `mtime`\|`size`\|`name` | |
| `offset` | int | ≥ 0, else 400 |
| `limit` | int | 1–500; **clamped to 500**; ≤ 0 or non-numeric → 400 |

**Two parameters were added beyond this table when the frontend was built, because §7.4 asks for
two things this contract could not express:**

| Param | Type | Notes |
|---|---|---|
| `conflict` | `"1"` \| `"0"` | Unresolved §4.1b conflicts only: a `conflict` event newer than the last `conflict_resolved` event for the same (folder, path). The Conflicts filter pairs it with `status=archived`. `ResolveConflict` now logs `conflict_resolved`, so a resolved row leaves the filter |
| `sort` | also `pinned_at` | Ascending, oldest pin first — what §7.4's "Auto-pinned on restore" and §7.6's wedge remedy are specified to show. Every sort now ends in `id ASC`, so an OFFSET-paged scroll cannot repeat a row when the sort key ties |

`catalog.FileQuery` gains `Pinned *bool`, `PinSource string` and `IDs []int64` to match. `ids` is
capped at 500 because it is a post-job reconciliation helper, not a bulk channel; §7.3's
`refetchIds` chunks at 500 or falls back to a plain `refetch()`.

Response `200`: `FileRecord[]` (each now carrying `pinned`, `thumb_attempts`, `thumb_error`).

### 6.2 `GET /api/v1/files/ids` — new

Resolves "select all in folder" without paging the whole catalog through the client.

Query: the same filter parameters as `/files` (`folder`, `status`, `ext`, `q`, `pinned`,
`pin_source`). No `offset`, `sort`, `limit` or `ids`.

Backed by `catalog.ListFileIDs(q, cap)` (§5.4).

Response `200`:

```json
{ "ids": [12, 13, 19], "total": 3, "truncated": false }
```

`ids` is capped at **`maxJobIDs = 20000`** — the same constant `POST /api/v1/jobs` enforces (§6.8),
declared once in `internal/api` and referenced by both. If the filter matches more, `ids` holds the
first 20000 by id ascending and `truncated` is `true`, with `total` reporting the real count.

**The two caps must be the same number and there is no chunking anywhere.** With 20000 here and 5000
there, the flagship D4 workflow — "select all in folder", then Restore — dies with a
`400 at most 5000 ids per job` toast for any folder over 5,000 files, and nothing in the design gets
the owner past it: `useJobRunner` (§7.2) tracks a single `jobId: number | null` and the SelectionBar's
progress strip is written against one job. Raising the job cap is one constant; teaching the hook to
fan out across `ceil(n/5000)` jobs and aggregate their counters is a second state machine in the
frontend for no benefit. **One cap, one job.** `CreateJob` inserts its 20,000 `job_items` rows in one
transaction with a prepared statement; on SQLite that is tens of milliseconds.

When `truncated` is `true` the UI selects those 20,000 ids and shows "selected the first 20,000 of N";
submitting them is a legal job.

**Rejected alternative:** letting `POST /api/v1/jobs` accept a filter object instead of an id array,
so the server expands the selection. Rejected because the selection the user sees and the selection
the server acts on would then be resolved at different instants, so files added by a sync between
the click and the enqueue would be silently included in a bulk archive. An explicit id list means
the user acts on exactly what was on screen.

### 6.3 `GET /api/v1/status` — changed

Response `200`:

```json
{
  "total_files": 12043,
  "synced_files": 9800,
  "archived_files": 2200,
  "missing_files": 43,
  "cold_storage_free_bytes": 481036337152,
  "folders": [
    {
      "label": "CameraRoll",
      "budget_bytes": 20000000000,
      "synced_size_bytes": 24100000000,
      "pinned_bytes": 24100000000,
      "over_budget": true,
      "wedged": true,
      "grace_minutes": 30,
      "enabled": true,
      "present_in_toml": true,
      "last_enforced_at": 1755712345,
      "syncthing_type": "sendreceive",
      "counts": {
        "total": 8021, "synced": 6000, "archived": 2000,
        "missing": 21, "pinned": 340
      }
    }
  ]
}
```

- `synced_size_bytes` keeps its name for compatibility with the existing e2e assertions, but its
  meaning is now `FolderSizes.total` — the on-device footprint including `archiving` and `restoring`.
- `pinned_bytes` is a **subset** of `synced_size_bytes`, never an addend.
- `over_budget` and `wedged` follow §4.3, computed live.
- The folder list comes from `cat.ListFolderConfigs()` (all rows, ordered by label), **not**
  `cfg.Folders`. Disabled and orphaned folders appear, flagged.
- `last_enforced_at` is epoch seconds, or `null` if never enforced. It is stamped on **every**
  enforcement path, including a folder skipped for `budget <= 0` or for being unavailable (§4.4), so
  "last checked N min ago" never freezes on a live daemon.
- `syncthing_type` is the folder type reported by the Syncthing API (§3.7), or `""` when the check
  did not run. Anything other than `"sendreceive"` or `""` means coldstore's deletions do not
  propagate.
- **Errors are no longer swallowed.** The four `FileCount` calls and the per-folder aggregates were
  all assigned to `_`, so a locked database rendered as zeros. Any error returns `500` with
  `{"error": "..."}`.

`counts` comes from one new aggregate per folder:

```go
func (c *Catalog) FolderCounts(folder string) (FolderCounts, error)
//   SELECT COUNT(*),
//          COUNT(CASE WHEN status='synced'   THEN 1 END),
//          COUNT(CASE WHEN status='archived' THEN 1 END),
//          COUNT(CASE WHEN status='missing'  THEN 1 END),
//          COUNT(CASE WHEN pinned=1 AND status IN ('synced','archiving','restoring') THEN 1 END)
//   FROM files WHERE folder_label = ?
```

### 6.4 `PUT /api/v1/folders/{label}` — changed, tightened

Request body — **only these three fields are accepted; all are optional; anything else is ignored**:

```json
{ "budget_bytes": 20000000000, "grace_minutes": 30, "enabled": true }
```

Go:

```go
type updateFolderRequest struct {
	BudgetBytes  *int64 `json:"budget_bytes"`
	GraceMinutes *int   `json:"grace_minutes"`
	Enabled      *bool  `json:"enabled"`
}
```

Pointers are mandatory. With value types, a PATCH-style request that only changes the budget decodes
`enabled` as `false` and silently disables the folder — and once D1 makes enforcement read this
table, that single click stops the folder being managed at all.

`syncthing_id` and `syncthing_path` are **gone from the request struct**. Under D1 they are TOML-owned
infrastructure and the UI may not write them.

Validation and status codes:

| Condition | Status | Body |
|---|---|---|
| success | `200` | the updated `FolderConfig` |
| malformed JSON | `400` | `{"error":"invalid request body"}` |
| `budget_bytes` present and ≤ 0 | `400` | `{"error":"budget_bytes must be positive"}` |
| `grace_minutes` present and < 0 | `400` | `{"error":"grace_minutes cannot be negative"}` |
| no `folder_configs` row for `label` | `404` | `{"error":"no folder named \"X\""}` |
| DB error | `500` | |

The `404` is load-bearing: only TOML reconciliation may create a folder row. The current handler
creates a phantom folder for any label, which `tests/e2e/api_config_test.go:30` currently asserts as
correct behaviour — that test encodes the old contract and gets rewritten (§8, step 5).

### 6.5 `POST /api/v1/files/{id}/pin` and `DELETE /api/v1/files/{id}/pin` — new

No request body. Sets or clears `files.pinned`. **Moves nothing** — this is "pin in place".

Response `200`:

```json
{
  "file": { "...": "FileRecord" },
  "folder": { "...": "the folder's entry from /status" },
  "warning": "24.1 GB pinned exceeds the 20 GB budget - nothing left to offload"
}
```

`warning` is non-null **iff `pinned_bytes > budget_bytes` after the write** — a state test, matching
§4.5 exactly. **The pin is applied either way** (D3: the user may knowingly wedge their own folder).
The `folder` block lets the UI update the budget bar without a second round trip.

There is a second warning, unrelated to the budget: whenever the returned `file.status` is
`archived`, the owner is looking at a filled pin badge on a file that is not on the device, and
`warning` says the pin is latent. Which wording it uses is a **transition** test on the status the
row had when the request arrived, not a state test on the status it has when the request leaves:

> **already `archived` on arrival** — the ordinary case, and legal (see the status table below):
> `this file is in cold storage; the pin will take effect when you restore it`
>
> **not `archived` on arrival** — the pin lost a race it never knew it was in. Because
> `Mover.SetPinned` takes the mover mutex (§4.6), a pin issued against a file the daemon is already
> offloading blocks until that move finishes and returns a row that changed underneath it:
> `this file finished archiving while the pin was being applied; the pin will take effect when you
> restore it`

The distinction is not cosmetic. Reporting the race for every pin on an archived file tells an owner
who pinned a photo that has been in cold storage for weeks that the daemon just took it, which sends
them looking for an offload that never happened. The handler therefore reads the row once before the
mover call; the window that read leaves open is exactly the window a concurrent offload can overtake.

An archived warning wins over the budget one, because it explains something the owner is otherwise
about to misread.

The write sets `pin_source = "user"` and `pinned_at = now`. `DELETE` clears `pinned`, `pinned_at` and
`pin_source`, and returns the same envelope with `warning` always `null`.

| Condition | Status |
|---|---|
| success | `200` |
| non-numeric id | `400` |
| no such file | `404` |
| DB error | `500` |

Pinning is legal in every file status, including `archived` (where it takes effect on restore) and
`missing`.

### 6.5a `POST /api/v1/files/pin` — new, bulk

```json
{ "ids": [12, 13, 19], "pinned": true }
```

One request, one mover-mutex acquisition, one `setPinnedBulk` (chunked at 500 ids), one recomputed
folder block. Response `200`:

```json
{ "updated": 3, "folders": [ { "...": "folderStatus" } ], "warning": null }
```

`folders` carries one entry per folder touched. `warning` follows the same state test as §6.5,
reported for the first wedged folder.

| Condition | Status |
|---|---|
| success | `200` |
| `ids` empty or absent | `400` |
| more than 5000 ids | `400` |
| DB error | `500` |

This exists because §7.5's bulk Pin over 400 selected files would otherwise be 400 HTTP round trips,
each recomputing a full `FolderSizes` + `FolderCounts` pair for the response's `folder` block, all
serialised behind `SetMaxOpenConns(1)` — 800 aggregates to set 400 bits — while `SetPinnedBulk` sat
unused. §7.5 points at this endpoint, not at a loop.

### 6.6 `POST /api/v1/files/{id}/restore` — unchanged shape, corrected codes

Synchronous single-file restore through the `Mover`. Response `200` is the updated `FileRecord`
(now with `pinned: true`).

| Condition | Status | Was |
|---|---|---|
| success | `200` | 200 |
| non-numeric id | `400` | 400 |
| no such file | `404` | 404 |
| file is already `synced` | `200`, the record, no move (invariant 12) | n/a |
| file is `archiving`/`restoring`/`missing` | `409` `{"error":"file state changed..."}` | **500 with a raw Go error** |
| lost the CAS race | `409` `{"error":"file state changed..."}` | n/a |
| folder orphaned, unmounted, or missing `.stfolder` | `409` `{"error":"folder ... is not currently available"}` | n/a |
| something already exists at the destination path | `409` `{"error":"a newer copy of this file already exists in the folder; coldstore adopted it"}` (§5.2) | n/a |
| move failed | `500` | 500 |

It stays synchronous. A single file on a LAN is a bounded wait, and keeping it lets the existing e2e
restore tests stand. Anything bulk goes through the job queue.

### 6.7 `POST /api/v1/files/{id}/archive` — new

Symmetric to restore: synchronous single-file archive through `Mover.ArchiveByID`. Bypasses grace and
budget, applies the free-space guard, clears the pin.

| Condition | Status |
|---|---|
| success | `200` (updated `FileRecord`) |
| file is already `archived` | `200`, the record, no move (invariant 12) |
| file is `archiving`/`restoring`/`missing` | `409` |
| cold storage below 5% free | `507` `{"error":"cold storage below 5% free"}` |
| folder orphaned, unmounted, or missing `.stfolder` | `409` |
| source changed on disk since the last scan | `409` `{"error":"the source file changed during the move"}` (§5.9) |

### 6.7a `POST /api/v1/files/{id}/resolve` — new

Resolves the `conflict` state from §4.1b: an archived file whose path has reappeared in the Syncthing
folder. Body `{"keep": "device"}` or `{"keep": "archive"}`. Runs through `Mover.ResolveConflict`.

- `keep: "device"` — delete the cold-storage copy, set the row `synced` with the on-disk metadata.
- `keep: "archive"` — rename the device file aside as `<name>.coldstore-superseded`, leave the row
  `archived`.

| Condition | Status |
|---|---|
| success | `200` (updated `FileRecord`) |
| `keep` not one of the two values | `400` |
| the row is not `archived`, or nothing exists at the syncthing path | `409` |
| folder unavailable | `409` |

### 6.8 `POST /api/v1/jobs` — new

Request:

```json
{ "action": "restore", "ids": [12, 13, 19] }
{ "action": "restore", "ids": [12, 13, 19], "force": true }
{ "action": "archive", "ids": [12, 13, 19] }
{ "action": "scan",    "folder": "CameraRoll" }
```

`action` is required. `ids` is required and non-empty for `restore` and `archive`, and **ignored**
for `scan`. `folder` is optional and only meaningful for `scan` (`""` or absent scans all folders).
`force` is only meaningful for `restore` and skips the budget pre-flight (§5.10).

Response `202`:

```json
{ "job_id": 17, "action": "restore", "state": "pending", "total": 3 }
```

| Condition | Status |
|---|---|
| accepted | `202` |
| unknown `action` | `400` |
| `ids` empty for restore/archive | `400` |
| more than 20000 ids | `400` `{"error":"at most 20000 ids per job"}` |
| restore would exceed a folder budget and `force` is absent | `409`, see below |
| DB error | `500` |

The `409` body carries what the UI needs to write a truthful confirm dialog (§5.10):

```json
{
  "error": "restoring these files would exceed the folder budget",
  "folder": "CameraRoll",
  "overage_bytes": 15000000000,
  "estimated_evictions": 2500
}
```

Ids that do not exist are accepted at enqueue time and fail as individual items with
`"file not found"`. Duplicate ids collapse on the composite primary key, and `total` reflects the
deduplicated count.

The handler pokes the worker's `wake` channel after committing.

### 6.9 `GET /api/v1/jobs/{id}` — new

Response `200`:

```json
{
  "id": 17,
  "action": "restore",
  "folder_label": "",
  "state": "failed",
  "total": 3,
  "done": 3,
  "failed_count": 1,
  "created_at": 1755712000,
  "started_at": 1755712002,
  "finished_at": 1755712031,
  "failed": [
    { "id": 19, "error": "file status is synced" }
  ]
}
```

`failed_count` is authoritative and is the `jobs.failed` **column**; `failed` is the failure detail
array. They cannot be the same struct field, which is why §6.0 gives `GET /jobs/{id}` its own
`jobResponse` type embedding `catalog.Job`.

`failed` is **truncated to the first 100 entries** so a 20000-item job cannot return a multi-megabyte
payload; when `failed_count > 100` the UI says "showing first 100 of N failures". It is **always an
array**, never `null` (§6.0 rule 2).

`error` is the job-level reason from `FailJob` (§5.4) and is `""` on a job that ended normally.

The per-failure error key is `error`, not `err`, matching the `{"error": ...}` envelope used
everywhere else in this API.

`404` for an unknown job id.

### 6.10 `GET /api/v1/jobs` — new

Query `limit` (default 20, max 100). Response `200`: an array of the job objects above **without**
the `failed` array. Newest first. This is how the UI recovers a job it was polling when the page was
reloaded, including one resumed after a daemon restart.

### 6.11 `POST /api/v1/scan` — changed

Kept at its existing path so the UI's refresh button and the e2e tests keep a target. Body ignored.
Now equivalent to `POST /api/v1/jobs {"action":"scan"}`.

Response `202`: `{"job_id": 18, "action": "scan", "state": "pending", "total": 1}`.

**This is a breaking change from `200 {"status":"ok"}`.** `tests/e2e/api_status_test.go` and
`useStatus.triggerScan` both change with it.

### 6.12 `GET /api/v1/events` — new

Query: `folder` (optional), `type` (optional, exact match), `class` (optional, `warning`), `limit`
(default 50, max 500).

`class=warning` selects every event type in §4.4's vocabulary **except** `archive`, `restore` and
`folder_seeded`. It is not a nicety: a single enforcement pass writes hundreds of `archive` rows, so
an unfiltered "last 10 events for this folder" would never once show the wedge warning, the
cold-storage warning or the stray-archive report — the three things the list exists to show.

Response `200`: `Event[]`, newest first — `{id, timestamp, event_type, folder, path, details}`. The
array is normalised to `[]`, never `null` (§6.0 rule 2).

This exists because D3 requires the wedge condition to "log ONE deduped warning event", and the
`events` table has never had a reader — no `ListEvents`, no route, no reference anywhere in
`web/src`. Writing the required warning into a table nothing reads would satisfy the letter of D3
and none of its intent. The ConfigPanel renders the last 10 warning-class events for the selected
folder.

### 6.13 `web/src/types/api.ts` — the full mirrored contract

```ts
export type FileStatus = "synced" | "archiving" | "archived" | "restoring" | "missing";
export type JobAction = "restore" | "archive" | "scan";
export type JobState  = "pending" | "running" | "done" | "failed";

export interface ApiError { error: string }

export interface FileRecord {
  id: number;
  folder_label: string;
  relative_path: string;
  filename: string;
  extension: string;
  mime_type: string;
  size_bytes: number;
  file_mtime: string;
  file_atime: string;          // was missing
  first_seen: string;
  archived_at?: string;
  status: FileStatus;          // "archiving" was missing
  thumbnail_path?: string;
  content_hash?: string;
  pinned: boolean;             // new
  pinned_at: number | null;    // new, epoch seconds
  pin_source: "" | "user" | "restore"; // new
  thumb_attempts: number;      // new
  thumb_last_attempt?: number; // new, epoch seconds
  thumb_error?: string;        // new
}

export interface FolderConfig {
  id: number;
  folder_label: string;
  syncthing_id: string;
  syncthing_path: string;
  budget_bytes: number;
  grace_minutes: number;
  enabled: boolean;
  present_in_toml: boolean;    // new
  last_enforced_at: number | null; // new
}

export interface FolderCounts {
  total: number; synced: number; archived: number; missing: number; pinned: number;
}

export interface FolderStatus {
  label: string;
  budget_bytes: number;
  synced_size_bytes: number;   // on-device total; pinned_bytes is a SUBSET of this
  pinned_bytes: number;        // new
  over_budget: boolean;        // new
  wedged: boolean;             // new
  grace_minutes: number;       // new
  enabled: boolean;            // new
  present_in_toml: boolean;    // new
  last_enforced_at: number | null; // new
  syncthing_type: string;      // new; "" when unverified (section 3.7)
  counts: FolderCounts;        // new
}

export interface StatusResponse {
  total_files: number;
  synced_files: number;
  archived_files: number;
  missing_files: number;
  cold_storage_free_bytes: number;
  folders: FolderStatus[];
}

export interface JobFailure { id: number; error: string }

export interface Job {
  id: number;
  action: JobAction;
  folder_label: string;
  state: JobState;
  total: number;
  done: number;
  failed_count: number;
  error: string;               // job-level reason; "" normally
  created_at: number;
  started_at: number | null;
  finished_at: number | null;
}

// GET /jobs/{id}; `failed` is always an array, never null (section 6.0 rule 2).
export interface JobDetail extends Job { failed: JobFailure[] }

export interface JobRequest {
  action: JobAction; ids?: number[]; folder?: string; force?: boolean;
}
export interface JobCreated  { job_id: number; action: JobAction; state: JobState; total: number }

// 409 body from POST /jobs when a restore would blow the budget (section 5.10).
export interface BudgetPreflightError {
  error: string; folder: string; overage_bytes: number; estimated_evictions: number;
}

export interface PinResponse { file: FileRecord; folder: FolderStatus; warning: string | null }
export interface BulkPinResponse { updated: number; folders: FolderStatus[]; warning: string | null }

export interface FileIDsResponse { ids: number[]; total: number; truncated: boolean }

export interface CatalogEvent {
  id: number; timestamp: string; event_type: string;
  folder: string; path: string; details: string;
}

export interface PersistentPath {
  id: number; folder_label: string; pattern: string;
  match_count: number;   // new; how many files this pattern currently protects (section 4.1a)
}
```

Nullability follows §6.0 rule 1 mechanically: `pinned_at`, `last_enforced_at`, `started_at`,
`finished_at` and `warning` are `T | null` here and Go pointers **without** `omitempty` there;
`archived_at`, `thumbnail_path`, `content_hash`, `thumb_last_attempt` and `thumb_error` are `T?` here
and carry `omitempty` there.

### 6.14 `api.NewServer` signature

The positional parameter list has already broken `tests/e2e/helpers_test.go` once and this work adds
at least a `Mover` and a `Worker`. Replace it with a deps struct in the **same commit** that fixes
the e2e helper:

```go
type Deps struct {
	Cfg     *config.Config
	Cat     *catalog.Catalog
	Mover   *daemon.Mover
	Worker  *daemon.Worker    // for the enqueue poke
	Scanner *daemon.Scanner
	Logger  *slog.Logger
}

func NewServer(d Deps) *Server
```

`restorer` and `offloader` disappear from `Server`; the API reaches them only through `Mover`.
`Server.folderPath` is deleted; path resolution is `cat.FolderPath` (§3.5).

---

## 7. Frontend plan

Stack as-is: React 19 with StrictMode on, Vite, Tailwind, no router, no state library. Everything
below stays inside that.

### 7.0 Shared foundations — build these first

**`web/src/lib/format.ts`.** One `formatSize`, **SI (1000-based)**, replacing four copies with two
different rounding schemes at `StatusBar.tsx:3`, `ConfigPanel.tsx:4`, `FileBrowser.tsx:5`,
`Lightbox.tsx:12`. `internal/config.ParseBudget` is SI, so a TOML budget of `"20GB"` currently
renders as "18.6 GB". Also export `parseSize(input: string): number` for the budget editor so a typed
"20 GB" round-trips to exactly the 20000000000 the owner wrote in TOML.

**`web/src/lib/api.ts`.**

```ts
export class ApiRequestError extends Error {
  constructor(public status: number, message: string) { super(message); }
}

export async function apiFetch<T>(url: string, init?: RequestInit): Promise<T> {
  const res = await fetch(url, init);
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`;
    try { const body = await res.json(); if (body?.error) msg = body.error; } catch { /* not JSON */ }
    throw new ApiRequestError(res.status, msg);
  }
  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}
```

The guarded JSON parse matters: `routes.go`'s catch-all means an unmatched method returns Go's
plain-text 405 and an unmatched path returns `index.html`. A bare `await res.json()` on those throws
a `SyntaxError` instead of showing the server's message — that is the current blank-page failure mode.

**The compliant hook shape.** `eslint-plugin-react-hooks` v7 flags
`useEffect(() => { refetch(); }, [refetch])` as `react-hooks/set-state-in-effect`; three of the four
current lint errors are literally that pattern (`useConfig.ts:17, :44, :76`). Every hook — existing
and new — moves to:

```ts
useEffect(() => {
  let cancelled = false;
  (async () => {
    try {
      const data = await apiFetch<T>(url);
      if (!cancelled) { setData(data); setError(null); }
    } catch (e) {
      if (!cancelled) setError(e instanceof Error ? e.message : String(e));
    } finally {
      if (!cancelled) setLoading(false);
    }
  })();
  return () => { cancelled = true; };
}, [url]);
```

State is only set inside the async continuation, which is what the rule actually checks, and the
cancelled flag handles StrictMode's double-mount. **No `eslint-disable`.** Establish this once,
before writing `useJob` and friends, or every new hook adds another error to an already-red lint.
Note that `web/node_modules` is not installed in the worktree — `npm run lint` there fails with
`eslint: command not found`, which is easy to mistake for a clean lint. Run `npm ci` first.

### 7.1 `useSelection` — new hook

```ts
function useSelection(orderedIds: number[]): {
  selected: Set<number>;
  isSelected(id: number): boolean;
  toggle(id: number): void;          // plain click on a checkbox
  toggleRange(id: number): void;     // shift-click: anchor..id inclusive, set to the anchor's state
  selectMany(ids: number[]): void;   // select-all-in-folder result
  clear(): void;
  count: number;
}
```

`orderedIds` is the currently rendered order, so a shift-click range means what the user sees. The
anchor is the last id passed to `toggle`, held in a ref. Ids that scroll out of the loaded window
stay selected — selection is a `Set<number>`, not an index range.

Shared by Gallery and FileBrowser so the bulk bar behaves identically in both.

### 7.2 `useJob` and `useJobRunner` — new hooks

```ts
function useJob(jobId: number | null): { job: Job | null; error: string | null }
```

Polls `GET /api/v1/jobs/{id}` every 1000 ms, stops when `state` is `done` or `failed`, clears its
interval on unmount, and tolerates the StrictMode double-mount (idempotent GET, interval keyed to
`jobId`).

```ts
function useJobRunner(): {
  jobId: number | null;
  job: Job | null;
  error: string | null;
  start(req: JobRequest): Promise<void>;
  dismiss(): void;
}
```

Wraps enqueue + poll. Job creation is only ever user-triggered — never fired from an effect, because
StrictMode would double-post it.

### 7.3 `useFiles` — reworked

Additions: `pinned?: boolean` and `pinSource?: "user" | "restore"` in `UseFilesOptions` and
`buildUrl`; `patchFile(id, partial)` and `removeFile(id)` so a pin, restore or archive updates the row
in place; `refetchIds(ids)`; `refetch` wrapped in `useCallback`.

`refetchIds(ids)` is backed by the **`ids` parameter added to `GET /api/v1/files` in §6.1**, capped at
500 per request. It chunks a larger set at 500, and falls back to a plain `refetch()` above 2,000 ids.
Do not invent a query shape here: without a server-side id filter, `refetchIds` degrades to fetching
the unfiltered first page, so after a bulk restore of 34 files the SelectionBar patches 50 arbitrary
rows with stale-but-plausible data and the 34 restored rows still render as "archived" with their
Restore buttons showing — the exact defect §7.7 says it is fixing.

Fix the request race properly: `fetchPage` currently early-returns when `loading` is true while
`loading` is also in its dependency list, so the filter-reset effect can fire while a page is in
flight and silently drop the reset. Replace the `loading` guard with an `AbortController` plus a
monotonically increasing request-generation counter in a ref; ignore any response whose generation is
stale.

### 7.4 Gallery — multi-select and the pin badge

**The tile must stop being a `<button>`** (`Gallery.tsx:68`). A checkbox and a pin toggle inside a
button are nested interactive elements: invalid HTML, a React warning, and a click on the inner
control that also opens the lightbox.

New tile: a `<div>` with `role="button"`, `tabIndex={0}` and an `onKeyDown` for Enter/Space, wrapping
the image. Overlays:

- **top-left** — a checkbox, visible when the tile is hovered/focused or when anything is selected.
- **top-right** — a pin toggle (filled pin when `file.pinned`), always visible when pinned.
- **bottom-right** — the existing `archived` pill, plus a distinct `pinned` pill in a different hue
  (archived stays amber; pinned is sky/indigo — they must not be confusable at thumbnail size).
- Failed-thumbnail tiles (`thumb_attempts >= 3`) show a small warning glyph with `thumb_error` as its
  `title`, instead of the current bare extension text.

Click semantics on the tile body:

| Gesture | Action |
|---|---|
| plain click | open the lightbox |
| ctrl/meta + click | `toggle(id)` |
| shift + click | `toggleRange(id)` |
| click on the checkbox | `toggle(id)`, `stopPropagation` |
| click on the pin | pin/unpin, `stopPropagation` |

The window-level keydown effect (`Gallery.tsx:42`) gains `Cmd/Ctrl+A` for select-all-visible and
`Escape` for clear-selection, and **must first check that `e.target` is not an `input`, `textarea` or
`[contenteditable]`** — the bulk bar and the pinned filter introduce focusable inputs and today's
handler would hijack Enter inside them.

A filter row above the grid, lifted into Gallery state and passed to `useFiles` as `status` /
`pinned` / `pinSource`:

**All · Synced · Archived · Pinned by me · Auto-pinned on restore · Conflicts**

The two pin filters exist because D2 pins **silently and permanently on every restore** and nothing
ever releases a pin, so the wedge state is the long-run destination of ordinary use. Nine months in,
the owner clicks "Pinned" and gets four thousand identical indigo badges, of which perhaps one
gigabyte was intentional; the banner's remedy ("unpin some files") requires triaging all of them by
hand, and the safest-looking action — rubber-band select, Unpin — is the one that releases the
favourites they deliberately protected. `pin_source` (migration 2) splits the two apart.
"Auto-pinned on restore" sorts **oldest `pinned_at` first**, because the oldest auto-pins are the
ones the owner has most certainly forgotten about, and it is the destination the §7.6 wedge banner
links to.

**Conflicts** filters `status=archived` files that have a `conflict` event (§4.1b) and renders the
two-button resolution.

### 7.5 `SelectionBar` — new shared component

A fixed bottom bar, rendered by both Gallery and FileBrowser whenever `count > 0`:

```
[ 34 selected ]  [Select all in folder]  [Restore] [Archive] [Pin] [Unpin]  [Clear]
```

- **Restore / Archive** → `useJobRunner().start({action, ids: [...selected]})`. While a job is
  running the bar becomes a progress strip: `Restoring 34 files — 12/34`, buttons disabled.
  A `409` from the restore pre-flight (§5.10) is **not** a toast: it opens a confirm dialog that
  names the number, and only "Restore anyway" re-submits with `force: true`.

  > Restoring these 300 files will exceed CameraRoll's 20 GB budget by 15 GB. Coldstore will archive
  > about **2,500 other files** to make room, and Syncthing will delete them from your devices.
  > [Cancel] [Restore anyway]

- **Pin / Unpin** → not jobs, and **not a loop**. One `POST /api/v1/files/pin {ids, pinned}` (§6.5a),
  then patch every returned row and the folder blocks. A loop of 400 requests would serialise 800
  aggregate queries behind `SetMaxOpenConns(1)` to set 400 bits.
- **Select all in folder** → `GET /api/v1/files/ids?...` with the current filters, then
  `selectMany(ids)`. Shows "selected the first 20,000 of N" when `truncated`. Because §6.2 and §6.8
  share one cap, whatever is selected here can always be submitted as one job.
- On job completion: show `done`/`failed_count`, iterate `job.failed` (always an array — §6.0 rule 2)
  listing the first failures inline with filename and error, `refetchIds` the touched set, refetch
  status, and keep the selection so the user can retry the failures.

### 7.6 StatusBar — budget bar with a pinned segment, and the wedge banner

Per folder, one bar with **two nested segments, not two additive ones**:

```
pinnedPct   = budget > 0 ? min(pinned_bytes / budget, 1) * 100 : 0
totalPct    = budget > 0 ? min(synced_size_bytes / budget, 1) * 100 : 0
unpinnedPct = max(totalPct - pinnedPct, 0)
```

Render the pinned segment first (indigo), then the unpinned remainder (green / amber above 70% /
red above 90%), inside a neutral track. Because `pinned_bytes ⊆ synced_size_bytes`, drawing them
additively would show 200% for a fully pinned folder.

When `over_budget` is true, add an over-budget treatment — a red hairline overflow cap at the right
edge of the track — so "at budget" and "over budget" are visually distinct despite both clamping to
100% width.

When `wedged` is true, render the D3 banner above the bar:

> **24.1 GB pinned exceeds 20 GB budget — nothing left to offload.**
> Unpin or archive some files to let coldstore free space.
> [Review auto-pinned files →]

The link sets the Gallery to the "Auto-pinned on restore" filter for that folder, oldest first, with
the bulk bar ready. A remedy the owner cannot act on in one click is not a remedy.

A `bulk_eviction` event (§5.10) renders as a dismissible strip in the StatusBar:

> **CameraRoll: 2,483 files (13.2 GB) were archived in the last enforcement pass** and removed from
> your synced devices. [See activity]

A folder whose `syncthing_type` is present and not `sendreceive` renders a persistent strip (§3.7):

> **CameraRoll is a Receive Only Syncthing folder** — coldstore cannot free space on your devices.
> Change it to Send & Receive in Syncthing, then re-enable the folder here.

`if (!status) return null` at `StatusBar.tsx:15` is deleted. On an API error the footer renders the
error line instead of vanishing; today a 401 and an empty install look identical.

The refresh button posts the scan job, shows its progress from `useJob`, stays disabled while
running, and refetches status on completion.

Label rows show `enabled: false` folders greyed with a "disabled" tag and `present_in_toml: false`
folders greyed with "not in coldstore.toml".

### 7.7 FileBrowser — checkbox column, pin column, archive action

- A leading `<th>`/`<td>` checkbox column. The header checkbox is tri-state over the loaded rows
  (`indeterminate` via a ref when some but not all are selected).
- Shift-click on a row checkbox calls `toggleRange`.
- A pin column: a clickable pin glyph, filled when pinned.
- Per-row actions: Download (always); Restore when `status === "archived"`; **Archive when
  `status === "synced"`** (new); Pin/Unpin (always).
- `handleRestore` (`FileBrowser.tsx:59`) and the identical defect in `Lightbox.tsx:53` currently
  `await fetch(...)` with no `res.ok` check and no refetch, so a successful restore leaves the row
  reading "archived" with its Restore button still showing until the folder changes. Both route
  through `apiFetch` and call `patchFile(id, returnedRecord)`; failures raise a toast.
- The `SelectionBar` from §7.5.

### 7.8 Lightbox

Add Pin/Unpin beside Restore, show the pinned state in the metadata strip, and route every action
through the parent's `patchFile` so the button state updates without a refetch. Add an Archive button
for synced files.

### 7.9 ConfigPanel — repaired

> **Status: IMPLEMENTED, with three deviations.**
> * The budget editor is a free-text size parsed by `parseSize`, not a "number of GB" box.
>   Budgets here span kilobytes (the demo installation) to terabytes, and a GB-only input renders
>   a 200 kB budget as `0.0002`. It accepts exactly what `coldstore.toml` accepts.
> * **The stray-archive line is not built.** §5.11's sweep is NOT IMPLEMENTED, so nothing writes
>   the report it would render, and a panel that can only ever say "nothing to report" is a claim
>   that cold storage has been swept when it has not.
> * **The `.stignore` chip is not built**, for the same reason: §3.8 is outstanding.
>
> The Syncthing chip renders only when `syncthing_type` is populated, which it never is until
> §3.7 lands. The "unverified — set `[syncthing] api_key`" variant is deliberately omitted: there
> is no Syncthing client in the daemon, so it would tell the owner to set a key that does nothing.

Once reconciliation seeds `folder_configs`, the cards appear (today the panel shows only "No folder
configs in database" on a fresh install, which also makes the persistent-path editor unreachable —
it renders only behind `selectedFolder`, which is only set by a button inside a card that never
exists).

Per folder card:

- **Budget** — a number input in GB plus a unit label, converted with `parseSize`, `PUT` on blur or
  Enter, with the live `synced_size_bytes` / `pinned_bytes` shown underneath. Rejects ≤ 0 client-side
  with an inline message before it can reach the server.
- **Grace** — a minutes number input, `PUT` on blur. 0 is valid and means "no grace"; negatives are
  rejected inline.
- **Enabled** — a toggle.
- Read-only: `syncthing_path`, `syncthing_id`, and the note "path and id come from coldstore.toml".
- Live `over_budget` / `wedged` chips and "last checked N min ago" from `last_enforced_at`.
- The persistent-path editor renders **always, for the selected folder**, and folder selection also
  responds to the sidebar so it is reachable independently.
- An **Activity** list: the last 10 **warning-class** events for the folder from
  `GET /api/v1/events?folder=...&class=warning`. The class filter is not cosmetic — without it a
  single enforcement pass writes hundreds of `archive` rows and the wedge warning, the cold-storage
  warning and the stray-archive report are all pushed out of the last ten within seconds. The
  warning class is every type in §4.4's table except `archive`, `restore` and `folder_seeded`.
- The persistent-path editor shows **"protects N of M files in this folder"** under each pattern,
  from `match_count` (§4.1a), plus the three matching rules in help text. A freshly typed pattern
  showing `0` is the feedback that stops an owner trusting their favourites to a glob that matches
  nothing.
- A **stray archive** line when §5.11's sweep has reported one: "47 files (2.3 GB) in cold storage
  are not referenced by the catalog", with the archive path.
- A **`.stignore`** chip when §3.8's check failed, naming the exact line to add.
- A **Syncthing** chip: `sendreceive` (ok), the actual type (error), or "unverified — set
  `[syncthing] api_key`".

`useFolders.updateFolder` narrows from `Partial<FolderConfig>` to
`{budget_bytes?: number; grace_minutes?: number; enabled?: boolean}`. As written today, the first
person to wire a budget editor with `updateFolder(label, {budget_bytes: n})` would silently write
`syncthing_path=''`, `grace_minutes=0` and `enabled=false` — which, once D1 makes enforcement read
this table, disables the folder on the first save.

### 7.10 App — error surfacing and the lint fix

- Move `handleFolderSelect` above the mount effect (or inline its two `setState` calls) to clear the
  `react-hooks/immutability` error at `App.tsx:23`, and add `selectedFolder` to the dep list.
- A top-level error banner fed by `useStatus().error`, distinguishing `401` ("check the username and
  password in coldstore.toml") from a network failure ("coldstore API unreachable"). Right now any
  API failure produces an empty sidebar, a null StatusBar and "Select a folder from the sidebar" —
  the blank-page symptom, with no message anywhere.
- A `ToastProvider` + `useToast()` context for transient action failures. Nothing like it exists and
  every per-file action and the bulk bar need somewhere to write.
- `useStatus` gains a 10-second polling interval, so `over_budget`, `wedged` and the budget bars
  update while a bulk job runs.

**Dev-mode note (D5, and see step 13):** basic auth wraps the whole mux including
`/api/v1/thumbnails/` and the SPA, and the Vite dev proxy forwards `/api` without credentials. With auth configured, the browser shows
its own native auth dialog on a 401 rather than the app's error UI, so the 401 branch of the error
banner cannot be exercised through `make dev-web` unless the dev proxy injects an `Authorization`
header. Add that header to `vite.config.ts` behind an env var.

---

## 8. Implementation order

Each step is independently landable and leaves the **whole module** green. "Green" is defined once,
here, and every step's proof includes it:

```
go build ./... && go vet ./... && go test ./... && (cd web && npm ci && npm run build)
```

`./...` includes `scripts/seed-demo` and `tests/e2e`. Two steps in the original plan were not
landable because they deleted catalog methods whose callers lived in packages the step never touched;
the rule that prevents a repeat is **a method is removed in the step that removes its last caller,
never earlier** (§3.1, §4.2), and every step below that removes or changes a signature carries an
explicit *call sites in this step* list.

**Migration numbers are assigned in landing order** (§1.3). Step 3 lands migration 2 and bumps
`schemaVersion` to 2, step 4 lands migration 3, step 8 lands migration 4, step 9 lands migration 5.
Never write a migration constant before the step that lands it: a binary that ships with
`schemaVersion = 5` and a `migrations` slice of `{1,2,5}` stamps `user_version = 5` on the owner's
database, and every migration numbered below that is skipped **forever** by
`if m.version <= current { continue }`. The jobs tables would then never be created and every job
endpoint would 500 with `no such table: jobs`, recoverable only by deleting the catalog — which
destroys the pins and budgets D1 and D2 exist to preserve.

Steps 1–3 are prerequisites for everything; do not reorder them.

---

**Step 0 — Unbreak the build and CI.**
Fix `tests/e2e/helpers_test.go:145` to pass `env.scanner` to `api.NewServer` (5 args). Add
`mkdir -p internal/api/dist && touch internal/api/dist/.gitkeep` to the unit and e2e CI jobs, which
the Makefile does and the workflow does not, so `//go:embed all:dist` fails on a clean checkout. Add
a `COPY coldstore.toml.example /app/coldstore.toml` to `docker/Dockerfile.test`, which
`docker/compose.test.yml` invokes with `--config /app/coldstore.toml`.
*Proof:* green as defined above, locally and on all CI jobs.

**Step 1 — Migration runner, no new columns.** *(schemaVersion 1)*
Add `migrate`, `PRAGMA user_version`, and `migration1InitialSchema` (the current `schemaSQL`
verbatim). Wire it into `Open`. Add `SetMaxOpenConns(1)`, `MkdirAll`, `Ping`, `_txlock=immediate`,
`writeSchemaMarker`, and the `--schema-version` / `--db-schema-version` flags with the `make install`
pre-check (§1.3a).
*Proof:* three new tests in `internal/catalog/catalog_test.go`.
`TestMigrateFreshDatabase` — `Open` a new path, assert `user_version == 1`.
`TestMigrateExistingPreVersionDatabase` — open a raw `sql.DB`, exec the old `schemaSQL` text, insert
a `files` row and a `folder_configs` row with `budget_bytes = 999`, close; then `catalog.Open` the
same path and assert no error, `user_version == 1`, both rows intact with `budget_bytes` still 999.
`TestMigrateIsIdempotent` — `Open`, `Close`, `Open` again; no error, version unchanged.
`TestMigrateRefusesNewerDatabase` — set `user_version = 99` by hand, assert `Open` errors.

**Step 2 — `fileColumns` + single scanner refactor.**
Pure refactor, no behaviour change, **no new columns**. The constant lists exactly the 14 columns that
exist at v1 (§2.6). Do not paste the final-state block from §2.6 into this step: it names four
columns that do not exist until migrations 2, 3 and 5 and four struct fields that do not exist until
§2.7, so `go build` fails on the scan destinations and, if you add the fields to get it compiling,
every test fails at runtime with `no such column: pinned`.
*Proof:* the entire existing test suite passes unchanged, and
`rg -c 'SELECT id, folder_label, relative_path' internal/catalog` returns **0**. (The obvious
alternative check, `rg 'relative_path, filename, extension' | wc -l`, returns 2 after a correct
refactor, not 1: `UpsertFile`'s INSERT list at `catalog.go:37` also matches, and so does the new
constant.)

**Step 3 — Migration 2: pins, and the pin primitives.** *(schemaVersion 2)*
Add `pinned`, `pinned_at`, `pin_source`, `idx_files_folder_status_pinned`, `idx_files_pin_source`.
Extend `fileColumns` and `scanFile`. Add `FileRecord.Pinned/PinnedAt/PinSource`, `setPinned`,
`setPinnedBulk`, `MarkRestored`, `MarkArchived`, `ClaimFile`, `ClaimUnpinnedFile`, `FolderSizes`. Add
`AND pinned = 0` and a `LIMIT` to `OffloadCandidates`.
*Call sites in this step:* `internal/daemon/daemon.go:161` (`OffloadCandidates` gains a `limit`
argument), and `Restorer.Restore` finishes with `MarkRestored`. `FolderSyncedSize` stays as a
`// Deprecated:` shim delegating to `FolderSizes`, so `daemon.go:131`, `handlers_status.go:42`,
`catalog_test.go:197` and `tests/e2e/api_status_test.go:62` keep compiling untouched.
*Proof:*
`TestUpsertFilePreservesPinnedOnRescan` — pin a file, `UpsertFile` the same
`(folder, relative_path)` again with a fresh `FileRecord`, assert `pinned`, `pinned_at` and
`pin_source` are unchanged. This is the guard against someone later adding `pinned = excluded.pinned`
to the `ON CONFLICT` list and wiping every pin on every scan.
`TestOffloadCandidatesExcludesPinned` — two eligible files, pin one, assert only the other comes back.
`TestFolderSizesCountsPinnedInTotal` — 100-byte pinned + 200-byte unpinned synced files ⇒
`(300, 100)`.
`TestMarkRestoredDoesNotDowngradeAUserPin` — pin with `pin_source='user'`, archive, restore, assert
`pin_source` is still `'user'`.
`TestRestoredFileIsPinnedAndSurvivesQuotaTick` (e2e, `tests/e2e/daemon_restore_test.go`) — the
regression that started this: archive a file, restore it, run `EnforceQuotas` with the folder over
budget, assert the file is still `synced` and still on disk.

**Step 4 — Migration 3 + D1 reconciliation + folder readiness.** *(schemaVersion 3)*
Add `present_in_toml`, `wedge_warned_at`, `last_enforced_at`, `files.last_seen` and their indexes.
Add `ReconcileFolderFromTOML`, `MarkFoldersAbsentFromTOML`, `UpdateFolderPolicy`,
`ActiveFolderConfigs`, `FolderConfigByLabel`, `FolderPath`, `ORDER BY folder_label` on
`ListFolderConfigs`. Call the reconciler from `main`. Add `FolderReady` (§3.6) and the
`newSyncthingFolder(t, dir)` test helper, and create `.stfolder` in every fixture. Switch
`EnforceQuotas`, `enforceFolder` and the scanner to `catalog.FolderConfigRecord` with the
`budget <= 0` guard, the per-folder grace with negatives clamped to 0, the `defer` epilogue
skeleton, and the per-iteration `FolderSizes` re-query. Replace `MarkMissingSyncedFiles` with the
`last_seen` generation sweep (§5.9) and the `d.Name()`-based skip rules.
*Call sites in this step:* `daemon.go:131` and `daemon.go:161` move to `FolderSizes` /
`ActiveFolderConfigs`; `internal/api/handlers_files.go:100 folderPath` and
`internal/daemon/restorer.go:72 folderPath` are deleted in favour of `cat.FolderPath`;
`daemon.go:237 generateThumbnails` uses `cat.FolderPath`. `UpsertFolderConfig` stays as a
`// Deprecated:` shim so `handlers_config.go:52`, `scripts/seed-demo/main.go:96` and
`catalog_test.go:268` keep compiling.
*Proof:*
`TestReconcileSeedsBudgetOnFirstInsertOnly` — reconcile with budget 100, `UpdateFolderPolicy` to 500,
reconcile again with budget 100, assert the budget is still 500 and `syncthing_path` was refreshed.
`TestReconcileMarksAbsentFolders` — reconcile A and B, then reconcile A alone; B has
`present_in_toml = 0`, still exists, and is absent from `ActiveFolderConfigs`.
`TestEnforcementUsesDatabaseBudget` (e2e) — start with a TOML budget of 50000, `UpdateFolderPolicy`
to 1000 with no restart, run a tick, assert files were offloaded down to 1000.
`TestZeroBudgetDoesNotArchiveEverything` — set `budget_bytes = 0`, run a tick, assert zero offloads
**and** that `last_enforced_at` was still stamped.
`TestScanDoesNotMarkFilesMissingWhileWalking` — start a scan against a folder with a slow
`WalkDir` (inject a fs), assert `FolderSizes` never returns 0 during the walk and no row is ever
observed as `missing`.
`TestScanWithUnavailableRootMarksNothingMissing` — remove `.stfolder`, scan, assert the error is
returned and every row keeps its status.
`TestScanIgnoresSyncthingTempFiles` — plant `.syncthing.X.mp4.tmp`, `2026/.syncthing.Y.mp4.tmp`,
`Z.coldstore-tmp` and `.stignore`, scan, assert none is catalogued.
`TestOrphanedFolderPathStillResolves` — mark a folder `present_in_toml = 0`, assert
`cat.FolderPath` still returns its path and the download handler serves a synced file from it.

**Step 5 — Tighten `PUT /api/v1/folders/{label}`, and remove `UpsertFolderConfig` from the API.**
Pointer fields, drop the infrastructure fields, 404 on unknown label, validation. Rewrite
`handlers_config.go` onto `UpdateFolderPolicy`.
*Call sites in this step:* `internal/api/handlers_config.go:52`,
`internal/catalog/catalog_test.go:268`.
*Proof:* rewrite `tests/e2e/api_config_test.go`. `TestAPIFolderUpdateRejectsUnknownLabel` — `PUT` to
`/api/v1/folders/Bogus` returns 404 and creates nothing (this replaces `TestAPIFolderCRUD`, which
asserts the opposite). `TestAPIFolderUpdateLeavesInfrastructureFieldsAlone` — a `PUT` carrying
`syncthing_path` does not change the stored path (replaces `TestAPIFolderUpdateOverwrites`).
`TestAPIFolderUpdatePartialDoesNotDisableFolder` — `PUT {"budget_bytes": 123}` leaves
`enabled = true`.

**Step 6 — D3 accounting through the API, and the event vocabulary.**
`FolderCounts`, `FolderBudget`, the `defer` epilogue body with the wedge latch, the §4.4 event-type
vocabulary, `LogEventDeduped` on `cold_storage_low` / `folder_unavailable`, `ListEvents` +
`GET /api/v1/events` with the `class=warning` filter, and the reworked `handleStatus` reading
`ListFolderConfigs` with no swallowed errors. Delete the `FolderSyncedSize` shim.
*Call sites in this step:* `internal/api/handlers_status.go:42`,
`internal/catalog/catalog_test.go:197` (`TestFolderSyncedSize` becomes `TestFolderSizes`),
`tests/e2e/api_status_test.go:62` (`TestAPIStatusFolderSyncedSize` becomes
`TestAPIStatusFolderSizes`).
*Proof:*
`TestStatusReportsPinnedBytesAsSubsetOfTotal` (e2e) — assert
`pinned_bytes <= synced_size_bytes` and both figures exact.
`TestWedgedFolderWarnsOnceAcrossTicks` — pin more bytes than the budget, run three enforcement ticks,
assert exactly one `folder_wedged` event; then set `wedge_warned_at` back 25 hours and tick again,
assert a second event.
`TestWedgeWarningSurvivesRestart` — tick, construct a fresh `Daemon` over the same catalog, tick
again, assert still exactly one event.
`TestUnwedgingClearsTheLatchOnTheWithinBudgetPath` — unpin below budget so the pass takes the
within-budget exit, tick, assert `wedge_warned_at IS NULL`. This is the epilogue-as-`defer`
regression: with the latch at the textual end of `enforceFolder` it never clears.
`TestColdStorageWarningIsNotSuppressedByTheWedgeWarning` — wedge a folder, tick, then drop free space
below 5% and tick again; assert **both** a `folder_wedged` and a `cold_storage_low` row exist.

**Step 7 — `Mover`, the move primitive, and single-file archive.**
`internal/daemon/mover.go`, constructed in `main`, injected into `api.Deps` and `daemon.New`.
`Offloader`/`Restorer` reduced to `MoveOut`/`MoveIn` with no catalog access; `Offloader.Offload`,
`Restorer.Restore` and `Restorer.folderPath` deleted. `enforceFolder` routes through
`Mover.ArchiveByID`'s automatic variant. `POST /api/v1/files/{id}/archive`,
`POST /api/v1/files/{id}/pin`, `DELETE .../pin`, `POST /api/v1/files/pin`,
`POST /api/v1/files/{id}/resolve`. Restore's 409s. **`crossDeviceMove` rewritten per §5.2b** — this
moves here from the old step 10, because every later step's moves depend on it and shipping the job
queue over the old primitive would widen the blast radius of every defect in it.
`UpsertFile` narrowed per §4.1b with its `(adopted, conflict, err)` return and the `MIN(file_mtime)`
rule.
*Seams this step must create, because the proofs below cannot be written without them:*
`Mover.moveFn func(src, dst string) error` (defaults to `moveFile`), and `crossDeviceMove` split into
`copyAndVerify(srcFile *os.File, dst io.Writer, h hash.Hash) (int64, error)` plus the path wrapper.
*Call sites in this step:* `internal/api/handlers_restore.go`, `internal/daemon/daemon.go:196`,
`tests/e2e/*` restore tests.
*Proof:*
`TestArchiveByIDRefusesNonSyncedFile` — 409, no filesystem change.
`TestArchiveByIDOnAlreadyArchivedFileIsANoOpSuccess` — invariant 12.
`TestArchiveByIDRespectsFreeSpaceGuard` — with `spaceCheck` stubbed to 2%, returns
`ErrColdStorageFull`, the file stays synced, and nothing is written to cold storage.
`TestClaimFileCASLosesCleanly` — flip a file to `archiving` behind the mover's back, then call
`ArchiveByID` with a `moveFn` that records its calls; assert `ErrFileStateChanged` and that `moveFn`
was **never invoked**.
`TestArchiveByIDRefusesWhenFolderUnavailable` — delete `.stfolder`, assert `ErrFolderUnavailable` and
no status write.
`TestRestoreByIDRefusesWhenDestinationOccupied` — plant a different file at the destination, assert
409, the row ends `synced`, the cold copy survives as `.coldstore-superseded`, and a `restore_conflict`
event exists.
`TestRestoreByIDCreatesMissingParentDirectories` — restore `2024/06/x.jpg` into a folder where
`2024/06/` no longer exists; asserts `MoveIn` kept the `MkdirAll`.
`TestCrossDeviceMoveVerifiesWhatLandedOnDisk` — via `copyAndVerify` with a writer that silently drops
the last byte: the move fails, the temp file is removed, `dst` does not exist, and `src` still does.
`TestCrossDeviceMovePreservesMtimeAndAtime` — archive a file with mtime 2019-06-12 and a known atime,
restore it, assert the on-disk mtime **and** atime and the post-rescan `files.file_mtime` are all
still 2019-06-12.
`TestCrossDeviceMoveDoesNotDeleteARewrittenSource` — after `copyAndVerify` returns, replace `src` with
a different inode (rename another file over it), assert `ErrSourceChanged`, `src` still exists with
the new content, and `dst` was set aside as `.coldstore-superseded`.
`TestCrossDeviceMoveRefusesAnExistingDestination` — assert `ErrDestinationExists` and that the
existing destination is byte-identical afterwards.
`TestUpsertFileDoesNotAdoptAnArchivedPath` — archive a file, recreate a **different** file at the same
relative path, scan, assert the row is still `archived` with its original `size_bytes` and that a
`conflict` event was written.

**Step 8 — Migration 4 + the job queue.** *(schemaVersion 4)*
Tables, the catalog job methods **including `FailRemainingItems`, `CompleteJobItemVirtual`, `FailJob`,
`ListFileIDs` and `FolderSizesForIDs`**, `Worker` with the `FailJob` error paths and the
consecutive-error backoff, `RecoverStartupState` (moved out of `ScanAll` — §5.6) called from `main`,
`ResumeInterruptedJobs`, the scan mutex, `POST/GET /api/v1/jobs`, the restore budget pre-flight and
`force`, `POST /api/v1/scan` returning 202, `GET /api/v1/files/ids`, `PurgeOldJobs`, §5.10's
`bulk_eviction` event, §5.11's stray sweep. `signal.NotifyContext` in `main`, `Run(ctx)`,
`http.Server.Shutdown`.
*Proof:*
`TestBulkRestoreJobCompletesAndPinsEverything` (e2e) — archive 20 files, enqueue one restore job,
drain the worker, assert `state == "done"`, `done == 20`, and all 20 are synced and pinned with
`pin_source = "restore"`.
`TestJobResumesAfterAKill` — enqueue 10 items; run the worker until item 4's **move has completed and
`MarkArchived` has committed but `CompleteJobItem` has not** (inject via a `moveFn` wrapper that
panics after the mover returns, or drive `Mover` directly and skip the bookkeeping); construct a fresh
worker over the same catalog, call `RecoverStartupState` then `ResumeInterruptedJobs`, drain, and
assert the job reaches `done` with `done == 10`, `failed_count == 0`, and no file moved twice. The
old context-cancel version of this test only ever exercised the graceful path, where the in-flight
item always reaches `CompleteJobItem`, so it asserted an outcome the kill path could not produce.
`TestJobItemFailureIsRecordedAndJobStateIsFailed` — one bad id among five good ones ⇒
`state == "failed"`, `done == 5`, `failed_count == 1`, and `failed[0].id` is the bad one.
`TestJobCountersReachTotalOnEveryTerminalPath` — four sub-cases (all-success, one item fails,
`ErrColdStorageFull` at item 12 of 800 via `FailRemainingItems`, `ClaimJobItem` error via `FailJob`);
each asserts `done == total` and `failed <= done` on the terminal row.
`TestFailedJobResponseIsAnArrayNotNull` — a fully successful job's `GET /jobs/{id}` body contains
`"failed": []`.
`TestRecoveryDoesNotRunFromScanAll` — a compile-and-grep guard: `rg -c 'recoverArchiving' internal/daemon/scanner.go` returns 0.
`TestRecoverRestoringPrefersTheColdSource` — plant a row in `restoring` with a **truncated** file at
the syncthing path and an intact one in cold storage; assert recovery reverts to `archived`, deletes
the partial, and leaves the cold copy untouched. Assert the opposite rule (existence alone) would
have failed it.
`TestRecoverArchivingDoesNotPromoteAPartial` — plant a short file at the archive path with the source
present; assert `synced` and that the partial is gone.
`TestRestoreJobOverBudgetIsRefusedWithoutForce` — 409 with `estimated_evictions > 0`; the same
request with `force: true` returns 202.
`TestConcurrentScanReturnsInProgress` — hold the scan mutex, call `ScanAll`, assert
`ErrScanInProgress` and that no row was marked missing.

**Step 9 — Thumbnail hardening (migration 5).** *(schemaVersion 5)*
`thumb_attempts`/`thumb_last_attempt`/`thumb_error`, `FilesNeedingThumbnails(exts, limit)` with
`thumb_attempts < 3`, `RecordThumbnailFailure`, `ResetThumbnailFailures(folder)`,
`exec.CommandContext` with a 30 s image / 60 s video deadline, video output written to `dst+".tmp"`
and renamed, `ProcessQueue(ctx, ...)`, and generation moved onto its own goroutine kicked by the tick
rather than run inline in the select loop.
*Seam this step must create:* `thumbnail.Generator.ffmpegPath string`, defaulting to `"ffmpeg"`, so
the timeout test can point it at a stub. `exec.Command("ffmpeg", ...)` is hardcoded at
`thumbnail.go:114` and `:147` today and the proof below cannot be written against it.
*Proof:*
`TestThumbnailFailureIsRecordedAndRetriedAtMostThreeTimes` — a corrupt file is attempted 3 times
across 5 queue runs and then never selected again.
`TestThumbnailGenerationRespectsLimit` — 500 eligible files, limit 200, exactly 200 returned.
`TestVideoThumbnailTimeoutLeavesNoPartialFile` — `ffmpegPath` points at a script that sleeps forever;
the call times out and no file exists at the final thumbnail path.

**Step 10 — Syncthing folder-type assertion (§3.7) and the `.stignore` check (§3.8).**
`internal/syncthing` with one read-only `GET /rest/config/folders` call, the startup assertion, the
`syncthing_misconfigured` / `syncthing_unverified` / `stignore_missing` events, `folderStatus.syncthing_type`.
*Proof:*
`TestReceiveOnlyFolderIsDisabledAtStartup` — an `httptest` Syncthing stub returning
`type: "receiveonly"`; assert `folder_configs.enabled == 0` and a `syncthing_misconfigured` event.
`TestUnreachableSyncthingDoesNotDisableAnything` — stub returns 500; assert every folder stays
enabled and one warning is logged.
`TestEmptyAPIKeySkipsTheCheck` — no key, no requests attempted, one `syncthing_unverified` event.

**Step 11 — Frontend foundations.** *(landed)*
`lib/format.ts` (SI, one copy), `lib/api.ts`, the compliant hook shape retrofitted onto `useStatus`,
`useFolders`, `usePersistentPaths` and `useFiles`, the `App.tsx` lint fix, the error banner, and the
`ToastProvider`.
*Proof:* `npm ci && npm run lint` reports **zero errors and zero warnings**, and `npm run build`
succeeds. Manually: stop the daemon, load the UI, see "coldstore API unreachable" instead of a blank
pane.

**Step 12 — Frontend control plane, and the last deletions.** *(landed; see §7.9 for what was
deliberately not built, and note the proof below is now two suites: `make test-web` — vitest and
Testing Library over jsdom — and `make test-playwright` — Chromium against the real daemon over a
freshly seeded demo installation.)*
`useSelection`, `useJob`, `useJobRunner`, `SelectionBar` with the bulk-pin endpoint and the
over-budget confirm dialog, the Gallery tile conversion with checkbox, pin and the six filters, the
FileBrowser checkbox and pin columns with the Archive action, the Lightbox pin, the StatusBar
two-segment bar, wedge banner, bulk-eviction strip and Syncthing strip, and the repaired ConfigPanel
with budget/grace/enabled editors, glob match counts and the warning-class Activity list.
*Call sites in this step:* `scripts/seed-demo/main.go:96` moves to `ReconcileFolderFromTOML` +
`UpdateFolderPolicy`, and **`UpsertFolderConfig` is deleted** — this is its last caller.
*Proof:* extend `scripts/seed-demo` first — it must produce pinned files with both `pin_source`
values, one deliberately wedged folder (`pinned_bytes > budget_bytes`), a disabled folder, an
orphaned folder (`present_in_toml = 0`), one conflicted file, a stray archive file, and a few files
with `thumb_attempts >= 3`, or none of these UI states are reachable in demo mode. Then a Playwright
run against the demo build asserting: shift-click selects a contiguous range; the bulk bar reports
"N selected"; a bulk restore shows progress and ends with the rows repainted as synced and pinned;
the wedged folder shows the banner with both figures and its link lands on the auto-pinned filter;
the budget bar's pinned segment never exceeds the total segment; editing a budget in ConfigPanel and
reloading shows the new value; a persistent pattern typed as `Albums/Starred/` reports a non-zero
match count and `Nope/*` reports zero.

**Step 13 — Deployment hardening (D5).**
`coldstore.toml.example` gains `username` / `password` (commented, with "required") and its
`listen_addr` becomes `192.168.1.10:8090` with a comment naming the LAN-interface rule; `main`
refuses to start when the API is bound to a non-loopback address with empty credentials;
`coldstore.service` gains `TimeoutStopSec=120` and `UMask=0077`; `docs/deployment.md` documents
`chmod 600`, the rollback hazard (§1.3a), and the Send & Receive requirement (§3.7).
*Proof:* `TestServerRefusesNonLoopbackBindWithoutCredentials` — `main`'s validation function returns
an error for `0.0.0.0:8090` with empty `username`, and no error for `127.0.0.1:8090`.

---

## 9. Documentation obligations

Landing this work without these leaves the docs actively lying, and three of them are load-bearing
for data safety rather than tidiness.

- **`[syncthing] api_url` / `api_key` become real (§3.7).** `docs/deployment.md` documents them as
  functional today and they are parsed and never read. Rather than deleting them, step 10 gives them
  exactly one job — verifying that each managed folder is Send & Receive — and the doc must describe
  that job and nothing more. Deleting them would remove the only cheap detection for the failure that
  silently voids the entire product.
- **Send & Receive is a requirement, stated next to the folder table** in both
  `docs/deployment.md` and `coldstore.toml.example`: *coldstore frees space on your devices by
  deleting files from this folder and letting Syncthing propagate the deletion. On a Receive Only
  folder nothing propagates, coldstore's budget does nothing, and clicking "Revert Local Changes" in
  Syncthing will start a full-library archive/restore flap. Set `api_key` and coldstore will check
  this for you at startup.*
- **Rolling back is dangerous (§1.3a).** `docs/deployment.md` gains a *Rolling back* section stating
  that an older binary opens a migrated catalog without complaint, reads neither `pinned` nor the
  database budget, and will archive every pinned file on its first quota tick — with Syncthing
  deleting each one from every device. Back up `catalog.db` before every upgrade; restore it
  alongside the older binary if you must roll back.
- **`budget` seeds the database on first run only** and is edited in the UI thereafter (§3.3).
  `coldstore.toml.example` and `docs/deployment.md` both say so, next to the `budget` key.
- **`.stfolder` is required** in every managed folder (§3.6) — coldstore refuses to scan, enforce or
  move a folder without it, because a bare directory is indistinguishable from an unmounted volume.
  **`.stignore` should contain `*.coldstore-tmp`** (§3.8); coldstore warns if it does not.
- **Persistent glob semantics** (§4.1a): a trailing `/` or `/*` protects the whole subtree; a pattern
  with a `/` matches the full relative path; a pattern without one also matches base names. The two
  example patterns in `coldstore.toml.example` change to `Albums/Starred/` and `Favorites/`.
- **D5, in full.** `coldstore.toml.example` ships `listen_addr = "192.168.1.10:8090"` with a comment
  that it must be the LAN interface, and commented `username` / `password` keys marked **required**.
  `docs/deployment.md` documents `chmod 600 /etc/coldstore/coldstore.toml`, and `coldstore.service`
  gains `UMask=0077`. `main` refuses to start when the bind address is not loopback and either
  credential is empty. Today the example ships `0.0.0.0:8090` with **no** `username`/`password` keys
  at all, `Server.Handler()` returns the bare mux when either is empty, and the only signal is one
  `API server starting without authentication` line in the journal — so a first-time deployment
  following the example puts an unauthenticated `POST /api/v1/jobs {"action":"archive"}` on the whole
  network, and coldstore's archives are Syncthing deletions on the owner's phone. The control plane
  turned a read-only viewer into a remote delete button; the posture has to move with it.
- **`coldstore.service` must gain `TimeoutStopSec=120`.** The default is 90 s and the initial scan
  plus the first thumbnail pass can exceed it, which is how rows end up stranded in `archiving` in
  the first place.
- **`docs/architecture.md`**: remove the PDF-thumbnail-via-`pdftoppm` claim (not implemented), and
  document explicitly that coldstore relies on Syncthing propagating its deletions — offloading is
  *meant* to free space on the phone. This is deliberate design, not a bug, and it must be written
  down before someone "fixes" it.

---

## 10. Reviewed and rejected

Three adversarial reviews were run against an earlier draft of this document. Every finding was
either folded into the text above or is recorded here with the reason it was not. Nothing was
dropped silently.

Two findings were **fixed by a different mechanism** than the reviewer proposed. The hazard is
closed in both cases; only the remedy differs, and the difference is recorded because a later reader
comparing the review to the document would otherwise think the finding was ignored.

**1. Unmounted-mountpoint detection: `.stfolder`, not an `st_dev` comparison.**
*Finding (blocking):* `Mover.RestoreByID` has no destination-availability check, so `MkdirAll` will
materialise a folder tree under an unmounted mountpoint on the root filesystem and the restore then
deletes each cold-storage source. **The hazard is real and is fixed in §3.6.** The proposed remedy
was to compare `st_dev` against the parent directory when `[[folders]]` declares a mountpoint. This
document uses `os.Stat(path/".stfolder")` instead: it needs no new config key, catches a wiped or
re-created volume as well as an unmounted one (an `st_dev` comparison does not), costs one `stat`,
and is the exact marker Syncthing itself invented to answer this question. An `st_dev` check would
also silently do nothing for the majority of folders, which are ordinary directories rather than
declared mountpoints.

**2. `recoverArchiving` with a complete archive copy and a vanished source: promote, do not delete.**
*Finding (serious):* a Syncthing-propagated deletion landing during an in-flight archive is
indistinguishable at recovery time from a completed move whose commit was lost, and the design's rule
can therefore resurrect a photo the owner deliberately deleted. **The ambiguity is real and is
handled in §5.6**, which logs `archive_unconfirmed` so the owner is told. The proposed remedy — treat
it as a user deletion, delete the stray copy, mark the row `missing` — is rejected. The two error
directions are not symmetric. Guessing "archived" wrong leaves a photo the owner meant to delete
sitting restorable in cold storage, visible in the Activity list and removable by hand. Guessing
"deleted" wrong destroys the only remaining copy of a file, irreversibly, with no way back. Invariant
9 decides it: keep the bytes, keep them referenced by a row, and say so loudly. §5.2b's temp-then-
rename ordering also shrinks the ambiguous window to the interval between one `rename` and one
`unlink`.

**3. Reverting a completed archive when a pin lands mid-move.**
*Finding (serious):* a pin set while an automatic offload of the same file is in flight is silently
discarded and the file is archived anyway, after the UI has confirmed the pin. **The hazard is real
and is fixed in §4.6** — `MarkArchived` no longer touches `pinned`, the automatic path claims with
`ClaimUnpinnedFile`, and `Mover.SetPinned` takes the mover mutex so the `200` cannot be returned
while a move of that file is in flight. The reviewer's *first* proposal, a post-move re-check that
discards the destination and rewrites the source when `pinned` became 1 during the copy, is rejected:
un-doing a completed, fsynced, re-hashed move introduces a second write path with its own torn-move
window, and it violates invariant 10 and invariant 9 to fix a case the mutex already prevents. The
reviewer's own fallback — "at minimum have `SetPinned` take the Mover mutex" — is what this document
adopts, plus an explicit `warning` in the pin response when the file finished archiving while the
pin was waiting, so the owner is never shown a pin badge on a file that just left the device.

**4. Client-side chunking of bulk jobs across multiple job ids.**
*Finding (blocking):* the 20000-id cap on `/files/ids` and the 5000-id cap on `POST /jobs` make
"select all in folder → Restore" fail with a 400 above 5,000 files. **The hazard is real and is fixed
in §6.2/§6.8** by raising the job cap to 20000 so both constants are the same symbol. The reviewer
offered chunking as the alternative; it is rejected. `useJobRunner` would need an array of job ids, a
cross-job counter aggregation, partial-failure semantics across jobs, and a progress strip that can
describe "job 2 of 3" — a second state machine in the frontend, for a case one integer constant
removes. One selection, one job.

No finding was rejected on the grounds that its premise was false. Every claim about the existing
tree that the reviews made was checked against `internal/`, `cmd/`, `scripts/`, `tests/` and
`coldstore.toml.example` at `68f59be`, and each one held: `recoverArchiving` really is called from
`ScanAll` (`scanner.go:30`) on the catalog-sync ticker; `crossDeviceMove` really has no `os.Chtimes`
and removes the source by path after a ninety-second window; `UpsertFile`'s `ON CONFLICT` really
flips `archived` to `synced`; `IsPersistent` really uses `filepath.Match` and both shipped example
patterns are of the broken shape; `strings.HasPrefix(relPath, ".st")` really does not match
`.syncthing.NAME.tmp`; `coldstore.toml.example` really ships `0.0.0.0:8090` with no credential keys;
`UpsertFolderConfig` and `FolderSyncedSize` really have callers in `scripts/seed-demo`,
`internal/api` and both test suites; and `rg 'relative_path, filename, extension' internal/catalog`
really returns 7 matches today and would return 2, not 1, after a correct step-2 refactor.
