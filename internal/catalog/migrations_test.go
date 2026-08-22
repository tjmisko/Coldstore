package catalog

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// legacySchemaSQL is the CREATE TABLE IF NOT EXISTS blob catalog.Open executed
// before the migration runner existed. A database written by it reports
// user_version = 0, exactly like a brand-new file, and the runner has to
// handle both without being able to tell them apart.
const legacySchemaSQL = `
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

func userVersion(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

// createDatabaseAtVersion builds the exact schema emitted by a historical
// migration runner. Version zero uses the populated pre-runner schema rather
// than an empty file, because that is the dangerous upgrade case.
func createDatabaseAtVersion(t *testing.T, dbPath string, version int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if version == 0 {
		if _, err := db.Exec(legacySchemaSQL); err != nil {
			db.Close()
			t.Fatal(err)
		}
		return db
	}
	for _, migration := range migrations {
		if migration.version > version {
			break
		}
		if _, err := db.Exec(migration.sql); err != nil {
			db.Close()
			t.Fatalf("applying historical migration %d: %v", migration.version, err)
		}
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, migration.version)); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	return db
}

func tableColumnNames(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}

func TestMigrateShouldStampTheCurrentSchemaVersionWhenTheDatabaseIsFresh(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	cat, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	if got := userVersion(t, dbPath); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}
}

func TestMigration7ShouldCreateTheCompleteAdditiveSchemaOnAFreshDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fresh.db")
	cat, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	for _, table := range []string{"tags", "file_tags"} {
		var count int
		if err := cat.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
			WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("table %s count = %d, want 1", table, count)
		}
	}

	fileColumns := tableColumnNames(t, cat.db, "files")
	for _, column := range []string{
		"description", "captured_local", "captured_offset_minutes", "captured_source",
		"gps_lat", "gps_lon", "gps_altitude_m", "gps_accuracy_m", "gps_source",
		"pair_key", "pair_source", "meta_scanned_at", "meta_attempts",
		"meta_last_attempt", "meta_error",
	} {
		if !fileColumns[column] {
			t.Errorf("files.%s is missing", column)
		}
	}
	if !tableColumnNames(t, cat.db, "folder_configs")["display_name"] {
		t.Error("folder_configs.display_name is missing")
	}

	for _, index := range []string{
		"idx_file_tags_tag", "idx_files_meta_queue", "idx_files_captured",
		"idx_files_gps", "idx_files_pair",
	} {
		var count int
		if err := cat.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
			WHERE type = 'index' AND name = ?`, index).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("index %s count = %d, want 1", index, count)
		}
	}

	var fileTagsSQL string
	if err := cat.db.QueryRow(`SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = 'file_tags'`).Scan(&fileTagsSQL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(fileTagsSQL), "WITHOUT ROWID") {
		t.Errorf("file_tags DDL does not use WITHOUT ROWID: %s", fileTagsSQL)
	}

	if _, err := cat.db.Exec(`INSERT INTO tags (name, name_fold, created_at)
		VALUES ('Trips', 'trips', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.Exec(`INSERT INTO tags (name, name_fold, created_at)
		VALUES ('TRIPS', 'trips', 2)`); err == nil {
		t.Error("tags.name_fold must be unique")
	}
	if _, err := cat.db.Exec(`INSERT INTO file_tags (file_id, tag_id, applied_at)
		VALUES (99, 1, 3)`); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.Exec(`INSERT INTO file_tags (file_id, tag_id, applied_at)
		VALUES (99, 1, 4)`); err == nil {
		t.Error("file_tags composite primary key must reject duplicate applications")
	}
}

func TestMigration7ShouldUpgradeEveryOlderSchemaWithoutLosingRows(t *testing.T) {
	for version := 0; version < 7; version++ {
		t.Run(fmt.Sprintf("schema_%d", version), func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "catalog.db")
			raw := createDatabaseAtVersion(t, dbPath, version)
			mtime := time.Date(2020, time.March, 4, 5, 6, 7, 0, time.UTC)
			if _, err := raw.Exec(`INSERT INTO files
				(folder_label, relative_path, filename, extension, mime_type, size_bytes,
				 file_mtime, file_atime, status, thumbnail_path, content_hash)
				VALUES ('Photos', 'kept.jpg', 'kept.jpg', '.jpg', 'image/jpeg', 12345,
				        ?, ?, 'archived', 'thumb.jpg', 'abc123')`, mtime, mtime); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(`INSERT INTO folder_configs
				(folder_label, syncthing_id, syncthing_path, budget_bytes, grace_minutes, enabled)
				VALUES ('Photos', 'sync-id', '/srv/Photos', 98765, 75, 1)`); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}

			cat, err := Open(dbPath)
			if err != nil {
				t.Fatalf("opening schema %d: %v", version, err)
			}
			defer cat.Close()

			if got := userVersion(t, dbPath); got != schemaVersion {
				t.Errorf("user_version = %d, want %d", got, schemaVersion)
			}
			file, err := cat.GetFile("Photos", "kept.jpg")
			if err != nil {
				t.Fatal(err)
			}
			if file.SizeBytes != 12345 || file.Status != StatusArchived ||
				file.ThumbnailPath != "thumb.jpg" || file.ContentHash != "abc123" {
				t.Errorf("preserved file = %+v", file)
			}
			if file.Description != "" || file.CapturedLocal != "" || file.CapturedSource != "" ||
				file.CapturedOffsetMin != nil || file.GPSLat != nil || file.GPSLon != nil ||
				file.GPSAltitudeM != nil || file.GPSAccuracyM != nil || file.GPSSource != "" ||
				file.PairKey != "" || file.PairSource != "" || file.MetaScannedAt != nil ||
				file.MetaAttempts != 0 || file.MetaLastAttempt != nil || file.MetaError != "" {
				t.Errorf("migration 7 defaults = %+v", file)
			}
			if file.Tags == nil || len(file.Tags) != 0 {
				t.Errorf("Tags = %#v, want non-nil empty slice", file.Tags)
			}
			folder, err := cat.FolderConfigByLabel("Photos")
			if err != nil {
				t.Fatal(err)
			}
			if folder.BudgetBytes != 98765 || folder.GraceMinutes != 75 || folder.DisplayName != "" {
				t.Errorf("preserved folder = %+v", folder)
			}
		})
	}
}

func TestMigration8ShouldPreserveJobsAndWidenOnlyTheActionConstraint(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "schema7.db")
	raw := createDatabaseAtVersion(t, dbPath, 7)
	res, err := raw.Exec(`INSERT INTO jobs
		(action, folder_label, state, total, done, failed, error, created_at, started_at)
		VALUES ('archive', 'Photos', 'running', 1, 0, 0, 'kept', 11, 12)`)
	if err != nil {
		t.Fatal(err)
	}
	oldID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO job_items (job_id, file_id, state, error)
		VALUES (?, 42, 'running', 'item kept')`, oldID); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	cat, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	job, _, err := cat.GetJob(oldID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if job.Action != JobActionArchive || job.FolderLabel != "Photos" || job.State != JobStateRunning ||
		job.Error != "kept" || job.Payload != "" || job.CreatedAt != 11 || job.StartedAt == nil || *job.StartedAt != 12 {
		t.Fatalf("preserved job = %+v", job)
	}
	var itemState, itemError string
	if err := cat.db.QueryRow(`SELECT state, error FROM job_items WHERE job_id = ? AND file_id = 42`,
		oldID).Scan(&itemState, &itemError); err != nil {
		t.Fatal(err)
	}
	if itemState != JobStateRunning || itemError != "item kept" {
		t.Fatalf("preserved job item = %q, %q", itemState, itemError)
	}
	newID, err := cat.CreateRenameJob("Photos", "old", "new")
	if err != nil {
		t.Fatalf("rename action was not admitted: %v", err)
	}
	if newID <= oldID {
		t.Fatalf("post-migration job id = %d, want greater than preserved %d", newID, oldID)
	}
	if _, err := cat.db.Exec(`INSERT INTO jobs (action, created_at) VALUES ('delete', 1)`); err == nil {
		t.Fatal("migration widened the jobs action CHECK beyond rename")
	}
	var indexes int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_jobs_state'`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != 1 {
		t.Fatalf("idx_jobs_state count = %d, want 1", indexes)
	}
}

