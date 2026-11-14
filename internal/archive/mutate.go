package archive

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/journal"
	"github.com/tjmisko/coldstore/internal/store"
)

type TagResult struct { // same wire shape as catalog.TagMutationResult
	Updated int      `json:"updated"`
	Created []string `json:"created"`
	Undo    struct {
		Changes []TagChange `json:"changes"`
	} `json:"undo"`
}

type TagChange struct {
	IDs    []int64  `json:"ids"`
	Add    []string `json:"add,omitempty"`
	Remove []string `json:"remove,omitempty"`
}

// itemTags is one Item's tag set while a Tag call is being worked out, keyed by
// folded name, with the display form kept for the journal.
type itemTags struct {
	entryIDs []int64 // the requested entries that lead to this Item
	prior    map[string]string
	current  map[string]string
}

type normalizedChange struct {
	ids                 []int64
	add                 []string // display names
	remove              []string // folded names
	addFold, removeFold []string
}

// Tag applies tag changes to the Items behind the given ENTRY ids. A tag lives
// on the Item, so every placement of it shows the change. One item_tagged is
// appended per Item whose tag set actually changes, all in one Append. Undo
// restores each Item's exact prior state.
func (s *Service) Tag(ctx context.Context, by string, changes []TagChange) (TagResult, error) {
	if err := s.enter(); err != nil {
		return TagResult{}, err
	}
	defer s.leave()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if err := s.catchUpIndexLocked(ctx); err != nil {
		return TagResult{}, err
	}
	normalized, err := normalizeTagChanges(changes)
	if err != nil {
		return TagResult{}, err
	}
	items := map[journal.ItemID]*itemTags{}
	var order []journal.ItemID
	entryFolder := map[journal.ItemID]string{}
	for _, change := range normalized {
		for _, id := range change.ids {
			entry, found, err := s.core.ix.Entry(id)
			if err != nil {
				return TagResult{}, err
			}
			if !found {
				return TagResult{}, fmt.Errorf("tagging entry %d: %w", id, ErrEntryNotFound)
			}
			state, seen := items[entry.Item]
			if !seen {
				state = newItemTags(entry)
				items[entry.Item] = state
				order = append(order, entry.Item)
				entryFolder[entry.Item] = entry.Folder
			}
			if !slices.Contains(state.entryIDs, id) {
				state.entryIDs = append(state.entryIDs, id)
			}
			applyTagChange(state, change)
		}
	}

	var recs []journal.Record
	var result TagResult
	result.Created = []string{}
	undo := map[string]*TagChange{}
	var undoOrder []string
	for _, item := range order {
		state := items[item]
		add, remove := tagDiff(state)
		if len(add) == 0 && len(remove) == 0 {
			continue
		}
		recs = append(recs, journal.Record{By: by, Type: journal.TypeItemTagged, Item: item, Add: add, Remove: remove})
		result.Updated += len(state.entryIDs)
		created, err := s.newTagNames(entryFolder[item], add)
		if err != nil {
			return TagResult{}, err
		}
		result.Created = appendMissing(result.Created, created)

		undoAdd, undoRemove := remove, add
		key := strings.Join(undoAdd, "\x00") + "\x01" + strings.Join(undoRemove, "\x00")
		group, ok := undo[key]
		if !ok {
			group = &TagChange{Add: undoAdd, Remove: undoRemove}
			undo[key] = group
			undoOrder = append(undoOrder, key)
		}
		group.IDs = append(group.IDs, state.entryIDs...)
	}
	result.Undo.Changes = []TagChange{}
	for _, key := range undoOrder {
		result.Undo.Changes = append(result.Undo.Changes, *undo[key])
	}
	if len(recs) == 0 {
		return result, nil
	}
	applied, err := s.appendAndApply(ctx, recs)
	if err != nil {
		return TagResult{}, fmt.Errorf("tagging: %w", err)
	}
	s.syncViews(applied)
	return result, nil
}

func normalizeTagChanges(changes []TagChange) ([]normalizedChange, error) {
	out := make([]normalizedChange, 0, len(changes))
	for _, change := range changes {
		n := normalizedChange{ids: change.IDs}
		for _, raw := range change.Add {
			display, fold, err := catalog.NormalizeTagName(raw)
			if err != nil {
				return nil, fmt.Errorf("tag %q: %w", raw, err)
			}
			n.add, n.addFold = append(n.add, display), append(n.addFold, fold)
		}
		for _, raw := range change.Remove {
			_, fold, err := catalog.NormalizeTagName(raw)
			if err != nil {
				return nil, fmt.Errorf("tag %q: %w", raw, err)
			}
			n.removeFold = append(n.removeFold, fold)
		}
		out = append(out, n)
	}
	return out, nil
}

func newItemTags(entry index.Entry) *itemTags {
	state := &itemTags{prior: map[string]string{}, current: map[string]string{}}
	for _, name := range entry.Tags {
		_, fold, err := catalog.NormalizeTagName(name)
		if err != nil {
			fold = strings.ToLower(name)
		}
		state.prior[fold], state.current[fold] = name, name
	}
	return state
}

