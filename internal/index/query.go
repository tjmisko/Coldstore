package index

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/journal"
	"github.com/tjmisko/coldstore/internal/store"
)

const (
	// MaxPageLimit is the largest page Entries returns (the API's maxPageLimit).
	MaxPageLimit = 500
	// DefaultPageLimit applies when EntryQuery.Limit is not positive.
	DefaultPageLimit = 100
	// MaxIDs is the largest id list EntryIDs or an IDs query handles (the API's maxJobIDs).
	MaxIDs = 20000
)

type Folder struct {
	Name, Kind  string
	DeclaredSeq uint64
}

type Placement struct {
	EntryID      int64 // seq of the placement_set that created this (folder, path)
	Folder, Path string
	Item         journal.ItemID
	Mtime        time.Time
	Hot, Pinned  bool
}

// Entry is one browsable row: a Placement joined with its Item and blob.
type Entry struct {
	Placement
	Hash         store.Hash
	Size         int64
	TrashedAt    *time.Time
	Description  string
	Tags         []string
	FirstSeen    time.Time // At of the creating placement_set
	DetectedType string    // operational; "" until derived
	ThumbState   string    // operational: "" | "done" | "failed"
	ThumbError   string
	BlobBad      bool
}

type EntryQuery struct {
	Folder        string // required unless IDs is set
	Trashed       *bool  // nil = live only; true = trashed only
	Extension     string
	Search        string   // case-insensitive substring of Path
	Tags          []string // AND; each matches the tag and its descendants
	Untagged      *bool
	IDs           []int64 // entry ids; when set every other filter is ignored
	SortBy        string  // "mtime" (default, newest first) | "size" | "name"
	Offset, Limit int
}

type BlobInfo struct {
	Size       int64
	AddedSeq   uint64
	VerifiedAt *time.Time
	Bad        bool
	BadReason  string
}

type TagCount struct {
	Name            string
	Count           int64 // live entries carrying exactly this tag
	DescendantCount int64 // live entries carrying this tag or a descendant
}

type FolderStats struct {
	Live, Trashed int64
	StoredBytes   int64 // bytes of the distinct current blobs this folder places
}

type ArchiveStats struct {
	BlobCount, BlobBytes, BadBlobCount int64
}

const entryColumns = `p.entry_id, p.folder, p.path, p.item_id, p.mtime, p.created_at, p.hot, p.pinned,
	i.current_hash, b.size, i.trashed_at, i.description,
	b.detected_type, b.thumb_state, b.thumb_error, b.bad`

const entryFrom = ` FROM placements p
	JOIN items i ON i.id = p.item_id
	JOIN blobs b ON b.hash = i.current_hash`

func (ix *Index) Folders() ([]Folder, error) {
	rows, err := ix.read.Query("SELECT name, kind, declared_seq FROM folders ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("listing folders: %w", err)
	}
	defer rows.Close()
	var folders []Folder
	for rows.Next() {
		var f Folder
		var seq int64
		if err := rows.Scan(&f.Name, &f.Kind, &seq); err != nil {
			return nil, fmt.Errorf("listing folders: %w", err)
		}
		f.DeclaredSeq = uint64(seq)
		folders = append(folders, f)
	}
	return folders, rows.Err()
}

func (ix *Index) FolderByName(name string) (Folder, bool, error) {
	var f Folder
	var seq int64
	err := ix.read.QueryRow("SELECT name, kind, declared_seq FROM folders WHERE name = ?", name).
		Scan(&f.Name, &f.Kind, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return Folder{}, false, nil
	}
	if err != nil {
		return Folder{}, false, fmt.Errorf("looking up folder %q: %w", name, err)
	}
	f.DeclaredSeq = uint64(seq)
	return f, true, nil
}

const placementColumns = "entry_id, folder, path, item_id, mtime, hot, pinned"

