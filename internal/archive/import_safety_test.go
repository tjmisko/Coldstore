package archive

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/fsx"
	"github.com/tjmisko/coldstore/internal/fsx/fsxtest"
)

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("root reads everything")
	}
}

func readReport(t *testing.T, path string) []failureLine {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening the failure report: %v", err)
	}
	defer f.Close()
	var lines []failureLine
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var line failureLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("report line %q: %v", scanner.Text(), err)
		}
		lines = append(lines, line)
	}
	return lines
}

func TestImportSafety(t *testing.T) {
	t.Run("should list an unreadable directory as failed with its path and still import its siblings when a directory cannot be read", func(t *testing.T) {
		skipIfRoot(t)
		s := openService(t, testOptions(t))
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a-locked/inside.txt": "x", "b.txt": "bravo", "c/d.txt": "delta"})
		locked := filepath.Join(src, "a-locked")
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

		status := runImport(t, s, "photos", src)

		if status.FilesCreated != 2 || status.FailedCount != 1 || status.UnreadableCount != 1 {
			t.Fatalf("status %+v", status)
		}
		resolved, _ := filepath.EvalSymlinks(locked)
		if len(status.Failed) != 1 || status.Failed[0].Path != resolved || status.Failed[0].Error == "" {
			t.Fatalf("failed = %+v, want the locked directory", status.Failed)
		}
		if status.SkippedCount != 0 {
			t.Fatalf("an unreadable directory was counted as a benign skip: %+v", status)
		}
	})

	t.Run("should import the target when the import root is a symlink to a directory", func(t *testing.T) {
		s := openService(t, testOptions(t))
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "alpha", "sub/b.txt": "bravo"})
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(src, link); err != nil {
			t.Fatal(err)
		}

		status := runImport(t, s, "photos", link)

		if status.FilesCreated != 2 || status.FailedCount != 0 || status.SkippedCount != 0 {
			t.Fatalf("status %+v", status)
		}
	})

	t.Run("should read an unreadable file once without retrying when the source refuses the read", func(t *testing.T) {
		skipIfRoot(t)
		opts := testOptions(t)
		sleeps := &sleepLog{}
		opts.Sleep = sleeps.sleep
		s := openService(t, opts)
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "alpha", "z.txt": "zulu"})
		if err := os.Chmod(filepath.Join(src, "a.txt"), 0o000); err != nil {
			t.Fatal(err)
		}

		status := runImport(t, s, "photos", src)

		if got := sleeps.snapshot(); len(got) != 0 {
			t.Fatalf("a source read error was retried: waits %v", got)
		}
		if status.State != "done" || status.FailedCount != 1 || status.UnreadableCount != 1 || status.FilesCreated != 1 {
			t.Fatalf("status %+v", status)
		}
	})

	t.Run("should abort the run after one file when an archive-side failure survives its retries", func(t *testing.T) {
		opts := testOptions(t)
		faulty := fsxtest.NewFaulty(fsx.OS())
		sleeps := &sleepLog{}
		opts.FS = faulty
		opts.Sleep = sleeps.sleep
		s := openService(t, opts)
		faulty.Reset()
		failEveryStagingWrite(faulty, opts, syscall.EROFS, 100)
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "alpha", "b.txt": "bravo", "c.txt": "charlie", "d.txt": "delta"})

		status := runImport(t, s, "photos", src)

		if status.State != "failed" || status.FilesSeen != 1 || status.FailedCount != 1 {
			t.Fatalf("status %+v", status)
		}
		if got := sleeps.snapshot(); len(got) != len(importBackoff) {
			t.Fatalf("waits %v, want exactly one file's retries", got)
		}
		if !strings.Contains(status.Error, "run stopped after 1 files seen") {
			t.Fatalf("error %q does not say how far the run got", status.Error)
		}
	})

	t.Run("should end the run as cancelled and allow a later run to resume when the import is cancelled", func(t *testing.T) {
		opts := testOptions(t)
		faulty := fsxtest.NewFaulty(fsx.OS())
		inSleep := make(chan struct{}, 1)
		release := make(chan struct{})
		opts.FS = faulty
		opts.Sleep = func(time.Duration) {
			select {
			case inSleep <- struct{}{}:
			default:
			}
			<-release
		}
		s := openService(t, opts)
		faulty.Reset()
		failEveryStagingWrite(faulty, opts, syscall.EIO, 1)
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "alpha", "b.txt": "bravo", "c.txt": "charlie"})
		id, err := s.ImportStart("photos", src)
		if err != nil {
			t.Fatal(err)
		}
		<-inSleep

		if err := s.ImportCancel(id); err != nil {
			t.Fatalf("ImportCancel: %v", err)
		}
		close(release)
		status := waitImport(t, s, id)

		if status.State != "cancelled" || !strings.Contains(status.Error, "run the import again") {
			t.Fatalf("status %+v", status)
		}
		if err := s.ImportCancel(id); !errors.Is(err, ErrImportNotRunning) {
			t.Fatalf("cancel of a finished run: %v", err)
		}
		if err := s.ImportCancel(999); !errors.Is(err, ErrImportUnknown) {
			t.Fatalf("cancel of an unknown run: %v", err)
		}
		resumed := runImport(t, s, "photos", src)
		if resumed.State != "done" || resumed.FilesCreated+resumed.FilesUnchanged != 3 || resumed.FailedCount != 0 {
			t.Fatalf("resumed status %+v", resumed)
		}
	})

	t.Run("should refuse a directory that contains the archive root when the import dir is an ancestor", func(t *testing.T) {
		opts := testOptions(t)
		s := openService(t, opts)
		for _, dir := range []string{filepath.Dir(opts.Root), "/"} {
			if _, err := s.ImportStart("photos", dir); !errors.Is(err, ErrImportDir) {
				t.Errorf("%s: err = %v, want ErrImportDir", dir, err)
			}
		}
	})

	t.Run("should refuse a symlink into the archive root when the link resolves inside it", func(t *testing.T) {
		opts := testOptions(t)
		s := openService(t, opts)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(opts.Root, link); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ImportStart("photos", link); !errors.Is(err, ErrImportDir) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("should write every failure to import-reports/<run>.jsonl when files fail", func(t *testing.T) {
		skipIfRoot(t)
		opts := testOptions(t)
		s := openService(t, opts)
		src := t.TempDir()
		writeTree(t, src, map[string]string{"ok.txt": "fine", "locked/x.txt": "y", "bad.txt": "z"})
		if err := os.Chmod(filepath.Join(src, "bad.txt"), 0o000); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(src, "locked"), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(src, "locked"), 0o755) })

		status := runImport(t, s, "photos", src)

		if filepath.Dir(status.ReportPath) != filepath.Join(opts.Root, "import-reports") ||
			!strings.HasSuffix(status.ReportPath, ".jsonl") {
			t.Fatalf("report path %q", status.ReportPath)
		}
		lines := readReport(t, status.ReportPath)
		kinds := map[string]string{}
		for _, line := range lines {
			if line.Error == "" {
				t.Errorf("report line without an error: %+v", line)
			}
			kinds[filepath.Base(line.Path)] = line.Kind
		}
		if len(lines) != 2 || kinds["locked"] != "unreadable" || kinds["bad.txt"] != "unreadable" {
			t.Fatalf("report lines %+v", lines)
		}
	})

	t.Run("should write no report when nothing fails", func(t *testing.T) {
		opts := testOptions(t)
		s := openService(t, opts)
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "alpha"})

		status := runImport(t, s, "photos", src)

		if status.ReportPath != "" {
			t.Fatalf("report path %q for a clean run", status.ReportPath)
		}
		if _, err := os.Stat(filepath.Join(opts.Root, "import-reports")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("import-reports exists after a clean run: %v", err)
		}
	})

	t.Run("should keep the complete failure list in the report when failures exceed the in-memory cap", func(t *testing.T) {
		skipIfRoot(t)
		s := openService(t, testOptions(t))
		src := t.TempDir()
		const total = maxFailureDetail + 5
		for i := 0; i < total; i++ {
			name := filepath.Join(src, "f"+strings.Repeat("0", 4-len(itoa(i)))+itoa(i))
			if err := os.WriteFile(name, []byte("x"), 0o000); err != nil {
				t.Fatal(err)
			}
		}
		status := runImport(t, s, "photos", src)
		if len(status.Failed) != maxFailureDetail || len(readReport(t, status.ReportPath)) != total {
			t.Fatalf("memory %d report %d, want %d in the report", len(status.Failed), len(readReport(t, status.ReportPath)), total)
		}
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for ; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}
