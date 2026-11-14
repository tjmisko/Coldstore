package scrub

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/fsx"
	"github.com/tjmisko/coldstore/internal/fsx/fsxtest"
	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/journal"
	"github.com/tjmisko/coldstore/internal/store"
)

var baseTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

type fixture struct {
	t        *testing.T
	root     string
	st       *store.Disk
	ix       *index.Index
	seq      uint64
	hashes   []store.Hash
	contents []string
}

// newFixture holds count named blobs of 100 bytes each.
func newFixture(t *testing.T, count int) *fixture {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(fsx.OS(), root)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := index.Open(filepath.Join(root, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	f := &fixture{t: t, root: root, st: st, ix: ix}
	for i := 0; i < count; i++ {
		content := fmt.Sprintf("blob-%02d-%s", i, strings.Repeat("x", 100))[:100]
		f.hashes = append(f.hashes, f.put(content, true))
		f.contents = append(f.contents, content)
	}
	return f
}

func (f *fixture) put(content string, named bool) store.Hash {
	f.t.Helper()
	h, size, err := f.st.Put(context.Background(), strings.NewReader(content), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	if !named {
		return h
	}
	f.seq++
	rec := journal.Record{Seq: f.seq, At: baseTime, By: "test", Type: journal.TypeBlobAdded, Hash: h.String(), Size: size}
	if err := f.ix.Apply(rec); err != nil {
		f.t.Fatal(err)
	}
	return h
}

func (f *fixture) flipByte(h store.Hash) {
	f.t.Helper()
	path := f.st.Path(h)
	must(f.t, os.Chmod(path, 0o644))
	data, err := os.ReadFile(path)
	must(f.t, err)
	data[10] ^= 0xff
	must(f.t, os.WriteFile(path, data, 0o644))
}

func (f *fixture) truncate(h store.Hash) {
	f.t.Helper()
	path := f.st.Path(h)
	must(f.t, os.Chmod(path, 0o644))
	must(f.t, os.Truncate(path, 40))
}

func (f *fixture) remove(h store.Hash) {
	f.t.Helper()
	must(f.t, os.Remove(f.st.Path(h)))
}

func (f *fixture) restore(h store.Hash, content string) {
	f.t.Helper()
	path := f.st.Path(h)
	must(f.t, os.MkdirAll(filepath.Dir(path), 0o755))
	if _, err := os.Stat(path); err == nil {
		must(f.t, os.Chmod(path, 0o644))
	}
	must(f.t, os.WriteFile(path, []byte(content), 0o444))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func noSleep(time.Duration) {}

func findingSet(report Report) map[string]string {
	out := map[string]string{}
	for _, finding := range report.Findings {
		out[finding.Hash.String()] = finding.Kind + ":" + finding.Reason
	}
	return out
}

// afterGrace is a clock far enough ahead that every blob file is past OrphanGrace.
func afterGrace() time.Time { return time.Now().Add(OrphanGrace + time.Minute) }

func damaged(t *testing.T) (*fixture, Report) {
	f := newFixture(t, 20)
	f.flipByte(f.hashes[0]) // A
	f.truncate(f.hashes[1]) // B
	f.remove(f.hashes[2])   // C
	f.put("unnamed blob D", false)
	report, err := Run(context.Background(), f.st, f.ix, Options{Sleep: noSleep, Now: afterGrace})
	must(t, err)
	return f, report
}

func TestRunCorruption(t *testing.T) {
	t.Run("should report exactly A, B and C as bad and D as orphan", func(t *testing.T) {
		f, report := damaged(t)
		dHash := store.Hash{}
		for h, v := range findingSet(report) {
			if v == "orphan:" {
				parsed, _ := store.ParseHash(h)
				dHash = parsed
			}
		}
		want := map[string]string{
			f.hashes[0].String(): "bad:hash_mismatch",
			f.hashes[1].String(): "bad:truncated",
			f.hashes[2].String(): "bad:missing",
			dHash.String():       "orphan:",
		}
		got := findingSet(report)
		if len(got) != len(want) {
			t.Fatalf("findings = %v, want %v", got, want)
		}
		for h, v := range want {
			if got[h] != v {
				t.Fatalf("finding for %s = %q, want %q (all: %v)", h, got[h], v, got)
			}
		}
	})

	t.Run("should flag nothing else when the other blobs are intact", func(t *testing.T) {
		_, report := damaged(t)
		if len(report.Findings) != 4 {
			t.Fatalf("%d findings: %v", len(report.Findings), report.Findings)
		}
		if report.Checked != 21 {
			t.Fatalf("Checked = %d, want 21 (20 named + 1 orphan)", report.Checked)
		}
		if len(report.Stray) != 0 {
			t.Fatalf("Stray = %v", report.Stray)
		}
	})

	t.Run("should mark only A, B and C bad in the index", func(t *testing.T) {
		f, _ := damaged(t)
		for i, h := range f.hashes {
			info, ok, err := f.ix.Blob(h)
			must(t, err)
			if !ok {
				t.Fatalf("blob %d vanished from the index", i)
			}
			if info.Bad != (i < 3) {
				t.Fatalf("blob %d bad = %v", i, info.Bad)
			}
			if info.VerifiedAt == nil {
				t.Fatalf("blob %d was not stamped verified", i)
			}
		}
		stats, _ := f.ix.ArchiveStats()
		if stats.BadBlobCount != 3 {
			t.Fatalf("BadBlobCount = %d", stats.BadBlobCount)
		}
	})

	t.Run("should record one blob_bad event per damaged blob", func(t *testing.T) {
		f, _ := damaged(t)
		events, err := f.ix.Events("", "blob_bad", 100)
		must(t, err)
		if len(events) != 3 {
			t.Fatalf("%d blob_bad events: %+v", len(events), events)
		}
		orphans, _ := f.ix.Events("", "blob_orphan", 100)
		if len(orphans) != 1 {
			t.Fatalf("%d blob_orphan events", len(orphans))
		}
	})

	t.Run("should never delete a blob when it is an orphan or damaged", func(t *testing.T) {
		f, _ := damaged(t)
		count := 0
		must(t, f.st.Walk(context.Background(), func(store.Hash, int64) error { count++; return nil }))
		if count != 20 { // 21 minus the deleted C
			t.Fatalf("%d blobs remain, want 20", count)
		}
	})

	t.Run("should clear bad when the blob is repaired and scrubbed again", func(t *testing.T) {
		f, _ := damaged(t)
		f.restore(f.hashes[0], f.contents[0])
		f.restore(f.hashes[1], f.contents[1])
		f.restore(f.hashes[2], f.contents[2])
		report, err := Run(context.Background(), f.st, f.ix, Options{Sleep: noSleep, Now: afterGrace})
		must(t, err)
		got := findingSet(report)
		if len(got) != 1 {
			t.Fatalf("findings after repair = %v, want only the orphan", got)
		}
		stats, _ := f.ix.ArchiveStats()
		if stats.BadBlobCount != 0 {
			t.Fatalf("BadBlobCount = %d after repair", stats.BadBlobCount)
		}
		info, _, _ := f.ix.Blob(f.hashes[0])
		if info.Bad || info.BadReason != "" {
			t.Fatalf("info = %+v", info)
		}
	})

	t.Run("should report a stray file when blobs holds a file that is not hash-named", func(t *testing.T) {
		f := newFixture(t, 2)
		stray := filepath.Join(f.root, "blobs", "sha256", "README")
		must(t, os.WriteFile(stray, []byte("hi"), 0o644))
		report, err := Run(context.Background(), f.st, f.ix, Options{Sleep: noSleep})
		must(t, err)
		if len(report.Stray) != 1 || report.Stray[0] != stray {
			t.Fatalf("Stray = %v", report.Stray)
		}
		events, _ := f.ix.Events("", "blob_stray", 10)
		if len(events) != 1 {
			t.Fatalf("%d blob_stray events", len(events))
		}
	})

	t.Run("should report nothing when every blob is intact", func(t *testing.T) {
		f := newFixture(t, 5)
		report, err := Run(context.Background(), f.st, f.ix, Options{Sleep: noSleep})
		must(t, err)
		if len(report.Findings) != 0 || report.Checked != 5 || report.CheckedBytes != 500 {
			t.Fatalf("report = %+v", report)
		}
	})

	t.Run("should handle an empty archive when nothing is stored", func(t *testing.T) {
		f := newFixture(t, 0)
		report, err := Run(context.Background(), f.st, f.ix, Options{Sleep: noSleep})
		must(t, err)
		if report.Checked != 0 {
			t.Fatalf("report = %+v", report)
		}
	})
}

func TestRunOrderAndCancel(t *testing.T) {
	t.Run("should verify never-verified blobs before older ones", func(t *testing.T) {
		f := newFixture(t, 20)
		// Blobs 10..19 were verified long ago; blob 10 the longest ago.
		for i := 10; i < 20; i++ {
			must(t, f.ix.BlobVerifiedSet(f.hashes[i], time.Unix(int64(1000+i), 0)))
		}
		var ticks atomic.Int64
		now := func() time.Time { return time.Date(2030, 1, 1, 0, 0, int(ticks.Add(1)), 0, time.UTC) }
		_, err := Run(context.Background(), f.st, f.ix, Options{Now: now, Sleep: noSleep})
		must(t, err)

		stamp := func(i int) time.Time {
			info, _, _ := f.ix.Blob(f.hashes[i])
			if info.VerifiedAt == nil {
				t.Fatalf("blob %d not verified", i)
			}
			return *info.VerifiedAt
		}
		for never := 0; never < 10; never++ {
			for old := 10; old < 20; old++ {
				if !stamp(never).Before(stamp(old)) {
					t.Fatalf("never-verified blob %d was checked after previously verified blob %d", never, old)
				}
			}
		}
		for old := 11; old < 20; old++ {
			if !stamp(old - 1).Before(stamp(old)) {
				t.Fatalf("blob %d (older) was checked after blob %d", old-1, old)
			}
		}
	})

	t.Run("should stop with the partial report when the context is cancelled", func(t *testing.T) {
		f := newFixture(t, 20)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		opts := Options{Sleep: noSleep, Progress: func(checked int, _ int64) {
			if checked == 3 {
				cancel()
			}
		}}
		report, err := Run(ctx, f.st, f.ix, opts)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if report.Checked != 3 || report.FinishedAt.IsZero() {
			t.Fatalf("report = %+v", report)
		}
	})

	t.Run("should resume from the oldest verified_at when a run is repeated after a cancel", func(t *testing.T) {
		f := newFixture(t, 10)
		ctx, cancel := context.WithCancel(context.Background())
		_, err := Run(ctx, f.st, f.ix, Options{Sleep: noSleep, Progress: func(checked int, _ int64) {
			if checked == 4 {
				cancel()
			}
		}})
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
		var seen int
		report, err := Run(context.Background(), f.st, f.ix, Options{Sleep: noSleep, Progress: func(int, int64) { seen++ }})
		must(t, err)
		if report.Checked != 10 {
			t.Fatalf("second run checked %d, want all 10", report.Checked)
		}
	})
}

func TestRunRate(t *testing.T) {
	t.Run("should sleep about N over R in total when the rate is limited", func(t *testing.T) {
		f := newFixture(t, 20) // 20 blobs x 100 bytes = 2000 bytes
		var slept time.Duration
		const rate = 100 // bytes per second -> 20 s
		_, err := Run(context.Background(), f.st, f.ix, Options{
			RateBytesPerSec: rate,
			Sleep:           func(d time.Duration) { slept += d },
		})
		must(t, err)
		want := 20 * time.Second
		if slept < want*9/10 || slept > want*11/10 {
			t.Fatalf("slept %v, want within 10%% of %v", slept, want)
		}
	})

	t.Run("should not sleep when the rate is zero", func(t *testing.T) {
		f := newFixture(t, 5)
		var slept time.Duration
		_, err := Run(context.Background(), f.st, f.ix, Options{Sleep: func(d time.Duration) { slept += d }})
		must(t, err)
		if slept != 0 {
			t.Fatalf("slept %v", slept)
		}
	})

	t.Run("should not charge the rate when a blob is missing", func(t *testing.T) {
		f := newFixture(t, 2)
		f.remove(f.hashes[0])
		var slept time.Duration
		_, err := Run(context.Background(), f.st, f.ix, Options{
			RateBytesPerSec: 100,
			Sleep:           func(d time.Duration) { slept += d },
		})
		must(t, err)
		if slept > 1100*time.Millisecond {
			t.Fatalf("slept %v for one readable 100 byte blob at 100 B/s", slept)
		}
	})
}

func TestRunOrphanGrace(t *testing.T) {
	t.Run("should not report an orphan when its file is younger than the grace window", func(t *testing.T) {
		f := newFixture(t, 2)
		f.put("linked by an ingest whose journal append is pending", false)
		report, err := Run(context.Background(), f.st, f.ix, Options{Sleep: noSleep})
		must(t, err)
		if len(report.Findings) != 0 {
			t.Fatalf("findings = %v, want none for an in-flight ingest", report.Findings)
		}
		if events, _ := f.ix.Events("", "blob_orphan", 10); len(events) != 0 {
			t.Fatalf("%d blob_orphan events for an in-flight ingest", len(events))
		}
	})

	t.Run("should report an orphan when its file is older than the grace window", func(t *testing.T) {
		f := newFixture(t, 2)
		h := f.put("abandoned unnamed blob", false)
		report, err := Run(context.Background(), f.st, f.ix, Options{Sleep: noSleep, Now: afterGrace})
		must(t, err)
		if got := findingSet(report); len(got) != 1 || got[h.String()] != "orphan:" {
			t.Fatalf("findings = %v", got)
		}
	})
}

func TestRunOrdinaryErrors(t *testing.T) {
	faultyStore := func(f *fixture, h store.Hash, cause error) *store.Disk {
		faulty := fsxtest.NewFaulty(fsx.OS())
		faulty.Inject(fsxtest.Fault{Op: fsxtest.OpOpen, PathContains: h.String(), Nth: 1, Err: &fs.PathError{Op: "open", Err: cause}})
		st, err := store.Open(faulty, f.root)
		must(t, err)
		return st
	}

	t.Run("should not flag the blob bad or raise blob_bad when open fails with EMFILE", func(t *testing.T) {
		f := newFixture(t, 5)
		st := faultyStore(f, f.hashes[2], syscall.EMFILE)
		report, err := Run(context.Background(), st, f.ix, Options{Sleep: noSleep})
		must(t, err)
		if len(report.Findings) != 0 {
			t.Fatalf("findings = %v, want none", report.Findings)
		}
		info, _, _ := f.ix.Blob(f.hashes[2])
		if info.Bad {
			t.Fatal("a transient error flagged the blob bad")
		}
		if events, _ := f.ix.Events("", "blob_bad", 10); len(events) != 0 {
			t.Fatalf("%d blob_bad events", len(events))
		}
		if len(report.Unverified) != 1 || report.Unverified[0].Hash != f.hashes[2] {
			t.Fatalf("Unverified = %+v", report.Unverified)
		}
	})

	t.Run("should still check every other blob when one fails with an ordinary error", func(t *testing.T) {
		f := newFixture(t, 5)
		st := faultyStore(f, f.hashes[0], syscall.EACCES)
		report, err := Run(context.Background(), st, f.ix, Options{Sleep: noSleep})
		must(t, err)
		if report.Checked != 4 {
			t.Fatalf("Checked = %d, want the 4 other blobs", report.Checked)
		}
	})

	t.Run("should leave the blob unstamped and retry it when the next pass runs", func(t *testing.T) {
		f := newFixture(t, 3)
		st := faultyStore(f, f.hashes[1], syscall.EMFILE)
		_, err := Run(context.Background(), st, f.ix, Options{Sleep: noSleep})
		must(t, err)
		info, _, _ := f.ix.Blob(f.hashes[1])
		if info.VerifiedAt != nil {
			t.Fatal("an unverified blob was stamped verified")
		}
		report, err := Run(context.Background(), f.st, f.ix, Options{Sleep: noSleep})
		must(t, err)
		if report.Checked != 3 || len(report.Unverified) != 0 {
			t.Fatalf("retry report = %+v", report)
		}
		info, _, _ = f.ix.Blob(f.hashes[1])
		if info.VerifiedAt == nil {
			t.Fatal("the retried blob was not stamped")
		}
	})

	t.Run("should still flag the blob bad when the device reports EIO", func(t *testing.T) {
		f := newFixture(t, 3)
		st := faultyStore(f, f.hashes[1], syscall.EIO)
		report, err := Run(context.Background(), st, f.ix, Options{Sleep: noSleep})
		must(t, err)
		if got := findingSet(report); got[f.hashes[1].String()] != "bad:unreadable" {
			t.Fatalf("findings = %v", got)
		}
	})
}

func TestRunManyOrdinaryErrors(t *testing.T) {
	const blobs = pageBlobs + 10
	inject := func(f *fixture, count int) *store.Disk {
		faulty := fsxtest.NewFaulty(fsx.OS())
		for _, h := range f.hashes[:count] {
			faulty.Inject(fsxtest.Fault{Op: fsxtest.OpOpen, PathContains: h.String(), Nth: 1, Err: &fs.PathError{Op: "open", Err: syscall.EMFILE}})
		}
		st, err := store.Open(faulty, f.root)
		must(t, err)
		return st
	}

	t.Run("should attempt every named blob when more than a page of them fail with ordinary errors", func(t *testing.T) {
		f := newFixture(t, 0)
		for i := 0; i < blobs; i++ {
			f.hashes = append(f.hashes, f.put(fmt.Sprintf("distinct-blob-%05d", i), true))
		}
		failing := pageBlobs + 5
		st := inject(f, failing)
		report, err := Run(context.Background(), st, f.ix, Options{Sleep: noSleep})
		must(t, err)
		if report.Checked != blobs-failing || len(report.Unverified) != failing {
			t.Fatalf("checked=%d unverified=%d, want %d and %d", report.Checked, len(report.Unverified), blobs-failing, failing)
		}
		if report.Checked+len(report.Unverified) != blobs {
			t.Fatal("some named blob was neither checked nor listed as unverified")
		}
	})
}