func TestMigration7ShouldPreserveEverySchema6Field(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "schema6.db")
	raw := createDatabaseAtVersion(t, dbPath, 6)
	mtime := time.Date(2021, time.April, 5, 6, 7, 8, 0, time.UTC)
	if _, err := raw.Exec(`INSERT INTO files
		(folder_label, relative_path, filename, extension, mime_type, size_bytes,
		 file_mtime, file_atime, status, thumbnail_path, content_hash,
		 pinned, pinned_at, pin_source, last_seen, thumb_attempts,
		 thumb_last_attempt, thumb_error, detected_type)
		VALUES ('Photos', 'live.heic', 'live.heic', '.heic', 'image/heic', 456,
		        ?, ?, 'synced', 'live.thumb.jpg', 'hash', 1, 101, 'user', 202,
		        2, 303, 'old failure', 'image/heic')`, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	cat, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	file, err := cat.GetFile("Photos", "live.heic")
	if err != nil {
		t.Fatal(err)
	}
	if !file.Pinned || file.PinnedAt == nil || *file.PinnedAt != 101 ||
		file.PinSource != PinSourceUser || file.LastSeen != 202 || file.ThumbAttempts != 2 ||
		file.ThumbLastAttempt == nil || *file.ThumbLastAttempt != 303 ||
		file.ThumbError != "old failure" || file.DetectedType != "image/heic" {
		t.Errorf("schema 6 fields changed during migration: %+v", file)
	}
}

