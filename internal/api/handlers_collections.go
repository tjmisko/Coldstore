package api

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tjmisko/coldstore/internal/archive"
	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/ingest"
	"github.com/tjmisko/coldstore/internal/store"
)

const (
	collectionsRoutePrefix = "/api/v1/collections/"

	headerSHA256 = "X-Coldstore-Sha256"
	headerMtime  = "X-Coldstore-Mtime"
	headerReuse  = "X-Coldstore-Reuse"
	headerSize   = "X-Coldstore-Size"

	maxReceiptHashes    = 1000
	maxReceiptBodyBytes = 128 << 10
	maxTrashBodyBytes   = 1 << 20

	downloadChunkBytes = 1 << 20
	retryAfterShutdown = "30"

	errBlobFailed = "blob failed verification"
)

var downloadChunks = sync.Pool{New: func() any { b := make([]byte, downloadChunkBytes); return &b }}

// handleHeadBlob answers whether the journal names a blob, it is on disk and it
// is not marked bad. It has no body, so an uploader can skip bytes the archive
// already holds.
func (s *Server) handleHeadBlob(w http.ResponseWriter, r *http.Request) {
	hash, err := store.ParseHash(r.PathValue("sha256"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "malformed sha256")
		return
	}
	if s.archive == nil {
		writeError(w, http.StatusNotFound, "blob not held")
		return
	}
	info, held, err := s.archive.Index().Blob(hash)
	if err != nil {
		s.logger.Error("reading blob record", "hash", hash.String(), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read blob")
		return
	}
	if !held || info.Bad {
		writeError(w, http.StatusNotFound, "blob not held")
		return
	}
	_, onDisk, err := s.archive.Store().Has(hash)
	if err != nil {
		// Only a missing file means "not held"; any other stat failure says
		// nothing about the blob, so the uploader must not be told to re-send.
		s.logger.Error("checking blob on disk", "hash", hash.String(), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to check blob")
		return
	}
	if !onDisk {
		writeError(w, http.StatusNotFound, "blob not held")
		return
	}
	w.Header().Set(headerSize, strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
}

// collectionUploadPath splits /api/v1/collections/{name}/files/{path...} by
// hand: the mux never sees this request (see bypassMuxPathCleaning).
func collectionUploadPath(urlPath string) (name, filePath string, ok bool) {
	rest := strings.TrimPrefix(urlPath, collectionsRoutePrefix)
	name, rest, found := strings.Cut(rest, "/")
	if !found || name == "" {
		return "", "", false
	}
	filePath, found = strings.CutPrefix(rest, "files/")
	if !found {
		return "", "", false
	}
	return name, filePath, true
}

type uploadResponse struct {
	ID      int64          `json:"id"`
	Item    string         `json:"item"`
	Hash    string         `json:"hash"`
	Size    int64          `json:"size"`
	Path    string         `json:"path"`
	Outcome ingest.Outcome `json:"outcome"`
}

// handlePutCollectionFile stores the request body as a new file in a
// collection. The body is streamed straight into the blob store and has NO size
// cap: a 50 GB video is an ordinary upload here. Every header and the path are
// validated before the first body byte is read.
func (s *Server) handlePutCollectionFile(w http.ResponseWriter, r *http.Request) {
	name, filePath, ok := collectionUploadPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "no such endpoint")
		return
	}
	if s.archive == nil || !s.archive.CollectionDeclared(name) {
		writeError(w, http.StatusNotFound, "no such collection")
		return
	}
	if _, err := ingest.CleanPath(filePath); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := store.ParseHash(r.Header.Get(headerSHA256))
	if err != nil {
		writeError(w, http.StatusBadRequest, headerSHA256+" must be 64 lowercase hex characters")
		return
	}
	mtime := time.Now().UTC()
	if raw := r.Header.Get(headerMtime); raw != "" {
		mtime, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, headerMtime+" must be RFC 3339")
			return
		}
	}

	actor := identityFromRequest(r).Login
	if actor == "" {
		actor = "api"
	}
	var result ingest.Result
	if r.Header.Get(headerReuse) == "1" {
		result, err = s.archive.Place(r.Context(), actor, name, filePath, mtime, hash)
	} else {
		result, err = s.archive.Ingest(r.Context(), actor, ingest.Arrival{
			Folder: name, Path: filePath, Mtime: mtime, Want: &hash,
			Open: func() (io.ReadCloser, error) { return r.Body, nil },
		})
	}
	if err != nil {
		s.writeUploadError(w, r, err)
		return
	}
	status := http.StatusCreated
	if result.Outcome == ingest.OutcomeUnchanged {
		status = http.StatusOK
	}
	writeJSON(w, status, uploadResponse{
		ID: apiID(result.EntryID), Item: string(result.Item), Hash: result.Hash.String(),
		Size: result.Size, Path: result.Path, Outcome: result.Outcome,
	})
}

