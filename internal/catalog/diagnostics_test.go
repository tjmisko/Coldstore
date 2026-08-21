package catalog

import (
	"testing"
	"time"
)

func TestDerivedFailureCountsShouldSeparateFailuresFromDetectionBacklog(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now()
	for _, f := range []*FileRecord{
		{FolderLabel: "Photos", RelativePath: "failed.jpg", Filename: "failed.jpg", Status: StatusSynced, FileMtime: now},
		{FolderLabel: "Photos", RelativePath: "pending", Filename: "pending", Status: StatusSynced, FileMtime: now},
	} {
		if _, err := cat.UpsertFile(f, now.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	failed, err := cat.GetFile("Photos", "failed.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.RecordThumbnailFailure(failed.ID, "decoder failed", now.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetDetectedType(failed.ID, "image/jpeg"); err != nil {
		t.Fatal(err)
	}

	counts, err := cat.DerivedFailureCounts()
	if err != nil {
		t.Fatal(err)
	}
	if counts.ThumbnailFailures != 1 || counts.DetectionBacklog != 1 {
		t.Errorf("counts = %+v, want one thumbnail failure and one detection backlog", counts)
	}
}
