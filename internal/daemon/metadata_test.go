package daemon

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	metadatapkg "github.com/tjmisko/coldstore/internal/metadata"
)

type metadataExtractFunc func(context.Context, string, string) (metadatapkg.Extracted, error)

func (fn metadataExtractFunc) Extract(ctx context.Context, path, ext string) (metadatapkg.Extracted, error) {
	return fn(ctx, path, ext)
}

func TestMetadataQueueExtensionsComeFromTheExtractor(t *testing.T) {
	want := metadatapkg.SupportedExtensions()
	if len(want) == 0 {
		t.Fatal("metadata queue would select nothing")
	}
	if !slices.Equal(metadataExts, want) {
		t.Fatalf("metadata queue extensions = %v, extractor supports %v", metadataExts, want)
	}
}

func TestExtractMetadataResolvesSyncedAndArchivedSources(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	cfg.Daemon.ThumbnailWorkers = 2
	declarePolicy(t, cfg, cat)

	synced := seedSyncedFile(t, cat, syncDir, "synced.jpg", 10)
	archived := seedSyncedFile(t, cat, syncDir, "nested/archived.mov", 20)
	archivePath := filepath.Join(coldDir, "archive", "TestFolder", archived.RelativePath)
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(syncDir, archived.RelativePath), archivePath); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateFileStatus(archived.ID, catalog.StatusArchived); err != nil {
		t.Fatal(err)
	}

	d := newTestDaemon(t, cfg, cat, testLogger())
	var mu sync.Mutex
	paths := make(map[string]string)
	d.metadataExtractor = metadataExtractFunc(func(_ context.Context, path, ext string) (metadatapkg.Extracted, error) {
		mu.Lock()
		paths[ext] = path
		mu.Unlock()
		return metadatapkg.Extracted{CapturedLocal: "2025-01-02 03:04:05", CapturedSource: "exif"}, nil
	})

	d.extractMetadata(context.Background())
	if paths[".jpg"] != filepath.Join(syncDir, synced.RelativePath) {
		t.Errorf("synced source = %q, want %q", paths[".jpg"], filepath.Join(syncDir, synced.RelativePath))
	}
	if paths[".mov"] != archivePath {
		t.Errorf("archived source = %q, want %q", paths[".mov"], archivePath)
	}
	for _, id := range []int64{synced.ID, archived.ID} {
		got, err := cat.GetFileByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.MetaScannedAt == nil || got.CapturedLocal != "2025-01-02 03:04:05" {
			t.Errorf("file %d extraction result = %+v", id, got)
		}
	}
}

func TestDerivedDataPassFinishesMetadataBeforeStartingThumbnails(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	cfg.Daemon.ThumbnailWorkers = 1
	declarePolicy(t, cfg, cat)
	file := seedJPEGFile(t, cat, syncDir, "photo.jpg")

	started := make(chan struct{})
	release := make(chan struct{})
	d := newTestDaemon(t, cfg, cat, testLogger())
	d.metadataExtractor = metadataExtractFunc(func(ctx context.Context, _, _ string) (metadatapkg.Extracted, error) {
		close(started)
		select {
		case <-release:
			return metadatapkg.Extracted{}, nil
		case <-ctx.Done():
			return metadatapkg.Extracted{}, ctx.Err()
		}
	})

	d.startDerivedDataPass(context.Background())
	<-started
	before, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.ThumbnailPath != "" {
		t.Fatalf("thumbnail started before metadata completed: %q", before.ThumbnailPath)
	}

	close(release)
	d.derivedWG.Wait()
	after, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.MetaScannedAt == nil {
		t.Error("metadata did not complete")
	}
	if after.ThumbnailPath == "" {
		t.Error("thumbnail did not run after metadata")
	}
}

