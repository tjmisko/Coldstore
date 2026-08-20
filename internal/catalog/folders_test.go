package catalog

import (
	"errors"
	"testing"
	"time"
)

func ptrInt64(v int64) *int64 { return &v }
func ptrInt(v int) *int       { return &v }
func ptrBool(v bool) *bool    { return &v }

func TestReconcileFolderFromTOMLShouldSeedThePolicyWhenTheRowIsCreated(t *testing.T) {
	cat := setupTestCatalog(t)

	inserted, err := cat.ReconcileFolderFromTOML("Photos", "abcd-1234", "/srv/sync/Photos", 100, 45)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("the first reconciliation of a folder must report an insert")
	}

	fc, err := cat.FolderConfigByLabel("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if fc.BudgetBytes != 100 {
		t.Errorf("BudgetBytes = %d, want the seeded 100", fc.BudgetBytes)
	}
	if fc.GraceMinutes != 45 {
		t.Errorf("GraceMinutes = %d, want the seeded 45", fc.GraceMinutes)
	}
	if !fc.Enabled || !fc.PresentInTOML {
		t.Error("a freshly declared folder is enabled and present in TOML")
	}
}

func TestReconcileFolderFromTOMLShouldNotClobberTheBudgetWhenTheUserHasEditedItInTheUI(t *testing.T) {
	cat := setupTestCatalog(t)

	if _, err := cat.ReconcileFolderFromTOML("Photos", "abcd-1234", "/srv/sync/Photos", 100, 30); err != nil {
		t.Fatal(err)
	}
	found, err := cat.UpdateFolderPolicy("Photos", ptrInt64(500), ptrInt(90), nil)
	if err != nil || !found {
		t.Fatalf("UpdateFolderPolicy = %v, %v", found, err)
	}

	// A restart re-reads the same TOML.
	inserted, err := cat.ReconcileFolderFromTOML("Photos", "efgh-5678", "/srv/sync/Pictures", 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("a second reconciliation must not insert")
	}

	fc, err := cat.FolderConfigByLabel("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if fc.BudgetBytes != 500 {
		t.Errorf("BudgetBytes = %d, want 500: TOML seeds the budget once and never again", fc.BudgetBytes)
	}
	if fc.GraceMinutes != 90 {
		t.Errorf("GraceMinutes = %d, want 90", fc.GraceMinutes)
	}
	if fc.SyncthingPath != "/srv/sync/Pictures" {
		t.Errorf("SyncthingPath = %q, want the refreshed TOML path", fc.SyncthingPath)
	}
	if fc.SyncthingID != "efgh-5678" {
		t.Errorf("SyncthingID = %q, want the refreshed TOML id", fc.SyncthingID)
	}
}

func TestMarkFoldersAbsentFromTOMLShouldRetainTheRowWhenAFolderIsNoLongerDeclared(t *testing.T) {
	cat := setupTestCatalog(t)

	if _, err := cat.ReconcileFolderFromTOML("Photos", "", "/srv/sync/Photos", 100, 30); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ReconcileFolderFromTOML("Docs", "", "/srv/sync/Docs", 200, 30); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.UpdateFolderPolicy("Docs", ptrInt64(777), nil, nil); err != nil {
		t.Fatal(err)
	}

	if err := cat.MarkFoldersAbsentFromTOML([]string{"Photos"}); err != nil {
		t.Fatal(err)
	}

	docs, err := cat.FolderConfigByLabel("Docs")
	if err != nil {
		t.Fatalf("an undeclared folder must never be deleted: %v", err)
	}
	if docs.PresentInTOML {
		t.Error("PresentInTOML = true, want false")
	}
	if docs.BudgetBytes != 777 {
		t.Errorf("BudgetBytes = %d, want 777: the owner's tuning survives", docs.BudgetBytes)
	}

	active, err := cat.ActiveFolderConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].FolderLabel != "Photos" {
		t.Errorf("active folders = %+v, want only Photos", active)
	}

	all, err := cat.ListFolderConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("ListFolderConfigs returned %d rows, want 2 including the orphan", len(all))
	}
}

func TestReconcileFolderFromTOMLShouldRestoreTheFolderWhenItIsRedeclared(t *testing.T) {
	cat := setupTestCatalog(t)

	if _, err := cat.ReconcileFolderFromTOML("Photos", "", "/srv/sync/Photos", 100, 30); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.UpdateFolderPolicy("Photos", ptrInt64(500), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := cat.MarkFoldersAbsentFromTOML(nil); err != nil {
		t.Fatal(err)
	}

	if _, err := cat.ReconcileFolderFromTOML("Photos", "", "/srv/sync/Photos", 100, 30); err != nil {
		t.Fatal(err)
	}

	fc, err := cat.FolderConfigByLabel("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if !fc.PresentInTOML {
		t.Error("re-declaring a folder must flip present_in_toml back to 1")
	}
	if fc.BudgetBytes != 500 {
		t.Errorf("BudgetBytes = %d, want 500: the budget survives the round trip", fc.BudgetBytes)
	}
}

func TestActiveFolderConfigsShouldExcludeAFolderWhenItIsDisabledInTheUI(t *testing.T) {
	cat := setupTestCatalog(t)

	if _, err := cat.ReconcileFolderFromTOML("Photos", "", "/srv/sync/Photos", 100, 30); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.UpdateFolderPolicy("Photos", nil, nil, ptrBool(false)); err != nil {
		t.Fatal(err)
	}

	active, err := cat.ActiveFolderConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Errorf("active folders = %+v, want none", active)
	}
}

