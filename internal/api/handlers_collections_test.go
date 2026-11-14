package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tjmisko/coldstore/internal/archive"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/store"
)

// setupArchiveServer is setupTestServer plus an open archive core with the
// collections "photos" and "books".
func setupArchiveServer(t *testing.T) (*Server, *archive.Service) {
	t.Helper()
	srv, _, _ := setupTestServer(t)
	service, err := archive.Open(t.Context(), archive.Options{
		Root:        srv.cfg.Storage.ColdStoragePath,
		Collections: []string{"photos", "books"},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	srv.archive = service
	return srv, service
}

func sha256Hex(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func send(srv *Server, method, target string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func putFile(srv *Server, collection, path, body string) *httptest.ResponseRecorder {
	return send(srv, http.MethodPut, "/api/v1/collections/"+collection+"/files/"+path,
		strings.NewReader(body), map[string]string{headerSHA256: sha256Hex(body)})
}

func sendJSON(srv *Server, method, target string, payload any) *httptest.ResponseRecorder {
	encoded, _ := json.Marshal(payload)
	return send(srv, method, target, bytes.NewReader(encoded), map[string]string{"Content-Type": "application/json"})
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return out
}

func mustPut(t *testing.T, srv *Server, collection, path, body string) uploadResponse {
	t.Helper()
	rec := putFile(srv, collection, path, body)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("PUT %s/%s = %d %s", collection, path, rec.Code, rec.Body.String())
	}
	return decode[uploadResponse](t, rec)
}

func journalText(t *testing.T, root string) string {
	t.Helper()
	segments, err := filepath.Glob(filepath.Join(root, "journal", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, segment := range segments {
		raw, err := os.ReadFile(segment)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(raw)
	}
	return all.String()
}

func TestBlobsAndUpload(t *testing.T) {
	t.Run("should answer 200 with the size when the blob is held", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		mustPut(t, srv, "photos", "a.txt", "hello archive")

		rec := send(srv, http.MethodHead, "/api/v1/blobs/"+sha256Hex("hello archive"), nil, nil)

		if rec.Code != http.StatusOK || rec.Header().Get(headerSize) != "13" {
			t.Fatalf("HEAD = %d size %q", rec.Code, rec.Header().Get(headerSize))
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("HEAD has a body: %q", rec.Body.String())
		}
	})

	t.Run("should answer 404 when the blob is not held", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		rec := send(srv, http.MethodHead, "/api/v1/blobs/"+sha256Hex("never uploaded"), nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("HEAD = %d, want 404", rec.Code)
		}
	})

	t.Run("should answer 404 when the blob is marked bad", func(t *testing.T) {
		srv, service := setupArchiveServer(t)
		uploaded := mustPut(t, srv, "photos", "a.txt", "soon to be bad")
		hash, _ := store.ParseHash(uploaded.Hash)
		if err := service.Index().BlobBadSet(hash, true, "hash_mismatch"); err != nil {
			t.Fatal(err)
		}

		rec := send(srv, http.MethodHead, "/api/v1/blobs/"+uploaded.Hash, nil, nil)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("HEAD = %d, want 404", rec.Code)
		}
	})

	t.Run("should answer 400 when the hash is malformed", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		for _, bad := range []string{"abc", strings.Repeat("G", 64), strings.ToUpper(sha256Hex("x"))} {
			rec := send(srv, http.MethodHead, "/api/v1/blobs/"+bad, nil, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("HEAD %q = %d, want 400", bad, rec.Code)
			}
		}
	})

	t.Run("should answer 404 when no archive is open", func(t *testing.T) {
		srv, _, _ := setupTestServer(t)
		rec := send(srv, http.MethodHead, "/api/v1/blobs/"+sha256Hex("x"), nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("HEAD = %d, want 404", rec.Code)
		}
	})

	t.Run("should answer 201 and place the file when a new file is uploaded", func(t *testing.T) {
		srv, service := setupArchiveServer(t)

		rec := putFile(srv, "photos", "trip/2024/a.txt", "first bytes")

		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
		}
		got := decode[uploadResponse](t, rec)
		if got.Outcome != "created" || got.Hash != sha256Hex("first bytes") || got.Size != 11 ||
			got.Path != "trip/2024/a.txt" || got.Item == "" || !isCollectionID(got.ID) {
			t.Fatalf("response %+v", got)
		}
		placement, found, err := service.Index().PlacementAt("photos", "trip/2024/a.txt")
		if err != nil || !found || apiID(placement.EntryID) != got.ID {
			t.Fatalf("placement %+v found=%v err=%v", placement, found, err)
		}
	})

	t.Run("should answer 200 unchanged when the same file is uploaded again", func(t *testing.T) {
		srv, service := setupArchiveServer(t)
		first := mustPut(t, srv, "photos", "a.txt", "same bytes")
		seqBefore := service.Index().AppliedSeq()

		rec := putFile(srv, "photos", "a.txt", "same bytes")

		if rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
		}
		second := decode[uploadResponse](t, rec)
		if second.Outcome != "unchanged" || second.ID != first.ID || second.Item != first.Item {
			t.Fatalf("second %+v first %+v", second, first)
		}
		if service.Index().AppliedSeq() != seqBefore {
			t.Fatal("an unchanged upload appended to the journal")
		}
	})

	t.Run("should answer 422 and keep nothing when the body does not match the header hash", func(t *testing.T) {
		srv, service := setupArchiveServer(t)
		seqBefore := service.Index().AppliedSeq()

		rec := send(srv, http.MethodPut, "/api/v1/collections/photos/files/a.txt",
			strings.NewReader("these bytes"), map[string]string{headerSHA256: sha256Hex("other bytes")})

		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
		}
		stats, err := service.Index().ArchiveStats()
		if err != nil || stats.BlobCount != 0 {
			t.Fatalf("stats %+v err %v", stats, err)
		}
		if _, found, _ := service.Index().PlacementAt("photos", "a.txt"); found {
			t.Fatal("a placement exists")
		}
		if service.Index().AppliedSeq() != seqBefore || strings.Contains(journalText(t, srv.cfg.Storage.ColdStoragePath), "blob_added") {
			t.Fatal("the journal recorded the rejected upload")
		}
		var blobs int
		if err := service.Store().Walk(t.Context(), func(store.Hash, int64) error { blobs++; return nil }); err != nil || blobs != 0 {
			t.Fatalf("store holds %d blobs, err %v", blobs, err)
		}
	})

	t.Run("should answer 400 when the hash header is missing", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		for name, headers := range map[string]map[string]string{
			"absent":    nil,
			"malformed": {headerSHA256: "not-a-hash"},
			"uppercase": {headerSHA256: strings.ToUpper(sha256Hex("x"))},
		} {
			rec := send(srv, http.MethodPut, "/api/v1/collections/photos/files/a.txt", strings.NewReader("x"), headers)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: PUT = %d, want 400", name, rec.Code)
			}
		}
	})

	t.Run("should answer 400 when the mtime header is not RFC 3339", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		rec := send(srv, http.MethodPut, "/api/v1/collections/photos/files/a.txt", strings.NewReader("x"),
			map[string]string{headerSHA256: sha256Hex("x"), headerMtime: "yesterday"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT = %d, want 400", rec.Code)
		}
	})

	t.Run("should store the mtime header on the placement when it is RFC 3339", func(t *testing.T) {
		srv, service := setupArchiveServer(t)
		rec := send(srv, http.MethodPut, "/api/v1/collections/photos/files/a.txt", strings.NewReader("x"),
			map[string]string{headerSHA256: sha256Hex("x"), headerMtime: "2021-02-03T04:05:06Z"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
		}
		placement, _, _ := service.Index().PlacementAt("photos", "a.txt")
		if got := placement.Mtime.UTC().Format("2006-01-02T15:04:05Z"); got != "2021-02-03T04:05:06Z" {
			t.Fatalf("mtime %s", got)
		}
	})

	t.Run("should answer 404 when the collection is not declared", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		for _, name := range []string{"nowhere", "TestFolder"} {
			rec := putFile(srv, name, "a.txt", "x")
			if rec.Code != http.StatusNotFound {
				t.Errorf("PUT into %q = %d, want 404", name, rec.Code)
			}
		}
	})

	t.Run("should answer 404 when no archive is open", func(t *testing.T) {
		srv, _, _ := setupTestServer(t)
		if rec := putFile(srv, "photos", "a.txt", "x"); rec.Code != http.StatusNotFound {
			t.Fatalf("PUT = %d, want 404", rec.Code)
		}
	})

	t.Run("should answer 400 when the path contains a .. segment", func(t *testing.T) {
		srv, service := setupArchiveServer(t)
		attacks := map[string]string{
			"literal dot-dot":       "../escape.txt",
			"nested dot-dot":        "a/../../escape.txt",
			"encoded dot-dot":       "%2e%2e/escape.txt",
			"encoded mixed case":    "%2E%2e/escape.txt",
			"leading slash":         "/etc/passwd",
			"nul byte":              "a%00b.txt",
			"empty segment":         "a//b.txt",
			"dot segment":           "a/./b.txt",
			"trailing slash":        "a/b/",
			"overlong segment":      strings.Repeat("x", 256),
			"overlong whole path":   strings.Repeat(strings.Repeat("d", 200)+"/", 21) + "f",
			"invalid utf-8":         "caf%e9.txt",
			"encoded slash escapes": "a%2f..%2fb",
		}
		for name, attack := range attacks {
			rec := putFile(srv, "photos", attack, "payload")
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: PUT %q = %d %s, want 400", name, attack, rec.Code, rec.Body.String())
			}
		}
		stats, _ := service.Index().ArchiveStats()
		if stats.BlobCount != 0 {
			t.Fatalf("%d blobs stored by rejected paths", stats.BlobCount)
		}
		if strings.Contains(journalText(t, srv.cfg.Storage.ColdStoragePath), "placement_set") {
			t.Fatal("a rejected path was journaled")
		}
	})

	t.Run("should place a held blob without a body when Reuse is set", func(t *testing.T) {
		srv, service := setupArchiveServer(t)
		mustPut(t, srv, "photos", "original.txt", "shared bytes")

		rec := send(srv, http.MethodPut, "/api/v1/collections/photos/files/copy.txt", nil,
			map[string]string{headerSHA256: sha256Hex("shared bytes"), headerReuse: "1"})

		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
		}
		got := decode[uploadResponse](t, rec)
		if got.Outcome != "copied" || got.Path != "copy.txt" || got.Size != 12 {
			t.Fatalf("response %+v", got)
		}
		if _, found, _ := service.Index().PlacementAt("photos", "copy.txt"); !found {
			t.Fatal("no placement for the reused blob")
		}
	})

	t.Run("should answer 409 when Reuse names a blob that is not held", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		rec := send(srv, http.MethodPut, "/api/v1/collections/photos/files/copy.txt", nil,
			map[string]string{headerSHA256: sha256Hex("not held"), headerReuse: "1"})
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "blob not held") {
			t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("should accept a body larger than any JSON body cap", func(t *testing.T) {
		const bodyBytes = 4 << 20 // above the 1 MiB trash cap and the 128 KiB receipt cap
		srv, service := setupArchiveServer(t)
		sum := sha256.New()
		if _, err := io.Copy(sum, newPatternReader(bodyBytes)); err != nil {
			t.Fatal(err)
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)

		rec := send(srv, http.MethodPut, "/api/v1/collections/photos/files/big.bin", newPatternReader(bodyBytes),
			map[string]string{headerSHA256: hex.EncodeToString(sum.Sum(nil))})

		runtime.ReadMemStats(&after)
		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
		}
		if got := decode[uploadResponse](t, rec); got.Size != bodyBytes {
			t.Fatalf("size %d", got.Size)
		}
		// A handler that buffered the body would allocate at least its size, and
		// more once the buffer grew. Streaming stays well under it.
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated >= bodyBytes {
			t.Fatalf("allocated %d bytes for a %d byte body: the body was buffered", allocated, bodyBytes)
		}
		if _, found, _ := service.Index().PlacementAt("photos", "big.bin"); !found {
			t.Fatal("no placement")
		}
	})

	t.Run("should record the Tailscale login as the journal actor", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		srv.cfg.Server.ListenAddr = config.DefaultListenAddr
		srv.cfg.Server.AuthMode = config.AuthModeTailscale
		srv.cfg.Server.TailscaleAllowedUsers = []string{tailscaleLogin}

		rec := send(srv, http.MethodPut, "/api/v1/collections/photos/files/a.txt", strings.NewReader("who did this"),
			map[string]string{headerSHA256: sha256Hex("who did this"), tailscaleUserLoginHeader: tailscaleLogin})

		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(journalText(t, srv.cfg.Storage.ColdStoragePath), `"by":"`+tailscaleLogin+`"`) {
			t.Fatal("the journal does not name the Tailscale login as the actor")
		}
	})

	t.Run("should answer 503 with Retry-After when the archive is shutting down", func(t *testing.T) {
		srv, service := setupArchiveServer(t)
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		rec := putFile(srv, "photos", "a.txt", "late")
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" {
			t.Fatalf("PUT = %d retry-after %q", rec.Code, rec.Header().Get("Retry-After"))
		}
	})
}

