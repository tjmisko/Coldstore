package archivetier

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tjmisko/coldstore/internal/archive"
	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/journal"
)

// history is a root after every kind of record the journal can hold in M1,
// plus what the derived state looked like before the final batch.
type history struct {
	Root string

	Dump     string
	Manifest string

	DumpBeforeFinal     string
	ManifestBeforeFinal string
}

type syncedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func entryByPath(t *testing.T, ix *index.Index, path string) index.Entry {
	t.Helper()
	for _, entry := range allEntries(t, ix, nil) {
		if entry.Path == path {
			return entry
		}
	}
	t.Fatalf("no live entry at %s", path)
	return index.Entry{}
}

// buildHistory: import, upload, a placed_beside collision, tags added and
// removed, a pin record, trash and untrash, then one last upload as the final
// batch.
func buildHistory(t *testing.T) history {
	t.Helper()
	ctx := context.Background()
	root := newRoot(t)

	service := openService(t, serviceOptions(root))
	ts := newAPIServer(t, root, service)

	// import
	source := writeSource(t, demoSource())
	id, err := service.ImportStart(collectionName, source)
	if err != nil {
		t.Fatalf("ImportStart: %v", err)
	}
	if status := waitForImport(t, service, id); status.State != "done" || status.FailedCount != 0 {
		t.Fatalf("import: %+v", status)
	}

	// upload, and a collision at an occupied path with different bytes
	mustPutFile(t, ts.URL, sourceFile{"Uploads/new.txt", "uploaded after the import\n"})
	collision := sourceFile{"Letters/a.txt", "a different letter that wants the same name\n"}
	status, body, err := putFile(&http.Client{}, ts.URL, collectionName, collision)
	if err != nil || status != http.StatusCreated || !strings.Contains(body, "placed_beside") {
		t.Fatalf("collision PUT = %d %s (%v), want 201 placed_beside", status, body, err)
	}

	// tags added and removed
	alps1 := entryByPath(t, service.Index(), "Trips/2024 Alps/IMG 001.jpg")
	alps2 := entryByPath(t, service.Index(), "Trips/2024 Alps/IMG 002.jpg")
	notes := entryByPath(t, service.Index(), "Trips/2025 Coast/notes.md")
	if _, err := service.Tag(ctx, "test", []archive.TagChange{
		{IDs: []int64{alps1.EntryID, alps2.EntryID}, Add: []string{"travel/alps", "scan"}},
		{IDs: []int64{notes.EntryID}, Add: []string{"travel/coast"}},
	}); err != nil {
		t.Fatalf("Tag add: %v", err)
	}
	if _, err := service.Tag(ctx, "test", []archive.TagChange{
		{IDs: []int64{alps2.EntryID}, Remove: []string{"scan"}},
	}); err != nil {
		t.Fatalf("Tag remove: %v", err)
	}

	// trash, untrash
	receipt1 := entryByPath(t, service.Index(), "Receipts/2025-01.pdf")
	receipt2 := entryByPath(t, service.Index(), "Receipts/2025-02.pdf")
	if _, err := service.Trash(ctx, "test", []int64{receipt1.EntryID, receipt2.EntryID}, true); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if _, err := service.Trash(ctx, "test", []int64{receipt2.EntryID}, false); err != nil {
		t.Fatalf("Untrash: %v", err)
	}
	closeService(t, service)

	// a pin record: no M1 API writes one, so append it with the lock the
	// daemon would hold and let Open replay it.
	pinned := true
	appendRecords(t, root, journal.Record{
		By: "test", Type: journal.TypePlacementPinned, Folder: collectionName, Path: "Letters/b.txt", Pinned: &pinned,
	})

	service = openService(t, serviceOptions(root))
	ts = newAPIServer(t, root, service)
	h := history{Root: root}
	tree := viewsTree(root, service)
	h.DumpBeforeFinal = dumpIndex(t, service.Index())
	h.ManifestBeforeFinal = manifestOf(t, tree, service.Index())

	// the final batch
	mustPutFile(t, ts.URL, sourceFile{"Final/last.txt", "the last thing that happened\n"})
	h.Dump = dumpIndex(t, service.Index())
	h.Manifest = manifestOf(t, tree, service.Index())
	pinnedEntry := entryByPath(t, service.Index(), "Letters/b.txt")
	trashed := true
	trashedEntries := allEntries(t, service.Index(), &trashed)
	closeService(t, service)

	if !pinnedEntry.Pinned {
		t.Fatal("the pin record did not pin Letters/b.txt")
	}
	if len(trashedEntries) != 1 || trashedEntries[0].Path != "Receipts/2025-01.pdf" {
		t.Fatalf("trash state = %+v, want only Receipts/2025-01.pdf trashed", trashedEntries)
	}
	if h.Dump == h.DumpBeforeFinal {
		t.Fatal("the final upload did not change the index")
	}
	for _, want := range []string{"travel/alps", "Letters/a.sha256-"} {
		if !strings.Contains(strings.ToLower(h.Dump), strings.ToLower(want)) {
			t.Fatalf("the history lacks %q in its dump:\n%s", want, firstLines(h.Dump, 60))
		}
	}
	return h
}

