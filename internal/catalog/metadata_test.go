package catalog

import (
	"slices"
	"testing"
	"time"

	metadatapkg "github.com/tjmisko/coldstore/internal/metadata"
)

func TestFilesNeedingMetadataSelectsOnlySupportedUnscannedStableFiles(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	synced := addFile(t, cat, "Photos", "synced.jpg", 1, now)
	archived := addFile(t, cat, "Photos", "archived.jpg", 1, now.Add(-time.Minute))
	if err := cat.UpdateFileStatus(archived.ID, StatusArchived); err != nil {
		t.Fatal(err)
	}

	done := addFile(t, cat, "Photos", "done.jpg", 1, now.Add(-2*time.Minute))
	if err := cat.SetExtractedMetadata(done.ID, metadatapkg.Extracted{}, now.Unix()); err != nil {
		t.Fatal(err)
	}

	exhausted := addFile(t, cat, "Photos", "exhausted.jpg", 1, now.Add(-3*time.Minute))
	for range maxMetadataAttempts {
		if err := cat.RecordMetadataFailure(exhausted.ID, "bad metadata", now.Unix()); err != nil {
			t.Fatal(err)
		}
	}

	missing := addFile(t, cat, "Photos", "missing.jpg", 1, now.Add(-4*time.Minute))
	if err := cat.UpdateFileStatus(missing.ID, StatusMissing); err != nil {
		t.Fatal(err)
	}

	unsupported := addFile(t, cat, "Photos", "document.pdf", 1, now.Add(-5*time.Minute))
	if _, err := cat.db.Exec(`UPDATE files SET extension = '.pdf' WHERE id = ?`, unsupported.ID); err != nil {
		t.Fatal(err)
	}

	queued, err := cat.FilesNeedingMetadata(metadatapkg.SupportedExtensions(), 100)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]int64, 0, len(queued))
	for _, file := range queued {
		got = append(got, file.ID)
	}
	want := []int64{synced.ID, archived.ID}
	if !slices.Equal(got, want) {
		t.Fatalf("queued IDs = %v, want %v", got, want)
	}

	empty, err := cat.FilesNeedingMetadata(nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("nil extension allowlist queued %d files", len(empty))
	}
}

