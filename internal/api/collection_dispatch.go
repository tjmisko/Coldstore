package api

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/tjmisko/coldstore/internal/archive"
	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/journal"
)

// CollectionIDBase is the first API file id a collection entry can have. A
// collection entry is one Placement; its id is CollectionIDBase plus the seq of
// the placement_set record that created it. 1<<52 is below JavaScript's
// Number.MAX_SAFE_INTEGER and far above any legacy SQLite rowid, so one id
// space serves the existing /api/v1/files/{id}/... routes without a collision.
const CollectionIDBase int64 = 1 << 52

const (
	collectionKindSync       = "sync"
	collectionKindCollection = "collection"
	errNotSupportedForFiles  = "not supported for collection files"
	errMixedIDs              = "ids mix legacy and collection files"
	collectionThumbSuffix    = ".thumb.jpg"
)

func isCollectionID(id int64) bool { return id >= CollectionIDBase }
func entryID(id int64) int64       { return id - CollectionIDBase }
func apiID(entryID int64) int64    { return CollectionIDBase + entryID }

// collectionFolder reports whether name is a collection. It is false for every
// name when no archive is open. A collection that was removed from
// coldstore.toml stays a collection while the journal still names it, so its
// files remain browsable.
func (s *Server) collectionFolder(name string) bool {
	if s.archive == nil || name == "" {
		return false
	}
	if s.archive.CollectionDeclared(name) {
		return true
	}
	folder, found, err := s.archive.Index().FolderByName(name)
	return err == nil && found && folder.Kind == journal.FolderKindCollection
}

// rejectCollectionID answers 400 and reports true when id names a collection
// file. It is the single guard the legacy-only handlers (pin, restore, archive,
// resolve, edit) carry. With no archive open it never fires.
func (s *Server) rejectCollectionID(w http.ResponseWriter, id int64) bool {
	if s.archive == nil || !isCollectionID(id) {
		return false
	}
	writeError(w, http.StatusBadRequest, errNotSupportedForFiles)
	return true
}

// rejectCollectionIDs is rejectCollectionID for a list of ids.
func (s *Server) rejectCollectionIDs(w http.ResponseWriter, ids []int64) bool {
	if s.archive == nil {
		return false
	}
	for _, id := range ids {
		if isCollectionID(id) {
			writeError(w, http.StatusBadRequest, errNotSupportedForFiles)
			return true
		}
	}
	return false
}

// idKinds counts how many of ids are legacy and how many are collection ids.
func idKinds(ids []int64) (legacy, collection int) {
	for _, id := range ids {
		if isCollectionID(id) {
			collection++
			continue
		}
		legacy++
	}
	return legacy, collection
}

func entryIDs(ids []int64) []int64 {
	entries := make([]int64, len(ids))
	for i, id := range ids {
		entries[i] = entryID(id)
	}
	return entries
}

