package catalog

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUpdateFilePreservesAndClearsOwnerMetadata(t *testing.T) {
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	_, err = cat.UpsertFile(&FileRecord{
		FolderLabel: "photos", RelativePath: "a.jpg", Filename: "a.jpg",
		Extension: ".jpg", Status: StatusSynced, FileMtime: time.Now(),
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	file, err := cat.GetFile("photos", "a.jpg")
	if err != nil {
		t.Fatal(err)
	}

	description := "jellyfish near the pier"
	captured := "2024-02-29 03:04:05.006"
	updated, err := cat.UpdateFile(file.ID, FileEdits{
		Description:   &description,
		CapturedLocal: &captured,
		GPS:           &GPSCoordinates{Lat: 0, Lon: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Description != description || updated.CapturedLocal != captured ||
		updated.CapturedSource != "manual" || updated.GPSSource != "manual" {
		t.Fatalf("updated metadata = %+v", updated)
	}
	if updated.GPSLat == nil || updated.GPSLon == nil || *updated.GPSLat != 0 || *updated.GPSLon != 0 {
		t.Fatalf("zero coordinates were not preserved: %v/%v", updated.GPSLat, updated.GPSLon)
	}

	_, err = cat.db.Exec(`UPDATE files SET meta_scanned_at = 10, meta_attempts = 3,
		meta_last_attempt = 9, meta_error = 'old error' WHERE id = ?`, file.ID)
	if err != nil {
		t.Fatal(err)
	}
	empty := ""
	cleared, err := cat.UpdateFile(file.ID, FileEdits{CapturedLocal: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.CapturedLocal != "" || cleared.CapturedSource != "" ||
		cleared.MetaScannedAt != nil || cleared.MetaAttempts != 0 ||
		cleared.MetaLastAttempt != nil || cleared.MetaError != "" {
		t.Fatalf("cleared capture queue state = %+v", cleared)
	}
	if cleared.GPSSource != "manual" || cleared.GPSLat == nil || cleared.GPSLon == nil {
		t.Fatalf("clearing capture metadata overwrote manual GPS: %+v", cleared)
	}
}

func TestUpdateFileRejectsUnknownID(t *testing.T) {
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	description := "no row"
	if _, err := cat.UpdateFile(999, FileEdits{Description: &description}); err != ErrNoSuchFile {
		t.Fatalf("error = %v, want ErrNoSuchFile", err)
	}
}
