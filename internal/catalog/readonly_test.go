package catalog

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenReadOnlyShouldNotCreateAMissingCatalog(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing", "catalog.db")
	cat, err := OpenReadOnly(dbPath)
	if cat != nil {
		cat.Close()
	}
	if !errors.Is(err, ErrCatalogNotFound) {
		t.Fatalf("error = %v, want ErrCatalogNotFound", err)
	}
	if _, err := os.Stat(filepath.Dir(dbPath)); !os.IsNotExist(err) {
		t.Errorf("read-only open created the parent directory: %v", err)
	}
}

func TestOpenReadOnlyShouldRefuseWritesWithoutChangingTheCatalog(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "catalog.db")
	writable, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writable.ReconcileFolderFromTOML("Photos", "", "/srv/photos", 100, 30); err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sidecarsBefore := make(map[string]bool)
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_, err := os.Stat(dbPath + suffix)
		sidecarsBefore[suffix] = err == nil
	}

	readOnly, err := OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readOnly.ListFolderConfigs(); err != nil {
		t.Fatalf("read through read-only catalog: %v", err)
	}
	if err := readOnly.LogEvent("must_not_write", "", "", ""); err == nil {
		t.Fatal("query-only catalog accepted a write")
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("read-only catalog access changed database bytes")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_, err := os.Stat(dbPath + suffix)
		if !sidecarsBefore[suffix] && !os.IsNotExist(err) {
			t.Errorf("read-only open created %s: %v", suffix, err)
		}
	}
}

func TestOpenReadOnlyShouldNotMigrateANewerCatalog(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cat.Close()
	raw, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	readOnly.Close()
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || userVersion(t, dbPath) != 99 {
		t.Error("read-only open changed or migrated the newer catalog")
	}
}

func TestOpenReadOnlyShouldRefuseAnUncheckpointedWALInsteadOfIgnoringOrCheckpointingIt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	writable, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer writable.Close()
	if _, err := writable.ReconcileFolderFromTOML("Photos", "", "/srv/photos", 100, 30); err != nil {
		t.Fatal(err)
	}
	walBefore, err := os.ReadFile(dbPath + "-wal")
	if err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenReadOnly(dbPath)
	if readOnly != nil {
		readOnly.Close()
	}
	if !errors.Is(err, ErrUncheckpointedWAL) {
		t.Fatalf("error = %v, want ErrUncheckpointedWAL", err)
	}
	walAfter, err := os.ReadFile(dbPath + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(walBefore, walAfter) {
		t.Error("read-only refusal checkpointed or changed the WAL")
	}
}