func (s *Server) writeUploadError(w http.ResponseWriter, r *http.Request, err error) {
	var corrupt *store.CorruptError
	switch {
	case errors.Is(err, archive.ErrBlobNotHeld):
		writeError(w, http.StatusConflict, "blob not held")
	case errors.Is(err, store.ErrHashMismatch):
		writeError(w, http.StatusUnprocessableEntity, "body does not match "+headerSHA256)
	case errors.Is(err, syscall.ENOSPC):
		writeError(w, http.StatusInsufficientStorage, "archive volume is full")
	case errors.Is(err, archive.ErrCollectionUnknown):
		writeError(w, http.StatusNotFound, "no such collection")
	case errors.Is(err, ingest.ErrBadPath):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, archive.ErrClosed):
		w.Header().Set("Retry-After", retryAfterShutdown)
		writeError(w, http.StatusServiceUnavailable, "shutting down")
	case errors.Is(err, archive.ErrBlobBad), errors.As(err, &corrupt):
		writeError(w, http.StatusServiceUnavailable, errBlobFailed)
	case r.Context().Err() != nil:
		// The client went away; nothing is listening for an answer.
		s.logger.Info("upload abandoned by the client", "path", r.URL.Path, "error", err)
	default:
		s.logger.Error("collection upload failed", "path", r.URL.Path, "error", err)
		writeError(w, http.StatusInternalServerError, "upload failed")
	}
}

// requireCollection answers 404 and reports false unless name is a declared
// collection of an open archive.
func (s *Server) requireCollection(w http.ResponseWriter, name string) bool {
	if s.archive != nil && s.archive.CollectionDeclared(name) {
		return true
	}
	writeError(w, http.StatusNotFound, "no such collection")
	return false
}

type receiptResponse struct {
	Results     []archive.ReceiptLine `json:"results"`
	AllVerified bool                  `json:"all_verified"`
}

func (s *Server) handleCollectionReceipt(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.requireCollection(w, name) {
		return
	}
	var raw []string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReceiptBodyBytes)).Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "body must be a JSON array of hex hashes")
		return
	}
	if len(raw) == 0 || len(raw) > maxReceiptHashes {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("between 1 and %d hashes are required", maxReceiptHashes))
		return
	}
	hashes := make([]store.Hash, len(raw))
	for i, text := range raw {
		hash, err := store.ParseHash(text)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("hash %d is malformed", i))
			return
		}
		hashes[i] = hash
	}
	lines, err := s.archive.Receipt(r.Context(), name, hashes)
	if err != nil {
		s.writeUploadError(w, r, err)
		return
	}
	allVerified := true
	for _, line := range lines {
		allVerified = allVerified && line.Held && line.Verified && line.InCollection
	}
	writeJSON(w, http.StatusOK, receiptResponse{Results: lines, AllVerified: allVerified})
}

type collectionTrashRequest struct {
	IDs     []int64 `json:"ids"`
	Trashed *bool   `json:"trashed"`
}

func (s *Server) handleCollectionTrash(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.requireCollection(w, name) {
		return
	}
	var req collectionTrashRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTrashBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Trashed == nil {
		writeError(w, http.StatusBadRequest, "trashed must be true or false")
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > maxJobIDs {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("between 1 and %d ids are required", maxJobIDs))
		return
	}
	if legacy, _ := idKinds(req.IDs); legacy > 0 {
		writeError(w, http.StatusBadRequest, "every id must belong to collection "+name)
		return
	}
	entryIDList := entryIDs(req.IDs)
	if !s.entriesBelongTo(w, name, entryIDList) {
		return
	}
	updated, err := s.archive.Trash(r.Context(), actorOf(r), entryIDList, *req.Trashed)
	if err != nil {
		s.writeUploadError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"updated": updated})
}

func actorOf(r *http.Request) string {
	if login := identityFromRequest(r).Login; login != "" {
		return login
	}
	return "api"
}

// entriesBelongTo answers 400 and reports false unless every entry id exists
// and lives in the named collection.
func (s *Server) entriesBelongTo(w http.ResponseWriter, collection string, ids []int64) bool {
	entries, err := s.archive.Index().Entries(index.EntryQuery{IDs: ids})
	if err != nil {
		s.logger.Error("reading entries", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read files")
		return false
	}
	found := make(map[int64]string, len(entries))
	for _, e := range entries {
		found[e.EntryID] = e.Folder
	}
	for _, id := range ids {
		if folder, ok := found[id]; !ok || folder != collection {
			writeError(w, http.StatusBadRequest, "every id must belong to collection "+collection)
			return false
		}
	}
	return true
}

// collectionEntryForRequest finds the entry the request's {id} names. It
// answers the request itself and reports false when it cannot.
func (s *Server) collectionEntryForRequest(w http.ResponseWriter, id int64) (index.Entry, bool) {
	entry, found, err := s.archive.Index().Entry(entryID(id))
	if err != nil {
		s.logger.Error("reading collection entry", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read file")
		return index.Entry{}, false
	}
	if !found {
		writeError(w, http.StatusNotFound, "file not found")
		return index.Entry{}, false
	}
	return entry, true
}

// resolveCollectionBytes is resolveFileBytes for a collection entry: the path
// is the blob itself. A blob marked bad is refused with 503; a blob the index
// names but the disk has lost is marked bad and answered 404.
func (s *Server) resolveCollectionBytes(w http.ResponseWriter, id int64) (*catalog.FileRecord, string, bool) {
	entry, ok := s.collectionEntryForRequest(w, id)
	if !ok {
		return nil, "", false
	}
	if entry.BlobBad {
		writeError(w, http.StatusServiceUnavailable, errBlobFailed)
		return nil, "", false
	}
	blobPath := s.archive.Store().Path(entry.Hash)
	if _, err := os.Stat(blobPath); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// EMFILE, EACCES, ...: nothing is known about the blob. Mark nothing.
			s.logger.Error("checking blob file", "hash", entry.Hash.String(), "error", err)
			writeError(w, http.StatusInternalServerError, "failed to read file")
			return nil, "", false
		}
		s.markBlobBad(entry.Hash, "missing")
		writeError(w, http.StatusNotFound, "blob file is missing")
		return nil, "", false
	}
	record := collectionFileRecord(entry)
	return &record, blobPath, true
}

