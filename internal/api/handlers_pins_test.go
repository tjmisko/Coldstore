package api

import (
	"strings"
	"testing"

	"github.com/tjmisko/coldstore/internal/catalog"
)

// The archived warning is a transition test. Both wordings tell the owner the
// pin is latent; only one of them claims the daemon archived the file out from
// under the request, and that claim has to be true when it is made.
func TestPinWarning(t *testing.T) {
	wedged := folderStatus{Wedged: true, PinnedBytes: 3000, BudgetBytes: 1000}
	roomy := folderStatus{Wedged: false, PinnedBytes: 10, BudgetBytes: 1000}

	tests := []struct {
		name    string
		pinned  bool
		before  catalog.FileStatus
		after   catalog.FileStatus
		folder  folderStatus
		want    *string
		wantNil bool
	}{
		{
			name:   "should say the file is in cold storage when it was already archived before the pin",
			pinned: true,
			before: catalog.StatusArchived,
			after:  catalog.StatusArchived,
			folder: roomy,
			want:   strPtr(archivedPinWarning),
		},
		{
			name:   "should say the archive raced the pin when the file was still synced when the request arrived",
			pinned: true,
			before: catalog.StatusSynced,
			after:  catalog.StatusArchived,
			folder: roomy,
			want:   strPtr(archivedDuringPinWarning),
		},
		{
			name:   "should say the archive raced the pin when the move was already in flight",
			pinned: true,
			before: catalog.StatusArchiving,
			after:  catalog.StatusArchived,
			folder: roomy,
			want:   strPtr(archivedDuringPinWarning),
		},
		{
			name:   "should prefer the cold-storage wording over the budget one when the folder is also wedged",
			pinned: true,
			before: catalog.StatusArchived,
			after:  catalog.StatusArchived,
			folder: wedged,
			want:   strPtr(archivedPinWarning),
		},
		{
			name:   "should warn about the budget when a pin wedges a folder full of on-device files",
			pinned: true,
			before: catalog.StatusSynced,
			after:  catalog.StatusSynced,
			folder: wedged,
			want:   wedgeWarning(wedged),
		},
		{
			name:    "should stay silent when the pin is well inside the budget",
			pinned:  true,
			before:  catalog.StatusSynced,
			after:   catalog.StatusSynced,
			folder:  roomy,
			wantNil: true,
		},
		{
			name:    "should stay silent when the request is an unpin",
			pinned:  false,
			before:  catalog.StatusArchived,
			after:   catalog.StatusArchived,
			folder:  wedged,
			wantNil: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pinWarning(tc.pinned, tc.before, tc.after, tc.folder)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("warning = %q, want null", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("warning = null, want %q", *tc.want)
			}
			if *got != *tc.want {
				t.Errorf("warning = %q, want %q", *got, *tc.want)
			}
		})
	}
}

// The specific untruth: an ordinary pin on a file that has been in cold
// storage for weeks reporting a race that never happened.
func TestPinWarningShouldNotClaimAnArchiveHappenedWhenTheFileWasAlreadyInColdStorage(t *testing.T) {
	got := pinWarning(true, catalog.StatusArchived, catalog.StatusArchived, folderStatus{})
	if got == nil {
		t.Fatal("warning = null: the owner is looking at a filled pin badge on a file that is not on the device")
	}
	if strings.Contains(*got, "finished archiving while the pin was being applied") {
		t.Errorf("warning = %q, but nothing was archived during this request", *got)
	}
	if !strings.Contains(*got, "restore it") {
		t.Errorf("warning = %q, want it to say the pin takes effect on restore", *got)
	}
}

func strPtr(s string) *string { return &s }
