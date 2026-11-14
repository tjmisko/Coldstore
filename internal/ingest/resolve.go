package ingest

import (
	"fmt"
	"time"

	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/journal"
	"github.com/tjmisko/coldstore/internal/store"
)

// State is the read side of the index that Resolve consults. *index.Index
// implements it.
type State interface {
	FolderByName(name string) (index.Folder, bool, error)
	PlacementAt(folder, path string) (index.Placement, bool, error)
	PlacementsByCurrentHash(folder string, h store.Hash) ([]index.Placement, error)
	Entry(id int64) (index.Entry, bool, error)
	Blob(h store.Hash) (index.BlobInfo, bool, error)
}

var _ State = (*index.Index)(nil)

// Resolve is the decision table of design 4.4 as journal records, evaluated
// top to bottom. It is pure: it reads state and returns records, it writes
// nothing. Result.EntryID is set only when the placement already exists; the
// caller learns the id of a new placement from the index after applying.
func Resolve(st State, a Arrival, h store.Hash, size int64, by string,
	newID func() ItemID, pathGone func(folder, path string) bool) ([]journal.Record, Result, error) {

	_, declared, err := st.FolderByName(a.Folder)
	if err != nil {
		return nil, Result{}, fmt.Errorf("resolving %s/%s: %w", a.Folder, a.Path, err)
	}
	if !declared {
		return nil, Result{}, fmt.Errorf("resolving %s/%s: %w", a.Folder, a.Path, ErrCollectionUnknown)
	}
	r := resolver{st: st, a: a, h: h, size: size, by: by, newID: newID}

	placed, found, err := st.PlacementAt(a.Folder, a.Path)
	if err != nil {
		return nil, Result{}, fmt.Errorf("resolving %s/%s: %w", a.Folder, a.Path, err)
	}
	if found {
		return r.atPlacedPath(placed)
	}
	return r.atFreePath(pathGone)
}

type resolver struct {
	st    State
	a     Arrival
	h     store.Hash
	size  int64
	by    string
	newID func() ItemID
}

func (r resolver) record(t string) journal.Record {
	return journal.Record{By: r.by, Type: t}
}

func (r resolver) mtime() *time.Time {
	if r.a.Mtime.IsZero() {
		return nil
	}
	t := r.a.Mtime.UTC()
	return &t
}

// blobAdded returns the blob_added record when the index does not name the blob yet.
func (r resolver) blobAdded() ([]journal.Record, error) {
	_, named, err := r.st.Blob(r.h)
	if err != nil {
		return nil, fmt.Errorf("looking up blob %s: %w", r.h, err)
	}
	if named {
		return nil, nil
	}
	rec := r.record(journal.TypeBlobAdded)
	rec.Hash, rec.Size = r.h.String(), r.size
	return []journal.Record{rec}, nil
}

func (r resolver) placementSet(item ItemID, path, asked string) journal.Record {
	rec := r.record(journal.TypePlacementSet)
	rec.Folder, rec.Path, rec.Item, rec.Mtime, rec.Asked = r.a.Folder, path, item, r.mtime(), asked
	return rec
}

func (r resolver) result(item ItemID, entryID int64, path string, outcome Outcome) Result {
	return Result{Item: item, EntryID: entryID, Hash: r.h, Size: r.size, Path: path, Outcome: outcome}
}

// newItem is blob_added?, item_created, placement_set at path.
func (r resolver) newItem(path, asked string, outcome Outcome) ([]journal.Record, Result, error) {
	recs, err := r.blobAdded()
	if err != nil {
		return nil, Result{}, err
	}
	item := r.newID()
	created := r.record(journal.TypeItemCreated)
	created.Item, created.Hash = item, r.h.String()
	recs = append(recs, created, r.placementSet(item, path, asked))
	return recs, r.result(item, 0, path, outcome), nil
}

func (r resolver) atPlacedPath(placed index.Placement) ([]journal.Record, Result, error) {
	entry, ok, err := r.st.Entry(placed.EntryID)
	if err != nil {
		return nil, Result{}, fmt.Errorf("resolving %s/%s: %w", r.a.Folder, r.a.Path, err)
	}
	if !ok {
		return nil, Result{}, fmt.Errorf("resolving %s/%s: placement %d has no entry", r.a.Folder, r.a.Path, placed.EntryID)
	}
	if entry.Hash == r.h {
		return nil, r.result(placed.Item, placed.EntryID, r.a.Path, OutcomeUnchanged), nil
	}
	if placed.Hot {
		return r.version(placed)
	}
	return r.beside()
}

// version is row 2: a hot placement changed in place.
func (r resolver) version(placed index.Placement) ([]journal.Record, Result, error) {
	recs, err := r.blobAdded()
	if err != nil {
		return nil, Result{}, err
	}
	versioned := r.record(journal.TypeItemVersioned)
	versioned.Item, versioned.Hash = placed.Item, r.h.String()
	recs = append(recs, versioned, r.placementSet(placed.Item, r.a.Path, ""))
	return recs, r.result(placed.Item, placed.EntryID, r.a.Path, OutcomeVersioned), nil
}

// beside is row 3: a cold path that already means other bytes.
func (r resolver) beside() ([]journal.Record, Result, error) {
	besidePath := BesidePath(r.a.Path, r.h)
	if _, err := CleanPath(besidePath); err != nil {
		return nil, Result{}, fmt.Errorf("placing %s beside %s: %w", r.h, r.a.Path, err)
	}
	existing, found, err := r.st.PlacementAt(r.a.Folder, besidePath)
	if err != nil {
		return nil, Result{}, fmt.Errorf("resolving %s/%s: %w", r.a.Folder, besidePath, err)
	}
	if !found {
		return r.newItem(besidePath, r.a.Path, OutcomePlacedBeside)
	}
	entry, ok, err := r.st.Entry(existing.EntryID)
	if err != nil {
		return nil, Result{}, fmt.Errorf("resolving %s/%s: %w", r.a.Folder, besidePath, err)
	}
	if !ok || entry.Hash != r.h {
		return nil, Result{}, fmt.Errorf("resolving %s/%s: %s is taken by different bytes", r.a.Folder, r.a.Path, besidePath)
	}
	return nil, r.result(existing.Item, existing.EntryID, besidePath, OutcomeUnchanged), nil
}

// atFreePath is rows 4 to 6: nothing is placed at the path.
func (r resolver) atFreePath(pathGone func(folder, path string) bool) ([]journal.Record, Result, error) {
	holders, err := r.st.PlacementsByCurrentHash(r.a.Folder, r.h)
	if err != nil {
		return nil, Result{}, fmt.Errorf("resolving %s/%s: %w", r.a.Folder, r.a.Path, err)
	}
	if len(holders) == 0 {
		return r.newItem(r.a.Path, "", OutcomeCreated)
	}
	for _, holder := range holders {
		if pathGone == nil || !pathGone(holder.Folder, holder.Path) {
			continue
		}
		cleared := r.record(journal.TypePlacementCleared)
		cleared.Folder, cleared.Path = holder.Folder, holder.Path
		recs := []journal.Record{cleared, r.placementSet(holder.Item, r.a.Path, "")}
		return recs, r.result(holder.Item, 0, r.a.Path, OutcomeRenamed), nil
	}
	recs := []journal.Record{r.placementSet(holders[0].Item, r.a.Path, "")}
	return recs, r.result(holders[0].Item, 0, r.a.Path, OutcomeCopied), nil
}
