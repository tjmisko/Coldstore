package daemon

import (
	"fmt"
	"strings"
	"testing"
)

// Design section 5.10's second fix. The restore pre-flight only covers the
// restore-shaped cause; a budget lowered in the UI sheds just as many files,
// and the only signal anywhere else is a red bar while Syncthing deletes them
// from every device the owner has.
func TestEnforcementShouldWriteOneLoudEventWhenAPassEvictsAnUnusualNumberOfFiles(t *testing.T) {
	syncDir, cfg, cat, dbPath := setupWedgeEnv(t)
	cfg.Folders[0].BudgetBytes = 1 << 40

	names := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("photo%02d.jpg", i)
		seedSyncedFile(t, cat, syncDir, name, 100)
		names = append(names, name)
	}
	declarePolicy(t, cfg, cat)
	d := newTestDaemon(t, cfg, cat, testLogger())
	ageFiles(t, cat, syncDir, names...)

	// The owner lowers the budget to something only one file fits in.
	budget := int64(100)
	found, err := cat.UpdateFolderPolicy("TestFolder", &budget, nil, nil)
	if err != nil || !found {
		t.Fatalf("setting the budget: found %v, err %v", found, err)
	}

	d.EnforceQuotas()

	if n := countEvents(t, dbPath, "bulk_eviction"); n != 1 {
		t.Fatalf("bulk_eviction events = %d, want exactly 1", n)
	}
	details := lastEventDetails(t, dbPath, "bulk_eviction")
	if !strings.Contains(details, "39 files") {
		t.Errorf("details = %q, want the file count", details)
	}
	if !strings.Contains(details, "3.9 kB") {
		t.Errorf("details = %q, want the byte total", details)
	}
}

func TestEnforcementShouldStaySilentWhenItEvictsAnOrdinaryNumberOfFiles(t *testing.T) {
	syncDir, cfg, cat, dbPath := setupWedgeEnv(t)
	cfg.Folders[0].BudgetBytes = 1 << 40

	names := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("photo%02d.jpg", i)
		seedSyncedFile(t, cat, syncDir, name, 100)
		names = append(names, name)
	}
	declarePolicy(t, cfg, cat)
	d := newTestDaemon(t, cfg, cat, testLogger())
	ageFiles(t, cat, syncDir, names...)

	// 3 files out of 40 is 7.5%: under both thresholds.
	budget := int64(3700)
	found, err := cat.UpdateFolderPolicy("TestFolder", &budget, nil, nil)
	if err != nil || !found {
		t.Fatalf("setting the budget: found %v, err %v", found, err)
	}

	d.EnforceQuotas()

	if n := countEvents(t, dbPath, "bulk_eviction"); n != 0 {
		t.Errorf("bulk_eviction events = %d, want 0: routine enforcement must stay quiet", n)
	}
}
