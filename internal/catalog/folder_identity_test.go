package catalog

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestReadFolderIdentitiesShouldNotCreateAMissingCatalog(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	identities, err := ReadFolderIdentities(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 0 {
		t.Errorf("identities = %+v, want none for a missing catalog", identities)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("read-only inspection created the catalog: stat error = %v", err)
	}
}

func TestReadFolderIdentitiesShouldReadALegacyCatalogWithoutMigratingOrWriting(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "catalog.db")
	raw, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(legacySchemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO folder_configs
		(folder_label, syncthing_id, syncthing_path, budget_bytes, grace_minutes, enabled)
		VALUES ('Photos', 'ph5wp-hswbp', '/srv/sync/Photos', 100, 30, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := ReadFolderIdentities(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	if len(identities) != 1 || identities[0].FolderLabel != "Photos" ||
		identities[0].SyncthingID != "ph5wp-hswbp" ||
		identities[0].SyncthingPath != "/srv/sync/Photos" {
		t.Errorf("identities = %+v, want the legacy Photos association", identities)
	}
	if string(after) != string(before) {
		t.Error("read-only identity inspection changed the database bytes")
	}
	if got := userVersion(t, dbPath); got != 0 {
		t.Errorf("user_version = %d, want legacy version 0: inspection must not migrate", got)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(dbPath + suffix); !os.IsNotExist(err) {
			t.Errorf("read-only identity inspection left %s behind: %v", suffix, err)
		}
	}
}

func TestListFolderIdentitiesShouldIncludeRemovedFolders(t *testing.T) {
	cat := setupTestCatalog(t)
	if _, err := cat.ReconcileFolderFromTOML("Photos", "", "/srv/sync/Photos", 100, 30); err != nil {
		t.Fatal(err)
	}
	if err := cat.MarkFoldersAbsentFromTOML(nil); err != nil {
		t.Fatal(err)
	}

	identities, err := cat.ListFolderIdentities()
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 || identities[0].FolderLabel != "Photos" {
		t.Errorf("identities = %+v, want removed Photos to retain its identity", identities)
	}
}