func (s *Server) markBlobBad(hash store.Hash, reason string) {
	ix := s.archive.Index()
	if err := ix.BlobBadSet(hash, true, reason); err != nil {
		s.logger.Warn("marking blob bad", "hash", hash.String(), "error", err)
	}
	if err := ix.EventRecord(time.Now().UTC(), "blob_bad", "", "", hash.String()+": "+reason); err != nil {
		s.logger.Warn("recording blob_bad", "hash", hash.String(), "error", err)
	}
}

// downloadCollectionFile is the verified full-body download.
func (s *Server) downloadCollectionFile(w http.ResponseWriter, id int64) {
	entry, ok := s.collectionEntryForRequest(w, id)
	if !ok {
		return
	}
	blob, size, err := s.archive.OpenVerified(entry.Hash)
	switch {
	case errors.Is(err, archive.ErrBlobBad):
		writeError(w, http.StatusServiceUnavailable, errBlobFailed)
		return
	case errors.Is(err, archive.ErrClosed):
		w.Header().Set("Retry-After", retryAfterShutdown)
		writeError(w, http.StatusServiceUnavailable, "shutting down")
		return
	case errors.Is(err, archive.ErrBlobNotHeld), errors.Is(err, store.ErrNotFound), errors.Is(err, os.ErrNotExist):
		s.markBlobBad(entry.Hash, "missing")
		writeError(w, http.StatusNotFound, "blob file is missing")
		return
	case err != nil:
		s.logger.Error("opening blob for download", "hash", entry.Hash.String(), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read file")
		return
	}
	defer blob.Close()

	header := w.Header()
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Content-Length", strconv.FormatInt(size, 10))
	header.Set("ETag", `"sha256:`+entry.Hash.String()+`"`)
	header.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, sanitizeFilename(entry.Path)))
	header.Set("X-Content-Type-Options", "nosniff")
	s.streamVerified(w, blob, size, entry.Hash)
}

// streamVerified copies blob to w through SHA-256 while holding back the final
// chunk. Once the last byte has been read from disk the hash is compared: on a
// match the held chunk is written; on a mismatch the blob is marked bad and the
// handler aborts, so the client receives fewer bytes than Content-Length and
// its read fails instead of ending cleanly on damaged bytes. A zero-length blob
// has nothing to hold back.
func (s *Server) streamVerified(w http.ResponseWriter, blob io.Reader, size int64, want store.Hash) {
	currentBuf, heldBuf := downloadChunks.Get().(*[]byte), downloadChunks.Get().(*[]byte)
	defer downloadChunks.Put(currentBuf)
	defer downloadChunks.Put(heldBuf)
	current, held := *currentBuf, *heldBuf
	heldLen := 0

	hasher := sha256.New()
	var total int64
	failure := ""
	for failure == "" {
		n, err := io.ReadFull(blob, current)
		if n > 0 {
			hasher.Write(current[:n])
			total += int64(n)
			if heldLen > 0 {
				if _, writeErr := w.Write(held[:heldLen]); writeErr != nil {
					return // the client left
				}
			}
			current, held = held, current
			heldLen = n
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			s.logger.Warn("reading blob for download", "hash", want.String(), "error", err)
			if !errors.Is(err, syscall.EIO) {
				// Not device evidence: say nothing about the blob, just abort.
				panic(http.ErrAbortHandler)
			}
			failure = "unreadable"
		}
	}
	if failure == "" {
		var got store.Hash
		copy(got[:], hasher.Sum(nil))
		switch {
		case total != size:
			failure = "truncated"
		case got != want:
			failure = "hash_mismatch"
		}
	}
	if failure != "" {
		s.markBlobBad(want, failure)
		s.logger.Error("blob failed verification during download", "hash", want.String(), "reason", failure)
		panic(http.ErrAbortHandler)
	}
	if heldLen > 0 {
		_, _ = w.Write(held[:heldLen])
	}
}
