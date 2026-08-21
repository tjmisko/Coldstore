package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjmisko/coldstore/internal/catalog"
)

// Defect 11. The quota tick's pre-check re-stats a candidate but compares only
// its MTIME, so a row whose catalogued SIZE disagrees with the disk is picked
// again on every single tick and refused by the mover every single time. An
// undeduped report writes one events row per tick, forever, in the same table
// the Activity list reads -- the cold_storage_low flood in a different
// costume.
//
// Asserting that an event merely EXISTS cannot tell LogEventDeduped from
// LogEvent, so this asserts the count stays at one across repeated ticks.
func TestEnforcementShouldWriteOneSourceChangedReportWhenTheSameFileStaysOutOfDateAcrossTicks(t *testing.T) {
	syncDir, cfg, cat, dbPath := setupWedgeEnv(t)
	cfg.Folders[0].BudgetBytes = 1000

	seedSyncedFile(t, cat, syncDir, "grown.jpg", 4000)
	ageFiles(t, cat, syncDir, "grown.jpg")

	// The file on disk is a different size from the row, and its mtime is
	// still old -- a row the last walk wrote before the file was replaced.
	// The tick's mtime pre-check waves it through and only the mover notices.
	full := filepath.Join(syncDir, "grown.jpg")
	if err := os.WriteFile(full, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := cat.GetFile("TestFolder", "grown.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, rec.FileMtime, rec.FileMtime); err != nil {
		t.Fatal(err)
	}

	d := newTestDaemon(t, cfg, cat, testLogger())
	declarePolicy(t, cfg, cat)

	for range 5 {
		d.EnforceQuotas()
	}

	after, err := cat.GetFile("TestFolder", "grown.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != catalog.StatusSynced {
		t.Fatalf("status = %q, want synced: the mover must refuse the move, not perform it",
			after.Status)
	}
	if n := countEvents(t, dbPath, "source_changed"); n != 1 {
		t.Errorf("source_changed events = %d after 5 ticks, want exactly 1: a condition "+
			"that persists must not write one row per tick", n)
	}
}

// Deduping must not make the report unreachable: the very first tick over a
// file whose row is out of date still has to say so.
func TestEnforcementShouldReportTheDisagreementWhenAFileFirstFailsTheSourceCheck(t *testing.T) {
	syncDir, cfg, cat, dbPath := setupWedgeEnv(t)
	cfg.Folders[0].BudgetBytes = 1000

	seedSyncedFile(t, cat, syncDir, "grown.jpg", 4000)
	ageFiles(t, cat, syncDir, "grown.jpg")

	full := filepath.Join(syncDir, "grown.jpg")
	if err := os.WriteFile(full, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := cat.GetFile("TestFolder", "grown.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, rec.FileMtime, rec.FileMtime); err != nil {
		t.Fatal(err)
	}

	d := newTestDaemon(t, cfg, cat, testLogger())
	declarePolicy(t, cfg, cat)
	d.EnforceQuotas()

	if n := countEvents(t, dbPath, "source_changed"); n != 1 {
		t.Fatalf("source_changed events = %d on the first tick, want 1", n)
	}
	details := lastEventDetails(t, dbPath, "source_changed")
	for _, want := range []string{"4096 bytes", "4000 bytes"} {
		if !strings.Contains(details, want) {
			t.Errorf("details = %q, want it to mention %q", details, want)
		}
	}
}
