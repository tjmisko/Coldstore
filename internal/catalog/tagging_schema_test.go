package catalog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTaggingMetadataShouldScanThroughSingleAndListQueries(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC)
	for _, path := range []string{"enriched.heic", "empty.jpg"} {
		if _, err := cat.UpsertFile(&FileRecord{
			FolderLabel:  "Photos",
			RelativePath: path,
			Filename:     path,
			Extension:    path[strings.LastIndex(path, "."):],
			FileMtime:    now,
			FileAtime:    now,
			Status:       StatusSynced,
		}, 1); err != nil {
			t.Fatal(err)
		}
	}

	enriched, err := cat.GetFile("Photos", "enriched.heic")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.Exec(`UPDATE files SET
		description = 'At the prime meridian',
		captured_local = '2025-01-02 03:04:05.006',
		captured_offset_minutes = 0, captured_source = 'exif',
		gps_lat = 0.0, gps_lon = 0.0, gps_altitude_m = -4.5,
		gps_accuracy_m = 0.75, gps_source = 'exif',
		pair_key = 'cid:ABC', pair_source = 'content_id',
		meta_scanned_at = 1001, meta_attempts = 2,
		meta_last_attempt = 1002, meta_error = 'retry retained'
		WHERE id = ?`, enriched.ID); err != nil {
		t.Fatal(err)
	}

	assertEnriched := func(t *testing.T, got *FileRecord) {
		t.Helper()
		if got.Description != "At the prime meridian" ||
			got.CapturedLocal != "2025-01-02 03:04:05.006" ||
			got.CapturedOffsetMin == nil || *got.CapturedOffsetMin != 0 ||
			got.CapturedSource != "exif" || got.GPSLat == nil || *got.GPSLat != 0 ||
			got.GPSLon == nil || *got.GPSLon != 0 || got.GPSAltitudeM == nil ||
			*got.GPSAltitudeM != -4.5 || got.GPSAccuracyM == nil || *got.GPSAccuracyM != 0.75 ||
			got.GPSSource != "exif" || got.PairKey != "cid:ABC" ||
			got.PairSource != "content_id" || got.MetaScannedAt == nil ||
			*got.MetaScannedAt != 1001 || got.MetaAttempts != 2 ||
			got.MetaLastAttempt == nil || *got.MetaLastAttempt != 1002 ||
			got.MetaError != "retry retained" {
			t.Errorf("scanned metadata = %+v", got)
		}
		if got.Tags == nil || len(got.Tags) != 0 {
			t.Errorf("Tags = %#v, want non-nil empty slice before tag hydration lands", got.Tags)
		}
	}

	got, err := cat.GetFileByID(enriched.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertEnriched(t, got)

	listed, err := cat.ListFiles(FileQuery{Folder: "Photos", SortBy: "name"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("ListFiles returned %d rows, want 2", len(listed))
	}
	assertEnriched(t, &listed[1])

	empty := listed[0]
	if empty.CapturedOffsetMin != nil || empty.GPSLat != nil || empty.GPSLon != nil ||
		empty.GPSAltitudeM != nil || empty.GPSAccuracyM != nil || empty.MetaScannedAt != nil ||
		empty.MetaLastAttempt != nil {
		t.Errorf("nullable defaults did not remain nil: %+v", empty)
	}
	if empty.Tags == nil || len(empty.Tags) != 0 {
		t.Errorf("default Tags = %#v, want []", empty.Tags)
	}
}

func TestFileRecordJSONShouldKeepUnknownMetadataDistinctFromOmittedDecorations(t *testing.T) {
	record := FileRecord{Tags: make([]string, 0)}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, required := range []string{
		`"captured_offset_minutes":null`, `"gps_lat":null`, `"gps_lon":null`,
		`"meta_scanned_at":null`, `"tags":[]`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("JSON %s does not contain required field %s", text, required)
		}
	}
	for _, omitted := range []string{"gps_altitude_m", "gps_accuracy_m", "meta_last_attempt"} {
		if strings.Contains(text, `"`+omitted+`"`) {
			t.Errorf("JSON %s unexpectedly contains omitted decoration %s", text, omitted)
		}
	}
}