// patternReader yields n bytes of a repeating pattern without allocating.
type patternReader struct{ remaining int }

func newPatternReader(n int) *patternReader { return &patternReader{remaining: n} }

func (p *patternReader) Read(buf []byte) (int, error) {
	if p.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(buf), p.remaining)
	for i := range n {
		buf[i] = byte((p.remaining - i) * 31)
	}
	p.remaining -= n
	return n, nil
}

func TestReceiptAndTrash(t *testing.T) {
	receipt := func(srv *Server, collection string, hashes []string) *httptest.ResponseRecorder {
		return sendJSON(srv, http.MethodPost, "/api/v1/collections/"+collection+"/receipt", hashes)
	}

	t.Run("should report every hash as held and verified when the batch was uploaded", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		var hashes []string
		for i := range 3 {
			body := fmt.Sprintf("file number %d", i)
			mustPut(t, srv, "photos", fmt.Sprintf("f%d.txt", i), body)
			hashes = append(hashes, sha256Hex(body))
		}

		rec := receipt(srv, "photos", hashes)

		if rec.Code != http.StatusOK {
			t.Fatalf("receipt = %d %s", rec.Code, rec.Body.String())
		}
		got := decode[receiptResponse](t, rec)
		if !got.AllVerified || len(got.Results) != 3 {
			t.Fatalf("receipt %+v", got)
		}
		for i, line := range got.Results {
			if line.Hash != hashes[i] || !line.Held || !line.Verified || !line.InCollection || line.Size == 0 {
				t.Errorf("line %d: %+v", i, line)
			}
		}
	})

	t.Run("should report held false when a hash was never uploaded", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		mustPut(t, srv, "photos", "a.txt", "present")

		got := decode[receiptResponse](t, receipt(srv, "photos", []string{sha256Hex("present"), sha256Hex("absent")}))

		if got.AllVerified || !got.Results[0].Held || got.Results[1].Held || got.Results[1].Verified {
			t.Fatalf("receipt %+v", got)
		}
	})

	t.Run("should report in_collection false when the blob is held only by another collection", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		mustPut(t, srv, "books", "a.txt", "other collection")

		got := decode[receiptResponse](t, receipt(srv, "photos", []string{sha256Hex("other collection")}))

		line := got.Results[0]
		if !line.Held || !line.Verified || line.InCollection || got.AllVerified {
			t.Fatalf("receipt %+v", got)
		}
	})

	t.Run("should answer 400 when more than 1000 hashes are sent", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		hashes := make([]string, 1001)
		for i := range hashes {
			hashes[i] = sha256Hex(fmt.Sprint(i))
		}
		if rec := receipt(srv, "photos", hashes); rec.Code != http.StatusBadRequest {
			t.Fatalf("1001 hashes = %d, want 400", rec.Code)
		}
		if rec := receipt(srv, "photos", nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("0 hashes = %d, want 400", rec.Code)
		}
		if rec := receipt(srv, "photos", []string{"nope"}); rec.Code != http.StatusBadRequest {
			t.Fatalf("malformed hash = %d, want 400", rec.Code)
		}
		if rec := receipt(srv, "undeclared", []string{sha256Hex("x")}); rec.Code != http.StatusNotFound {
			t.Fatalf("unknown collection = %d, want 404", rec.Code)
		}
	})

	t.Run("should answer 400 when the receipt body is over the cap", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		huge := `["` + strings.Repeat("a", maxReceiptBodyBytes+1) + `"]`
		rec := send(srv, http.MethodPost, "/api/v1/collections/photos/receipt", strings.NewReader(huge), nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("receipt = %d, want 400", rec.Code)
		}
	})

	t.Run("should move the files to trash and back when trash is toggled", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		first := mustPut(t, srv, "photos", "a.txt", "alpha")
		mustPut(t, srv, "photos", "b.txt", "bravo")
		listed := func(status string) int {
			rec := send(srv, http.MethodGet, "/api/v1/files?folder=photos&status="+status, nil, nil)
			return len(decode[[]map[string]any](t, rec))
		}

		rec := sendJSON(srv, http.MethodPost, "/api/v1/collections/photos/trash",
			map[string]any{"ids": []int64{first.ID}, "trashed": true})
		if rec.Code != http.StatusOK || decode[map[string]int](t, rec)["updated"] != 1 {
			t.Fatalf("trash = %d %s", rec.Code, rec.Body.String())
		}
		if listed("archived") != 1 || listed("trashed") != 1 {
			t.Fatal("the file did not move to trash")
		}

		rec = sendJSON(srv, http.MethodPost, "/api/v1/collections/photos/trash",
			map[string]any{"ids": []int64{first.ID}, "trashed": false})
		if rec.Code != http.StatusOK || decode[map[string]int](t, rec)["updated"] != 1 {
			t.Fatalf("untrash = %d %s", rec.Code, rec.Body.String())
		}
		if listed("archived") != 2 || listed("trashed") != 0 {
			t.Fatal("the file did not come back from trash")
		}
	})

	t.Run("should answer 400 when an id belongs to another collection", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		mine := mustPut(t, srv, "photos", "a.txt", "alpha")
		theirs := mustPut(t, srv, "books", "b.txt", "bravo")

		for name, ids := range map[string][]int64{
			"other collection": {mine.ID, theirs.ID},
			"legacy id":        {mine.ID, 7},
			"unknown entry":    {mine.ID, apiID(999999)},
		} {
			rec := sendJSON(srv, http.MethodPost, "/api/v1/collections/photos/trash", map[string]any{"ids": ids, "trashed": true})
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: trash = %d %s, want 400", name, rec.Code, rec.Body.String())
			}
		}
		if got := send(srv, http.MethodGet, "/api/v1/files?folder=photos&status=trashed", nil, nil); strings.Contains(got.Body.String(), "a.txt") {
			t.Fatal("a rejected request still trashed a file")
		}
	})

	t.Run("should answer 400 when trash is missing the trashed flag or has no ids", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		first := mustPut(t, srv, "photos", "a.txt", "alpha")
		for name, payload := range map[string]any{
			"no flag": map[string]any{"ids": []int64{first.ID}},
			"no ids":  map[string]any{"ids": []int64{}, "trashed": true},
		} {
			if rec := sendJSON(srv, http.MethodPost, "/api/v1/collections/photos/trash", payload); rec.Code != http.StatusBadRequest {
				t.Errorf("%s: trash = %d, want 400", name, rec.Code)
			}
		}
	})
}