func TestUpdateFolderPolicyShouldLeaveOmittedFieldsAloneWhenOnlyOneIsSupplied(t *testing.T) {
	cat := setupTestCatalog(t)

	if _, err := cat.ReconcileFolderFromTOML("Photos", "abcd", "/srv/sync/Photos", 100, 30); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.UpdateFolderPolicy("Photos", ptrInt64(123), nil, nil); err != nil {
		t.Fatal(err)
	}

	fc, err := cat.FolderConfigByLabel("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if fc.BudgetBytes != 123 {
		t.Errorf("BudgetBytes = %d, want 123", fc.BudgetBytes)
	}
	if fc.GraceMinutes != 30 {
		t.Errorf("GraceMinutes = %d, want the untouched 30", fc.GraceMinutes)
	}
	if !fc.Enabled {
		t.Error("a partial update must not disable the folder")
	}
	if fc.SyncthingPath != "/srv/sync/Photos" {
		t.Errorf("SyncthingPath = %q: policy updates never touch infrastructure fields", fc.SyncthingPath)
	}
}

func TestUpdateFolderPolicyShouldReportNotFoundWhenTheLabelIsUnknown(t *testing.T) {
	cat := setupTestCatalog(t)

	found, err := cat.UpdateFolderPolicy("Bogus", ptrInt64(1), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("UpdateFolderPolicy must never insert a row")
	}

	all, err := cat.ListFolderConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Errorf("ListFolderConfigs returned %d rows, want 0", len(all))
	}
}

