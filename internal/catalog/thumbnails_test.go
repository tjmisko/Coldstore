package catalog

import (
	"testing"
	"time"
)

func TestFilesNeedingThumbnailsShouldStopAtTheLimitWhenTheQueueIsLarge(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	for i := 0; i < 500; i++ {
		addFile(t, cat, "Photos", "f"+itoa(i)+".jpg", 1, now.Add(-time.Duration(i)*time.Minute))
	}

	files, err := cat.FilesNeedingThumbnailsLimited([]string{".jpg"}, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 200 {
		t.Errorf("got %d files, want exactly 200: an unbounded queue blocks the daemon's select loop", len(files))
	}
}

func TestFilesNeedingThumbnailsShouldDropAFileWhenItHasFailedTheMaximumNumberOfTimes(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	corrupt := addFile(t, cat, "Photos", "corrupt.jpg", 1, now)
	healthy := addFile(t, cat, "Photos", "healthy.jpg", 1, now)

	for attempt := 1; attempt <= maxThumbnailAttempts; attempt++ {
		files, err := cat.FilesNeedingThumbnailsLimited([]string{".jpg"}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if !containsID(files, corrupt.ID) {
			t.Fatalf("attempt %d: the corrupt file must still be queued", attempt)
		}
		if err := cat.RecordThumbnailFailure(corrupt.ID, "not a jpeg", now.Unix()); err != nil {
			t.Fatal(err)
		}
	}

	files, err := cat.FilesNeedingThumbnailsLimited([]string{".jpg"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if containsID(files, corrupt.ID) {
		t.Errorf("a file that failed %d times must leave the queue for good", maxThumbnailAttempts)
	}
	if !containsID(files, healthy.ID) {
		t.Error("a healthy file must stay queued")
	}

	got, err := cat.GetFileByID(corrupt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ThumbAttempts != maxThumbnailAttempts {
		t.Errorf("ThumbAttempts = %d, want %d", got.ThumbAttempts, maxThumbnailAttempts)
	}
	if got.ThumbError != "not a jpeg" {
		t.Errorf("ThumbError = %q, want the last failure reason", got.ThumbError)
	}
	if got.ThumbLastAttempt == nil || *got.ThumbLastAttempt != now.Unix() {
		t.Errorf("ThumbLastAttempt = %v, want %d", got.ThumbLastAttempt, now.Unix())
	}
}

func TestFilesNeedingThumbnailsShouldSkipAFileWhenItAlreadyHasAThumbnail(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	done := addFile(t, cat, "Photos", "done.jpg", 1, now)
	pending := addFile(t, cat, "Photos", "pending.jpg", 1, now)
	if err := cat.UpdateThumbnailPath(done.ID, "Photos/done.webp"); err != nil {
		t.Fatal(err)
	}

	files, err := cat.FilesNeedingThumbnailsLimited([]string{".jpg"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != pending.ID {
		t.Fatalf("queue = %+v, want only the file without a thumbnail", files)
	}
}

func TestUpdateThumbnailPathShouldClearTheRecordedErrorWhenAnAttemptFinallySucceeds(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	file := addFile(t, cat, "Photos", "a.jpg", 1, now)
	if err := cat.RecordThumbnailFailure(file.ID, "transient ffmpeg error", now.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateThumbnailPath(file.ID, "Photos/a.webp"); err != nil {
		t.Fatal(err)
	}

	got, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ThumbError != "" {
		t.Errorf("ThumbError = %q, want empty after a success", got.ThumbError)
	}
	if got.ThumbnailPath != "Photos/a.webp" {
		t.Errorf("ThumbnailPath = %q", got.ThumbnailPath)
	}
}

func TestResetThumbnailFailuresShouldRequeueAFolderWhenTheOwnerAsksForARetry(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	photo := addFile(t, cat, "Photos", "a.jpg", 1, now)
	doc := addFile(t, cat, "Docs", "b.jpg", 1, now)
	for i := 0; i < maxThumbnailAttempts; i++ {
		if err := cat.RecordThumbnailFailure(photo.ID, "boom", now.Unix()); err != nil {
			t.Fatal(err)
		}
		if err := cat.RecordThumbnailFailure(doc.ID, "boom", now.Unix()); err != nil {
			t.Fatal(err)
		}
	}

	reset, err := cat.ResetThumbnailFailures("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if reset != 1 {
		t.Errorf("reset %d rows, want 1", reset)
	}

	files, err := cat.FilesNeedingThumbnailsLimited([]string{".jpg"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !containsID(files, photo.ID) {
		t.Error("the reset folder's file must be queued again")
	}
	if containsID(files, doc.ID) {
		t.Error("a folder that was not reset must stay out of the queue")
	}
}

func containsID(files []FileRecord, id int64) bool {
	for _, f := range files {
		if f.ID == id {
			return true
		}
	}
	return false
}
