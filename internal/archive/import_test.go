package archive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/fsx"
	"github.com/tjmisko/coldstore/internal/fsx/fsxtest"
)

func TestImport(t *testing.T) {
	t.Run("should refuse a directory that is not absolute", func(t *testing.T) {
		s := openService(t, testOptions(t))
		if _, err := s.ImportStart("photos", "relative/dir"); !errors.Is(err, ErrImportDir) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("should refuse a path that is not a directory", func(t *testing.T) {
		s := openService(t, testOptions(t))
		file := filepath.Join(t.TempDir(), "f.txt")
		writeTree(t, filepath.Dir(file), map[string]string{"f.txt": "x"})
		if _, err := s.ImportStart("photos", file); !errors.Is(err, ErrImportDir) {
			t.Fatalf("err = %v", err)
		}
		if _, err := s.ImportStart("photos", filepath.Join(t.TempDir(), "missing")); !errors.Is(err, ErrImportDir) {
			t.Fatalf("missing dir err = %v", err)
		}
	})

	t.Run("should refuse a directory inside the archive root", func(t *testing.T) {
		opts := testOptions(t)
		s := openService(t, opts)
		for _, dir := range []string{opts.Root, filepath.Join(opts.Root, "blobs")} {
			if _, err := s.ImportStart("photos", dir); !errors.Is(err, ErrImportDir) {
				t.Fatalf("%s: err = %v", dir, err)
			}
		}
	})

	t.Run("should refuse an undeclared collection", func(t *testing.T) {
		s := openService(t, testOptions(t))
		if _, err := s.ImportStart("music", t.TempDir()); !errors.Is(err, ErrCollectionUnknown) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("should report no status when the run id is unknown", func(t *testing.T) {
		s := openService(t, testOptions(t))
		if _, ok := s.ImportStatus(42); ok {
			t.Fatal("status for an unknown run")
		}
	})

	t.Run("should refuse a second import while one is running", func(t *testing.T) {
		opts := testOptions(t)
		faulty := fsxtest.NewFaulty(fsx.OS())
		release := make(chan struct{})
		opts.FS = faulty
		opts.Sleep = func(time.Duration) { <-release }
		s := openService(t, opts)
		faulty.Reset()
		failEveryStagingWrite(faulty, opts, syscall.EIO, 1)
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "alpha"})

		id, err := s.ImportStart("photos", src)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ImportStart("photos", src); !errors.Is(err, ErrImportRunning) {
			t.Fatalf("second start err = %v", err)
		}
		close(release)
		status := waitImport(t, s, id)
		if status.State != "done" || status.FilesCreated != 1 {
			t.Fatalf("status %+v", status)
		}
		if _, err := s.ImportStart("photos", src); err != nil {
			t.Fatalf("start after finishing: %v", err)
		}
	})

	t.Run("should keep only the last 16 finished runs when more are started", func(t *testing.T) {
		s := openService(t, testOptions(t))
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "alpha"})
		for i := 0; i < 20; i++ {
			runImport(t, s, "photos", src)
		}
		if _, ok := s.ImportStatus(1); ok {
			t.Fatal("run 1 should have been evicted")
		}
		if _, ok := s.ImportStatus(4); ok {
			t.Fatal("run 4 should have been evicted")
		}
		if _, ok := s.ImportStatus(5); !ok {
			t.Fatal("run 5 should be kept")
		}
		if _, ok := s.ImportStatus(20); !ok {
			t.Fatal("run 20 should be kept")
		}
	})

	t.Run("should cap the failure detail at 1000 entries and keep counting when many files fail", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root reads everything")
		}
		s := openService(t, testOptions(t))
		src := t.TempDir()
		const total = 1003
		for i := 0; i < total; i++ {
			path := filepath.Join(src, fmt.Sprintf("f%04d", i))
			if err := os.WriteFile(path, []byte("x"), 0o000); err != nil {
				t.Fatal(err)
			}
		}
		status := runImport(t, s, "photos", src)
		if status.FailedCount != total || len(status.Failed) != 1000 || status.State != "done" {
			t.Fatalf("failed=%d detail=%d state=%s", status.FailedCount, len(status.Failed), status.State)
		}
	})

	t.Run("should count skipped entries when the directory holds a symlink", func(t *testing.T) {
		s := openService(t, testOptions(t))
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "alpha"})
		if err := os.Symlink(filepath.Join(src, "a.txt"), filepath.Join(src, "link")); err != nil {
			t.Fatal(err)
		}
		status := runImport(t, s, "photos", src)
		if status.FilesSeen != 1 || status.SkippedCount != 1 || status.State != "done" {
			t.Fatalf("status %+v", status)
		}
	})

	t.Run("should end the run as failed when the service closes mid-import", func(t *testing.T) {
		opts := testOptions(t)
		faulty := fsxtest.NewFaulty(fsx.OS())
		inSleep := make(chan struct{})
		opts.FS = faulty
		var s *Service
		opts.Sleep = func(time.Duration) {
			select {
			case inSleep <- struct{}{}:
			default:
			}
			<-s.ctx.Done()
		}
		s = openService(t, opts)
		faulty.Reset()
		failEveryStagingWrite(faulty, opts, syscall.EIO, 4)
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "alpha"})
		id, err := s.ImportStart("photos", src)
		if err != nil {
			t.Fatal(err)
		}
		<-inSleep
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		status, ok := s.ImportStatus(id)
		if !ok || status.State != "failed" || status.Error == "" {
			t.Fatalf("status %+v ok=%v", status, ok)
		}
	})
}
