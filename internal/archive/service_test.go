package archive

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/crashpoint"
	"github.com/tjmisko/coldstore/internal/fsx"
	"github.com/tjmisko/coldstore/internal/fsx/fsxtest"
	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/ingest"
	"github.com/tjmisko/coldstore/internal/journal"
	"github.com/tjmisko/coldstore/internal/store"
)

func TestService(t *testing.T) {
	t.Run("should create one item and one blob when the same content is imported at one path twice", func(t *testing.T) {
		opts := testOptions(t)
		s := openService(t, opts)
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "alpha", "sub/b.txt": "bravo"})

		first := runImport(t, s, "photos", src)
		second := runImport(t, s, "photos", src)

		if first.State != "done" || first.FilesCreated != 2 || first.FilesSeen != 2 {
			t.Fatalf("first run %+v", first)
		}
		if second.FilesUnchanged != second.FilesSeen || second.FilesSeen != 2 || second.FilesCreated != 0 {
			t.Fatalf("second run %+v", second)
		}
		stats, err := s.Index().ArchiveStats()
		if err != nil || stats.BlobCount != 2 {
			t.Fatalf("stats %+v err %v", stats, err)
		}
		folder, err := s.Index().FolderStats("photos")
		if err != nil || folder.Live != 2 {
			t.Fatalf("folder stats %+v err %v", folder, err)
		}
		recordsBefore := s.core.j.LastSeq()
		runImport(t, s, "photos", src)
		if s.core.j.LastSeq() != recordsBefore {
			t.Fatal("an unchanged import appended to the journal")
		}
	})

	t.Run("should fill gaps and report agreement when a second directory with overlapping content is imported", func(t *testing.T) {
		s := openService(t, testOptions(t))
		dirA, dirB := t.TempDir(), t.TempDir()
		writeTree(t, dirA, map[string]string{"shared.txt": "same", "a-only.txt": "a", "conflict.txt": "A version"})
		writeTree(t, dirB, map[string]string{"shared.txt": "same", "b-only.txt": "b", "conflict.txt": "B version"})

		runA := runImport(t, s, "photos", dirA)
		runB := runImport(t, s, "photos", dirB)
		if runA.FilesCreated != 3 {
			t.Fatalf("A %+v", runA)
		}
		if runB.FilesSeen != 3 || runB.FilesUnchanged != 1 || runB.FilesCreated != 1 || runB.FilesOther != 1 || runB.FailedCount != 0 {
			t.Fatalf("B %+v", runB)
		}
		thirdB := runImport(t, s, "photos", dirB)
		thirdA := runImport(t, s, "photos", dirA)
		for name, run := range map[string]ImportStatus{"B": thirdB, "A": thirdA} {
			if run.FilesUnchanged != run.FilesSeen || run.FilesSeen != 3 {
				t.Fatalf("third run of %s %+v", name, run)
			}
		}
		besideEntries, err := s.Index().Entries(index.EntryQuery{Folder: "photos", Search: ".sha256-", Limit: 10})
		if err != nil || len(besideEntries) != 1 {
			t.Fatalf("beside entries %d err %v", len(besideEntries), err)
		}
		entries, err := s.Index().Entries(index.EntryQuery{Folder: "photos", Limit: 100})
		if err != nil || len(entries) != 5 {
			t.Fatalf("entries %d err %v", len(entries), err)
		}
	})

	t.Run("should leave the source directory identical when an import finishes", func(t *testing.T) {
		s := openService(t, testOptions(t))
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "alpha", "d/e/b.txt": "bravo"})
		old := time.Date(2019, 1, 2, 3, 4, 5, 0, time.UTC)
		if err := os.Chtimes(filepath.Join(src, "a.txt"), old, old); err != nil {
			t.Fatal(err)
		}
		before := snapshotTree(t, src)
		status := runImport(t, s, "photos", src)
		if status.State != "done" {
			t.Fatalf("status %+v", status)
		}
		if after := snapshotTree(t, src); !equalLines(before, after) {
			t.Fatalf("source changed:\nbefore %v\nafter  %v", before, after)
		}
	})

	t.Run("should heal the index at Open when a record was appended but not applied", func(t *testing.T) {
		opts := testOptions(t)
		s := openService(t, opts)
		mustIngest(t, s, "photos", "first.txt", "one")

		crashpoint.Arm("ingest.journal.appended", 1, func() { panic("simulated crash") })
		func() {
			defer func() { _ = recover() }()
			_, _ = s.Ingest(context.Background(), "test", memArrival("photos", "second.txt", "two"))
		}()
		crashpoint.Disarm()
		if applied, last := s.Index().AppliedSeq(), s.core.j.LastSeq(); applied >= last {
			t.Fatalf("fixture failed: applied %d, journal %d", applied, last)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}

		reopened := openService(t, opts)
		entry, found, err := reopened.Index().PlacementAt("photos", "second.txt")
		if err != nil || !found {
			t.Fatalf("second.txt not healed: found=%v err=%v", found, err)
		}
		if reopened.Index().AppliedSeq() != reopened.core.j.LastSeq() {
			t.Fatal("index is still behind the journal")
		}
		if _, err := os.Stat(filepath.Join(opts.Root, "views", "photos", "second.txt")); err != nil {
			t.Fatalf("view not regenerated for %+v: %v", entry, err)
		}
	})

	t.Run("should catch the index up and write one item when an arrival is retried after an apply failure", func(t *testing.T) {
		s := openService(t, testOptions(t))
		mustIngest(t, s, "photos", "first.txt", "one")

		crashpoint.Arm("ingest.journal.appended", 1, func() { panic("simulated crash") })
		func() {
			defer func() { _ = recover() }()
			_, _ = s.Ingest(context.Background(), "test", memArrival("photos", "second.txt", "two"))
		}()
		crashpoint.Disarm()
		if applied, last := s.Index().AppliedSeq(), s.core.j.LastSeq(); applied >= last {
			t.Fatalf("fixture failed: applied %d, journal %d", applied, last)
		}

		if _, err := s.Ingest(context.Background(), "test", memArrival("photos", "second.txt", "two")); err != nil {
			t.Fatal(err)
		}
		if s.Index().AppliedSeq() != s.core.j.LastSeq() {
			t.Fatal("index is still behind the journal")
		}
		created := 0
		err := s.core.j.Replay(context.Background(), 0, func(rec journal.Record) error {
			if rec.Type == journal.TypeItemCreated {
				created++
			}
			return nil
		})
		if err != nil || created != 2 {
			t.Fatalf("item_created records %d (want 2) err %v", created, err)
		}
		if _, found, err := s.Index().PlacementAt("photos", "second.txt"); err != nil || !found {
			t.Fatalf("second.txt placement: found=%v err=%v", found, err)
		}
	})

	t.Run("should rebuild the index when index.db is corrupt at Open", func(t *testing.T) {
		opts := testOptions(t)
		s := openService(t, opts)
		mustIngest(t, s, "photos", "a.txt", "alpha")
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			_ = os.Remove(filepath.Join(opts.Root, "index.db"+suffix))
		}
		if err := os.WriteFile(filepath.Join(opts.Root, "index.db"), bytes.Repeat([]byte("garbage!"), 600), 0o644); err != nil {
			t.Fatal(err)
		}

		reopened := openService(t, opts)
		if _, found, err := reopened.Index().PlacementAt("photos", "a.txt"); err != nil || !found {
			t.Fatalf("placement lost: found=%v err=%v", found, err)
		}
		events, err := reopened.Index().Events("", "index_rebuilt", 10)
		if err != nil || len(events) != 1 {
			t.Fatalf("events %+v err %v", events, err)
		}
	})

	t.Run("should refuse to open when the journal format is newer and change nothing", func(t *testing.T) {
		opts := testOptions(t)
		s := openService(t, opts)
		mustIngest(t, s, "photos", "a.txt", "alpha")
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(opts.Root, "journal", "FORMAT"), []byte("99\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		before := snapshotTree(t, opts.Root)
		_, err := Open(context.Background(), opts)
		if !errors.Is(err, journal.ErrFormatNewer) {
			t.Fatalf("err = %v", err)
		}
		if after := snapshotTree(t, opts.Root); !equalLines(before, after) {
			t.Fatalf("Open modified the root:\nbefore %v\nafter  %v", before, after)
		}
		report, err := FormatVersions(opts.Root)
		if err != nil || !report.Newer || report.JournalDisk != 99 || report.JournalBinary != journal.FormatVersion {
			t.Fatalf("FormatVersions = %+v, %v", report, err)
		}
	})

	t.Run("should refuse to open when the index format is newer", func(t *testing.T) {
		opts := testOptions(t)
		s := openService(t, opts)
		mustIngest(t, s, "photos", "a.txt", "alpha")
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(opts.Root, "index.db")
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		var version [4]byte
		binary.BigEndian.PutUint32(version[:], 99)
		if _, err := f.WriteAt(version[:], 60); err != nil {
			t.Fatal(err)
		}
		f.Close()

		_, err = Open(context.Background(), opts)
		if !errors.Is(err, index.ErrFormatNewer) {
			t.Fatalf("err = %v", err)
		}
		report, err := FormatVersions(opts.Root)
		if err != nil || report.IndexDisk != 99 || !report.Newer {
			t.Fatalf("FormatVersions = %+v, %v", report, err)
		}
		// The refused Open must have released the lock.
		lock, err := journal.AcquireLock(filepath.Join(opts.Root, "journal"))
		if err != nil {
			t.Fatalf("lock still held after a refused Open: %v", err)
		}
		_ = lock.Release()
	})

	t.Run("should fail with ErrLocked when a second process opens the same root", func(t *testing.T) {
		opts := testOptions(t)
		s := openService(t, opts)
		cmd := exec.Command(os.Args[0], "-test.run=^TestLockChild$", "-test.count=1")
		cmd.Env = append(os.Environ(), "COLDSTORE_LOCKCHILD_ROOT="+opts.Root)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("child failed: %v\n%s", err, out)
		}
		if !bytes.Contains(out, []byte("CHILD_GOT_ERRLOCKED")) {
			t.Fatalf("child output %s", out)
		}
		_ = s
	})

	t.Run("should fail with ErrLocked when RebuildIndex or ScrubOffline runs while the service is open", func(t *testing.T) {
		opts := testOptions(t)
		s := openService(t, opts)
		mustIngest(t, s, "photos", "a.txt", "alpha")
		if _, err := RebuildIndex(context.Background(), opts); !errors.Is(err, journal.ErrLocked) {
			t.Fatalf("RebuildIndex err = %v", err)
		}
		if _, err := ScrubOffline(context.Background(), opts); !errors.Is(err, journal.ErrLocked) {
			t.Fatalf("ScrubOffline err = %v", err)
		}
	})

	t.Run("should rebuild and scrub offline when the service is closed", func(t *testing.T) {
		opts := testOptions(t)
		s := openService(t, opts)
		mustIngest(t, s, "photos", "a.txt", "alpha")
		mustIngest(t, s, "photos", "b.txt", "bravo")
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(opts.Root, "views")); err != nil {
			t.Fatal(err)
		}
		report, err := RebuildIndex(context.Background(), opts)
		if err != nil || report.Entries != 2 || report.ViewsLinked != 2 || report.Records == 0 || report.Torn != nil {
			t.Fatalf("RebuildIndex = %+v, %v", report, err)
		}
		scrubbed, err := ScrubOffline(context.Background(), opts)
		if err != nil || scrubbed.Checked != 2 || len(scrubbed.Findings) != 0 {
			t.Fatalf("ScrubOffline = %+v, %v", scrubbed, err)
		}
	})

	t.Run("should record a journal_torn event when the tail was truncated at Open", func(t *testing.T) {
		opts := testOptions(t)
		s := openService(t, opts)
		mustIngest(t, s, "photos", "a.txt", "alpha")
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		segment := filepath.Join(opts.Root, "journal", "000001.jsonl")
		f, err := os.OpenFile(segment, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(`{"seq":99,"at":"2026-01-01T00:00:00Z","by":"x","t":"blob_a`); err != nil {
			t.Fatal(err)
		}
		f.Close()

		reopened := openService(t, opts)
		events, err := reopened.Index().Events("", "journal_torn", 10)
		if err != nil || len(events) != 1 {
			t.Fatalf("events %+v err %v", events, err)
		}
		if _, found, _ := reopened.Index().PlacementAt("photos", "a.txt"); !found {
			t.Fatal("intact record lost with the torn tail")
		}
	})

	t.Run("should declare a new collection when it appears in Options.Collections", func(t *testing.T) {
		opts := testOptions(t)
		opts.Collections = []string{"photos"}
		s := openService(t, opts)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		opts.Collections = []string{"photos", "books"}
		reopened := openService(t, opts)
		folder, found, err := reopened.Index().FolderByName("books")
		if err != nil || !found || folder.Kind != journal.FolderKindCollection {
			t.Fatalf("books = %+v found=%v err=%v", folder, found, err)
		}
		if !reopened.CollectionDeclared("books") || reopened.CollectionDeclared("music") {
			t.Fatal("CollectionDeclared disagrees with Options.Collections")
		}
		before := reopened.core.j.LastSeq()
		_ = reopened.Close()
		again := openService(t, opts)
		if again.core.j.LastSeq() != before {
			t.Fatal("reopening re-declared a collection")
		}
	})

	t.Run("should refuse an arrival for a collection that is not declared", func(t *testing.T) {
		s := openService(t, testOptions(t))
		_, err := s.Ingest(context.Background(), "test", memArrival("music", "a.txt", "x"))
		if !errors.Is(err, ErrCollectionUnknown) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("should refuse every operation when the service is closed", func(t *testing.T) {
		s := openService(t, testOptions(t))
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Ingest(context.Background(), "test", memArrival("photos", "a.txt", "x")); !errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("second Close = %v", err)
		}
	})

	t.Run("should place a held blob under a new path when Place is called", func(t *testing.T) {
		s := openService(t, testOptions(t))
		first := mustIngest(t, s, "photos", "a.txt", "alpha")
		res, err := s.Place(context.Background(), "test", "photos", "copy/a.txt", time.Time{}, first.Hash)
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != ingest.OutcomeCopied || res.Item != first.Item || res.EntryID == 0 {
			t.Fatalf("res = %+v", res)
		}
		var unheld store.Hash
		unheld[0] = 1
		if _, err := s.Place(context.Background(), "test", "photos", "x.txt", time.Time{}, unheld); !errors.Is(err, ErrBlobNotHeld) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("should run at most UploadConcurrency Puts at once when uploads arrive together", func(t *testing.T) {
		opts := testOptions(t)
		opts.UploadConcurrency = 2
		s := openService(t, opts)
		var inFlight, peak atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				a := memArrival("photos", fmt.Sprintf("f%d.txt", i), fmt.Sprintf("body %d", i))
				a.Open = func() (io.ReadCloser, error) {
					now := inFlight.Add(1)
					for {
						old := peak.Load()
						if now <= old || peak.CompareAndSwap(old, now) {
							break
						}
					}
					time.Sleep(15 * time.Millisecond)
					return &countingReader{r: bytes.NewReader([]byte(fmt.Sprintf("body %d", i))), onClose: func() { inFlight.Add(-1) }}, nil
				}
				if _, err := s.Ingest(context.Background(), "upload", a); err != nil {
					t.Errorf("Ingest %d: %v", i, err)
				}
			}(i)
		}
		wg.Wait()
		if peak.Load() > 2 {
			t.Fatalf("peak concurrent Puts = %d, want <= 2", peak.Load())
		}
	})
}

type countingReader struct {
	r       io.Reader
	onClose func()
	once    sync.Once
}

func (c *countingReader) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *countingReader) Close() error               { c.once.Do(c.onClose); return nil }

