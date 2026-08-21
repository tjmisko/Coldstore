//go:build e2e_live

package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// This file is named to sort last: it is the only test that lowers the managed
// folder's budget, and while that budget is low the daemon is free to evict
// anything else in the folder. Every other live test would then be racing an
// enforcement tick it did not ask for.

// enforcementTimeout must clear at least two quota ticks. The container's
// scan_interval_minutes is 1, and the change has to be picked up by a tick that
// starts AFTER the policy write commits, so one interval is not enough.
const enforcementTimeout = 200 * time.Second

// TestLiveShouldOffloadOnTheNextTickWhenTheBudgetIsLoweredThroughTheAPI is the
// live proof of D1 and D2 together: policy lives in the database, the running
// daemon picks it up on its next tick with no restart, and a pinned file is
// never a candidate no matter how far over budget the folder is.
func TestLiveShouldOffloadOnTheNextTickWhenTheBudgetIsLoweredThroughTheAPI(t *testing.T) {
	const size = 250_000
	const pinnedRel = "quota-pinned.bin"
	evictRels := []string{"quota-evict-1.bin", "quota-evict-2.bin"}

	before := live.folder(t, liveFolderLabel)

	writeManaged(t, pinnedRel, randomBytes(size, 53))
	for i, rel := range evictRels {
		writeManaged(t, rel, randomBytes(size, byte(59+i)))
	}
	// Back-dated so they are the oldest files in the folder and outside any
	// grace window. Oldest-first is the eviction order, so this also makes the
	// selection deterministic instead of dependent on what earlier tests left
	// behind.
	ageManaged(t, pinnedRel)
	for _, rel := range evictRels {
		ageManaged(t, rel)
	}

	live.scanAndWait(t)

	pinned, found := live.fileByRelPath(t, pinnedRel)
	if !found {
		t.Fatalf("no catalog record for %q", pinnedRel)
	}
	live.mustJSON(t, http.MethodPost,
		fmt.Sprintf("/api/v1/files/%d/pin", pinned.ID), nil, nil, http.StatusOK)

	current := live.folder(t, liveFolderLabel)
	// A budget that can only be met by evicting BOTH unpinned aged files and
	// cannot be met by evicting one. If the daemon touches the pinned file
	// instead, the folder ends up under budget with the wrong file gone and the
	// assertions below say so.
	target := current.SyncedSizeBytes - 2*size + size/2

	t.Cleanup(func() {
		restore := map[string]any{
			"budget_bytes":  before.BudgetBytes,
			"grace_minutes": before.GraceMinutes,
			"enabled":       before.Enabled,
		}
		live.mustJSON(t, http.MethodPut, "/api/v1/folders/"+liveFolderLabel, restore, nil, http.StatusOK)
		live.mustJSON(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/files/%d/pin", pinned.ID), nil, nil, http.StatusOK)
	})

	// The whole point of D1: this is a database write through the HTTP API of a
	// daemon that is already running. No restart, no config reload, no SIGHUP.
	live.mustJSON(t, http.MethodPut, "/api/v1/folders/"+liveFolderLabel, map[string]any{
		"budget_bytes":  target,
		"grace_minutes": 0,
		"enabled":       true,
	}, nil, http.StatusOK)

	for _, rel := range evictRels {
		live.awaitFile(t, rel, enforcementTimeout,
			"the quota tick to offload "+rel+" after the budget was lowered through the API",
			func(f liveFile) bool { return f.Status == "archived" })
	}

	// D2: the pinned file is still on the device, and it is still pinned.
	still, found := live.fileByRelPath(t, pinnedRel)
	if !found {
		t.Fatalf("the pinned file vanished from the catalog entirely")
	}
	if still.Status != "synced" {
		t.Errorf("pinned file status = %q after enforcement, want synced; a pinned file is never a candidate", still.Status)
	}
	if !still.Pinned {
		t.Error("the pin was cleared by the quota tick")
	}
	if !live.existsIn(serviceColdstore, managedPath(pinnedRel)) {
		t.Errorf("%s was offloaded despite being pinned", pinnedRel)
	}

	after := live.folder(t, liveFolderLabel)
	if after.OverBudget {
		t.Errorf("folder still over budget after enforcement: %d bytes on device, budget %d",
			after.SyncedSizeBytes, after.BudgetBytes)
	}
	// D3: pinned bytes are counted against the budget, so they are visible in
	// the folder block rather than hidden from it.
	if after.PinnedBytes < size {
		t.Errorf("pinned_bytes = %d, want at least %d (the pinned file)", after.PinnedBytes, size)
	}
	if after.LastEnforcedAt == nil {
		t.Error("last_enforced_at is null after an enforcement tick ran")
	}

	// And the automatic offload reaches the phone exactly as a manual one does.
	for _, rel := range evictRels {
		await(t, propagationTimeout,
			"Syncthing to propagate the automatic offload of "+rel+" to device B",
			goneFromDeviceB(rel))
	}
}

// TestLiveShouldReportTheFolderAsWedgedWhenPinnedBytesExceedTheBudget is D3's
// user-visible half: coldstore does not silently thrash, it says it cannot
// converge.
func TestLiveShouldReportTheFolderAsWedgedWhenPinnedBytesExceedTheBudget(t *testing.T) {
	const rel = "quota-wedge.bin"
	const size = 300_000

	before := live.folder(t, liveFolderLabel)

	writeManaged(t, rel, randomBytes(size, 67))
	live.scanAndWait(t)

	file, found := live.fileByRelPath(t, rel)
	if !found {
		t.Fatalf("no catalog record for %q", rel)
	}

	t.Cleanup(func() {
		live.mustJSON(t, http.MethodPut, "/api/v1/folders/"+liveFolderLabel, map[string]any{
			"budget_bytes":  before.BudgetBytes,
			"grace_minutes": before.GraceMinutes,
			"enabled":       before.Enabled,
		}, nil, http.StatusOK)
		live.mustJSON(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/files/%d/pin", file.ID), nil, nil, http.StatusOK)
	})

	// Budget below the size of the one file we are about to pin: even offloading
	// every other byte in the folder cannot get under it.
	live.mustJSON(t, http.MethodPut, "/api/v1/folders/"+liveFolderLabel, map[string]any{
		"budget_bytes":  int64(size / 2),
		"grace_minutes": before.GraceMinutes,
		"enabled":       false, // disabled: this test is about reporting, not eviction
	}, nil, http.StatusOK)

	var pinned struct {
		File    liveFile   `json:"file"`
		Folder  liveFolder `json:"folder"`
		Warning *string    `json:"warning"`
	}
	live.mustJSON(t, http.MethodPost,
		fmt.Sprintf("/api/v1/files/%d/pin", file.ID), nil, &pinned, http.StatusOK)

	if !pinned.Folder.Wedged {
		t.Errorf("wedged = false with pinned_bytes %d over budget %d",
			pinned.Folder.PinnedBytes, pinned.Folder.BudgetBytes)
	}
	if pinned.Warning == nil {
		t.Error("the pin response carries no warning for a folder it just wedged")
	} else {
		t.Logf("wedge warning returned to the client: %s", *pinned.Warning)
	}

	// And the same verdict from the status endpoint, which is what the banner
	// actually reads.
	if !live.folder(t, liveFolderLabel).Wedged {
		t.Error("GET /api/v1/status does not report the folder as wedged")
	}
}