func TestFilesNeedingMetadataUsesABoundedDefault(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()
	for i := 0; i < defaultMetadataLimit+25; i++ {
		addFile(t, cat, "Photos", "file-"+itoa(i)+".jpg", 1,
			now.Add(-time.Duration(i)*time.Second))
	}

	queued, err := cat.FilesNeedingMetadata([]string{".jpg"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != defaultMetadataLimit {
		t.Fatalf("default queue size = %d, want %d", len(queued), defaultMetadataLimit)
	}
}

func TestSetExtractedMetadataPreservesManualOwnerValuesAtomically(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()
	file := addFile(t, cat, "Photos", "manual.jpg", 1, now)

	if _, err := cat.db.Exec(`UPDATE files SET
		captured_local = '1998-07-04 12:34:56',
		captured_offset_minutes = -420,
		captured_source = 'manual',
		gps_lat = 1.25, gps_lon = -2.5, gps_altitude_m = 3.75,
		gps_accuracy_m = 4.5, gps_source = 'exif',
		pair_key = 'cid:existing-authority', pair_source = 'content_id'
		WHERE id = ?`, file.ID); err != nil {
		t.Fatal(err)
	}
	if err := cat.RecordMetadataFailure(file.ID, "temporary read error", 100); err != nil {
		t.Fatal(err)
	}

	offset := 120
	lat, lon, altitude, accuracy := 0.0, 0.0, -7.25, 0.5
	extracted := metadatapkg.Extracted{
		CapturedLocal:  "2025-08-09 10:11:12.013",
		CapturedOffset: &offset,
		CapturedSource: "exif",
		Lat:            &lat,
		Lon:            &lon,
		AltitudeM:      &altitude,
		AccuracyM:      &accuracy,
		GPSSource:      "quicktime",
		ContentID:      "  NEW-AUTHORITY  ",
	}
	if err := cat.SetExtractedMetadata(file.ID, extracted, 200); err != nil {
		t.Fatal(err)
	}

	got, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CapturedLocal != "1998-07-04 12:34:56" || got.CapturedOffsetMin == nil ||
		*got.CapturedOffsetMin != -420 || got.CapturedSource != "manual" {
		t.Errorf("manual capture was overwritten: %+v", got)
	}
	if got.GPSLat == nil || *got.GPSLat != 0 || got.GPSLon == nil || *got.GPSLon != 0 ||
		got.GPSAltitudeM == nil || *got.GPSAltitudeM != -7.25 ||
		got.GPSAccuracyM == nil || *got.GPSAccuracyM != 0.5 || got.GPSSource != "quicktime" {
		t.Errorf("extracted GPS was not recorded with real zero coordinates: %+v", got)
	}
	if got.PairKey != "cid:existing-authority" || got.PairSource != "content_id" {
		t.Errorf("metadata pass overwrote existing authority: key=%q source=%q", got.PairKey, got.PairSource)
	}
	if got.MetaScannedAt == nil || *got.MetaScannedAt != 200 || got.MetaAttempts != 1 ||
		got.MetaLastAttempt == nil || *got.MetaLastAttempt != 100 || got.MetaError != "" {
		t.Errorf("success bookkeeping = scanned %v attempts %d last %v error %q",
			got.MetaScannedAt, got.MetaAttempts, got.MetaLastAttempt, got.MetaError)
	}
}

func TestSetExtractedMetadataPersistsNormalizedContentIDOverABasenameGuess(t *testing.T) {
	cat := setupTestCatalog(t)
	file := addFile(t, cat, "Photos", "clip.mov", 1, time.Now().UTC())
	if _, err := cat.db.Exec(`UPDATE files SET extension = '.mov',
		pair_key = 'stem:photos/clip', pair_source = 'basename'
		WHERE id = ?`, file.ID); err != nil {
		t.Fatal(err)
	}

	if err := cat.SetExtractedMetadata(file.ID, metadatapkg.Extracted{
		ContentID: "  F61FDA97-19FB-4D2E-9173-3B6128AD0FD4  ",
	}, 123); err != nil {
		t.Fatal(err)
	}
	got, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PairKey != "cid:f61fda97-19fb-4d2e-9173-3b6128ad0fd4" ||
		got.PairSource != "content_id" {
		t.Errorf("pair authority = %q (%q), want normalized content id",
			got.PairKey, got.PairSource)
	}
}

func TestSetExtractedMetadataDoesNotOverwriteAFutureManualPairAuthority(t *testing.T) {
	cat := setupTestCatalog(t)
	file := addFile(t, cat, "Photos", "manually-paired.mov", 1, time.Now().UTC())
	if _, err := cat.db.Exec(`UPDATE files SET extension = '.mov',
		pair_key = 'manual:chosen-pair', pair_source = 'manual'
		WHERE id = ?`, file.ID); err != nil {
		t.Fatal(err)
	}

	if err := cat.SetExtractedMetadata(file.ID, metadatapkg.Extracted{
		ContentID: "different-content-id",
	}, 123); err != nil {
		t.Fatal(err)
	}
	got, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PairKey != "manual:chosen-pair" || got.PairSource != "manual" {
		t.Errorf("manual pair authority was overwritten: %q (%q)", got.PairKey, got.PairSource)
	}
}

func TestSetExtractedMetadataPreservesManualGPSAndRecordsAnEmptySuccess(t *testing.T) {
	cat := setupTestCatalog(t)
	file := addFile(t, cat, "Photos", "empty.jpg", 1, time.Now().UTC())
	if _, err := cat.db.Exec(`UPDATE files SET
		captured_local = 'old', captured_source = 'exif',
		gps_lat = 64.1, gps_lon = -21.9, gps_altitude_m = 12,
		gps_accuracy_m = 3, gps_source = 'manual'
		WHERE id = ?`, file.ID); err != nil {
		t.Fatal(err)
	}

	if err := cat.SetExtractedMetadata(file.ID, metadatapkg.Extracted{}, 321); err != nil {
		t.Fatal(err)
	}
	got, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CapturedLocal != "" || got.CapturedOffsetMin != nil || got.CapturedSource != "" {
		t.Errorf("honest empty capture result = %q %v %q",
			got.CapturedLocal, got.CapturedOffsetMin, got.CapturedSource)
	}
	if got.GPSLat == nil || *got.GPSLat != 64.1 || got.GPSLon == nil || *got.GPSLon != -21.9 ||
		got.GPSAltitudeM == nil || *got.GPSAltitudeM != 12 ||
		got.GPSAccuracyM == nil || *got.GPSAccuracyM != 3 || got.GPSSource != "manual" {
		t.Errorf("manual GPS was overwritten: %+v", got)
	}
	if got.MetaScannedAt == nil || *got.MetaScannedAt != 321 {
		t.Errorf("MetaScannedAt = %v, want successful empty scan at 321", got.MetaScannedAt)
	}
}

func TestMetadataFailureBudgetAndManualResetAreIndependent(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()
	photo := addFile(t, cat, "Photos", "bad.jpg", 1, now)
	doc := addFile(t, cat, "Docs", "bad.jpg", 1, now)

	if err := cat.RecordThumbnailFailure(photo.ID, "thumbnail failed", 50); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= maxMetadataAttempts; attempt++ {
		if err := cat.RecordMetadataFailure(photo.ID, "metadata failed", int64(attempt)); err != nil {
			t.Fatal(err)
		}
		if err := cat.RecordMetadataFailure(doc.ID, "metadata failed", int64(attempt)); err != nil {
			t.Fatal(err)
		}
	}

	queued, err := cat.FilesNeedingMetadata([]string{".jpg"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if containsID(queued, photo.ID) || containsID(queued, doc.ID) {
		t.Fatal("three metadata failures must exhaust the extraction queue budget")
	}

	reset, err := cat.ResetMetadataFailures("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if reset != 1 {
		t.Fatalf("reset %d rows, want only the selected folder", reset)
	}
	queued, err = cat.FilesNeedingMetadata([]string{".jpg"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !containsID(queued, photo.ID) || containsID(queued, doc.ID) {
		t.Errorf("queue after folder reset = %+v", queued)
	}

	got, err := cat.GetFileByID(photo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MetaAttempts != 0 || got.MetaLastAttempt != nil || got.MetaError != "" {
		t.Errorf("metadata failure state not cleared: %+v", got)
	}
	if got.ThumbAttempts != 1 || got.ThumbError != "thumbnail failed" {
		t.Errorf("metadata reset changed thumbnail state: %+v", got)
	}
}