func applyTagChange(state *itemTags, change normalizedChange) {
	for i, fold := range change.addFold {
		if _, has := state.current[fold]; !has {
			state.current[fold] = change.add[i]
		}
	}
	for _, fold := range change.removeFold {
		delete(state.current, fold)
	}
}

// tagDiff is the display names to add and remove to go from prior to current.
func tagDiff(state *itemTags) (add, remove []string) {
	for fold, display := range state.current {
		if _, had := state.prior[fold]; !had {
			add = append(add, display)
		}
	}
	for fold, display := range state.prior {
		if _, has := state.current[fold]; !has {
			remove = append(remove, display)
		}
	}
	slices.Sort(add)
	slices.Sort(remove)
	return add, remove
}

// newTagNames are the added names no live entry of the folder carries yet.
func (s *Service) newTagNames(folder string, added []string) ([]string, error) {
	if len(added) == 0 {
		return nil, nil
	}
	known, err := s.core.ix.Tags(folder, "", index.MaxPageLimit)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, tag := range known {
		have[strings.ToLower(tag.Name)] = true
	}
	var created []string
	for _, name := range added {
		if !have[strings.ToLower(name)] {
			created = append(created, name)
		}
	}
	return created, nil
}

func appendMissing(into, from []string) []string {
	for _, name := range from {
		if !slices.Contains(into, name) {
			into = append(into, name)
		}
	}
	return into
}

// Trash appends one item_trashed or item_untrashed per Item whose state
// changes, in one Append, and syncs views. updated counts the requested
// entries whose Item changed. M1 has no purge: nothing here destroys bytes.
func (s *Service) Trash(ctx context.Context, by string, entryIDs []int64, trashed bool) (updated int, err error) {
	if err := s.enter(); err != nil {
		return 0, err
	}
	defer s.leave()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if err := s.catchUpIndexLocked(ctx); err != nil {
		return 0, err
	}
	recordType := journal.TypeItemUntrashed
	if trashed {
		recordType = journal.TypeItemTrashed
	}
	var recs []journal.Record
	seen := map[journal.ItemID]bool{}
	counted := map[int64]bool{}
	for _, id := range entryIDs {
		entry, found, err := s.core.ix.Entry(id)
		if err != nil {
			return 0, err
		}
		if !found {
			return 0, fmt.Errorf("trashing entry %d: %w", id, ErrEntryNotFound)
		}
		if (entry.TrashedAt != nil) == trashed || counted[id] {
			continue
		}
		counted[id] = true
		updated++
		if seen[entry.Item] {
			continue
		}
		seen[entry.Item] = true
		recs = append(recs, journal.Record{By: by, Type: recordType, Item: entry.Item})
	}
	if len(recs) == 0 {
		return 0, nil
	}
	applied, err := s.appendAndApply(ctx, recs)
	if err != nil {
		return 0, fmt.Errorf("trashing: %w", err)
	}
	s.syncViews(applied)
	return updated, nil
}

type ReceiptLine struct {
	Hash         string `json:"hash"`
	Held         bool   `json:"held"`          // the journal names it
	Verified     bool   `json:"verified"`      // store.Verify passed during this call
	InCollection bool   `json:"in_collection"` // a live placement in this collection has it as current
	Size         int64  `json:"size"`
}

// Receipt answers, for each hash and in order, whether the archive holds it,
// whether it verifies right now, and whether it is live in the collection. A
// verification failure marks the blob bad and records blob_bad.
func (s *Service) Receipt(ctx context.Context, collection string, hashes []store.Hash) ([]ReceiptLine, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	if !s.collections[collection] {
		return nil, fmt.Errorf("receipt for %q: %w", collection, ErrCollectionUnknown)
	}
	lines := make([]ReceiptLine, 0, len(hashes))
	for _, h := range hashes {
		line, err := s.receiptLine(ctx, collection, h)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, nil
}

func (s *Service) receiptLine(ctx context.Context, collection string, h store.Hash) (ReceiptLine, error) {
	line := ReceiptLine{Hash: h.String()}
	info, held, err := s.core.ix.Blob(h)
	if err != nil {
		return line, err
	}
	if !held {
		return line, nil
	}
	line.Held, line.Size = true, info.Size
	switch err := s.verifyBlob(ctx, h, info); {
	case err == nil:
		line.Verified = true
	case ctx.Err() != nil:
		return line, ctx.Err()
	case !errors.Is(err, ErrBlobBad):
		return line, err
	}
	line.InCollection, err = s.liveInCollection(collection, h)
	return line, err
}

func isBlobBad(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrBlobBad.Error())
}

func (s *Service) liveInCollection(collection string, h store.Hash) (bool, error) {
	placements, err := s.core.ix.PlacementsByCurrentHash(collection, h)
	if err != nil {
		return false, err
	}
	for _, p := range placements {
		entry, found, err := s.core.ix.Entry(p.EntryID)
		if err != nil {
			return false, err
		}
		if found && entry.TrashedAt == nil {
			return true, nil
		}
	}
	return false, nil
}