// bypassMuxPathCleaning hands PUT /api/v1/collections/... straight to the
// upload handler. http.ServeMux answers any path containing "." or ".." segments
// or repeated slashes with a redirect to the cleaned path; for an upload that
// would turn a hostile path into a 301 the client might follow, instead of the
// 400 the route promises. The handler validates the path itself.
func (s *Server) bypassMuxPathCleaning(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, collectionsRoutePrefix) {
			s.handlePutCollectionFile(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// collectionFileRecord renders one entry in the catalog.FileRecord JSON shape
// so the existing UI components need no second model.
func collectionFileRecord(e index.Entry) catalog.FileRecord {
	filename := path.Base(e.Path)
	extension := strings.ToLower(path.Ext(filename))
	record := catalog.FileRecord{
		ID:           apiID(e.EntryID),
		FolderLabel:  e.Folder,
		RelativePath: e.Path,
		Filename:     filename,
		Extension:    extension,
		MimeType:     mime.TypeByExtension(extension),
		SizeBytes:    e.Size,
		FileMtime:    e.Mtime,
		FileAtime:    e.FirstSeen,
		FirstSeen:    e.FirstSeen,
		Status:       catalog.StatusArchived,
		ContentHash:  e.Hash.String(),
		PinSource:    "",
		DetectedType: e.DetectedType,
		ThumbError:   e.ThumbError,
		Description:  e.Description,
		Tags:         e.Tags,
	}
	if record.Tags == nil {
		record.Tags = []string{}
	}
	if e.ThumbState == "done" {
		record.ThumbnailPath = e.Hash.RelPath() + collectionThumbSuffix
	}
	if e.TrashedAt != nil {
		trashedAt := *e.TrashedAt
		record.Status = catalog.StatusTrashed
		record.TrashedAt = &trashedAt
		record.TrashedFrom = catalog.StatusArchived
	}
	return record
}

func collectionFileRecords(entries []index.Entry) []catalog.FileRecord {
	records := make([]catalog.FileRecord, 0, len(entries))
	for _, e := range entries {
		records = append(records, collectionFileRecord(e))
	}
	return records
}

// unsupportedForCollections are the filter parameters that have no meaning for a
// collection in M1 (no capture metadata, pins or conflicts).
var unsupportedForCollections = []string{
	"captured_from", "captured_to", "undated", "near", "pinned", "pin_source", "conflict", "described",
}

// requestTargetsCollection decides whether a files query is about collection
// entries. It answers the request itself (and reports ok == false) when the
// ids mix legacy and collection files. An unparsable ids list is left to the
// legacy parser to report.
func (s *Server) requestTargetsCollection(w http.ResponseWriter, r *http.Request) (collection, ok bool) {
	if s.archive == nil {
		return false, true
	}
	v := r.URL.Query()
	folderIsCollection := s.collectionFolder(v.Get("folder"))
	raw := v.Get("ids")
	if raw == "" {
		return folderIsCollection, true
	}
	ids, err := parseIDList(raw)
	if err != nil {
		return false, true
	}
	legacy, collectionCount := idKinds(ids)
	if legacy > 0 && collectionCount > 0 {
		writeError(w, http.StatusBadRequest, errMixedIDs)
		return false, false
	}
	if collectionCount > 0 {
		return true, true
	}
	if folderIsCollection {
		writeError(w, http.StatusBadRequest, errMixedIDs)
		return false, false
	}
	return false, true
}

// parseCollectionQuery reads the filter half of a files query for a
// collection. It reports false when it has already answered the request, and
// emptyPage true when the status filter selects nothing a collection can hold.
func parseCollectionQuery(w http.ResponseWriter, r *http.Request, idCap int) (q index.EntryQuery, emptyPage, ok bool) {
	v := r.URL.Query()
	for _, name := range unsupportedForCollections {
		if v.Get(name) != "" {
			writeError(w, http.StatusBadRequest, name+" is not supported for collections")
			return q, false, false
		}
	}
	tags, err := catalog.NormalizeTagFilters(v["tag"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return q, false, false
	}
	if len(tags) > catalog.MaxTagFilters {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d tag filters per request", catalog.MaxTagFilters))
		return q, false, false
	}
	q = index.EntryQuery{
		Folder:    v.Get("folder"),
		Extension: v.Get("ext"),
		Search:    v.Get("q"),
		Tags:      tags,
	}
	switch v.Get("untagged") {
	case "":
	case "0", "1":
		untagged := v.Get("untagged") == "1"
		q.Untagged = &untagged
	default:
		writeError(w, http.StatusBadRequest, `untagged must be "1" or "0"`)
		return q, false, false
	}
	if raw := v.Get("ids"); raw != "" {
		ids, err := parseIDList(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid ids")
			return q, false, false
		}
		if len(ids) > idCap {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d ids per request", idCap))
			return q, false, false
		}
		q.IDs = entryIDs(ids)
		return q, false, true
	}
	switch catalog.FileStatus(v.Get("status")) {
	case "", catalog.StatusArchived:
	case catalog.StatusTrashed:
		trashed := true
		q.Trashed = &trashed
	default:
		return q, true, true
	}
	return q, false, true
}

func (s *Server) listCollectionFiles(w http.ResponseWriter, r *http.Request) {
	q, emptyPage, ok := parseCollectionQuery(w, r, maxInlineIDs)
	if !ok {
		return
	}
	if emptyPage {
		writeJSON(w, http.StatusOK, []catalog.FileRecord{})
		return
	}
	switch sortBy := r.URL.Query().Get("sort"); sortBy {
	case "pinned_at":
		writeError(w, http.StatusBadRequest, "sort=pinned_at is not supported for collections")
		return
	case "size", "name":
		q.SortBy = sortBy
	}
	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		offset, err := strconv.Atoi(offsetStr)
		if err != nil || offset < 0 {
			writeError(w, http.StatusBadRequest, "invalid offset")
			return
		}
		q.Offset = offset
	}
	q.Limit = defaultCollectionPageLimit
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		q.Limit = min(limit, maxPageLimit)
	}
	entries, err := s.archive.Index().Entries(q)
	if err != nil {
		s.logger.Error("listing collection entries", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list files")
		return
	}
	writeJSON(w, http.StatusOK, collectionFileRecords(entries))
}

// defaultCollectionPageLimit matches the legacy default page.
const defaultCollectionPageLimit = 50

func (s *Server) listCollectionFileIDs(w http.ResponseWriter, r *http.Request) {
	q, emptyPage, ok := parseCollectionQuery(w, r, maxJobIDs)
	if !ok {
		return
	}
	if emptyPage {
		writeJSON(w, http.StatusOK, fileIDsResponse{IDs: []int64{}})
		return
	}
	ids, total, err := s.archive.Index().EntryIDs(q, maxJobIDs)
	if err != nil {
		s.logger.Error("listing collection entry ids", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list file ids")
		return
	}
	apiIDs := make([]int64, len(ids))
	for i, id := range ids {
		apiIDs[i] = apiID(id)
	}
	writeJSON(w, http.StatusOK, fileIDsResponse{
		IDs:       apiIDs,
		Total:     total,
		Truncated: total > int64(len(apiIDs)),
	})
}

// listCollectionTags answers GET /tags?folder=<collection> from the index.
func (s *Server) listCollectionTags(w http.ResponseWriter, folder, prefix string, limit int) {
	tags, err := s.archive.Index().Tags(folder, prefix, limit)
	if err != nil {
		s.logger.Error("listing collection tags", "folder", folder, "error", err)
		writeError(w, http.StatusInternalServerError, "tag operation failed")
		return
	}
	out := make([]catalog.Tag, 0, len(tags))
	for i, tag := range tags {
		out = append(out, catalog.Tag{ID: int64(i + 1), Name: tag.Name, Count: tag.Count, DescendantCount: tag.DescendantCount})
	}
	writeJSON(w, http.StatusOK, out)
}

// listCollectionEvents answers GET /events?folder=<collection> from the index.
func (s *Server) listCollectionEvents(w http.ResponseWriter, folder, eventType string, limit int) {
	events, err := s.archive.Index().Events(folder, eventType, limit)
	if err != nil {
		s.logger.Error("listing collection events", "folder", folder, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list events")
		return
	}
	out := make([]catalog.Event, 0, len(events))
	for _, e := range events {
		out = append(out, catalog.Event{
			ID: e.ID, Timestamp: e.At, EventType: e.Type, Folder: e.Folder, Path: e.Path, Details: e.Details,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// tagWire is the wire shape of catalog.TagMutationResult, with collection ids.
type tagWire struct {
	Updated int      `json:"updated"`
	Created []string `json:"created"`
	Undo    struct {
		Changes []archive.TagChange `json:"changes"`
	} `json:"undo"`
}

func tagWireFrom(result archive.TagResult) tagWire {
	var wire tagWire
	wire.Updated = result.Updated
	wire.Created = result.Created
	if wire.Created == nil {
		wire.Created = []string{}
	}
	wire.Undo.Changes = make([]archive.TagChange, 0, len(result.Undo.Changes))
	for _, change := range result.Undo.Changes {
		ids := make([]int64, len(change.IDs))
		for i, id := range change.IDs {
			ids[i] = apiID(id)
		}
		wire.Undo.Changes = append(wire.Undo.Changes, archive.TagChange{IDs: ids, Add: change.Add, Remove: change.Remove})
	}
	return wire
}

// bulkTagsTargetCollection handles POST /files/tags when the ids are collection
// ids. It reports true when it answered the request, including with a 400 for a
// request that mixes legacy and collection ids.
func (s *Server) bulkTagsTargetCollection(w http.ResponseWriter, r *http.Request, changes []catalog.TagFileMutation) bool {
	var legacy, collection int
	for _, change := range changes {
		l, c := idKinds(change.IDs)
		legacy, collection = legacy+l, collection+c
	}
	if collection == 0 {
		return false
	}
	if legacy > 0 {
		writeError(w, http.StatusBadRequest, errMixedIDs)
		return true
	}
	if collection > maxJobIDs {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d ids per request", maxJobIDs))
		return true
	}
	wireChanges := make([]archive.TagChange, len(changes))
	for i, change := range changes {
		wireChanges[i] = archive.TagChange{IDs: entryIDs(change.IDs), Add: change.Add, Remove: change.Remove}
	}
	result, err := s.archive.Tag(r.Context(), actorOf(r), wireChanges)
	if err != nil {
		s.writeTagError(w, "updating bulk collection tags", err)
		return true
	}
	writeJSON(w, http.StatusOK, tagWireFrom(result))
	return true
}

// mutateCollectionFileTags is mutateFileTags for one collection entry.
func (s *Server) mutateCollectionFileTags(w http.ResponseWriter, r *http.Request, id int64, removeOnly bool) {
	if _, ok := s.collectionEntryForRequest(w, id); !ok {
		return
	}
	var req fileTagsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if removeOnly && len(req.Add) != 0 {
		writeError(w, http.StatusBadRequest, "add is not allowed in a DELETE request")
		return
	}
	if len(req.Add)+len(req.Remove) == 0 {
		writeError(w, http.StatusBadRequest, "add or remove must not be empty")
		return
	}
	result, err := s.archive.Tag(r.Context(), actorOf(r), []archive.TagChange{
		{IDs: []int64{entryID(id)}, Add: req.Add, Remove: req.Remove},
	})
	if err != nil {
		s.writeTagError(w, "updating collection file tags", err)
		return
	}
	entry, ok := s.collectionEntryForRequest(w, id)
	if !ok {
		return
	}
	created := result.Created
	if created == nil {
		created = []string{}
	}
	writeJSON(w, http.StatusOK, fileTagsResponse{File: collectionFileRecord(entry), Created: created})
}

type archiveStatus struct {
	BlobCount           int64      `json:"blob_count"`
	BlobBytes           int64      `json:"blob_bytes"`
	BadBlobCount        int64      `json:"bad_blob_count"`
	JournalSeq          uint64     `json:"journal_seq"`
	ScrubRunning        bool       `json:"scrub_running"`
	ScrubLastFinishedAt *time.Time `json:"scrub_last_finished_at"`
}

// appendCollectionStatus adds one folderStatus per collection after the legacy
// folders and returns the archive block, or nil when no archive is open.
// journal_seq is the index's applied seq: the service keeps it level with the
// journal after every write.
func (s *Server) appendCollectionStatus(folders *[]folderStatus) (*archiveStatus, error) {
	if s.archive == nil {
		return nil, nil
	}
	ix := s.archive.Index()
	declared, err := ix.Folders()
	if err != nil {
		return nil, err
	}
	for _, folder := range declared {
		if folder.Kind != journal.FolderKindCollection {
			continue
		}
		stats, err := ix.FolderStats(folder.Name)
		if err != nil {
			return nil, err
		}
		*folders = append(*folders, folderStatus{
			Kind:          collectionKindCollection,
			StoredBytes:   stats.StoredBytes,
			Label:         folder.Name,
			DisplayName:   folder.Name,
			Enabled:       true,
			PresentInTOML: s.archive.CollectionDeclared(folder.Name),
			Counts:        catalog.FolderCounts{Total: stats.Live + stats.Trashed, Archived: stats.Live, Trashed: stats.Trashed},
		})
	}
	archiveStats, err := ix.ArchiveStats()
	if err != nil {
		return nil, err
	}
	scrubStatus := s.archive.ScrubStatus()
	block := &archiveStatus{
		BlobCount: archiveStats.BlobCount, BlobBytes: archiveStats.BlobBytes, BadBlobCount: archiveStats.BadBlobCount,
		JournalSeq: ix.AppliedSeq(), ScrubRunning: scrubStatus.Running,
	}
	if scrubStatus.Last != nil && !scrubStatus.Last.FinishedAt.IsZero() {
		finished := scrubStatus.Last.FinishedAt
		block.ScrubLastFinishedAt = &finished
	}
	return block, nil
}