func TestMetadataCancellationDoesNotBurnAnAttempt(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	file := seedSyncedFile(t, cat, syncDir, "photo.jpg", 10)

	started := make(chan struct{})
	d := newTestDaemon(t, cfg, cat, testLogger())
	d.metadataExtractor = metadataExtractFunc(func(ctx context.Context, _, _ string) (metadatapkg.Extracted, error) {
		close(started)
		<-ctx.Done()
		return metadatapkg.Extracted{}, ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.extractMetadata(ctx)
	}()
	<-started
	cancel()
	<-done

	got, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MetaAttempts != 0 || got.MetaLastAttempt != nil || got.MetaError != "" || got.MetaScannedAt != nil {
		t.Errorf("shutdown changed metadata attempt state: %+v", got)
	}
}

func TestMetadataFailureIsAttemptedOnlyOncePerPass(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	file := seedSyncedFile(t, cat, syncDir, "broken.jpg", 10)

	d := newTestDaemon(t, cfg, cat, testLogger())
	d.metadataExtractor = metadataExtractFunc(func(context.Context, string, string) (metadatapkg.Extracted, error) {
		return metadatapkg.Extracted{}, errors.New("decoder rejected metadata")
	})
	d.extractMetadata(context.Background())

	got, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MetaAttempts != 1 {
		t.Errorf("attempts after one pass = %d, want 1", got.MetaAttempts)
	}
}

func TestMetadataQueueUsesConfiguredWorkerConcurrency(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	cfg.Daemon.ThumbnailWorkers = 3
	declarePolicy(t, cfg, cat)
	for i := 0; i < 7; i++ {
		seedSyncedFile(t, cat, syncDir, "photo-"+string(rune('a'+i))+".jpg", 10)
	}
	files, err := cat.FilesNeedingMetadata(metadataExts, 20)
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{}, len(files))
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	d := newTestDaemon(t, cfg, cat, testLogger())
	d.metadataExtractor = metadataExtractFunc(func(ctx context.Context, _, _ string) (metadatapkg.Extracted, error) {
		current := active.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		active.Add(-1)
		return metadatapkg.Extracted{}, ctx.Err()
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.processMetadataQueue(context.Background(), files,
			func(file *catalog.FileRecord) string { return filepath.Join(syncDir, file.RelativePath) },
			cfg.Daemon.ThumbnailWorkers)
	}()
	for range cfg.Daemon.ThumbnailWorkers {
		<-started
	}
	if got := maximum.Load(); got != int32(cfg.Daemon.ThumbnailWorkers) {
		t.Errorf("maximum concurrent extractors = %d, want %d", got, cfg.Daemon.ThumbnailWorkers)
	}
	select {
	case <-started:
		t.Fatal("more extractors started than the configured worker count")
	default:
	}
	close(release)
	<-done
}

func TestManualScanResetsMetadataFailureBudget(t *testing.T) {
	env := newWorkerEnv(t)
	file := seedSyncedFile(t, env.cat, env.syncDir, "photo.jpg", 10)
	for range 3 {
		if err := env.cat.RecordMetadataFailure(file.ID, "missing decoder", time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}
	blocked, err := env.cat.FilesNeedingMetadata([]string{".jpg"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 0 {
		t.Fatalf("failure budget did not block metadata queue: %+v", blocked)
	}

	jobID, err := env.cat.CreateJob(catalog.JobActionScan, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	env.worker.RunOnce(context.Background())
	if job := env.job(t, jobID); job.State != catalog.JobStateDone {
		t.Fatalf("scan job state = %q, want done", job.State)
	}

	requeued, err := env.cat.FilesNeedingMetadata([]string{".jpg"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(requeued) != 1 || requeued[0].ID != file.ID {
		t.Errorf("metadata queue after manual rescan = %+v", requeued)
	}
	got, err := env.cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MetaAttempts != 0 || got.MetaLastAttempt != nil || got.MetaError != "" {
		t.Errorf("metadata failure state after manual rescan = %+v", got)
	}
}

func seedJPEGFile(t *testing.T, cat *catalog.Catalog, syncDir, relPath string) *catalog.FileRecord {
	t.Helper()
	path := filepath.Join(syncDir, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{R: 120, G: 40, B: 20, A: 255})
		}
	}
	if err := jpeg.Encode(file, img, nil); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	record := &catalog.FileRecord{
		FolderLabel:  "TestFolder",
		RelativePath: relPath,
		Filename:     filepath.Base(relPath),
		Extension:    filepath.Ext(relPath),
		MimeType:     "image/jpeg",
		SizeBytes:    info.Size(),
		FileMtime:    info.ModTime().UTC(),
		FileAtime:    info.ModTime().UTC(),
		Status:       catalog.StatusSynced,
	}
	if _, err := cat.UpsertFile(record, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	got, err := cat.GetFile("TestFolder", relPath)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
