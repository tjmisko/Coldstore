package thumbnail

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

// fakeFFmpeg writes a shell script that stands in for ffmpeg and returns its
// path. body is the script body after the shebang.
func fakeFFmpeg(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// seedVideo catalogs a video record whose bytes are irrelevant: the fake
// ffmpeg never looks at them.
func seedVideo(t *testing.T, cat *catalog.Catalog, rel string) *catalog.FileRecord {
	t.Helper()
	if _, err := cat.UpsertFile(&catalog.FileRecord{
		FolderLabel:  "Videos",
		RelativePath: rel,
		Filename:     filepath.Base(rel),
		Extension:    ".mp4",
		SizeBytes:    1024,
		FileMtime:    time.Now().UTC(),
		Status:       catalog.StatusSynced,
	}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	f, err := cat.GetFile("Videos", rel)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestGenerateShouldFailFastWhenFFmpegExceedsItsTimeout(t *testing.T) {
	gen, cat := setupTestThumb(t)
	gen.SetFFmpeg(fakeFFmpeg(t, "sleep 30"), 150*time.Millisecond)

	file := seedVideo(t, cat, "clip.mp4")

	start := time.Now()
	err := gen.Generate(file, filepath.Join(t.TempDir(), "clip.mp4"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a hung ffmpeg should produce an error, not block forever")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want it to name the timeout", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("generation took %s; the per-invocation timeout did not fire", elapsed)
	}
}

func TestGenerateShouldNotRecordAThumbnailPathWhenFFmpegFails(t *testing.T) {
	gen, cat := setupTestThumb(t)
	gen.SetFFmpeg(fakeFFmpeg(t, "echo 'moov atom not found' >&2; exit 1"), time.Second)

	file := seedVideo(t, cat, "corrupt.mp4")

	if err := gen.Generate(file, filepath.Join(t.TempDir(), "corrupt.mp4")); err == nil {
		t.Fatal("a failing ffmpeg should produce an error")
	}

	after, err := cat.GetFile("Videos", "corrupt.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if after.ThumbnailPath != "" {
		t.Errorf("thumbnail_path = %q, want empty after a failed generation", after.ThumbnailPath)
	}
	if _, err := os.Stat(gen.ThumbPath("Videos", "corrupt.mp4")); !os.IsNotExist(err) {
		t.Error("a failed generation must not leave a partial thumbnail behind")
	}
}

func TestProcessQueueShouldRecordAFailureWhenAFileCannotBeThumbnailed(t *testing.T) {
	gen, cat := setupTestThumb(t)
	gen.SetFFmpeg(fakeFFmpeg(t, "exit 1"), time.Second)

	file := seedVideo(t, cat, "broken.mp4")
	srcDir := t.TempDir()

	gen.ProcessQueue([]catalog.FileRecord{*file}, func(f *catalog.FileRecord) string {
		return filepath.Join(srcDir, f.RelativePath)
	}, 1)

	after, err := cat.GetFile("Videos", "broken.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if after.ThumbAttempts != 1 {
		t.Errorf("thumb_attempts = %d, want 1", after.ThumbAttempts)
	}
	if after.ThumbError == "" {
		t.Error("thumb_error should record why the attempt failed")
	}
	if after.ThumbLastAttempt == nil {
		t.Error("thumb_last_attempt should be stamped")
	}
}

func TestProcessQueueShouldStopRetryingWhenAFileHasFailedTooManyTimes(t *testing.T) {
	gen, cat := setupTestThumb(t)
	gen.SetFFmpeg(fakeFFmpeg(t, "exit 1"), time.Second)

	seedVideo(t, cat, "hopeless.mp4")
	srcDir := t.TempDir()
	srcFn := func(f *catalog.FileRecord) string { return filepath.Join(srcDir, f.RelativePath) }

	exts := []string{".mp4"}

	for pass := 1; pass <= 4; pass++ {
		queued, err := cat.FilesNeedingThumbnails(exts, 10)
		if err != nil {
			t.Fatal(err)
		}
		if pass <= 3 && len(queued) != 1 {
			t.Fatalf("pass %d: queued %d files, want 1", pass, len(queued))
		}
		if pass == 4 {
			if len(queued) != 0 {
				t.Fatalf("pass 4: the file is still queued after 3 failures (%d rows)", len(queued))
			}
			break
		}
		gen.ProcessQueue(queued, srcFn, 1)
	}
}

func TestProcessQueueShouldNotBurnAttemptsWhenTheDaemonIsShuttingDown(t *testing.T) {
	gen, cat := setupTestThumb(t)
	gen.SetFFmpeg(fakeFFmpeg(t, "exit 1"), time.Second)

	file := seedVideo(t, cat, "interrupted.mp4")
	srcDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	gen.ProcessQueueContext(ctx, []catalog.FileRecord{*file}, func(f *catalog.FileRecord) string {
		return filepath.Join(srcDir, f.RelativePath)
	}, 1)

	after, err := cat.GetFile("Videos", "interrupted.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if after.ThumbAttempts != 0 {
		t.Errorf("thumb_attempts = %d, want 0: shutdown is not the file's fault", after.ThumbAttempts)
	}
}

func TestProcessQueueShouldStopEarlyWhenContextIsCancelledMidBatch(t *testing.T) {
	gen, cat := setupTestThumb(t)

	// Each invocation sleeps, so cancellation lands while the batch is running.
	gen.SetFFmpeg(fakeFFmpeg(t, "sleep 5"), 10*time.Second)

	var records []catalog.FileRecord
	for i := range 8 {
		records = append(records, *seedVideo(t, cat, filepath.Join("batch", string(rune('a'+i))+".mp4")))
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	srcDir := t.TempDir()
	start := time.Now()
	gen.ProcessQueueContext(ctx, records, func(f *catalog.FileRecord) string {
		return filepath.Join(srcDir, f.RelativePath)
	}, 1)
	elapsed := time.Since(start)

	// One in-flight invocation is killed by the cancelled context; the
	// remaining seven must never start.
	if elapsed > 5*time.Second {
		t.Errorf("queue took %s after cancellation; workers kept picking up files", elapsed)
	}
}
