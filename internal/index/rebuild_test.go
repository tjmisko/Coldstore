package index

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/fsx"
	"github.com/tjmisko/coldstore/internal/journal"
)

func journalWith(t *testing.T, s *script) *journal.Log {
	t.Helper()
	ctx := context.Background()
	log, err := journal.Open(ctx, fsx.OS(), filepath.Join(t.TempDir(), "journal"), journal.Options{})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	// One Append per record keeps each record its own batch, as Replay expects.
	for _, rec := range s.recs {
		rec.Seq, rec.Batch = 0, 0
		if _, err := log.Append(ctx, rec); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	return log
}

func TestRebuild(t *testing.T) {
	ctx := context.Background()

	t.Run("should produce an identical dump when the index is rebuilt from the journal", func(t *testing.T) {
		s := fullScript()
		log := journalWith(t, s)

		incremental := openTemp(t)
		if err := log.Replay(ctx, 0, incremental.Apply); err != nil {
			t.Fatalf("Replay: %v", err)
		}
		rebuilt, err := Rebuild(ctx, log, filepath.Join(t.TempDir(), "rebuilt.db"))
		if err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
		defer rebuilt.Close()
		if got, want := dumpOf(t, rebuilt), dumpOf(t, incremental); got != want {
			t.Fatalf("rebuilt dump differs:\n%s\n--- want\n%s", got, want)
		}
		if rebuilt.AppliedSeq() != uint64(len(s.recs)) {
			t.Fatalf("AppliedSeq = %d, want %d", rebuilt.AppliedSeq(), len(s.recs))
		}
	})

	t.Run("should rebuild in several batches when the journal has more than 1000 records", func(t *testing.T) {
		s := &script{}
		s.folder("P", "collection")
		for i := 0; i < 2*rebuildBatchRecords+5; i++ {
			s.blob(hashOf(string(rune('A'+i%26))+time.Duration(i).String()), int64(i))
		}
		log := journalWith(t, s)
		rebuilt, err := Rebuild(ctx, log, filepath.Join(t.TempDir(), "index.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer rebuilt.Close()
		if rebuilt.AppliedSeq() != uint64(len(s.recs)) {
			t.Fatalf("AppliedSeq = %d, want %d", rebuilt.AppliedSeq(), len(s.recs))
		}
	})

	t.Run("should replace the old file when the path already holds an index", func(t *testing.T) {
		log := journalWith(t, fullScript())
		path := filepath.Join(t.TempDir(), "index.db")
		old, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		mustApplyAll(t, old, (&script{}).folder("Other", "collection"))
		old.Close()
		rebuilt, err := Rebuild(ctx, log, path)
		if err != nil {
			t.Fatal(err)
		}
		defer rebuilt.Close()
		if _, ok, _ := rebuilt.FolderByName("Other"); ok {
			t.Fatal("rebuilt index kept a row that is not in the journal")
		}
	})

	t.Run("should lose operational columns and keep every journal-derived row when rebuilt", func(t *testing.T) {
		s := fullScript()
		log := journalWith(t, s)
		path := filepath.Join(t.TempDir(), "index.db")
		ix, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Replay(ctx, 0, ix.Apply); err != nil {
			t.Fatal(err)
		}
		a := hashOf("a")
		must(t, ix.BlobVerifiedSet(a, baseTime))
		must(t, ix.BlobBadSet(a, true, "missing"))
		must(t, ix.BlobDerivedSet(a, "image/jpeg", "done", ""))
		must(t, ix.EventRecord(baseTime, "blob_bad", "", "", "x"))
		before := dumpOf(t, ix)
		ix.Close()

		rebuilt, err := Rebuild(ctx, log, path)
		if err != nil {
			t.Fatal(err)
		}
		defer rebuilt.Close()
		if dumpOf(t, rebuilt) != before {
			t.Fatal("journal-derived rows changed")
		}
		info, _, _ := rebuilt.Blob(a)
		if info.Bad || info.VerifiedAt != nil {
			t.Fatalf("operational state survived: %+v", info)
		}
		events, _ := rebuilt.Events("", "", 10)
		if len(events) != 0 {
			t.Fatalf("events survived: %v", events)
		}
		todo, _ := rebuilt.BlobsToDerive(10)
		if len(todo) == 0 {
			t.Fatal("derive state survived")
		}
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func closedIndexFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.db")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustApplyAll(t, ix, fullScript())
	// Grow the file past a few pages so truncation lands inside real content.
	for i := 0; i < 300; i++ {
		must(t, ix.EventRecord(baseTime, "blob_bad", "P", "p", string(make([]byte, 200))))
	}
	must(t, ix.Close())
	return path
}

func TestOpenStale(t *testing.T) {
	t.Run("should return ErrStale when index.db is overwritten with garbage", func(t *testing.T) {
		path := closedIndexFile(t)
		garbage := make([]byte, 8192)
		for i := range garbage {
			garbage[i] = byte(i*7 + 3)
		}
		must(t, os.WriteFile(path, garbage, 0o600))
		if _, err := Open(path); !errors.Is(err, ErrStale) {
			t.Fatalf("Open error = %v, want ErrStale", err)
		}
	})

	t.Run("should return ErrStale when index.db is truncated to half", func(t *testing.T) {
		path := closedIndexFile(t)
		info, err := os.Stat(path)
		must(t, err)
		must(t, os.Truncate(path, info.Size()/2))
		if _, err := Open(path); !errors.Is(err, ErrStale) {
			t.Fatalf("Open error = %v, want ErrStale", err)
		}
	})

	t.Run("should return ErrFormatNewer when user_version is above the supported format", func(t *testing.T) {
		path := closedIndexFile(t)
		raw, err := sql.Open("sqlite3", path)
		must(t, err)
		_, err = raw.Exec("PRAGMA user_version = 99")
		must(t, err)
		must(t, raw.Close())
		if _, err := Open(path); !errors.Is(err, ErrFormatNewer) {
			t.Fatalf("Open error = %v, want ErrFormatNewer", err)
		}
	})

	t.Run("should return ErrStale when user_version is below the supported format", func(t *testing.T) {
		path := closedIndexFile(t)
		raw, err := sql.Open("sqlite3", path)
		must(t, err)
		_, err = raw.Exec("PRAGMA user_version = 0")
		must(t, err)
		must(t, raw.Close())
		if _, err := Open(path); !errors.Is(err, ErrStale) {
			t.Fatalf("Open error = %v, want ErrStale", err)
		}
	})
}
