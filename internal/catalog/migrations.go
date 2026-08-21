package catalog

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
)

type migration struct {
	version int
	sql     string
}

// migrations MUST be append-only and MUST stay in ascending version order.
// Never edit the SQL of a migration that has shipped; add a new one instead.
//
// A version number is ASSIGNED WHEN THE MIGRATION LANDS, never reserved in
// advance.
var migrations = []migration{
	{1, migration1InitialSchema},
	{2, migration2Pins},
	{3, migration3FolderState},
	{4, migration4Jobs},
	{5, migration5ThumbnailFailures},
	{6, migration6DetectedType},
}

// schemaVersion is the version a freshly migrated database reports.
// It equals the highest entry above and is bumped in the same commit as that
// entry.
const schemaVersion = 6

// SchemaVersion returns the schema version this binary writes.
func SchemaVersion() int { return schemaVersion }

// DatabaseSchemaVersion reports the user_version of an existing catalog file
// without migrating it. A file that does not exist reports 0.
func DatabaseSchemaVersion(dbPath string) (int, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("stating database: %w", err)
	}
	if err := ensureNoUncheckpointedWAL(dbPath); err != nil {
		return 0, err
	}
	db, err := sql.Open("sqlite3", readOnlyDSN(dbPath))
	if err != nil {
		return 0, fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("reading user_version: %w", err)
	}
	return version, nil
}

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

// schemaMarkerName is an advisory file written next to the catalog so an
// install check, a backup, or a human can see which schema wrote the database
// without opening SQLite.
const schemaMarkerName = ".coldstore-schema"

func writeSchemaMarker(dir string, version int) error {
	if dir == "" {
		dir = "."
	}
	return os.WriteFile(filepath.Join(dir, schemaMarkerName),
		[]byte(fmt.Sprintf("%d\n", version)), 0o644)
}

// migration1InitialSchema is the pre-migration schemaSQL blob, verbatim, with
// every IF NOT EXISTS clause intact. It must stay idempotent: a database
// created before the migration runner existed also reports user_version = 0,
// and migration 1 has to be a no-op against it rather than an error.
//
// Only migration 1 is idempotent. Migrations 2 and up are written bare so that
// a runner bug that replays one fails loudly instead of silently succeeding.
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

// migration2Pins adds the explicit per-file pin (D2, D3).
//
// The defaults are constants, which SQLite's ALTER TABLE ADD COLUMN
// requires. Every pre-existing row becomes unpinned with an empty pin_source,
// which is the correct starting state.
//
// pin_source is empty (never pinned), "user" (an explicit pin toggle), or
// "restore" (auto-pinned by a restore). The constraint is enforced in Go
// rather than SQL because ALTER TABLE ADD COLUMN cannot add a CHECK.
const migration2Pins = `
ALTER TABLE files ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE files ADD COLUMN pinned_at INTEGER;
ALTER TABLE files ADD COLUMN pin_source TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_files_folder_status_pinned
	ON files(folder_label, status, pinned, size_bytes);
CREATE INDEX idx_files_pin_source
	ON files(folder_label, pinned, pin_source, pinned_at);
`

// migration3FolderState adds folder reconciliation state, the wedge latch and
// the scan generation (D1, D3).
//
// present_in_toml defaults to 1 so that rows created before this migration are
// treated as declared until the first reconciliation pass corrects them.
//
// files.last_seen holds the epoch second at which the scan that last observed
// this file on disk started. DEFAULT 0 means every pre-existing row looks
// unseen, which is harmless: the missing-detection sweep only ever runs after
// a walk that completed without error, and that walk stamps every file it
// finds.
const migration3FolderState = `
ALTER TABLE folder_configs ADD COLUMN present_in_toml INTEGER NOT NULL DEFAULT 1;
ALTER TABLE folder_configs ADD COLUMN wedge_warned_at INTEGER;
ALTER TABLE folder_configs ADD COLUMN last_enforced_at INTEGER;

ALTER TABLE files ADD COLUMN last_seen INTEGER NOT NULL DEFAULT 0;

CREATE INDEX idx_files_folder_last_seen ON files(folder_label, status, last_seen);
CREATE INDEX idx_events_folder_type_ts ON events(folder, event_type, timestamp);
`

// migration4Jobs adds the async job queue (D4).
//
// There is deliberately no REFERENCES / ON DELETE CASCADE clause: foreign keys
// are off per-connection by default and the DSN does not enable them, so the
// clause would parse, be stored, and do nothing. job_items is deleted
// explicitly, in the same transaction as its jobs row.
//
// WITHOUT ROWID on job_items: the composite primary key is the entire useful
// content of the row alongside two small columns, and it gives free
// deduplication when the UI submits the same file id twice in one selection.
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

// migration5ThumbnailFailures adds thumbnail failure tracking.
//
// thumb_last_attempt is a nullable INTEGER epoch, not a DATETIME: ALTER TABLE
// ADD COLUMN rejects NOT NULL DEFAULT CURRENT_TIMESTAMP.
const migration5ThumbnailFailures = `
ALTER TABLE files ADD COLUMN thumb_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE files ADD COLUMN thumb_last_attempt INTEGER;
ALTER TABLE files ADD COLUMN thumb_error TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_files_thumb_queue
	ON files(thumbnail_path, thumb_attempts, status);
`

// migration6DetectedType adds the content-sniffed type (H0b).
//
// mime_type is derived from the extension, so it is empty for exactly the
// files this column exists to reach: the extension-less bookmark exports and
// notes that sync alongside a camera roll. An extension-keyed thumbnail or
// preview queue can never select them.
//
// The default is the empty string, which every pre-existing row therefore
// takes. That is the correct starting state and is load-bearing: empty means
// "not yet sniffed", so the next scan backfills the whole catalog once and
// then stops. A file the sniff genuinely cannot identify records
// 'application/octet-stream' rather than staying empty, so it is not re-read
// forever.
//
// The index is on (detected_type, thumbnail_path, thumb_attempts) rather than
// extending idx_files_thumb_queue, because the queue predicate becomes a
// disjunction -- extension IN (...) OR detected_type IN (...) -- and SQLite
// serves an OR of two indexed branches far better than one composite index it
// can only use for half the clause.
const migration6DetectedType = `
ALTER TABLE files ADD COLUMN detected_type TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_files_detected_type_queue
	ON files(detected_type, thumbnail_path, thumb_attempts, status);
`
