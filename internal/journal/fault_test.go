package journal

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/tjmisko/coldstore/internal/fsx"
	"github.com/tjmisko/coldstore/internal/fsx/fsxtest"
)

// faultScenario opens a log on a Faulty FS, appends one seed record, then
// returns the log, the FS and the number of calls made so far. rotate makes the
// next Append start a new segment.
func faultScenario(t *testing.T, rotate bool) (*Log, *fsxtest.Faulty, string, int) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "j")
	faulty := fsxtest.NewFaulty(fsx.OS())
	opts := Options{}
	if rotate {
		opts.SegmentBytes = 1
	}
	l, err := Open(bg, faulty, dir, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := l.Append(bg, folderRec("seed")); err != nil {
		t.Fatalf("seed Append: %v", err)
	}
	return l, faulty, dir, len(faulty.Calls())
}

func appendTrio(l *Log) error {
	_, err := l.Append(bg, folderRec("a"), folderRec("b"), folderRec("c"))
	return err
}

// recordsOnDisk reopens dir with the real filesystem and returns the folder
// names replayed.
func recordsOnDisk(t *testing.T, dir string) []string {
	t.Helper()
	l, err := Open(bg, fsx.OS(), dir, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l.Close()
	var names []string
	for _, rec := range replayAll(t, l, 0) {
		names = append(names, rec.Folder)
	}
	return names
}

func assertAllOrNothing(t *testing.T, got []string) {
	t.Helper()
	switch len(got) {
	case 1, 4: // the seed alone, or the seed and the whole batch
		return
	}
	t.Fatalf("partial batch visible after reopen: %v", got)
}

func TestAppendFaults(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		name := "a plain append"
		if rotate {
			name = "an append that rotates"
		}
		l, faulty, _, base := faultScenario(t, rotate)
		if err := appendTrio(l); err != nil {
			t.Fatal(err)
		}
		trace := faulty.Calls()[base:]
		_ = l.Close()
		if len(trace) == 0 {
			t.Fatal("clean append made no filesystem calls")
		}

		for i, call := range trace {
			t.Run(fmt.Sprintf("should return an error and expose no partial batch on replay when %s hits call %d (%s %s) of %s", call.Op, i+1, call.Op, filepath.Base(call.Path), name), func(t *testing.T) {
				l, faulty, dir, base := faultScenario(t, rotate)
				faulty.FailCall(base+i+1, nil)
				err := appendTrio(l)
				_ = l.Close()
				if err == nil && call.Op != fsxtest.OpClose {
					t.Fatal("Append succeeded although a call failed")
				}
				got := recordsOnDisk(t, dir)
				assertAllOrNothing(t, got)
				if err == nil && len(got) != 4 {
					t.Fatalf("Append reported success but replay has %v", got)
				}
			})
		}

		for _, short := range []struct {
			label string
			err   error
		}{{"a silent short write", nil}, {"a short write with an error", errors.New("disk full")}} {
			t.Run(fmt.Sprintf("should return an error and expose no partial batch on replay when %s hits %s", short.label, name), func(t *testing.T) {
				l, faulty, dir, _ := faultScenario(t, rotate)
				faulty.Inject(fsxtest.Fault{Op: fsxtest.OpWrite, PathContains: segmentSuffix, Nth: 1, Err: short.err, Short: true})
				if err := appendTrio(l); err == nil {
					t.Fatal("Append succeeded although the write was short")
				}
				_ = l.Close()
				got := recordsOnDisk(t, dir)
				assertAllOrNothing(t, got)
			})
		}
	}

	t.Run("should refuse further appends until reopened when a write fails", func(t *testing.T) {
		l, faulty, dir, _ := faultScenario(t, false)
		boom := errors.New("write exploded")
		faulty.Inject(fsxtest.Fault{Op: fsxtest.OpWrite, PathContains: segmentSuffix, Nth: 1, Err: boom})
		first := appendTrio(l)
		if !errors.Is(first, boom) {
			t.Fatalf("first Append = %v", first)
		}
		if l.LastSeq() != 1 {
			t.Fatalf("LastSeq advanced to %d on failure", l.LastSeq())
		}
		second := appendTrio(l) // the fault has fired and is spent; the log must still refuse
		if !errors.Is(second, boom) {
			t.Fatalf("second Append = %v, want the first error", second)
		}
		_ = l.Close()

		reopened := openLog(t, dir, Options{})
		if seq := appendOne(t, reopened, "after"); seq != 2 {
			t.Fatalf("append after reopen got seq %d", seq)
		}
	})
	t.Run("should refuse further appends until reopened when an fsync fails", func(t *testing.T) {
		l, faulty, _, _ := faultScenario(t, false)
		faulty.Inject(fsxtest.Fault{Op: fsxtest.OpSync, PathContains: segmentSuffix, Nth: 1})
		if err := appendTrio(l); err == nil {
			t.Fatal("Append succeeded although fsync failed")
		}
		if err := appendTrio(l); err == nil {
			t.Fatal("poisoned log accepted an append")
		}
		if l.LastSeq() != 1 {
			t.Fatalf("LastSeq advanced to %d on failure", l.LastSeq())
		}
	})
	t.Run("should fail every queued caller when the shared fsync fails", func(t *testing.T) {
		l, faulty, _, _ := faultScenario(t, false)
		faulty.Inject(fsxtest.Fault{Op: fsxtest.OpSync, PathContains: segmentSuffix, Nth: 1})
		const callers = 8
		errs := make(chan error, callers)
		for i := 0; i < callers; i++ {
			go func() { errs <- appendTrio(l) }()
		}
		failed := 0
		for i := 0; i < callers; i++ {
			if err := <-errs; err != nil {
				failed++
			}
		}
		if failed == 0 {
			t.Fatal("no caller saw the fsync failure")
		}
		if l.LastSeq()%3 != 1 {
			t.Fatalf("LastSeq %d splits a batch", l.LastSeq())
		}
	})
}

func TestOpenFaults(t *testing.T) {
	// Count the calls of a clean Open on a torn journal, then fail each one.
	prepare := func(t *testing.T) string {
		dir := filepath.Join(t.TempDir(), "j")
		l := openLog(t, dir, Options{})
		appendOne(t, l, "a")
		appendOne(t, l, "b")
		_ = l.Close()
		full := readFile(t, segmentPath(dir, 1))
		writeFile(t, segmentPath(dir, 1), full[:len(full)-9])
		return dir
	}
	clean := fsxtest.NewFaulty(fsx.OS())
	l, err := Open(bg, clean, prepare(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	calls := len(clean.Calls())
	for i := 1; i <= calls; i++ {
		t.Run(fmt.Sprintf("should leave the journal recoverable when call %d of Open fails", i), func(t *testing.T) {
			dir := prepare(t)
			faulty := fsxtest.NewFaulty(fsx.OS())
			faulty.FailCall(i, nil)
			if l, err := Open(bg, faulty, dir, Options{}); err == nil {
				_ = l.Close() // a failing Close call is allowed to be swallowed
			}
			got := recordsOnDisk(t, dir)
			if len(got) != 1 {
				t.Fatalf("after a failed Open and a clean one, replay = %v", got)
			}
		})
	}
}
