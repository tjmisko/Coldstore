package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/filetype"
)

func writeFile(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScanShouldRecordTheDetectedTypeWhenAFileHasNoExtension(t *testing.T) {
	// The case H0b exists for. mime.TypeByExtension answers nothing for a
	// file called "bookmarks", so without a content sniff the row is
	// unreachable by any extension-keyed queue.
	s, cat, folder, dir := newHygieneScanner(t)
	writeFile(t, dir, "bookmarks", []byte("https://example.invalid/one\nhttps://example.invalid/two\n"))

	if err := s.ScanFolderContext(context.Background(), folder); err != nil {
		t.Fatalf("scan: %v", err)
	}

	rec, err := cat.GetFile(folder.FolderLabel, "bookmarks")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Extension != "" {
		t.Fatalf("Extension = %q, want empty: the fixture is wrong", rec.Extension)
	}
	if rec.DetectedType != filetype.TypeText {
		t.Errorf("DetectedType = %q, want %q", rec.DetectedType, filetype.TypeText)
	}
}

func TestScanShouldRecordTheDetectedTypeWhenTheExtensionLies(t *testing.T) {
	s, cat, folder, dir := newHygieneScanner(t)
	writeFile(t, dir, "screenshot.txt", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))

	if err := s.ScanFolderContext(context.Background(), folder); err != nil {
		t.Fatalf("scan: %v", err)
	}

	rec, err := cat.GetFile(folder.FolderLabel, "screenshot.txt")
	if err != nil {
		t.Fatal(err)
	}
	if rec.DetectedType != filetype.TypePNG {
		t.Errorf("DetectedType = %q, want %q: the sniff reads bytes, not the name", rec.DetectedType, filetype.TypePNG)
	}
}

func TestScanShouldRecordUnknownWhenTheFileIsUnidentifiable(t *testing.T) {
	s, cat, folder, dir := newHygieneScanner(t)
	writeFile(t, dir, "blob", []byte{0x01, 0x00, 0x02, 0x03, 0xFF, 0xFE})

	if err := s.ScanFolderContext(context.Background(), folder); err != nil {
		t.Fatalf("scan: %v", err)
	}

	rec, err := cat.GetFile(folder.FolderLabel, "blob")
	if err != nil {
		t.Fatal(err)
	}
	if rec.DetectedType != filetype.TypeUnknown {
		t.Errorf("DetectedType = %q, want %q", rec.DetectedType, filetype.TypeUnknown)
	}
}

func TestScanShouldLeaveTheSniffQueueEmptyWhenEveryFileHasBeenSeen(t *testing.T) {
	// The gate: the second scan must have nothing left to sniff. Without it
	// the daemon re-opens every file in every folder on every catalog tick.
	s, cat, folder, dir := newHygieneScanner(t)
	for _, name := range []string{"bookmarks", "notes.txt", "a.jpg", "blob"} {
		writeFile(t, dir, name, []byte("\xFF\xD8\xFFsomething"))
	}

	if err := s.ScanFolderContext(context.Background(), folder); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	queued, err := cat.FilesNeedingSniff(folder.FolderLabel, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Fatalf("after the first scan %d rows still need sniffing", len(queued))
	}

	if err := s.ScanFolderContext(context.Background(), folder); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	queued, err = cat.FilesNeedingSniff(folder.FolderLabel, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Errorf("the second scan re-queued %d rows: the sniff must be gated on change, not run every pass", len(queued))
	}
}

func TestScanShouldResniffAFileWhenItsContentChanges(t *testing.T) {
	s, cat, folder, dir := newHygieneScanner(t)
	path := writeFile(t, dir, "notes", []byte("just some text\n"))

	if err := s.ScanFolderContext(context.Background(), folder); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	rec, err := cat.GetFile(folder.FolderLabel, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if rec.DetectedType != filetype.TypeText {
		t.Fatalf("DetectedType = %q, want %q", rec.DetectedType, filetype.TypeText)
	}

	// Replaced in place with a PDF, mtime moved forward so the upsert can see
	// that the bytes are not the ones that were sniffed.
	if err := os.WriteFile(path, []byte("%PDF-1.4\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	if err := s.ScanFolderContext(context.Background(), folder); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	rec, err = cat.GetFile(folder.FolderLabel, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if rec.DetectedType != filetype.TypePDF {
		t.Errorf("DetectedType = %q, want %q: changed bytes must be re-sniffed", rec.DetectedType, filetype.TypePDF)
	}
}

func TestScanShouldNotFailWhenAFileDisappearsBeforeTheSniff(t *testing.T) {
	// The sniff runs after the walk, so a file deleted in between is normal.
	// It must record something and leave the queue rather than failing the
	// scan or being re-opened forever.
	s, cat, folder, dir := newHygieneScanner(t)
	path := writeFile(t, dir, "fleeting", []byte("text\n"))

	if err := s.ScanFolderContext(context.Background(), folder); err != nil {
		t.Fatalf("first scan: %v", err)
	}

	// Put the row back into the sniff queue the way a changed file would, then
	// take the file away, so the sniff pass reads a path that is gone.
	mtime := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if _, err := cat.UpsertFile(&catalog.FileRecord{
		FolderLabel:  folder.FolderLabel,
		RelativePath: "fleeting",
		Filename:     "fleeting",
		SizeBytes:    5,
		FileMtime:    mtime,
		FileAtime:    mtime,
		Status:       catalog.StatusSynced,
	}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if got := s.sniffFolder(context.Background(), folder); got != 1 {
		t.Errorf("sniffed %d rows, want 1: a missing file must still be recorded", got)
	}

	rec, err := cat.GetFile(folder.FolderLabel, "fleeting")
	if err != nil {
		t.Fatal(err)
	}
	if rec.DetectedType != filetype.TypeUnknown {
		t.Errorf("DetectedType = %q, want %q: an unreadable file must still leave the queue",
			rec.DetectedType, filetype.TypeUnknown)
	}
}