func TestOpenShouldCreateTheDatabaseDirectoryWhenItDoesNotExist(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nested", "deeper", "catalog.db")
	cat, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("database file not created: %v", err)
	}
}

func TestOpenShouldWriteTheAdvisorySchemaMarkerWhenMigrationSucceeds(t *testing.T) {
	dir := t.TempDir()
	cat, err := Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	marker, err := os.ReadFile(filepath.Join(dir, schemaMarkerName))
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%d\n", schemaVersion); string(marker) != want {
		t.Errorf("marker = %q, want %q", string(marker), want)
	}
}

func TestMigrateShouldPreserveEveryRowWhenTheDatabaseUsesTheOldSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	// Build a database exactly as the pre-migration binary would have, and
	// populate it with the two kinds of row the owner cannot afford to lose:
	// a catalogued file and a folder policy they tuned by hand.
	raw, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(legacySchemaSQL); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2019, 6, 12, 10, 30, 0, 0, time.UTC)
	if _, err := raw.Exec(`INSERT INTO files
		(folder_label, relative_path, filename, extension, mime_type, size_bytes,
		 file_mtime, file_atime, status, thumbnail_path)
		VALUES ('Photos', '2019/IMG_1.jpg', 'IMG_1.jpg', '.jpg', 'image/jpeg', 4096,
		        ?, ?, 'archived', 'Photos/IMG_1.webp')`, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO folder_configs
		(folder_label, syncthing_id, syncthing_path, budget_bytes, grace_minutes, enabled)
		VALUES ('Photos', 'abcd-1234', '/srv/sync/Photos', 999, 45, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO persistent_paths (folder_label, pattern)
		VALUES ('Photos', 'Albums/Starred/')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO events (event_type, folder, path, details)
		VALUES ('archive', 'Photos', '2019/IMG_1.jpg', 'offloaded')`); err != nil {
		t.Fatal(err)
	}
	if got := userVersion(t, dbPath); got != 0 {
		t.Fatalf("legacy database reports user_version %d, want 0", got)
	}
	raw.Close()

	cat, err := Open(dbPath)
	if err != nil {
		t.Fatalf("migrating a populated legacy database: %v", err)
	}
	defer cat.Close()

	if got := userVersion(t, dbPath); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}

	file, err := cat.GetFile("Photos", "2019/IMG_1.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if file.SizeBytes != 4096 {
		t.Errorf("SizeBytes = %d, want 4096", file.SizeBytes)
	}
	if file.Status != StatusArchived {
		t.Errorf("Status = %q, want archived", file.Status)
	}
	if file.ThumbnailPath != "Photos/IMG_1.webp" {
		t.Errorf("ThumbnailPath = %q, want Photos/IMG_1.webp", file.ThumbnailPath)
	}
	if !file.FileMtime.Equal(mtime) {
		t.Errorf("FileMtime = %v, want %v", file.FileMtime, mtime)
	}

	// The new columns arrive at their documented defaults, not as NULLs.
	if file.Pinned {
		t.Error("a pre-existing row must migrate in unpinned")
	}
	if file.PinSource != PinSourceNone {
		t.Errorf("PinSource = %q, want empty", file.PinSource)
	}
	if file.PinnedAt != nil {
		t.Errorf("PinnedAt = %v, want nil", *file.PinnedAt)
	}
	if file.LastSeen != 0 {
		t.Errorf("LastSeen = %d, want 0", file.LastSeen)
	}
	if file.ThumbAttempts != 0 {
		t.Errorf("ThumbAttempts = %d, want 0", file.ThumbAttempts)
	}
	if file.DetectedType != "" {
		// Empty means "never sniffed", which is what puts a migrated row into
		// FilesNeedingSniff so the catalog backfills itself once.
		t.Errorf("DetectedType = %q, want empty", file.DetectedType)
	}

	fc, err := cat.FolderConfigByLabel("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if fc.BudgetBytes != 999 {
		t.Errorf("BudgetBytes = %d, want 999 (the user's tuned value)", fc.BudgetBytes)
	}
	if fc.GraceMinutes != 45 {
		t.Errorf("GraceMinutes = %d, want 45", fc.GraceMinutes)
	}
	if !fc.PresentInTOML {
		t.Error("a row that predates present_in_toml must be treated as declared")
	}
	if fc.WedgeWarnedAt != nil || fc.LastEnforcedAt != nil {
		t.Error("wedge_warned_at and last_enforced_at must migrate in as NULL")
	}

	paths, err := cat.ListPersistentPaths("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0].Pattern != "Albums/Starred/" {
		t.Errorf("persistent paths = %+v, want the one seeded pattern", paths)
	}

	var events int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Errorf("events = %d, want 1", events)
	}
}

func TestMigrateShouldChangeNothingWhenTheDatabaseIsAlreadyCurrent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	first, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.UpsertFile(&FileRecord{
		FolderLabel:  "Photos",
		RelativePath: "a.jpg",
		Filename:     "a.jpg",
		Extension:    ".jpg",
		SizeBytes:    10,
		FileMtime:    time.Now().UTC(),
		Status:       StatusSynced,
	}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	file, err := first.GetFile("Photos", "a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.ReconcileFolderFromTOML("Photos", "sync-id", "/srv/Photos", 1000, 30); err != nil {
		t.Fatal(err)
	}
	if _, err := first.db.Exec(`UPDATE files SET
		description = 'kept caption', captured_local = '2024-05-06 07:08:09.010',
		captured_offset_minutes = -420, captured_source = 'exif',
		gps_lat = 0.0, gps_lon = 0.0, gps_altitude_m = -12.5,
		gps_accuracy_m = 3.25, gps_source = 'exif',
		pair_key = 'cid:uuid', pair_source = 'content_id',
		meta_scanned_at = 111, meta_attempts = 2, meta_last_attempt = 222,
		meta_error = 'kept error' WHERE id = ?`, file.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := first.db.Exec(`UPDATE folder_configs SET display_name = 'Family Photos'
		WHERE folder_label = 'Photos'`); err != nil {
		t.Fatal(err)
	}
	res, err := first.db.Exec(`INSERT INTO tags (name, name_fold, created_at)
		VALUES ('Trips/Iceland', 'trips/iceland', 123)`)
	if err != nil {
		t.Fatal(err)
	}
	tagID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.db.Exec(`INSERT INTO file_tags (file_id, tag_id, applied_at)
		VALUES (?, ?, 456)`, file.ID, tagID); err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopening a current database: %v", err)
	}
	defer second.Close()

	if got := userVersion(t, dbPath); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}
	got, err := second.GetFile("Photos", "a.jpg")
	if err != nil {
		t.Fatalf("row lost across reopen: %v", err)
	}
	if got.Description != "kept caption" || got.CapturedLocal != "2024-05-06 07:08:09.010" ||
		got.CapturedOffsetMin == nil || *got.CapturedOffsetMin != -420 ||
		got.GPSLat == nil || *got.GPSLat != 0 || got.GPSLon == nil || *got.GPSLon != 0 ||
		got.GPSAltitudeM == nil || *got.GPSAltitudeM != -12.5 ||
		got.GPSAccuracyM == nil || *got.GPSAccuracyM != 3.25 ||
		got.PairKey != "cid:uuid" || got.MetaScannedAt == nil || *got.MetaScannedAt != 111 ||
		got.MetaAttempts != 2 || got.MetaLastAttempt == nil || *got.MetaLastAttempt != 222 ||
		got.MetaError != "kept error" {
		t.Errorf("migration 7 fields changed across current reopen: %+v", got)
	}
	folder, err := second.FolderConfigByLabel("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if folder.DisplayName != "Family Photos" {
		t.Errorf("DisplayName = %q, want Family Photos", folder.DisplayName)
	}
	var tagLinks int
	if err := second.db.QueryRow(`SELECT COUNT(*) FROM file_tags ft
		JOIN tags t ON t.id = ft.tag_id
		WHERE ft.file_id = ? AND t.name_fold = 'trips/iceland'`, got.ID).Scan(&tagLinks); err != nil {
		t.Fatal(err)
	}
	if tagLinks != 1 {
		t.Errorf("preserved tag links = %d, want 1", tagLinks)
	}
}

func TestMigrateShouldRefuseToOpenWhenTheDatabaseIsNewerThanTheBinary(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "future.db")

	raw, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	cat, err := Open(dbPath)
	if err == nil {
		cat.Close()
		t.Fatal("Open must refuse a database written by a newer binary")
	}
}

func TestMigrateShouldLeaveThePreviousVersionInPlaceWhenAMigrationFails(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "broken.db")

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// A one-off runner whose second migration is a valid statement followed by
	// a broken one: proof that the version bump and the DDL commit together.
	saved := migrations
	migrations = []migration{
		{1, migration1InitialSchema},
		{2, `CREATE TABLE half_applied (id INTEGER); THIS IS NOT SQL;`},
	}
	defer func() { migrations = saved }()

	if err := migrate(db); err == nil {
		t.Fatal("migrate must report a failing migration")
	}

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Errorf("user_version = %d, want 1 (the last version that fully applied)", version)
	}

	var tables int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'half_applied'`).
		Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Error("a failed migration must not leave partial DDL behind")
	}
}

func TestDatabaseSchemaVersionShouldReportZeroWhenTheCatalogDoesNotExist(t *testing.T) {
	version, err := DatabaseSchemaVersion(filepath.Join(t.TempDir(), "absent.db"))
	if err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Errorf("version = %d, want 0", version)
	}
}

func TestDatabaseSchemaVersionShouldReportTheStoredVersionWhenTheCatalogExists(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	cat, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cat.Close()

	version, err := DatabaseSchemaVersion(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion() {
		t.Errorf("version = %d, want %d", version, SchemaVersion())
	}
}

func TestMigrationsShouldBeAppendOnlyAndAscendingWhenTheRunnerLoadsThem(t *testing.T) {
	for i, m := range migrations {
		if m.version != i+1 {
			t.Fatalf("migrations[%d].version = %d, want %d: migration numbers are assigned "+
				"in landing order and must never be reserved in advance", i, m.version, i+1)
		}
	}
	if last := migrations[len(migrations)-1].version; last != schemaVersion {
		t.Errorf("schemaVersion = %d but the highest migration is %d", schemaVersion, last)
	}
}
