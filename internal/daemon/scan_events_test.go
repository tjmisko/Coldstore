package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScannerShouldRecordSuccessfulAndFailedScansForOfflineHealth(t *testing.T) {
	cat, scanner, folder, root := seedScannedFolder(t)
	completedBefore, err := cat.ListEvents(folder.FolderLabel, "scan_completed", "", 10)
	if err != nil {
		t.Fatal(err)
	}

	if err := scanner.ScanFolder(folder); err != nil {
		t.Fatal(err)
	}
	if events, err := cat.ListEvents(folder.FolderLabel, "scan_completed", "", 10); err != nil || len(events) != len(completedBefore)+1 {
		t.Fatalf("scan_completed events = %d, %v, want %d", len(events), err, len(completedBefore)+1)
	}

	if err := os.Remove(filepath.Join(root, ".stfolder")); err != nil {
		t.Fatal(err)
	}
	if err := scanner.ScanFolder(folder); err == nil {
		t.Fatal("scan without .stfolder unexpectedly succeeded")
	}
	if events, err := cat.ListEvents(folder.FolderLabel, "scan_failed", "", 10); err != nil || len(events) != 1 {
		t.Fatalf("scan_failed events = %d, %v, want 1", len(events), err)
	}
}
