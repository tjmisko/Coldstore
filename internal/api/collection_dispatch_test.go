package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/store"
)

func TestCollectionDispatch(t *testing.T) {
	t.Run("should list collection entries with the FileRecord shape when folder names a collection", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		uploaded := mustPut(t, srv, "photos", "trip/a.TXT", "alpha")

		rec := send(srv, http.MethodGet, "/api/v1/files?folder=photos", nil, nil)

		if rec.Code != http.StatusOK {
			t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
		}
		files := decode[[]catalog.FileRecord](t, rec)
		if len(files) != 1 {
			t.Fatalf("%d files", len(files))
		}
		file := files[0]
		if file.ID != uploaded.ID || file.FolderLabel != "photos" || file.RelativePath != "trip/a.TXT" ||
			file.Filename != "a.TXT" || file.Extension != ".txt" || file.SizeBytes != 5 ||
			file.Status != catalog.StatusArchived || file.ContentHash != sha256Hex("alpha") ||
			file.Pinned || file.PinnedAt != nil || file.PinSource != "" || file.Tags == nil ||
			file.TrashedAt != nil || file.FirstSeen.IsZero() {
			t.Fatalf("record %+v", file)
		}
		raw := rec.Body.String()
		for _, key := range []string{`"pinned_at":null`, `"tags":[]`, `"gps_lat":null`, `"captured_local":""`} {
			if !strings.Contains(raw, key) {
				t.Errorf("response lacks %s: %s", key, raw)
			}
		}
	})

	t.Run("should use ids at or above CollectionIDBase for collection entries", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		mustPut(t, srv, "photos", "a.txt", "alpha")
		mustPut(t, srv, "photos", "b.txt", "bravo")

		files := decode[[]catalog.FileRecord](t, send(srv, http.MethodGet, "/api/v1/files?folder=photos", nil, nil))
		ids := decode[fileIDsResponse](t, send(srv, http.MethodGet, "/api/v1/files/ids?folder=photos", nil, nil))

		if CollectionIDBase != 1<<52 {
			t.Fatalf("CollectionIDBase = %d", CollectionIDBase)
		}
		for _, file := range files {
			if file.ID < CollectionIDBase || !isCollectionID(file.ID) || apiID(entryID(file.ID)) != file.ID {
				t.Errorf("id %d is not a collection id", file.ID)
			}
		}
		if len(ids.IDs) != 2 || ids.Total != 2 || ids.Truncated {
			t.Fatalf("ids %+v", ids)
		}
		for _, id := range ids.IDs {
			if id < CollectionIDBase {
				t.Errorf("ids route returned legacy id %d", id)
			}
		}
	})

	t.Run("should list legacy files unchanged when folder names a sync folder", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		srv.cat.UpsertFile(&catalog.FileRecord{
			FolderLabel: "TestFolder", RelativePath: "legacy.jpg", Filename: "legacy.jpg", Extension: ".jpg",
			SizeBytes: 10, FileMtime: time.Now().UTC(), Status: catalog.StatusSynced,
		}, time.Now().UnixNano())
		mustPut(t, srv, "photos", "a.txt", "alpha")

		files := decode[[]catalog.FileRecord](t, send(srv, http.MethodGet, "/api/v1/files?folder=TestFolder", nil, nil))
		all := decode[[]catalog.FileRecord](t, send(srv, http.MethodGet, "/api/v1/files", nil, nil))

		if len(files) != 1 || files[0].RelativePath != "legacy.jpg" || isCollectionID(files[0].ID) {
			t.Fatalf("legacy listing %+v", files)
		}
		if len(all) != 1 {
			t.Fatalf("the all-folders view lists %d files; collections are browsed by selecting them", len(all))
		}
	})

	t.Run("should answer 400 when ids mix legacy and collection files", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		uploaded := mustPut(t, srv, "photos", "a.txt", "alpha")

		for _, target := range []string{
			fmt.Sprintf("/api/v1/files?ids=1,%d", uploaded.ID),
			fmt.Sprintf("/api/v1/files/ids?ids=%d,1", uploaded.ID),
			"/api/v1/files?folder=photos&ids=1",
		} {
			if rec := send(srv, http.MethodGet, target, nil, nil); rec.Code != http.StatusBadRequest {
				t.Errorf("GET %s = %d, want 400", target, rec.Code)
			}
		}
		rec := send(srv, http.MethodGet, fmt.Sprintf("/api/v1/files?ids=%d", uploaded.ID), nil, nil)
		if rec.Code != http.StatusOK || len(decode[[]catalog.FileRecord](t, rec)) != 1 {
			t.Fatalf("an all-collection ids list = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("should answer 400 when a capture filter is used on a collection", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		for _, param := range []string{
			"captured_from=2020-01-01", "captured_to=2020-01-01", "undated=1", "near=1,2,3",
			"pinned=1", "pin_source=user", "conflict=1", "described=1", "sort=pinned_at",
		} {
			rec := send(srv, http.MethodGet, "/api/v1/files?folder=photos&"+param, nil, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400", param, rec.Code)
			}
			name := strings.SplitN(param, "=", 2)[0]
			if !strings.Contains(rec.Body.String(), name) {
				t.Errorf("%s: error does not name the parameter: %s", param, rec.Body.String())
			}
		}
	})

	t.Run("should return an empty page when the status filter selects nothing a collection holds", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		mustPut(t, srv, "photos", "a.txt", "alpha")
		rec := send(srv, http.MethodGet, "/api/v1/files?folder=photos&status=synced", nil, nil)
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("status=synced = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("should treat sort=captured as mtime and sort by name and size when asked", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		mustPut(t, srv, "photos", "b.txt", "a longer body")
		mustPut(t, srv, "photos", "a.txt", "x")
		first := func(query string) string {
			files := decode[[]catalog.FileRecord](t, send(srv, http.MethodGet, "/api/v1/files?folder=photos&"+query, nil, nil))
			return files[0].RelativePath
		}
		if first("sort=name") != "a.txt" || first("sort=size") != "b.txt" {
			t.Fatal("sort by name or size is wrong")
		}
		if rec := send(srv, http.MethodGet, "/api/v1/files?folder=photos&sort=captured", nil, nil); rec.Code != http.StatusOK {
			t.Fatalf("sort=captured = %d", rec.Code)
		}
	})

	t.Run("should tag a collection entry through the existing tag route", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		uploaded := mustPut(t, srv, "photos", "a.txt", "alpha")
		other := mustPut(t, srv, "photos", "b.txt", "bravo")

		rec := sendJSON(srv, http.MethodPost, fmt.Sprintf("/api/v1/files/%d/tags", uploaded.ID), map[string]any{"add": []string{"Trip/2024"}})
		if rec.Code != http.StatusOK {
			t.Fatalf("tag = %d %s", rec.Code, rec.Body.String())
		}
		tagged := decode[fileTagsResponse](t, rec)
		if tagged.File.ID != uploaded.ID || len(tagged.File.Tags) != 1 || tagged.File.Tags[0] != "Trip/2024" ||
			len(tagged.Created) == 0 {
			t.Fatalf("response %+v", tagged)
		}

		tags := decode[[]catalog.Tag](t, send(srv, http.MethodGet, "/api/v1/tags?folder=photos", nil, nil))
		if len(tags) == 0 || tags[0].Count+tags[0].DescendantCount == 0 {
			t.Fatalf("tags %+v", tags)
		}
		if legacy := decode[[]catalog.Tag](t, send(srv, http.MethodGet, "/api/v1/tags", nil, nil)); len(legacy) != 0 {
			t.Fatalf("the legacy tag list leaks collection tags: %+v", legacy)
		}
		filtered := decode[[]catalog.FileRecord](t, send(srv, http.MethodGet, "/api/v1/files?folder=photos&tag=trip", nil, nil))
		if len(filtered) != 1 || filtered[0].ID != uploaded.ID {
			t.Fatalf("tag filter %+v", filtered)
		}

		bulk := sendJSON(srv, http.MethodPost, "/api/v1/files/tags",
			map[string]any{"ids": []int64{uploaded.ID, other.ID}, "add": []string{"keep"}})
		if bulk.Code != http.StatusOK {
			t.Fatalf("bulk tag = %d %s", bulk.Code, bulk.Body.String())
		}
		result := decode[tagWire](t, bulk)
		if result.Updated != 2 || len(result.Undo.Changes) == 0 || result.Undo.Changes[0].IDs[0] < CollectionIDBase {
			t.Fatalf("bulk result %+v", result)
		}
		undo := sendJSON(srv, http.MethodPost, "/api/v1/files/tags", map[string]any{"changes": result.Undo.Changes})
		if undo.Code != http.StatusOK {
			t.Fatalf("undo = %d %s", undo.Code, undo.Body.String())
		}
		after := decode[[]catalog.FileRecord](t, send(srv, http.MethodGet, "/api/v1/files?folder=photos&tag=keep", nil, nil))
		if len(after) != 0 {
			t.Fatalf("the undo left %d files tagged", len(after))
		}

		rec = sendJSON(srv, http.MethodDelete, fmt.Sprintf("/api/v1/files/%d/tags", uploaded.ID), map[string]any{"remove": []string{"trip/2024"}})
		if rec.Code != http.StatusOK || len(decode[fileTagsResponse](t, rec).File.Tags) != 0 {
			t.Fatalf("remove = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("should answer 400 when a bulk tag request mixes legacy and collection ids", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		uploaded := mustPut(t, srv, "photos", "a.txt", "alpha")
		rec := sendJSON(srv, http.MethodPost, "/api/v1/files/tags",
			map[string]any{"ids": []int64{1, uploaded.ID}, "add": []string{"x"}})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("mixed bulk tag = %d, want 400", rec.Code)
		}
	})

	t.Run("should answer 404 when a tagged collection entry does not exist", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		rec := sendJSON(srv, http.MethodPost, fmt.Sprintf("/api/v1/files/%d/tags", apiID(999)), map[string]any{"add": []string{"x"}})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("tag = %d, want 404", rec.Code)
		}
	})

	t.Run("should answer 400 when a collection id is pinned", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		uploaded := mustPut(t, srv, "photos", "a.txt", "alpha")
		for name, rec := range map[string]func() int{
			"pin": func() int {
				return send(srv, http.MethodPost, fmt.Sprintf("/api/v1/files/%d/pin", uploaded.ID), nil, nil).Code
			},
			"unpin": func() int {
				return send(srv, http.MethodDelete, fmt.Sprintf("/api/v1/files/%d/pin", uploaded.ID), nil, nil).Code
			},
			"bulk": func() int {
				return sendJSON(srv, http.MethodPost, "/api/v1/files/pin", map[string]any{"ids": []int64{uploaded.ID}, "pinned": true}).Code
			},
			"patch": func() int {
				return sendJSON(srv, http.MethodPatch, fmt.Sprintf("/api/v1/files/%d", uploaded.ID), map[string]any{"description": "x"}).Code
			},
			"restore": func() int {
				return send(srv, http.MethodPost, fmt.Sprintf("/api/v1/files/%d/restore", uploaded.ID), nil, nil).Code
			},
			"archive": func() int {
				return send(srv, http.MethodPost, fmt.Sprintf("/api/v1/files/%d/archive", uploaded.ID), nil, nil).Code
			},
			"resolve": func() int {
				return sendJSON(srv, http.MethodPost, fmt.Sprintf("/api/v1/files/%d/resolve", uploaded.ID), map[string]any{"keep": "device"}).Code
			},
		} {
			if code := rec(); code != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400", name, code)
			}
		}
	})

	t.Run("should answer 400 when a job names a collection id", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		uploaded := mustPut(t, srv, "photos", "a.txt", "alpha")
		rec := sendJSON(srv, http.MethodPost, "/api/v1/jobs", map[string]any{"action": "trash", "ids": []int64{uploaded.ID}})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not supported for collection files") {
			t.Fatalf("job = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("should list the events of a collection when folder names one", func(t *testing.T) {
		srv, service := setupArchiveServer(t)
		if err := service.Index().EventRecord(time.Now(), "ingest_failed", "photos", "x.txt", "boom"); err != nil {
			t.Fatal(err)
		}
		events := decode[[]catalog.Event](t, send(srv, http.MethodGet, "/api/v1/events?folder=photos", nil, nil))
		if len(events) != 1 || events[0].EventType != "ingest_failed" || events[0].Path != "x.txt" || events[0].Details != "boom" {
			t.Fatalf("events %+v", events)
		}
		for _, event := range decode[[]catalog.Event](t, send(srv, http.MethodGet, "/api/v1/events?folder=TestFolder", nil, nil)) {
			if event.EventType == "ingest_failed" {
				t.Fatalf("a collection event leaked into the legacy list: %+v", event)
			}
		}
	})

	t.Run("should include collections with kind collection in status", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		mustPut(t, srv, "photos", "a.txt", "alpha")
		mustPut(t, srv, "photos", "b.txt", "bravo!")
		trashed := mustPut(t, srv, "photos", "c.txt", "charlie")
		sendJSON(srv, http.MethodPost, "/api/v1/collections/photos/trash", map[string]any{"ids": []int64{trashed.ID}, "trashed": true})

		rec := send(srv, http.MethodGet, "/api/v1/status", nil, nil)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
		}
		status := decode[struct {
			Folders []folderStatus `json:"folders"`
			Archive *archiveStatus `json:"archive"`
		}](t, rec)
		if len(status.Folders) != 3 || status.Folders[0].Kind != "sync" || status.Folders[0].Label != "TestFolder" {
			t.Fatalf("folders %+v", status.Folders)
		}
		var photos *folderStatus
		for i := range status.Folders[1:] {
			folder := &status.Folders[1+i]
			if folder.Kind != "collection" {
				t.Errorf("%s has kind %q", folder.Label, folder.Kind)
			}
			if folder.Label == "photos" {
				photos = folder
			}
		}
		if photos == nil || photos.Counts.Total != 3 || photos.Counts.Archived != 2 || photos.Counts.Trashed != 1 ||
			photos.BudgetBytes != 0 || !photos.Enabled || !photos.PresentInTOML || photos.StoredBytes != 5+6+7 {
			t.Fatalf("photos %+v", photos)
		}
		if status.Archive == nil || status.Archive.BlobCount != 3 || status.Archive.BlobBytes != 18 ||
			status.Archive.BadBlobCount != 0 || status.Archive.JournalSeq == 0 || status.Archive.ScrubRunning {
			t.Fatalf("archive %+v", status.Archive)
		}
	})

	t.Run("should report archive null in status when the archive is not open", func(t *testing.T) {
		srv, _, _ := setupTestServer(t)
		rec := send(srv, http.MethodGet, "/api/v1/status", nil, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"archive":null`) {
			t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"kind":"sync"`) {
			t.Fatalf("legacy folder lacks kind: %s", rec.Body.String())
		}
	})

	t.Run("should treat a collection id as an ordinary missing file when no archive is open", func(t *testing.T) {
		srv, _, _ := setupTestServer(t)
		rec := send(srv, http.MethodPost, fmt.Sprintf("/api/v1/files/%d/pin", CollectionIDBase+1), nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("pin = %d, want the legacy 404", rec.Code)
		}
	})
}

