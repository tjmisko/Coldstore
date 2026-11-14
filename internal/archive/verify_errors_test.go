package archive

import (
	"context"
	"io/fs"
	"syscall"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/fsx/fsxtest"
	"github.com/tjmisko/coldstore/internal/store"
)

func failBlobOpen(faulty *fsxtest.Faulty, s *Service, h store.Hash, cause error) {
	faulty.Inject(fsxtest.Fault{Op: fsxtest.OpOpen, PathContains: s.Store().Path(h), Nth: 1, Err: &fs.PathError{Op: "open", Err: cause}})
}

func TestReceiptTransientErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("should report status error and mark nothing bad when verification fails with EMFILE", func(t *testing.T) {
		s, faulty, _, _ := faultyService(t)
		a := mustIngest(t, s, "photos", "a.txt", "alpha")
		b := mustIngest(t, s, "photos", "b.txt", "bravo")
		faulty.Reset()
		failBlobOpen(faulty, s, a.Hash, syscall.EMFILE)

		lines, err := s.Receipt(ctx, "photos", []store.Hash{a.Hash, b.Hash})
		if err != nil {
			t.Fatalf("a transient error failed the whole receipt: %v", err)
		}
		if lines[0].Status != ReceiptError || lines[0].Verified || lines[0].Error == "" || !lines[0].Held {
			t.Fatalf("line 0 = %+v", lines[0])
		}
		if lines[1].Status != ReceiptVerified || !lines[1].Verified {
			t.Fatalf("line 1 = %+v, want the next hash still verified", lines[1])
		}
		info, _, err := s.Index().Blob(a.Hash)
		if err != nil || info.Bad {
			t.Fatalf("blob info %+v err %v: a transient error marked it bad", info, err)
		}
		if events, _ := s.Index().Events("", "blob_bad", 10); len(events) != 0 {
			t.Fatalf("%d blob_bad events for a transient error", len(events))
		}
	})
}

func TestScrubSchedule(t *testing.T) {
	scrubOptions := func(t *testing.T) Options {
		opts := testOptions(t)
		opts.ScrubInterval = time.Hour
		return opts
	}
	passStarted := func(s *Service) bool {
		time.Sleep(100 * time.Millisecond)
		status := s.ScrubStatus()
		return status.Running || status.Last != nil
	}

	t.Run("should not start a scheduled pass while an import run is active", func(t *testing.T) {
		s := openService(t, scrubOptions(t))
		mustIngest(t, s, "photos", "a.txt", "alpha")
		s.importMu.Lock()
		s.importBusy = true
		s.importMu.Unlock()
		s.scrubIfDue()
		if passStarted(s) {
			t.Fatal("a scrub pass started during an import")
		}
	})

	t.Run("should not start a pass right after a restart when the last pass completed recently", func(t *testing.T) {
		s := openService(t, scrubOptions(t))
		mustIngest(t, s, "photos", "a.txt", "alpha") // never verified
		if err := s.Index().EventRecord(s.now().Add(-time.Minute), scrubDoneEvent, "", "", "earlier process"); err != nil {
			t.Fatal(err)
		}
		s.scrubIfDue()
		if passStarted(s) {
			t.Fatal("a scrub pass started although one completed a minute ago")
		}
	})

	t.Run("should start a pass when the last completed pass is older than the interval", func(t *testing.T) {
		s := openService(t, scrubOptions(t))
		mustIngest(t, s, "photos", "a.txt", "alpha")
		if err := s.Index().EventRecord(s.now().Add(-2*time.Hour), scrubDoneEvent, "", "", "earlier process"); err != nil {
			t.Fatal(err)
		}
		s.scrubIfDue()
		if !passStarted(s) {
			t.Fatal("no pass started although the last one is two hours old")
		}
	})

	t.Run("should record scrub_done when a pass completes", func(t *testing.T) {
		s := openService(t, scrubOptions(t))
		mustIngest(t, s, "photos", "a.txt", "alpha")
		if err := s.ScrubStart(0); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for s.ScrubStatus().Last == nil && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		events, err := s.Index().Events("", scrubDoneEvent, 5)
		if err != nil || len(events) != 1 {
			t.Fatalf("events %+v err %v", events, err)
		}
	})
}