func TestFolderPathShouldResolveWhenTheFolderIsOrphaned(t *testing.T) {
	cat := setupTestCatalog(t)

	if _, err := cat.ReconcileFolderFromTOML("Photos", "", "/srv/sync/Photos", 100, 30); err != nil {
		t.Fatal(err)
	}
	if err := cat.MarkFoldersAbsentFromTOML(nil); err != nil {
		t.Fatal(err)
	}

	path, err := cat.FolderPath("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/srv/sync/Photos" {
		t.Errorf("FolderPath = %q, want /srv/sync/Photos: an orphan's files stay browsable", path)
	}
}

func TestFolderPathShouldReportNoSuchFolderWhenTheLabelHasNoRow(t *testing.T) {
	cat := setupTestCatalog(t)

	if _, err := cat.FolderPath("Bogus"); !errors.Is(err, ErrNoSuchFolder) {
		t.Fatalf("err = %v, want ErrNoSuchFolder", err)
	}
	if _, err := cat.FolderConfigByLabel("Bogus"); !errors.Is(err, ErrNoSuchFolder) {
		t.Fatalf("err = %v, want ErrNoSuchFolder", err)
	}
}

func TestSetWedgeWarnedAtShouldLatchAndClearWhenTheFolderWedgesAndRecovers(t *testing.T) {
	cat := setupTestCatalog(t)

	if _, err := cat.ReconcileFolderFromTOML("Photos", "", "/srv/sync/Photos", 100, 30); err != nil {
		t.Fatal(err)
	}

	fc, err := cat.FolderConfigByLabel("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if fc.WedgeWarnedAt != nil {
		t.Fatal("a fresh folder is not latched")
	}

	at := time.Now().Unix()
	if err := cat.SetWedgeWarnedAt("Photos", &at); err != nil {
		t.Fatal(err)
	}
	fc, err = cat.FolderConfigByLabel("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if fc.WedgeWarnedAt == nil || *fc.WedgeWarnedAt != at {
		t.Fatalf("WedgeWarnedAt = %v, want %d", fc.WedgeWarnedAt, at)
	}

	if err := cat.SetWedgeWarnedAt("Photos", nil); err != nil {
		t.Fatal(err)
	}
	fc, err = cat.FolderConfigByLabel("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if fc.WedgeWarnedAt != nil {
		t.Error("clearing the latch must write NULL, so the next wedge warns immediately")
	}
}

func TestSetLastEnforcedAtShouldRecordTheTickWhenTheDaemonLooksAtTheFolder(t *testing.T) {
	cat := setupTestCatalog(t)

	if _, err := cat.ReconcileFolderFromTOML("Photos", "", "/srv/sync/Photos", 0, 30); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Unix()
	if err := cat.SetLastEnforcedAt("Photos", at); err != nil {
		t.Fatal(err)
	}

	fc, err := cat.FolderConfigByLabel("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if fc.LastEnforcedAt == nil || *fc.LastEnforcedAt != at {
		t.Errorf("LastEnforcedAt = %v, want %d even for a zero-budget folder", fc.LastEnforcedAt, at)
	}
}

func TestMarkFilesMissingBeforeGenerationShouldSweepOnlyTheRowsTheWalkDidNotTouch(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()
	gen := now.Unix()

	seen := &FileRecord{FolderLabel: "Photos", RelativePath: "seen.jpg", Filename: "seen.jpg",
		SizeBytes: 1, FileMtime: now, FileAtime: now, Status: StatusSynced}
	if _, err := cat.UpsertFile(seen, gen); err != nil {
		t.Fatal(err)
	}
	stale := &FileRecord{FolderLabel: "Photos", RelativePath: "stale.jpg", Filename: "stale.jpg",
		SizeBytes: 1, FileMtime: now, FileAtime: now, Status: StatusSynced}
	if _, err := cat.UpsertFile(stale, gen-100); err != nil {
		t.Fatal(err)
	}
	inFlight := &FileRecord{FolderLabel: "Photos", RelativePath: "moving.jpg", Filename: "moving.jpg",
		SizeBytes: 1, FileMtime: now, FileAtime: now, Status: StatusSynced}
	if _, err := cat.UpsertFile(inFlight, gen-100); err != nil {
		t.Fatal(err)
	}
	moving, err := cat.GetFile("Photos", "moving.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateFileStatus(moving.ID, StatusArchiving); err != nil {
		t.Fatal(err)
	}

	swept, err := cat.MarkFilesMissingBeforeGeneration("Photos", gen)
	if err != nil {
		t.Fatal(err)
	}
	if swept != 1 {
		t.Errorf("swept %d rows, want 1", swept)
	}

	got, err := cat.GetFile("Photos", "seen.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSynced {
		t.Errorf("seen.jpg status = %q, want synced", got.Status)
	}
	got, err = cat.GetFile("Photos", "stale.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusMissing {
		t.Errorf("stale.jpg status = %q, want missing", got.Status)
	}
	got, err = cat.GetFile("Photos", "moving.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusArchiving {
		t.Errorf("moving.jpg status = %q, want archiving: a move in flight is never swept", got.Status)
	}
}

func TestUpsertFileSeenShouldAdvanceTheGenerationWhenAPathIsSeenAgain(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	rec := &FileRecord{FolderLabel: "Photos", RelativePath: "a.jpg", Filename: "a.jpg",
		SizeBytes: 1, FileMtime: now, FileAtime: now, Status: StatusSynced}
	if _, err := cat.UpsertFile(rec, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.UpsertFile(rec, 2000); err != nil {
		t.Fatal(err)
	}

	got, err := cat.GetFile("Photos", "a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSeen != 2000 {
		t.Errorf("LastSeen = %d, want 2000", got.LastSeen)
	}

	swept, err := cat.MarkFilesMissingBeforeGeneration("Photos", 2000)
	if err != nil {
		t.Fatal(err)
	}
	if swept != 0 {
		t.Errorf("swept %d rows, want 0", swept)
	}
}

func TestFolderCountsShouldTallyEveryStatusWhenTheFolderHasAMixture(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	synced := addFile(t, cat, "Photos", "a.jpg", 1, now)
	archived := addFile(t, cat, "Photos", "b.jpg", 1, now)
	missing := addFile(t, cat, "Photos", "c.jpg", 1, now)
	addFile(t, cat, "Docs", "d.txt", 1, now)

	if err := cat.UpdateFileStatus(archived.ID, StatusArchived); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateFileStatus(missing.ID, StatusMissing); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetPinned(synced.ID, true, PinSourceUser, now.Unix()); err != nil {
		t.Fatal(err)
	}

	counts, err := cat.FolderCounts("Photos")
	if err != nil {
		t.Fatal(err)
	}
	want := FolderCounts{Total: 3, Synced: 1, Archived: 1, Missing: 1, Pinned: 1}
	if counts != want {
		t.Errorf("counts = %+v, want %+v", counts, want)
	}
}

func TestLogEventDedupedShouldSuppressTheSecondEventWhenItLandsInsideTheWindow(t *testing.T) {
	cat := setupTestCatalog(t)

	for i := 0; i < 3; i++ {
		if err := cat.LogEventDeduped("cold_storage_low", "Photos", "", "4.2% free", 60); err != nil {
			t.Fatal(err)
		}
	}

	var n int
	if err := cat.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE event_type = 'cold_storage_low'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("events = %d, want 1: a per-tick condition must not flood the table", n)
	}
}

func TestLogEventDedupedShouldNotSuppressADifferentTypeWhenBothConcernTheSameFolder(t *testing.T) {
	cat := setupTestCatalog(t)

	if err := cat.LogEventDeduped("folder_wedged", "Photos", "", "wedged", 60); err != nil {
		t.Fatal(err)
	}
	if err := cat.LogEventDeduped("cold_storage_low", "Photos", "", "4.2% free", 60); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("events = %d, want 2: two conditions must never suppress each other", n)
	}
}
