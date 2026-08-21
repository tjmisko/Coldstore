package catalog

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestExpandPairGroupsIsFolderScopedAndDoesNotGroupEmptyKeys(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()
	still := addFile(t, cat, "Photos", "IMG_1.HEIC", 10, now)
	movie := addFile(t, cat, "Photos", "IMG_1.MOV", 20, now)
	otherFolder := addFile(t, cat, "Import", "IMG_1.MOV", 30, now)
	emptyA := addFile(t, cat, "Photos", "plain-a.jpg", 1, now)
	emptyB := addFile(t, cat, "Photos", "plain-b.jpg", 1, now)
	if err := cat.SetPairAssignments([]PairAssignment{
		{ID: still.ID, Key: "cid:same", Source: "content_id"},
		{ID: movie.ID, Key: "cid:same", Source: "content_id"},
		{ID: otherFolder.ID, Key: "cid:same", Source: "content_id"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := cat.ExpandPairGroups([]int64{still.ID, emptyA.ID, emptyA.ID, 999999})
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{still.ID, movie.ID, emptyA.ID, 999999}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("expanded ids = %v, want %v", got, want)
	}
	if slices.Contains(got, otherFolder.ID) || slices.Contains(got, emptyB.ID) {
		t.Fatalf("expansion crossed folder or empty-key boundary: %v", got)
	}
}

func TestExpandPairGroupsChunksTheLargestLegalJob(t *testing.T) {
	cat := setupTestCatalog(t)
	tx, err := cat.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO files
		(folder_label, relative_path, filename, size_bytes, file_mtime, file_atime,
		 first_seen, status, last_seen, pair_key, pair_source)
		VALUES ('Photos', ?, ?, 1, ?, ?, ?, 'synced', ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	ids := make([]int64, 0, 20000)
	var companionID int64
	for i := 0; i < 20001; i++ {
		name := fmt.Sprintf("f-%05d.jpg", i)
		key, source := "", ""
		if i == 0 || i == 20000 {
			key, source = "cid:edge", "content_id"
		}
		res, err := stmt.Exec(name, name, stamp, stamp, stamp, int64(i+1), key, source)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if i < 20000 {
			ids = append(ids, id)
		} else {
			companionID = id
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	got, err := cat.ExpandPairGroups(ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 20001 || !slices.Contains(got, companionID) {
		t.Fatalf("expanded %d ids, companion present %v; want 20001 and true",
			len(got), slices.Contains(got, companionID))
	}
}

func TestPairCandidatesFoldCaseAndEditedAndAppendedSidecars(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()
	anchor := addPairTestFile(t, cat, "Photos", "Trips/IMG_1234.MOV", now)
	for _, rel := range []string{
		"Trips/img_1234.heic", "Trips/IMG_E1234.JPG", "Trips/IMG_1234.AAE",
		"Trips/IMG_E1234.JPG.XMP",
	} {
		addPairTestFile(t, cat, "Photos", rel, now)
	}
	addPairTestFile(t, cat, "Photos", "Other/IMG_1234.HEIC", now)
	addPairTestFile(t, cat, "Photos", "Trips/IMG_9999.HEIC", now)

	got, err := cat.PairCandidates(*anchor)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("candidate count = %d, want 5: %+v", len(got), got)
	}
	if stem := CanonicalPairStem("IMG_E1234.HEIC"); stem != "img_1234" {
		t.Errorf("edited canonical stem = %q", stem)
	}
	if stem := CanonicalPairStem("IMG_E1234.JPG.XMP"); stem != "img_1234" {
		t.Errorf("appended XMP canonical stem = %q", stem)
	}
}

func addPairTestFile(t *testing.T, cat *Catalog, folder, rel string, stamp time.Time) *FileRecord {
	t.Helper()
	rec := &FileRecord{
		FolderLabel: folder, RelativePath: rel, Filename: path.Base(rel),
		Extension: strings.ToLower(path.Ext(rel)), SizeBytes: 1,
		FileMtime: stamp, FileAtime: stamp, Status: StatusSynced,
	}
	if _, err := cat.UpsertFile(rec, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	got, err := cat.GetFile(folder, rel)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSetDegradedPairUsesOneStemAndLaterCIDPromotesAMember(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()
	a := addFile(t, cat, "Photos", "IMG_1.MOV", 1, now)
	b := addFile(t, cat, "Photos", "IMG_1.HEIC", 1, now)
	if err := cat.SetPairAssignments([]PairAssignment{
		{ID: a.ID, Key: "cid:known-on-video", Source: "content_id"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetDegradedPair([]int64{a.ID, b.ID}, "stem:img_1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{a.ID, b.ID} {
		got, _ := cat.GetFileByID(id)
		if got.PairKey != "stem:img_1" || got.PairSource != "basename" {
			t.Errorf("degraded file %d = %q (%q)", id, got.PairKey, got.PairSource)
		}
	}
	if err := cat.SetPairAssignments([]PairAssignment{
		{ID: b.ID, Key: "cid:confirmed", Source: "content_id"},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := cat.GetFileByID(b.ID)
	if got.PairKey != "cid:confirmed" || got.PairSource != "content_id" {
		t.Errorf("promoted still = %q (%q)", got.PairKey, got.PairSource)
	}
}

func TestCreateJobExpandsPairMembersTransactionally(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()
	a := addFile(t, cat, "Photos", "a.heic", 1, now)
	b := addFile(t, cat, "Photos", "a.mov", 1, now)
	if err := cat.SetPairAssignments([]PairAssignment{
		{ID: a.ID, Key: "cid:a", Source: "content_id"},
		{ID: b.ID, Key: "cid:a", Source: "content_id"},
	}); err != nil {
		t.Fatal(err)
	}
	jobID, err := cat.CreateJob(JobActionArchive, "", []int64{a.ID})
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := cat.GetJob(jobID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if job.Total != 2 {
		t.Fatalf("expanded job total = %d, want 2", job.Total)
	}
	first, _ := cat.ClaimJobItem(jobID)
	second, _ := cat.ClaimJobItem(jobID)
	if !slices.Equal([]int64{first, second}, []int64{a.ID, b.ID}) {
		t.Fatalf("job items = %v, want pair", []int64{first, second})
	}
}

func TestPreflightedRestoreJobRefusesASelectionThatIsNoLongerClosed(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()
	a := addFile(t, cat, "Photos", "changed.heic", 1, now)
	b := addFile(t, cat, "Photos", "changed.mov", 1, now)
	if err := cat.SetPairAssignments([]PairAssignment{
		{ID: a.ID, Key: "cid:changed", Source: "content_id"},
		{ID: b.ID, Key: "cid:changed", Source: "content_id"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.CreatePreflightedMoveJob(JobActionRestore, "", []int64{a.ID}); !errors.Is(err, ErrPairSelectionChanged) {
		t.Fatalf("checked job error = %v, want ErrPairSelectionChanged", err)
	}
	jobs, err := cat.ListJobs(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("closure failure left a job behind: %+v", jobs)
	}
}

func TestCountEvictionsSkipsAWholePairWhenOneMemberIsProtected(t *testing.T) {
	cat := setupTestCatalog(t)
	old := time.Now().UTC().Add(-72 * time.Hour)
	still := addFile(t, cat, "Photos", "pair.heic", 100, old)
	movie := addFile(t, cat, "Photos", "pair.mov", 100, old.Add(time.Minute))
	solo := addFile(t, cat, "Photos", "solo.jpg", 100, old.Add(2*time.Minute))
	if err := cat.SetPairAssignments([]PairAssignment{
		{ID: still.ID, Key: "cid:protected", Source: "content_id"},
		{ID: movie.ID, Key: "cid:protected", Source: "content_id"},
	}); err != nil {
		t.Fatal(err)
	}

	count, capped, err := cat.CountEvictionsUpTo("Photos", 50, func(rel string) bool {
		return rel == movie.RelativePath
	})
	if err != nil {
		t.Fatal(err)
	}
	if capped || count != 1 {
		t.Fatalf("evictions = %d capped %v, want only unpaired %s", count, capped, solo.RelativePath)
	}
}
