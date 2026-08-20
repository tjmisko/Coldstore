package catalog

import (
	"testing"
	"time"
)

// addFile inserts a synced file with a controllable age and size.
func addFile(t *testing.T, cat *Catalog, folder, relPath string, size int64, mtime time.Time) *FileRecord {
	t.Helper()
	rec := &FileRecord{
		FolderLabel:  folder,
		RelativePath: relPath,
		Filename:     relPath,
		Extension:    ".jpg",
		MimeType:     "image/jpeg",
		SizeBytes:    size,
		FileMtime:    mtime,
		FileAtime:    mtime,
		Status:       StatusSynced,
	}
	if _, err := cat.UpsertFile(rec, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	got, err := cat.GetFile(folder, relPath)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestOffloadCandidatesShouldSkipAFileWhenItIsPinned(t *testing.T) {
	cat := setupTestCatalog(t)
	old := time.Now().UTC().Add(-72 * time.Hour)

	keep := addFile(t, cat, "Photos", "keep.jpg", 100, old)
	drop := addFile(t, cat, "Photos", "drop.jpg", 100, old.Add(time.Minute))

	if err := cat.SetPinned(keep.ID, true, PinSourceUser, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	candidates, err := cat.OffloadCandidates("Photos", defaultOffloadCandidateLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(candidates))
	}
	if candidates[0].ID != drop.ID {
		t.Errorf("candidate = %q, want the unpinned file", candidates[0].RelativePath)
	}
}

func TestOffloadCandidatesShouldReturnOldestFirstWhenSeveralAreEligible(t *testing.T) {
	cat := setupTestCatalog(t)
	base := time.Now().UTC().Add(-100 * time.Hour)

	addFile(t, cat, "Photos", "newest.jpg", 1, base.Add(2*time.Hour))
	addFile(t, cat, "Photos", "oldest.jpg", 1, base)
	addFile(t, cat, "Photos", "middle.jpg", 1, base.Add(time.Hour))

	candidates, err := cat.OffloadCandidates("Photos", defaultOffloadCandidateLimit)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"oldest.jpg", "middle.jpg", "newest.jpg"}
	if len(candidates) != len(want) {
		t.Fatalf("got %d candidates, want %d", len(candidates), len(want))
	}
	for i, name := range want {
		if candidates[i].RelativePath != name {
			t.Errorf("candidates[%d] = %q, want %q", i, candidates[i].RelativePath, name)
		}
	}
}

func TestOffloadCandidatesShouldStopAtTheLimitWhenTheFolderHasMoreEligibleFiles(t *testing.T) {
	cat := setupTestCatalog(t)
	base := time.Now().UTC().Add(-100 * time.Hour)

	for i := 0; i < 10; i++ {
		addFile(t, cat, "Photos", string(rune('a'+i))+".jpg", 1, base.Add(time.Duration(i)*time.Minute))
	}

	candidates, err := cat.OffloadCandidates("Photos", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 3 {
		t.Fatalf("got %d candidates, want 3", len(candidates))
	}
	if candidates[0].RelativePath != "a.jpg" {
		t.Errorf("first candidate = %q, want the oldest", candidates[0].RelativePath)
	}
}

func TestUpsertFileShouldPreserveThePinWhenTheSamePathIsRescanned(t *testing.T) {
	cat := setupTestCatalog(t)
	mtime := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	file := addFile(t, cat, "Photos", "a.jpg", 100, mtime)
	pinnedAt := time.Now().Unix()
	if err := cat.SetPinned(file.ID, true, PinSourceUser, pinnedAt); err != nil {
		t.Fatal(err)
	}

	// A rescan sees the same path with fresh metadata.
	if _, err := cat.UpsertFile(&FileRecord{
		FolderLabel:  "Photos",
		RelativePath: "a.jpg",
		Filename:     "a.jpg",
		Extension:    ".jpg",
		MimeType:     "image/jpeg",
		SizeBytes:    200,
		FileMtime:    mtime.Add(time.Minute),
		FileAtime:    mtime.Add(time.Minute),
		Status:       StatusSynced,
	}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}

	got, err := cat.GetFile("Photos", "a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if got.SizeBytes != 200 {
		t.Errorf("SizeBytes = %d, want the rescanned 200", got.SizeBytes)
	}
	if !got.Pinned {
		t.Fatal("a rescan must never clear a pin")
	}
	if got.PinSource != PinSourceUser {
		t.Errorf("PinSource = %q, want user", got.PinSource)
	}
	if got.PinnedAt == nil || *got.PinnedAt != pinnedAt {
		t.Errorf("PinnedAt = %v, want %d", got.PinnedAt, pinnedAt)
	}
}

func TestUpsertFileShouldPreserveThumbnailFailuresWhenTheSamePathIsRescanned(t *testing.T) {
	cat := setupTestCatalog(t)
	mtime := time.Now().UTC().Add(-time.Hour)

	file := addFile(t, cat, "Photos", "a.jpg", 100, mtime)
	if err := cat.RecordThumbnailFailure(file.ID, "ffmpeg exploded", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	if _, err := cat.UpsertFile(&FileRecord{
		FolderLabel: "Photos", RelativePath: "a.jpg", Filename: "a.jpg",
		Extension: ".jpg", SizeBytes: 100, FileMtime: mtime, FileAtime: mtime,
		Status: StatusSynced,
	}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}

	got, err := cat.GetFile("Photos", "a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if got.ThumbAttempts != 1 {
		t.Errorf("ThumbAttempts = %d, want 1: a rescan must not reset the retry budget", got.ThumbAttempts)
	}
}

func TestFolderSizesShouldCountPinnedBytesInsideTheTotalWhenFilesArePinned(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	pinned := addFile(t, cat, "Photos", "pinned.jpg", 100, now)
	addFile(t, cat, "Photos", "loose.jpg", 200, now)
	if err := cat.SetPinned(pinned.ID, true, PinSourceUser, now.Unix()); err != nil {
		t.Fatal(err)
	}

	total, pinnedBytes, err := cat.FolderSizes("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if total != 300 {
		t.Errorf("total = %d, want 300", total)
	}
	if pinnedBytes != 100 {
		t.Errorf("pinned = %d, want 100", pinnedBytes)
	}
	if pinnedBytes > total {
		t.Error("pinned bytes are a subset of the total, never an addend")
	}
}

func TestFolderSizesShouldCountTransientStatesWhenAMoveIsInFlight(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	leaving := addFile(t, cat, "Photos", "leaving.jpg", 100, now)
	arriving := addFile(t, cat, "Photos", "arriving.jpg", 200, now)
	gone := addFile(t, cat, "Photos", "gone.jpg", 400, now)

	if err := cat.UpdateFileStatus(leaving.ID, StatusArchiving); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateFileStatus(arriving.ID, StatusRestoring); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateFileStatus(gone.ID, StatusArchived); err != nil {
		t.Fatal(err)
	}

	total, _, err := cat.FolderSizes("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if total != 300 {
		t.Errorf("total = %d, want 300: archiving and restoring bytes are on the device, archived bytes are not", total)
	}
}

func TestFolderBudgetShouldNotReportOverBudgetWhenTheBudgetIsUnconfigured(t *testing.T) {
	b := FolderBudget{BudgetBytes: 0, TotalBytes: 1 << 40, PinnedBytes: 1 << 40}
	if b.OverBudget() {
		t.Error("a non-positive budget means unconfigured, never archive everything")
	}
	if b.Wedged() {
		t.Error("a non-positive budget cannot be wedged")
	}
}

func TestFolderBudgetShouldReportWedgedOnlyWhenPinnedBytesAloneExceedTheBudget(t *testing.T) {
	overButNotWedged := FolderBudget{BudgetBytes: 100, TotalBytes: 150, PinnedBytes: 40}
	if !overButNotWedged.OverBudget() {
		t.Error("150 bytes against a 100 byte budget is over budget")
	}
	if overButNotWedged.Wedged() {
		t.Error("wedged must not fire while unpinned files remain to offload")
	}

	wedged := FolderBudget{BudgetBytes: 100, TotalBytes: 150, PinnedBytes: 120}
	if !wedged.Wedged() {
		t.Error("120 pinned bytes against a 100 byte budget is wedged")
	}
}

func TestMarkRestoredShouldPinTheFileWhenTheRestoreCommits(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	file := addFile(t, cat, "Photos", "a.jpg", 100, now)
	if err := cat.UpdateFileStatus(file.ID, StatusArchived); err != nil {
		t.Fatal(err)
	}
	if ok, err := cat.ClaimFile(file.ID, StatusArchived, StatusRestoring); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}

	at := now.Unix()
	ok, err := cat.MarkRestored(file.ID, at)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("MarkRestored must succeed against a restoring row")
	}

	got, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSynced {
		t.Errorf("Status = %q, want synced", got.Status)
	}
	if !got.Pinned {
		t.Fatal("a restored file must be pinned, or the next quota tick takes it straight back")
	}
	if got.PinSource != PinSourceRestore {
		t.Errorf("PinSource = %q, want restore", got.PinSource)
	}
	if got.ArchivedAt != nil {
		t.Error("archived_at must be cleared on restore")
	}

	// The whole point: it is no longer an offload candidate.
	candidates, err := cat.OffloadCandidates("Photos", defaultOffloadCandidateLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Errorf("got %d candidates, want 0: a restored file must survive the next tick", len(candidates))
	}
}

func TestMarkRestoredShouldKeepTheUserPinWhenTheFileWasAlreadyPinnedByHand(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	file := addFile(t, cat, "Photos", "a.jpg", 100, now)
	pinnedAt := now.Add(-time.Hour).Unix()
	if err := cat.SetPinned(file.ID, true, PinSourceUser, pinnedAt); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateFileStatus(file.ID, StatusArchived); err != nil {
		t.Fatal(err)
	}
	if ok, err := cat.ClaimFile(file.ID, StatusArchived, StatusRestoring); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if _, err := cat.MarkRestored(file.ID, now.Unix()); err != nil {
		t.Fatal(err)
	}

	got, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PinSource != PinSourceUser {
		t.Errorf("PinSource = %q, want user: a restore must never downgrade an explicit pin", got.PinSource)
	}
	if got.PinnedAt == nil || *got.PinnedAt != pinnedAt {
		t.Errorf("PinnedAt = %v, want the original %d", got.PinnedAt, pinnedAt)
	}
}

func TestMarkRestoredShouldDoNothingWhenTheRowIsNotRestoring(t *testing.T) {
	cat := setupTestCatalog(t)
	file := addFile(t, cat, "Photos", "a.jpg", 100, time.Now().UTC())

	ok, err := cat.MarkRestored(file.ID, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("MarkRestored must refuse a row that is not restoring")
	}

	got, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pinned {
		t.Error("a lost CAS must not leave a pin behind")
	}
}

func TestMarkArchivedShouldLeaveThePinInPlaceWhenTheFileWasPinnedMidMove(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	file := addFile(t, cat, "Photos", "a.jpg", 100, now)
	if ok, err := cat.ClaimUnpinnedFile(file.ID); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	// The owner pins while the copy is in flight.
	if err := cat.SetPinned(file.ID, true, PinSourceUser, now.Unix()); err != nil {
		t.Fatal(err)
	}

	ok, err := cat.MarkArchived(file.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("MarkArchived must succeed against an archiving row")
	}

	got, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusArchived {
		t.Errorf("Status = %q, want archived", got.Status)
	}
	if !got.Pinned {
		t.Error("a pin taken mid-move survives the archive as a latent instruction")
	}
	if got.ArchivedAt == nil {
		t.Error("archived_at must be stamped")
	}
}

func TestClaimUnpinnedFileShouldFailWhenThePinCommittedFirst(t *testing.T) {
	cat := setupTestCatalog(t)
	file := addFile(t, cat, "Photos", "a.jpg", 100, time.Now().UTC())

	if err := cat.SetPinned(file.ID, true, PinSourceUser, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	ok, err := cat.ClaimUnpinnedFile(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("the automatic offload path must lose its claim against a pinned file")
	}

	got, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSynced {
		t.Errorf("Status = %q, want synced: a lost claim writes nothing", got.Status)
	}
}

func TestClaimFileShouldFailWhenAnotherActorAlreadyChangedTheStatus(t *testing.T) {
	cat := setupTestCatalog(t)
	file := addFile(t, cat, "Photos", "a.jpg", 100, time.Now().UTC())

	if ok, err := cat.ClaimFile(file.ID, StatusSynced, StatusArchiving); err != nil || !ok {
		t.Fatalf("first claim = %v, %v", ok, err)
	}
	ok, err := cat.ClaimFile(file.ID, StatusSynced, StatusArchiving)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a second claim against the same start state must lose")
	}
}

func TestSetPinnedShouldClearTheAttributionWhenTheFileIsUnpinned(t *testing.T) {
	cat := setupTestCatalog(t)
	file := addFile(t, cat, "Photos", "a.jpg", 100, time.Now().UTC())

	if err := cat.SetPinned(file.ID, true, PinSourceRestore, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetPinned(file.ID, false, "", 0); err != nil {
		t.Fatal(err)
	}

	got, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pinned {
		t.Error("Pinned = true, want false")
	}
	if got.PinSource != PinSourceNone {
		t.Errorf("PinSource = %q, want empty", got.PinSource)
	}
	if got.PinnedAt != nil {
		t.Errorf("PinnedAt = %v, want nil", *got.PinnedAt)
	}
}

func TestSetPinnedShouldRejectThePinWhenTheSourceIsNotOneOfTheTwoKnownValues(t *testing.T) {
	cat := setupTestCatalog(t)
	file := addFile(t, cat, "Photos", "a.jpg", 100, time.Now().UTC())

	if err := cat.SetPinned(file.ID, true, "whatever", time.Now().Unix()); err == nil {
		t.Fatal("an unknown pin source must be rejected: the CHECK lives in Go, not in SQL")
	}
	got, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pinned {
		t.Error("a rejected pin must not be written")
	}
}

func TestSetPinnedBulkShouldPinEveryIdWhenTheSelectionSpansMoreThanOneChunk(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	var ids []int64
	for i := 0; i < idChunkSize+7; i++ {
		rec := addFile(t, cat, "Photos", "f"+itoa(i)+".jpg", 1, now)
		ids = append(ids, rec.ID)
	}

	at := now.Unix()
	updated, err := cat.SetPinnedBulk(ids, true, PinSourceUser, at)
	if err != nil {
		t.Fatal(err)
	}
	if updated != int64(len(ids)) {
		t.Errorf("updated = %d, want %d", updated, len(ids))
	}

	pinned := true
	got, total, err := cat.ListFileIDs(FileQuery{Folder: "Photos", Pinned: &pinned}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != int64(len(ids)) {
		t.Errorf("total = %d, want %d", total, len(ids))
	}
	if len(got) != len(ids) {
		t.Errorf("pinned ids = %d, want %d", len(got), len(ids))
	}
}

func TestListFilesShouldFilterByPinSourceWhenAskedForAutoPinnedFiles(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	byUser := addFile(t, cat, "Photos", "user.jpg", 1, now)
	byRestore := addFile(t, cat, "Photos", "restore.jpg", 1, now)
	addFile(t, cat, "Photos", "loose.jpg", 1, now)

	if err := cat.SetPinned(byUser.ID, true, PinSourceUser, now.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetPinned(byRestore.ID, true, PinSourceRestore, now.Unix()); err != nil {
		t.Fatal(err)
	}

	pinned := true
	files, err := cat.ListFiles(FileQuery{Folder: "Photos", Pinned: &pinned, PinSource: PinSourceRestore})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != byRestore.ID {
		t.Fatalf("got %+v, want only the auto-pinned file", files)
	}
}

func TestCountUnpinnedSyncedOldestUpToShouldCountTheEvictionsAFolderWouldSufferWhenRestoringOverBudget(t *testing.T) {
	cat := setupTestCatalog(t)
	base := time.Now().UTC().Add(-100 * time.Hour)

	for i := 0; i < 5; i++ {
		addFile(t, cat, "Photos", "f"+itoa(i)+".jpg", 100, base.Add(time.Duration(i)*time.Minute))
	}
	pinnedFile := addFile(t, cat, "Photos", "pinned.jpg", 100, base.Add(-time.Hour))
	if err := cat.SetPinned(pinnedFile.ID, true, PinSourceUser, base.Unix()); err != nil {
		t.Fatal(err)
	}

	count, capped, err := cat.CountUnpinnedSyncedOldestUpTo("Photos", 250)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3: three 100-byte files cover a 250-byte overage", count)
	}
	if capped {
		t.Error("capped = true, want false")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

func TestFolderCountsShouldNotCountAPinWhenTheFileIsAlreadyInColdStorage(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	onDevice := addFile(t, cat, "Photos", "here.jpg", 100, now)
	inCold := addFile(t, cat, "Photos", "gone.jpg", 900, now)
	if err := cat.UpdateFileStatus(inCold.ID, StatusArchived); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{onDevice.ID, inCold.ID} {
		if err := cat.SetPinned(id, true, PinSourceUser, now.Unix()); err != nil {
			t.Fatal(err)
		}
	}

	counts, err := cat.FolderCounts("Photos")
	if err != nil {
		t.Fatal(err)
	}
	// The pinned COUNT has to agree with the pinned BYTES the same response
	// carries, and FolderSizes excludes archived rows: an archived pin is a
	// latent instruction, not a claim on the device.
	if counts.Pinned != 1 {
		t.Errorf("counts.Pinned = %d, want 1 (an archived pin occupies no space on the device)",
			counts.Pinned)
	}

	_, pinnedBytes, err := cat.FolderSizes("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if pinnedBytes != 100 {
		t.Errorf("pinned bytes = %d, want 100", pinnedBytes)
	}
}

func TestFolderLabelsForIDsShouldNameEveryFolderTheSelectionTouches(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	a := addFile(t, cat, "Photos", "a.jpg", 1, now)
	b := addFile(t, cat, "Docs", "b.pdf", 1, now)
	c := addFile(t, cat, "Photos", "c.jpg", 1, now)

	labels, err := cat.FolderLabelsForIDs([]int64{a.ID, b.ID, c.ID, 99999})
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 2 || labels[0] != "Docs" || labels[1] != "Photos" {
		t.Errorf("labels = %v, want [Docs Photos] (sorted, deduplicated, unknown ids dropped)", labels)
	}
}

func TestHumanSIShouldMatchTheUnitsTheOwnerWroteInTheirConfig(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{20_000_000_000, "20 GB"},
		{24_100_000_000, "24.1 GB"},
		{1_500_000, "1.5 MB"},
		{2_000_000_000_000, "2 TB"},
	}
	for _, tc := range cases {
		if got := HumanSI(tc.bytes); got != tc.want {
			t.Errorf("HumanSI(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}