// TestLockChild is the second process of the ErrLocked test; it does nothing
// unless the parent set the environment.
func TestPathConflicts(t *testing.T) {
	// withConflictingJournal builds an archive whose journal holds placements
	// "a" and "a/b" in one folder, as a daemon without the Resolve check could
	// have written, plus one healthy placement "z.txt".
	withConflictingJournal := func(t *testing.T) Options {
		t.Helper()
		opts := testOptions(t)
		s := openService(t, opts)
		first := mustIngest(t, s, "photos", "a", "alpha")
		mustIngest(t, s, "photos", "z.txt", "zulu")
		_, err := s.appendAndApply(context.Background(), []journal.Record{{
			By: "test", Type: journal.TypePlacementSet, Folder: "photos", Path: "a/b", Item: first.Item,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return opts
	}

	t.Run("should open and link every non-conflicting placement when the index holds both a and a/b", func(t *testing.T) {
		opts := withConflictingJournal(t)
		if err := os.RemoveAll(filepath.Join(opts.Root, "views")); err != nil {
			t.Fatal(err)
		}
		s, err := Open(context.Background(), opts)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		for _, name := range []string{"a", "z.txt"} {
			if _, err := os.Lstat(filepath.Join(opts.Root, "views", "photos", name)); err != nil {
				t.Errorf("views/photos/%s missing: %v", name, err)
			}
		}
		events, err := s.Index().Events("", "views_sync_failed", 10)
		if err != nil {
			t.Fatal(err)
		}
		var surfaced bool
		for _, e := range events {
			surfaced = surfaced || (e.Type == "views_sync_failed" && e.Path == "photos/a/b")
		}
		if !surfaced {
			t.Fatalf("no views_sync_failed event for photos/a/b: %+v", events)
		}
	})

	t.Run("should rebuild the index and report the failed view when the journal holds both a and a/b", func(t *testing.T) {
		opts := withConflictingJournal(t)
		if err := os.RemoveAll(filepath.Join(opts.Root, "views")); err != nil {
			t.Fatal(err)
		}
		report, err := RebuildIndex(context.Background(), opts)
		if err != nil {
			t.Fatalf("RebuildIndex: %v", err)
		}
		if report.ViewsFailed != 1 || report.ViewsLinked < 1 {
			t.Fatalf("report = %+v", report)
		}
	})

	t.Run("should reject with ErrPathConflict and write nothing when an arrival nests under a placed file", func(t *testing.T) {
		s := openService(t, testOptions(t))
		mustIngest(t, s, "photos", "a", "alpha")
		before := s.core.j.LastSeq()
		_, err := s.Ingest(context.Background(), "test", memArrival("photos", "a/b", "bravo"))
		if !errors.Is(err, ingest.ErrPathConflict) {
			t.Fatalf("err = %v, want ErrPathConflict", err)
		}
		if s.core.j.LastSeq() != before {
			t.Fatal("a rejected arrival appended to the journal")
		}
	})

	t.Run("should reject with ErrPathConflict when an arrival is the directory of a placed file", func(t *testing.T) {
		s := openService(t, testOptions(t))
		mustIngest(t, s, "photos", "a/b", "alpha")
		_, err := s.Ingest(context.Background(), "test", memArrival("photos", "a", "bravo"))
		if !errors.Is(err, ingest.ErrPathConflict) {
			t.Fatalf("err = %v, want ErrPathConflict", err)
		}
	})
}

func TestLockChild(t *testing.T) {
	root := os.Getenv("COLDSTORE_LOCKCHILD_ROOT")
	if root == "" {
		t.Skip("only runs as a child of the lock test")
	}
	opts := Options{Root: root, Collections: []string{"photos"}, Logger: quietLogger()}
	_, err := Open(context.Background(), opts)
	if errors.Is(err, journal.ErrLocked) {
		fmt.Println("CHILD_GOT_ERRLOCKED")
		return
	}
	t.Fatalf("child Open = %v", err)
}

// faultyService opens a service whose every filesystem call goes through a Faulty.
func faultyService(t *testing.T) (*Service, *fsxtest.Faulty, Options, *sleepLog) {
	t.Helper()
	opts := testOptions(t)
	faulty := fsxtest.NewFaulty(fsx.OS())
	sleeps := &sleepLog{}
	opts.FS, opts.Sleep = faulty, sleeps.sleep
	s := openService(t, opts)
	faulty.Reset()
	return s, faulty, opts, sleeps
}

func failEveryStagingWrite(faulty *fsxtest.Faulty, opts Options, err error, times int) {
	// A fired fault stops the later ones from counting that call, so `times`
	// faults with Nth 1 fail the first `times` staging writes, one each.
	for n := 0; n < times; n++ {
		faulty.Inject(fsxtest.Fault{Op: fsxtest.OpWrite, PathContains: filepath.Join(opts.Root, "tmp") + string(filepath.Separator), Nth: 1, Err: err})
	}
}

func journalNamesAnyBlob(t *testing.T, s *Service) bool {
	t.Helper()
	named := false
	err := s.core.j.Replay(context.Background(), 0, func(rec journal.Record) error {
		if rec.Type == journal.TypeBlobAdded {
			named = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return named
}

func TestServiceFaults(t *testing.T) {
	t.Run("should return an error, record an ingest_failed event and leave the source untouched when Put fails with EIO", func(t *testing.T) {
		s, faulty, opts, _ := faultyService(t)
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "alpha"})
		before := snapshotTree(t, src)
		failEveryStagingWrite(faulty, opts, syscall.EIO, 1)

		source := ingest.ImportDir(src, "photos", nil)
		arrival := <-source.Arrivals(context.Background())
		_, err := s.Ingest(context.Background(), "test", arrival)
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("err = %v", err)
		}
		events, evErr := s.Index().Events("photos", "ingest_failed", 10)
		if evErr != nil || len(events) != 1 || events[0].Path != "a.txt" {
			t.Fatalf("events %+v err %v", events, evErr)
		}
		if after := snapshotTree(t, src); !equalLines(before, after) {
			t.Fatal("source changed")
		}
	})

	t.Run("should retry with 1s, 5s and 25s backoff and then report the file when Put keeps failing", func(t *testing.T) {
		s, faulty, opts, sleeps := faultyService(t)
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "alpha"})
		failEveryStagingWrite(faulty, opts, syscall.EIO, 4)

		status := runImport(t, s, "photos", src)
		want := []time.Duration{time.Second, 5 * time.Second, 25 * time.Second}
		got := sleeps.snapshot()
		if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("sleeps = %v", got)
		}
		if status.State != "failed" || !strings.Contains(status.Error, "run stopped after 1 files seen") || status.FailedCount != 1 || len(status.Failed) != 1 || status.Failed[0].Path != "a.txt" {
			t.Fatalf("status %+v", status)
		}
		events, err := s.Index().Events("photos", "ingest_failed", 10)
		if err != nil || len(events) != 4 {
			t.Fatalf("events %d err %v", len(events), err)
		}
	})

	t.Run("should succeed on a later attempt when Put fails only once", func(t *testing.T) {
		s, faulty, opts, sleeps := faultyService(t)
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "alpha"})
		failEveryStagingWrite(faulty, opts, syscall.EIO, 1)
		status := runImport(t, s, "photos", src)
		if status.FilesCreated != 1 || status.FailedCount != 0 || len(sleeps.snapshot()) != 1 {
			t.Fatalf("status %+v sleeps %v", status, sleeps.snapshot())
		}
	})

	t.Run("should end the run as failed when the disk is full after the last attempt", func(t *testing.T) {
		s, faulty, opts, _ := faultyService(t)
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "alpha", "b.txt": "bravo"})
		failEveryStagingWrite(faulty, opts, syscall.ENOSPC, 4)

		status := runImport(t, s, "photos", src)
		if status.State != "failed" || status.Error == "" || status.FailedCount != 1 {
			t.Fatalf("status %+v", status)
		}
		if status.FilesSeen != 1 {
			t.Fatalf("the run kept going after ENOSPC: %+v", status)
		}
	})

	t.Run("should not retry when the supplied hash does not match", func(t *testing.T) {
		s, _, _, sleeps := faultyService(t)
		wrong := store.Hash{1, 2, 3}
		a := memArrival("photos", "a.txt", "alpha")
		a.Want = &wrong
		_, err := s.ingestWithRetry(context.Background(), a)
		if !errors.Is(err, store.ErrHashMismatch) {
			t.Fatalf("err = %v", err)
		}
		if len(sleeps.snapshot()) != 0 {
			t.Fatalf("slept %v", sleeps.snapshot())
		}
		if journalNamesAnyBlob(t, s) {
			t.Fatal("a mismatched upload was journaled")
		}
	})

	t.Run("should name no blob in the journal when Put failed", func(t *testing.T) {
		s, faulty, opts, _ := faultyService(t)
		failEveryStagingWrite(faulty, opts, syscall.EIO, 1)
		if _, err := s.Ingest(context.Background(), "test", memArrival("photos", "a.txt", "alpha")); err == nil {
			t.Fatal("Ingest succeeded")
		}
		if journalNamesAnyBlob(t, s) {
			t.Fatal("the journal names a blob whose Put failed")
		}
		stats, err := s.Index().ArchiveStats()
		if err != nil || stats.BlobCount != 0 {
			t.Fatalf("stats %+v err %v", stats, err)
		}
	})

	t.Run("should fail the arrival when the file changes size while it is being read", func(t *testing.T) {
		s := openService(t, testOptions(t))
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "alpha"})
		arrival := <-ingest.ImportDir(src, "photos", nil).Arrivals(context.Background())
		reader, err := arrival.Open()
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(filepath.Join(src, "a.txt"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(" grew")
		f.Close()
		arrival.Open = func() (io.ReadCloser, error) { return reader, nil }

		_, err = s.Ingest(context.Background(), "import", arrival)
		if !errors.Is(err, ingest.ErrChangedWhileReading) {
			t.Fatalf("err = %v", err)
		}
		if journalNamesAnyBlob(t, s) {
			t.Fatal("a file that changed while being read was journaled")
		}
	})
}