// reopened opens the root, returns what the index and views look like, and
// closes it again.
func reopened(t *testing.T, root string, logger *slog.Logger) (dump, manifest string, events []index.Event) {
	t.Helper()
	opts := serviceOptions(root)
	if logger != nil {
		opts.Logger = logger
	}
	service, err := archive.Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	dump = dumpIndex(t, service.Index())
	manifest = manifestOf(t, viewsTree(root, service), service.Index())
	events, err = service.Index().Events("", "", 100)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	closeService(t, service)
	return dump, manifest, events
}

func removeIndexFiles(t *testing.T, root string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(root, "index.db"+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

func assertEqualToHistory(t *testing.T, h history, dump, manifest string) {
	t.Helper()
	if dump != h.Dump {
		t.Errorf("the index differs from the original\noriginal:\n%s\ngot:\n%s", firstLines(h.Dump, 60), firstLines(dump, 60))
	}
	if manifest != h.Manifest {
		t.Errorf("views.Manifest differs from the original\noriginal:\n%s\ngot:\n%s", h.Manifest, manifest)
	}
}

func TestIndexLossAndRebuild(t *testing.T) {
	t.Run("should equal the original index when index.db is deleted", func(t *testing.T) {
		h := buildHistory(t)
		removeIndexFiles(t, h.Root)

		dump, manifest, _ := reopened(t, h.Root, nil)

		assertEqualToHistory(t, h, dump, manifest)
	})

	t.Run("should equal the original index when index.db is overwritten with garbage", func(t *testing.T) {
		h := buildHistory(t)
		removeIndexFiles(t, h.Root)
		garbage := bytes.Repeat([]byte("not a sqlite file, not at all. "), 300)
		if err := os.WriteFile(filepath.Join(h.Root, "index.db"), garbage, 0o644); err != nil {
			t.Fatal(err)
		}

		dump, manifest, events := reopened(t, h.Root, nil)

		assertEqualToHistory(t, h, dump, manifest)
		if !hasEvent(events, "index_rebuilt") {
			t.Error("no index_rebuilt event after the garbage index was rebuilt")
		}
	})

	t.Run("should equal the original index when index.db is truncated", func(t *testing.T) {
		h := buildHistory(t)
		path := filepath.Join(h.Root, "index.db")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			_ = os.Remove(path + suffix)
		}
		if err := os.Truncate(path, info.Size()/2); err != nil {
			t.Fatal(err)
		}

		dump, manifest, _ := reopened(t, h.Root, nil)

		assertEqualToHistory(t, h, dump, manifest)
	})

	t.Run("should equal the original index when rebuild-index runs", func(t *testing.T) {
		h := buildHistory(t)

		report, err := archive.RebuildIndex(context.Background(), serviceOptions(h.Root))
		if err != nil {
			t.Fatalf("RebuildIndex: %v", err)
		}
		dump, manifest, _ := reopened(t, h.Root, nil)

		assertEqualToHistory(t, h, dump, manifest)
		if report.Torn != nil || report.Records == 0 || report.Entries == 0 {
			t.Errorf("unexpected rebuild report %+v", report)
		}
	})

	t.Run("should regenerate views identically when views is deleted", func(t *testing.T) {
		h := buildHistory(t)
		if err := os.RemoveAll(filepath.Join(h.Root, "views")); err != nil {
			t.Fatal(err)
		}

		if _, err := archive.RebuildIndex(context.Background(), serviceOptions(h.Root)); err != nil {
			t.Fatalf("RebuildIndex: %v", err)
		}
		dump, manifest, _ := reopened(t, h.Root, nil)

		assertEqualToHistory(t, h, dump, manifest)
	})

	t.Run("should drop the torn final record with a warning when the journal is cut mid-line", func(t *testing.T) {
		h := buildHistory(t)
		segments, err := filepath.Glob(filepath.Join(h.Root, "journal", "*.jsonl"))
		if err != nil || len(segments) == 0 {
			t.Fatalf("no journal segments: %v", err)
		}
		last := segments[len(segments)-1]
		raw, err := os.ReadFile(last)
		if err != nil {
			t.Fatal(err)
		}
		body := raw[:len(raw)-1] // without the final newline
		lineStart := bytes.LastIndexByte(body, '\n') + 1
		if err := os.Truncate(last, int64(lineStart+(len(body)-lineStart)/2)); err != nil {
			t.Fatal(err)
		}
		logs := &syncedBuffer{}
		logger := slog.New(slog.NewTextHandler(logs, nil))

		dump, manifest, events := reopened(t, h.Root, logger)

		if dump != h.DumpBeforeFinal {
			t.Errorf("the index is not the original minus the final batch\nwant:\n%s\ngot:\n%s",
				firstLines(h.DumpBeforeFinal, 60), firstLines(dump, 60))
		}
		if manifest != h.ManifestBeforeFinal {
			t.Errorf("views.Manifest is not the original minus the final batch\nwant:\n%s\ngot:\n%s", h.ManifestBeforeFinal, manifest)
		}
		if !hasEvent(events, "journal_torn") {
			t.Errorf("no journal_torn event; events: %+v", events)
		}
		if !strings.Contains(strings.ToLower(logs.String()), "torn") {
			t.Errorf("no torn-tail log line; log:\n%s", logs.String())
		}
	})
}

func hasEvent(events []index.Event, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func TestFormatVersion(t *testing.T) {
	t.Run("should refuse to open when the journal format is newer", func(t *testing.T) {
		h := buildHistory(t)
		formatPath := filepath.Join(h.Root, "journal", "FORMAT")
		if err := os.WriteFile(formatPath, []byte("999\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		before := treeSnapshot(t, h.Root)

		_, err := archive.Open(context.Background(), serviceOptions(h.Root))
		if !errors.Is(err, journal.ErrFormatNewer) {
			t.Fatalf("archive.Open error = %v, want journal.ErrFormatNewer", err)
		}

		cmd := exec.Command(coldstoreBinary(t), "format-version", "--config",
			writeColdstoreConfig(t, h.Root, collectionConfigBlock()))
		out, runErr := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 3 {
			t.Fatalf("coldstore format-version exit = %v, want 3\n%s", runErr, out)
		}

		after := treeSnapshot(t, h.Root)
		if strings.Join(before, "\n") != strings.Join(after, "\n") {
			t.Errorf("the root changed while refusing to open\nbefore: %v\nafter:  %v", before, after)
		}
		if _, statErr := os.Stat(filepath.Join(h.Root, "journal", "LOCK")); statErr == nil {
			// A LOCK file is created by AcquireLock; the refusal happens before
			// it, so it must be exactly as it was (present from earlier opens).
			lock, lockErr := journal.AcquireLock(filepath.Join(h.Root, "journal"))
			if lockErr != nil {
				t.Errorf("the lock was left held after the refusal: %v", lockErr)
			} else {
				_ = lock.Release()
			}
		}
	})
}
