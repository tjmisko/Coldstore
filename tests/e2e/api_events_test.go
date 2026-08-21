//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
)

type eventJSON struct {
	ID        int64  `json:"id"`
	Timestamp string `json:"timestamp"`
	EventType string `json:"event_type"`
	Folder    string `json:"folder"`
	Path      string `json:"path"`
	Details   string `json:"details"`
}

func listEvents(t *testing.T, env *testEnv, query string) []eventJSON {
	t.Helper()
	w := do(t, env, "GET", "/api/v1/events"+query, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /events%s status = %d, body: %s", query, w.Code, w.Body.String())
	}
	var events []eventJSON
	if err := json.NewDecoder(w.Body).Decode(&events); err != nil {
		t.Fatalf("decoding %q: %v", w.Body.String(), err)
	}
	return events
}

// Until this endpoint existed, every warning the control plane learned to
// write -- folder_wedged, cold_storage_low, bulk_eviction, restore_conflict --
// was unreachable outside SQLite, and the SPA fallback answered the path with
// 200 text/html so a client's JSON.parse threw on markup.
func TestEventsShouldBeReadableOverTheAPIWhenTheDaemonHasWrittenSome(t *testing.T) {
	env := newTestEnv(t)
	file := seedAgedFile(t, env, "photo.jpg", 1000)
	env.archive(t, file.ID)

	events := listEvents(t, env, "")
	if len(events) == 0 {
		t.Fatal("no events; the archive above writes one")
	}
	var found bool
	for _, e := range events {
		if e.EventType == "archive" && e.Path == "photo.jpg" {
			found = true
			if e.Folder != "TestFolder" || e.Timestamp == "" || e.Details == "" {
				t.Errorf("archive event is missing fields: %+v", e)
			}
		}
	}
	if !found {
		t.Errorf("no archive event for photo.jpg; got %+v", events)
	}
}

// A single enforcement pass writes hundreds of archive rows. Without the
// warning class the wedge warning the whole of D3 exists to produce would
// never once appear in a "last 10 events" list.
func TestEventsShouldHideRoutinePerFileRowsWhenTheWarningClassIsAsked(t *testing.T) {
	env := newTestEnv(t)
	for i := 0; i < 5; i++ {
		file := seedAgedFile(t, env, "photo"+padInt(i, 2)+".jpg", 1000)
		env.archive(t, file.ID)
	}
	if err := env.cat.LogEvent("folder_wedged", "TestFolder", "",
		"2 kB pinned exceeds the 1 kB budget - nothing left to offload"); err != nil {
		t.Fatal(err)
	}

	all := listEvents(t, env, "?folder=TestFolder")
	if len(all) < 6 {
		t.Fatalf("unfiltered list has %d events, want at least 6", len(all))
	}

	warnings := listEvents(t, env, "?folder=TestFolder&class=warning")
	if len(warnings) != 1 || warnings[0].EventType != "folder_wedged" {
		t.Fatalf("class=warning returned %+v, want only the wedge warning", warnings)
	}
}

func TestEventsShouldFilterOnTypeAndReturnNewestFirst(t *testing.T) {
	env := newTestEnv(t)
	env.cat.LogEvent("bulk_eviction", "TestFolder", "", "first")
	env.cat.LogEvent("bulk_eviction", "TestFolder", "", "second")
	env.cat.LogEvent("cold_storage_low", "TestFolder", "", "unrelated")

	events := listEvents(t, env, "?type=bulk_eviction")
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Details != "second" {
		t.Errorf("first entry is %q, want the newest (%q)", events[0].Details, "second")
	}
}

func TestEventsShouldReturnAnEmptyArrayWhenNothingHasHappened(t *testing.T) {
	env := newTestEnv(t)

	w := do(t, env, "GET", "/api/v1/events?folder=Nonexistent", nil)
	if body := w.Body.String(); body != "[]\n" {
		t.Errorf("body = %q, want an empty array and never null", body)
	}
}

func TestEventsShouldRejectAnUnknownClassAndANonPositiveLimit(t *testing.T) {
	env := newTestEnv(t)

	for _, query := range []string{"?class=everything", "?limit=0", "?limit=-1", "?limit=abc"} {
		w := do(t, env, "GET", "/api/v1/events"+query, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET /events%s status = %d, want 400", query, w.Code)
		}
	}
}

// The SPA catch-all used to answer every unmatched /api path with 200
// text/html, which is what turned two missing endpoints into silent successes
// that a client parsed as markup.
func TestUnknownAPIPathsShouldReturnAJSONNotFoundRatherThanTheSPA(t *testing.T) {
	env := newTestEnv(t)

	for _, path := range []string{"/api/v1/nonexistent", "/api/v1/files/1/nonsense", "/api/nope"} {
		w := do(t, env, "GET", path, nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s content-type = %q, want application/json", path, ct)
		}
		var body map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Errorf("GET %s body is not JSON: %q", path, w.Body.String())
			continue
		}
		if body["error"] == "" {
			t.Errorf("GET %s body = %v, want an error message", path, body)
		}
	}
}
