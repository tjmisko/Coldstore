package catalog

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
	first.Close()

	second, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopening a current database: %v", err)
	}
	defer second.Close()

	if got := userVersion(t, dbPath); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}
	if _, err := second.GetFile("Photos", "a.jpg"); err != nil {
		t.Errorf("row lost across reopen: %v", err)
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