func TestCollectionFileRecord(t *testing.T) {
	hash, _ := store.ParseHash(sha256Hex("x"))
	created := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	entry := index.Entry{
		Placement: index.Placement{EntryID: 9, Folder: "photos", Path: "a/b.JPG", Mtime: created.Add(-time.Hour)},
		Hash:      hash, Size: 123, FirstSeen: created, DetectedType: "jpeg",
		ThumbState: "done", Tags: nil,
	}

	t.Run("should point thumbnail_path at the blob thumbnail when the thumbnail exists", func(t *testing.T) {
		record := collectionFileRecord(entry)
		want := hash.RelPath() + ".thumb.jpg"
		if record.ThumbnailPath != want || !strings.HasPrefix(want, "sha256/") || record.DetectedType != "jpeg" {
			t.Fatalf("thumbnail %q detected %q", record.ThumbnailPath, record.DetectedType)
		}
	})

	t.Run("should leave thumbnail_path empty when no thumbnail exists", func(t *testing.T) {
		entry := entry
		entry.ThumbState = "failed"
		entry.ThumbError = "no renderer"
		record := collectionFileRecord(entry)
		if record.ThumbnailPath != "" || record.ThumbError != "no renderer" {
			t.Fatalf("record %+v", record)
		}
	})

	t.Run("should mark the record trashed from archived when the item is in trash", func(t *testing.T) {
		entry := entry
		entry.TrashedAt = &created
		record := collectionFileRecord(entry)
		if record.Status != catalog.StatusTrashed || record.TrashedFrom != catalog.StatusArchived ||
			record.TrashedAt == nil || !record.TrashedAt.Equal(created) {
			t.Fatalf("record %+v", record)
		}
	})

	t.Run("should round-trip ids through apiID and entryID", func(t *testing.T) {
		for _, id := range []int64{1, 2, 1 << 40} {
			if got := entryID(apiID(id)); got != id || !isCollectionID(apiID(id)) || isCollectionID(id) {
				t.Errorf("id %d round-tripped to %d", id, got)
			}
		}
	})
}