// jpegMagic is enough bytes for the sniffer to call a file image/jpeg.
const jpegMagic = "\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01"

func TestDerive(t *testing.T) {
	ctx := context.Background()
	fakeRender := func(calls *[]string) func(context.Context, string, string, string, string) error {
		return func(_ context.Context, src, ext, detected, dst string) error {
			*calls = append(*calls, ext+"|"+detected)
			return os.WriteFile(dst, []byte("thumb"), 0o644)
		}
	}

	t.Run("should write the thumbnail under thumbnails/sha256 and record done when Render succeeds", func(t *testing.T) {
		opts := testOptions(t)
		var calls []string
		opts.Render = fakeRender(&calls)
		s := openService(t, opts)
		res := mustIngest(t, s, "photos", "pic.jpg", jpegMagic)
		if n := s.deriveOnce(ctx); n != 1 {
			t.Fatalf("deriveOnce = %d", n)
		}
		want := filepath.Join(opts.Root, "thumbnails", "sha256", res.Hash.String()[:2], res.Hash.String()[2:4], res.Hash.String()+".thumb.jpg")
		if body, err := os.ReadFile(want); err != nil || string(body) != "thumb" {
			t.Fatalf("thumbnail at %s: %q %v", want, body, err)
		}
		entry, _, _ := s.Index().Entry(res.EntryID)
		if entry.ThumbState != "done" || entry.DetectedType != "image/jpeg" || entry.ThumbError != "" {
			t.Fatalf("entry %+v", entry)
		}
		if len(calls) != 1 || calls[0] != ".jpg|image/jpeg" {
			t.Fatalf("render calls %v", calls)
		}
		if n := s.deriveOnce(ctx); n != 0 {
			t.Fatalf("a derived blob was offered again: %d", n)
		}
		tmpLeft, _ := filepath.Glob(filepath.Join(filepath.Dir(want), "*.tmp*"))
		if len(tmpLeft) != 0 {
			t.Fatalf("temp files left: %v", tmpLeft)
		}
	})

	t.Run("should record failed after three attempts when Render keeps failing", func(t *testing.T) {
		opts := testOptions(t)
		attempts := 0
		opts.Render = func(context.Context, string, string, string, string) error {
			attempts++
			return errors.New("decoder exploded")
		}
		s := openService(t, opts)
		res := mustIngest(t, s, "photos", "pic.jpg", jpegMagic)
		for s.deriveOnce(ctx) > 0 {
		}
		entry, _, _ := s.Index().Entry(res.EntryID)
		if attempts != 3 || entry.ThumbState != "failed" || entry.ThumbError != "decoder exploded" {
			t.Fatalf("attempts %d entry %+v", attempts, entry)
		}
	})

	t.Run("should classify an extension-less file by its bytes", func(t *testing.T) {
		opts := testOptions(t)
		var calls []string
		opts.Render = fakeRender(&calls)
		s := openService(t, opts)
		res := mustIngest(t, s, "photos", "scan", jpegMagic)
		s.deriveOnce(ctx)
		entry, _, _ := s.Index().Entry(res.EntryID)
		if entry.DetectedType != "image/jpeg" || entry.ThumbState != "done" {
			t.Fatalf("entry %+v", entry)
		}
		if len(calls) != 1 || calls[0] != "|image/jpeg" {
			t.Fatalf("render calls %v", calls)
		}
	})

	t.Run("should record a final state without an error when the file cannot be thumbnailed", func(t *testing.T) {
		opts := testOptions(t)
		var calls []string
		opts.Render = fakeRender(&calls)
		s := openService(t, opts)
		res := mustIngest(t, s, "photos", "notes.txt", "just some text")
		s.deriveOnce(ctx)
		entry, _, _ := s.Index().Entry(res.EntryID)
		if len(calls) != 0 || entry.ThumbState != "failed" || entry.ThumbError != "" || entry.DetectedType == "" {
			t.Fatalf("calls %v entry %+v", calls, entry)
		}
	})

	t.Run("should sniff but not render when Render is nil", func(t *testing.T) {
		s := openService(t, testOptions(t))
		res := mustIngest(t, s, "photos", "pic.jpg", jpegMagic)
		s.deriveOnce(ctx)
		entry, _, _ := s.Index().Entry(res.EntryID)
		if entry.DetectedType != "image/jpeg" || entry.ThumbState == "done" {
			t.Fatalf("entry %+v", entry)
		}
	})

	t.Run("should derive in the background when Start is called and an ingest wakes the worker", func(t *testing.T) {
		opts := testOptions(t)
		var calls []string
		opts.Render = fakeRender(&calls)
		s := openService(t, opts)
		s.Start(ctx)
		res := mustIngest(t, s, "photos", "pic.jpg", jpegMagic)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			entry, _, _ := s.Index().Entry(res.EntryID)
			if entry.ThumbState == "done" {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("the worker never derived the blob")
	})
}

func TestScrubService(t *testing.T) {
	waitScrub := func(t *testing.T, s *Service) ScrubStatus {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			status := s.ScrubStatus()
			if !status.Running && status.Last != nil {
				return status
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatal("scrub never finished")
		return ScrubStatus{}
	}

	t.Run("should report the pass when a scrub is started", func(t *testing.T) {
		s := openService(t, testOptions(t))
		mustIngest(t, s, "photos", "a.txt", "alpha")
		if err := s.ScrubStart(0); err != nil {
			t.Fatal(err)
		}
		status := waitScrub(t, s)
		if status.Last.Checked != 1 || len(status.Last.Findings) != 0 || status.Total != 1 {
			t.Fatalf("status %+v", status)
		}
	})

	t.Run("should refuse a second scrub while one is running", func(t *testing.T) {
		s := openService(t, testOptions(t))
		s.scrubMu.Lock()
		s.scrubbing = true
		s.scrubMu.Unlock()
		if err := s.ScrubStart(0); !errors.Is(err, ErrScrubRunning) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("should start a scheduled pass only when the last one is older than the interval", func(t *testing.T) {
		opts := testOptions(t)
		opts.ScrubInterval = time.Hour
		s := openService(t, opts)
		mustIngest(t, s, "photos", "a.txt", "alpha")

		s.scrubIfDue() // never verified: due
		first := waitScrub(t, s).Last
		s.scrubIfDue() // just finished: not due
		time.Sleep(50 * time.Millisecond)
		if s.ScrubStatus().Last != first {
			t.Fatal("a second pass started right after the first")
		}
		s.opts.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
		s.scrubIfDue()
		deadline := time.Now().Add(10 * time.Second)
		for s.ScrubStatus().Last == first && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		if s.ScrubStatus().Last == first {
			t.Fatal("no pass started when the last one was older than the interval")
		}
	})
}