func downloadServer(t *testing.T) (*httptest.Server, *archive.Service, *Server) {
	t.Helper()
	srv, service := setupArchiveServer(t)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, service, srv
}

func damageBlob(t *testing.T, service *archive.Service, hexHash string, damage func(path string)) {
	t.Helper()
	hash, err := store.ParseHash(hexHash)
	if err != nil {
		t.Fatal(err)
	}
	path := service.Store().Path(hash)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	damage(path)
}

func TestVerifiedDownload(t *testing.T) {
	const body = "the exact bytes of a small file"

	t.Run("should deliver the exact bytes with a matching ETag when the blob is intact", func(t *testing.T) {
		ts, _, srv := downloadServer(t)
		uploaded := mustPut(t, srv, "photos", "dir/a.txt", body)

		resp, err := http.Get(fmt.Sprintf("%s/api/v1/files/%d/download", ts.URL, uploaded.ID))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)

		if err != nil || string(got) != body {
			t.Fatalf("body %q err %v", got, err)
		}
		if resp.StatusCode != http.StatusOK || resp.ContentLength != int64(len(body)) ||
			resp.Header.Get("ETag") != `"sha256:`+sha256Hex(body)+`"` ||
			!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
			t.Fatalf("status %d length %d headers %v", resp.StatusCode, resp.ContentLength, resp.Header)
		}
	})

	t.Run("should deliver every byte when the blob spans several chunks", func(t *testing.T) {
		ts, _, srv := downloadServer(t)
		big := strings.Repeat("0123456789abcdef", (2*downloadChunkBytes+12345)/16)
		uploaded := mustPut(t, srv, "photos", "big.bin", big)

		resp, err := http.Get(fmt.Sprintf("%s/api/v1/files/%d/download", ts.URL, uploaded.ID))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)

		if err != nil || len(got) != len(big) || string(got) != big {
			t.Fatalf("read %d of %d bytes, err %v", len(got), len(big), err)
		}
	})

	t.Run("should deliver an empty body when the blob is zero-length", func(t *testing.T) {
		ts, _, srv := downloadServer(t)
		uploaded := mustPut(t, srv, "photos", "empty.txt", "")

		resp, err := http.Get(fmt.Sprintf("%s/api/v1/files/%d/download", ts.URL, uploaded.ID))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)

		if err != nil || len(got) != 0 || resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d body %q err %v", resp.StatusCode, got, err)
		}
	})

	t.Run("should fail the transfer short of Content-Length when one byte of the blob is flipped", func(t *testing.T) {
		ts, service, srv := downloadServer(t)
		uploaded := mustPut(t, srv, "photos", "a.txt", body)
		damageBlob(t, service, uploaded.Hash, func(path string) {
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteAt([]byte{'X'}, 3); err != nil {
				t.Fatal(err)
			}
		})
		url := fmt.Sprintf("%s/api/v1/files/%d/download", ts.URL, uploaded.ID)

		received, readErr := readAll(url)

		if readErr == nil {
			t.Fatalf("the client read %d bytes cleanly from a damaged blob", received)
		}
		if received >= len(body) {
			t.Fatalf("the client received all %d bytes of damaged data", received)
		}
		hash, _ := store.ParseHash(uploaded.Hash)
		info, _, _ := service.Index().Blob(hash)
		if !info.Bad {
			t.Fatal("the blob was not marked bad")
		}
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("the next request = %d, want 503", resp.StatusCode)
		}
	})

	t.Run("should fail the transfer when the blob is truncated", func(t *testing.T) {
		ts, service, srv := downloadServer(t)
		uploaded := mustPut(t, srv, "photos", "a.txt", body)
		damageBlob(t, service, uploaded.Hash, func(path string) {
			if err := os.Truncate(path, 5); err != nil {
				t.Fatal(err)
			}
		})

		received, readErr := readAll(fmt.Sprintf("%s/api/v1/files/%d/download", ts.URL, uploaded.ID))

		if readErr == nil || received >= len(body) {
			t.Fatalf("received %d bytes, err %v", received, readErr)
		}
	})

	t.Run("should answer 503 when the blob is already marked bad", func(t *testing.T) {
		srv, service := setupArchiveServer(t)
		uploaded := mustPut(t, srv, "photos", "a.txt", body)
		hash, _ := store.ParseHash(uploaded.Hash)
		if err := service.Index().BlobBadSet(hash, true, "hash_mismatch"); err != nil {
			t.Fatal(err)
		}

		rec := send(srv, http.MethodGet, fmt.Sprintf("/api/v1/files/%d/download", uploaded.ID), nil, nil)

		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "blob failed verification") {
			t.Fatalf("download = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("should answer 404-class and never 200 when the blob file is deleted", func(t *testing.T) {
		srv, service := setupArchiveServer(t)
		uploaded := mustPut(t, srv, "photos", "a.txt", body)
		damageBlob(t, service, uploaded.Hash, func(path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		})

		rec := send(srv, http.MethodGet, fmt.Sprintf("/api/v1/files/%d/download", uploaded.ID), nil, nil)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("download = %d %s, want 404", rec.Code, rec.Body.String())
		}
	})

	t.Run("should answer 404 when the entry does not exist", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		rec := send(srv, http.MethodGet, fmt.Sprintf("/api/v1/files/%d/download", apiID(4242)), nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("download = %d, want 404", rec.Code)
		}
	})

	t.Run("should serve a range from preview when the blob is intact", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		uploaded := mustPut(t, srv, "photos", "notes.txt", "0123456789")

		rec := send(srv, http.MethodGet, fmt.Sprintf("/api/v1/files/%d/preview", uploaded.ID), nil, nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "0123456789" {
			t.Fatalf("preview = %d %q", rec.Code, rec.Body.String())
		}
		if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("content type %q", rec.Header().Get("Content-Type"))
		}
	})

	t.Run("should serve a byte range from preview when the file is a type that is ranged", func(t *testing.T) {
		srv, _ := setupArchiveServer(t)
		pdf := "%PDF-1.4\n" + strings.Repeat("x", 64)
		uploaded := mustPut(t, srv, "photos", "doc.pdf", pdf)

		rec := send(srv, http.MethodGet, fmt.Sprintf("/api/v1/files/%d/preview", uploaded.ID), nil,
			map[string]string{"Range": "bytes=2-5"})

		if rec.Code != http.StatusPartialContent || rec.Body.String() != pdf[2:6] {
			t.Fatalf("preview = %d %q", rec.Code, rec.Body.String())
		}
	})

	t.Run("should answer 503 from preview when the blob is marked bad", func(t *testing.T) {
		srv, service := setupArchiveServer(t)
		uploaded := mustPut(t, srv, "photos", "notes.txt", "0123456789")
		hash, _ := store.ParseHash(uploaded.Hash)
		if err := service.Index().BlobBadSet(hash, true, "hash_mismatch"); err != nil {
			t.Fatal(err)
		}

		rec := send(srv, http.MethodGet, fmt.Sprintf("/api/v1/files/%d/preview", uploaded.ID), nil, nil)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("preview = %d, want 503", rec.Code)
		}
	})
}

// readAll reads a response body to the end and reports how many bytes arrived
// and the first error, whether it came from the request or from the body.
func readAll(url string) (int, error) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	return len(got), err
}
