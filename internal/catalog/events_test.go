package catalog

import (
	"testing"
)

func TestListEventsShouldReturnAnEmptyArrayWhenNothingHasHappened(t *testing.T) {
	cat := setupTestCatalog(t)

	events, err := cat.ListEvents("", "", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if events == nil {
		t.Fatal("ListEvents returned nil; it must never be null on the wire")
	}
	if len(events) != 0 {
		t.Errorf("got %d events, want 0", len(events))
	}
}

func TestListEventsShouldReturnTheNewestFirstWhenSeveralWereWritten(t *testing.T) {
	cat := setupTestCatalog(t)

	for _, details := range []string{"first", "second", "third"} {
		if err := cat.LogEvent("archive", "Photos", "p.jpg", details); err != nil {
			t.Fatal(err)
		}
	}

	events, err := cat.ListEvents("", "", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	if events[0].Details != "third" {
		t.Errorf("first entry = %q, want the newest %q", events[0].Details, "third")
	}
	if events[0].Timestamp.IsZero() {
		t.Error("timestamp did not scan")
	}
}

// class=warning is the only reason the list is useful: one enforcement pass
// writes hundreds of archive rows and would bury every warning under them.
func TestListEventsShouldExcludeRoutinePerFileTypesWhenTheWarningClassIsAsked(t *testing.T) {
	cat := setupTestCatalog(t)

	for _, typ := range []string{"archive", "restore", "folder_seeded"} {
		if err := cat.LogEvent(typ, "Photos", "", "routine"); err != nil {
			t.Fatal(err)
		}
	}
	for _, typ := range []string{"folder_wedged", "cold_storage_low", "bulk_eviction"} {
		if err := cat.LogEvent(typ, "Photos", "", "loud"); err != nil {
			t.Fatal(err)
		}
	}

	events, err := cat.ListEvents("Photos", "", EventClassWarning, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d warning-class events, want 3: %+v", len(events), events)
	}
	for _, e := range events {
		if e.Details != "loud" {
			t.Errorf("routine event %q leaked into the warning class", e.EventType)
		}
	}
}

func TestListEventsShouldFilterOnFolderAndTypeAndClampTheLimit(t *testing.T) {
	cat := setupTestCatalog(t)

	cat.LogEvent("conflict", "Photos", "a.jpg", "one")
	cat.LogEvent("conflict", "Videos", "b.mp4", "two")
	cat.LogEvent("bulk_eviction", "Photos", "", "three")

	byFolder, err := cat.ListEvents("Photos", "", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(byFolder) != 2 {
		t.Errorf("folder=Photos returned %d, want 2", len(byFolder))
	}

	byType, err := cat.ListEvents("", "conflict", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(byType) != 2 {
		t.Errorf("type=conflict returned %d, want 2", len(byType))
	}

	clamped, err := cat.ListEvents("", "", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(clamped) != 3 {
		t.Errorf("got %d, want all 3 (the clamp is a ceiling, not a filter)", len(clamped))
	}
}