func scanPlacement(row interface{ Scan(...any) error }) (Placement, error) {
	var p Placement
	var item string
	var mtime int64
	var hot, pinned int
	if err := row.Scan(&p.EntryID, &p.Folder, &p.Path, &item, &mtime, &hot, &pinned); err != nil {
		return Placement{}, err
	}
	p.Item = journal.ItemID(item)
	p.Mtime = fromNanos(mtime)
	p.Hot, p.Pinned = hot != 0, pinned != 0
	return p, nil
}

func (ix *Index) PlacementAt(folder, path string) (Placement, bool, error) {
	row := ix.read.QueryRow("SELECT "+placementColumns+" FROM placements WHERE folder = ? AND path = ?", folder, path)
	p, err := scanPlacement(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Placement{}, false, nil
	}
	if err != nil {
		return Placement{}, false, fmt.Errorf("looking up placement %s/%s: %w", folder, path, err)
	}
	return p, true, nil
}

func (ix *Index) queryPlacements(query string, args ...any) ([]Placement, error) {
	rows, err := ix.read.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Placement
	for rows.Next() {
		p, err := scanPlacement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PlacementsByCurrentHash lists the placements in folder whose Item currently has bytes h.
func (ix *Index) PlacementsByCurrentHash(folder string, h store.Hash) ([]Placement, error) {
	out, err := ix.queryPlacements(`SELECT p.entry_id, p.folder, p.path, p.item_id, p.mtime, p.hot, p.pinned
		FROM placements p JOIN items i ON i.id = p.item_id
		WHERE p.folder = ? AND i.current_hash = ? ORDER BY p.path`, folder, h.String())
	if err != nil {
		return nil, fmt.Errorf("listing placements of %s in %q: %w", h, folder, err)
	}
	return out, nil
}

// PlacementsByItem lists every placement of an Item, in every folder.
func (ix *Index) PlacementsByItem(item journal.ItemID) ([]Placement, error) {
	out, err := ix.queryPlacements("SELECT "+placementColumns+" FROM placements WHERE item_id = ? ORDER BY folder, path", string(item))
	if err != nil {
		return nil, fmt.Errorf("listing placements of item %q: %w", item, err)
	}
	return out, nil
}

// LiveLinks streams every live, untrashed placement as (folder, path, current hash).
func (ix *Index) LiveLinks(fn func(folder, path string, h store.Hash) error) error {
	rows, err := ix.read.Query(`SELECT p.folder, p.path, i.current_hash
		FROM placements p JOIN items i ON i.id = p.item_id
		WHERE i.trashed_at IS NULL ORDER BY p.folder, p.path`)
	if err != nil {
		return fmt.Errorf("listing live links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var folder, path, hex string
		if err := rows.Scan(&folder, &path, &hex); err != nil {
			return fmt.Errorf("listing live links: %w", err)
		}
		h, err := store.ParseHash(hex)
		if err != nil {
			return fmt.Errorf("listing live links: %w", err)
		}
		if err := fn(folder, path, h); err != nil {
			return err
		}
	}
	return rows.Err()
}

// LiveLinkAt reports the current hash at (folder, path) when it is placed and not trashed.
func (ix *Index) LiveLinkAt(folder, path string) (store.Hash, bool, error) {
	var hex string
	err := ix.read.QueryRow(`SELECT i.current_hash FROM placements p JOIN items i ON i.id = p.item_id
		WHERE p.folder = ? AND p.path = ? AND i.trashed_at IS NULL`, folder, path).Scan(&hex)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Hash{}, false, nil
	}
	if err != nil {
		return store.Hash{}, false, fmt.Errorf("looking up link %s/%s: %w", folder, path, err)
	}
	h, err := store.ParseHash(hex)
	if err != nil {
		return store.Hash{}, false, err
	}
	return h, true, nil
}

func scanEntry(row interface{ Scan(...any) error }) (Entry, error) {
	var e Entry
	var item, hex string
	var mtime, created int64
	var hot, pinned, bad int
	var trashed sql.NullInt64
	err := row.Scan(&e.EntryID, &e.Folder, &e.Path, &item, &mtime, &created, &hot, &pinned,
		&hex, &e.Size, &trashed, &e.Description,
		&e.DetectedType, &e.ThumbState, &e.ThumbError, &bad)
	if err != nil {
		return Entry{}, err
	}
	hash, err := store.ParseHash(hex)
	if err != nil {
		return Entry{}, err
	}
	e.Item = journal.ItemID(item)
	e.Mtime = fromNanos(mtime)
	e.FirstSeen = fromNanos(created)
	e.Hot, e.Pinned, e.BlobBad = hot != 0, pinned != 0, bad != 0
	e.Hash = hash
	if trashed.Valid {
		at := fromNanos(trashed.Int64)
		e.TrashedAt = &at
	}
	return e, nil
}

func (ix *Index) Entry(id int64) (Entry, bool, error) {
	row := ix.read.QueryRow("SELECT "+entryColumns+entryFrom+" WHERE p.entry_id = ?", id)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("looking up entry %d: %w", id, err)
	}
	entries := []Entry{e}
	if err := ix.attachTags(entries); err != nil {
		return Entry{}, false, err
	}
	return entries[0], true, nil
}

// escapeLike makes s literal inside a LIKE pattern with ESCAPE '\'.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

// buildWhere turns q into a WHERE clause over the entryFrom aliases.
func buildWhere(q EntryQuery) (string, []any, error) {
	if len(q.IDs) > 0 {
		if len(q.IDs) > MaxIDs {
			return "", nil, fmt.Errorf("at most %d ids are allowed, got %d", MaxIDs, len(q.IDs))
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(q.IDs)), ",")
		args := make([]any, len(q.IDs))
		for i, id := range q.IDs {
			args[i] = id
		}
		return " WHERE p.entry_id IN (" + marks + ")", args, nil
	}
	if q.Folder == "" {
		return "", nil, errors.New("a folder or ids is required")
	}
	clauses := []string{"p.folder = ?"}
	args := []any{q.Folder}
	if q.Trashed != nil && *q.Trashed {
		clauses = append(clauses, "i.trashed_at IS NOT NULL")
	} else {
		clauses = append(clauses, "i.trashed_at IS NULL")
	}
	if ext := strings.ToLower(strings.TrimPrefix(q.Extension, ".")); ext != "" {
		clauses = append(clauses, `ufold(p.path) LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike("."+ext))
	}
	if q.Search != "" {
		clauses = append(clauses, "instr(ufold(p.path), ?) > 0")
		args = append(args, strings.ToLower(q.Search))
	}
	if len(q.Tags) > 0 {
		folds := make([]string, 0, len(q.Tags))
		for _, raw := range q.Tags {
			_, fold, err := catalog.NormalizeTagName(raw)
			if err != nil {
				return "", nil, fmt.Errorf("tag filter %q: %w", raw, err)
			}
			folds = append(folds, fold)
		}
		for _, fold := range folds {
			clauses = append(clauses, `EXISTS (SELECT 1 FROM item_tags it JOIN tags t ON t.id = it.tag_id
				WHERE it.item_id = p.item_id AND (t.fold = ? OR t.fold LIKE ? ESCAPE '\'))`)
			args = append(args, fold, escapeLike(fold)+"/%")
		}
	}
	if q.Untagged != nil {
		not := ""
		if *q.Untagged {
			not = "NOT "
		}
		clauses = append(clauses, not+"EXISTS (SELECT 1 FROM item_tags it WHERE it.item_id = p.item_id)")
	}
	return " WHERE " + strings.Join(clauses, " AND "), args, nil
}

func orderBy(sortBy string) (string, error) {
	switch sortBy {
	case "", "mtime":
		return " ORDER BY p.mtime DESC, p.entry_id DESC", nil
	case "size":
		return " ORDER BY b.size DESC, p.entry_id DESC", nil
	case "name":
		return " ORDER BY ufold(p.path) ASC, p.path ASC, p.entry_id ASC", nil
	}
	return "", fmt.Errorf("unknown sort %q", sortBy)
}

func clampLimit(limit, max int) int {
	if limit <= 0 {
		return DefaultPageLimit
	}
	if limit > max {
		return max
	}
	return limit
}

// Entries returns one page of entries. The page is capped at MaxPageLimit
// (MaxIDs when q.IDs is set).
func (ix *Index) Entries(q EntryQuery) ([]Entry, error) {
	where, args, err := buildWhere(q)
	if err != nil {
		return nil, fmt.Errorf("querying entries: %w", err)
	}
	order, err := orderBy(q.SortBy)
	if err != nil {
		return nil, fmt.Errorf("querying entries: %w", err)
	}
	limit := clampLimit(q.Limit, MaxPageLimit)
	if len(q.IDs) > 0 {
		limit = MaxIDs
		if q.Limit > 0 {
			limit = clampLimit(q.Limit, MaxIDs)
		}
	}
	offset := max(q.Offset, 0)
	args = append(args, limit, offset)
	rows, err := ix.read.Query("SELECT "+entryColumns+entryFrom+where+order+" LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, fmt.Errorf("querying entries: %w", err)
	}
	defer rows.Close()
	entries := []Entry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("querying entries: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("querying entries: %w", err)
	}
	if err := ix.attachTags(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// attachTags fills Tags (display names, sorted by fold) for every entry.
func (ix *Index) attachTags(entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	byItem := map[journal.ItemID][]int{}
	var items []any
	for i := range entries {
		entries[i].Tags = []string{}
		if _, seen := byItem[entries[i].Item]; !seen {
			items = append(items, string(entries[i].Item))
		}
		byItem[entries[i].Item] = append(byItem[entries[i].Item], i)
	}
	// Chunked so the statement stays under SQLite's variable limit.
	const chunk = 500
	for start := 0; start < len(items); start += chunk {
		end := min(start+chunk, len(items))
		marks := strings.TrimSuffix(strings.Repeat("?,", end-start), ",")
		rows, err := ix.read.Query(`SELECT it.item_id, t.name FROM item_tags it JOIN tags t ON t.id = it.tag_id
			WHERE it.item_id IN (`+marks+`) ORDER BY t.fold`, items[start:end]...)
		if err != nil {
			return fmt.Errorf("loading tags: %w", err)
		}
		for rows.Next() {
			var item, name string
			if err := rows.Scan(&item, &name); err != nil {
				rows.Close()
				return fmt.Errorf("loading tags: %w", err)
			}
			for _, i := range byItem[journal.ItemID(item)] {
				entries[i].Tags = append(entries[i].Tags, name)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("loading tags: %w", err)
		}
	}
	return nil
}

// EntryIDs returns up to limit matching entry ids in query order, and the total
// number of matches. limit is capped at MaxIDs.
func (ix *Index) EntryIDs(q EntryQuery, limit int) (ids []int64, total int64, err error) {
	where, args, err := buildWhere(q)
	if err != nil {
		return nil, 0, fmt.Errorf("querying entry ids: %w", err)
	}
	order, err := orderBy(q.SortBy)
	if err != nil {
		return nil, 0, fmt.Errorf("querying entry ids: %w", err)
	}
	if err := ix.read.QueryRow("SELECT COUNT(*)"+entryFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting entries: %w", err)
	}
	limit = clampLimit(limit, MaxIDs)
	rows, err := ix.read.Query("SELECT p.entry_id"+entryFrom+where+order+" LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, 0, fmt.Errorf("querying entry ids: %w", err)
	}
	defer rows.Close()
	ids = []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, 0, fmt.Errorf("querying entry ids: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, total, rows.Err()
}

func (ix *Index) Blob(h store.Hash) (BlobInfo, bool, error) {
	var info BlobInfo
	var seq int64
	var verified sql.NullInt64
	var bad int
	err := ix.read.QueryRow("SELECT size, added_seq, verified_at, bad, bad_reason FROM blobs WHERE hash = ?", h.String()).
		Scan(&info.Size, &seq, &verified, &bad, &info.BadReason)
	if errors.Is(err, sql.ErrNoRows) {
		return BlobInfo{}, false, nil
	}
	if err != nil {
		return BlobInfo{}, false, fmt.Errorf("looking up blob %s: %w", h, err)
	}
	info.AddedSeq = uint64(seq)
	info.Bad = bad != 0
	if verified.Valid {
		at := fromNanos(verified.Int64)
		info.VerifiedAt = &at
	}
	return info, true, nil
}

// BlobHashes streams every blob the journal names, in hash order.
func (ix *Index) BlobHashes(fn func(h store.Hash, size int64) error) error {
	rows, err := ix.read.Query("SELECT hash, size FROM blobs ORDER BY hash")
	if err != nil {
		return fmt.Errorf("listing blobs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var hex string
		var size int64
		if err := rows.Scan(&hex, &size); err != nil {
			return fmt.Errorf("listing blobs: %w", err)
		}
		h, err := store.ParseHash(hex)
		if err != nil {
			return fmt.Errorf("listing blobs: %w", err)
		}
		if err := fn(h, size); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Tags lists the tags carried by live entries of folder whose folded name starts
// with the folded prefix, ordered by folded name.
func (ix *Index) Tags(folder, prefix string, limit int) ([]TagCount, error) {
	foldedPrefix := strings.ToLower(strings.TrimSpace(prefix))
	limit = clampLimit(limit, MaxPageLimit)
	rows, err := ix.read.Query(`
		SELECT t.name,
			COUNT(DISTINCT CASE WHEN t2.id = t.id THEN p.entry_id END),
			COUNT(DISTINCT p.entry_id)
		FROM tags t
		JOIN tags t2 ON t2.fold = t.fold OR substr(t2.fold, 1, length(t.fold) + 1) = t.fold || '/'
		JOIN item_tags it ON it.tag_id = t2.id
		JOIN items i ON i.id = it.item_id AND i.trashed_at IS NULL
		JOIN placements p ON p.item_id = it.item_id AND p.folder = ?
		WHERE substr(t.fold, 1, ?) = ?
		GROUP BY t.id ORDER BY t.fold LIMIT ?`,
		folder, len([]rune(foldedPrefix)), foldedPrefix, limit)
	if err != nil {
		return nil, fmt.Errorf("listing tags of %q: %w", folder, err)
	}
	defer rows.Close()
	out := []TagCount{}
	for rows.Next() {
		var c TagCount
		if err := rows.Scan(&c.Name, &c.Count, &c.DescendantCount); err != nil {
			return nil, fmt.Errorf("listing tags of %q: %w", folder, err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (ix *Index) FolderStats(folder string) (FolderStats, error) {
	var s FolderStats
	err := ix.read.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN i.trashed_at IS NULL THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN i.trashed_at IS NOT NULL THEN 1 ELSE 0 END), 0)
		FROM placements p JOIN items i ON i.id = p.item_id WHERE p.folder = ?`, folder).
		Scan(&s.Live, &s.Trashed)
	if err != nil {
		return FolderStats{}, fmt.Errorf("counting entries of %q: %w", folder, err)
	}
	err = ix.read.QueryRow(`SELECT COALESCE(SUM(b.size), 0) FROM blobs b WHERE b.hash IN
		(SELECT i.current_hash FROM placements p JOIN items i ON i.id = p.item_id WHERE p.folder = ?)`, folder).
		Scan(&s.StoredBytes)
	if err != nil {
		return FolderStats{}, fmt.Errorf("summing stored bytes of %q: %w", folder, err)
	}
	return s, nil
}

func (ix *Index) ArchiveStats() (ArchiveStats, error) {
	var s ArchiveStats
	err := ix.read.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0), COALESCE(SUM(bad), 0) FROM blobs`).
		Scan(&s.BlobCount, &s.BlobBytes, &s.BadBlobCount)
	if err != nil {
		return ArchiveStats{}, fmt.Errorf("counting blobs: %w", err)
	}
	return s, nil
}
