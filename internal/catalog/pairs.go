package catalog

import (
	"database/sql"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

type filePairGroup struct {
	identity string
	files    []FileRecord
	activity time.Time
}

// PairAssignment is one row in an atomic pairing update. Source is either
// "content_id" or "basename". A content-id assignment may replace a degraded
// basename guess; a basename assignment never replaces stronger evidence.
type PairAssignment struct {
	ID     int64
	Key    string
	Source string
}

// PairCandidateRef is the small projection needed to build the daemon's
// per-pass candidate index. It avoids materialising every metadata column for
// a large photo library merely to group names.
type PairCandidateRef struct {
	ID           int64
	FolderLabel  string
	RelativePath string
	Filename     string
	Extension    string
}

type pairQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// ExpandPairGroups returns the requested ids plus every row which shares a
// non-empty pair key in the same folder. Empty pair keys are deliberately not
// groups. Requested ids that no longer exist are preserved so the job worker
// can report its established "file not found" result.
//
// The two-stage lookup is chunked because a legal job contains 20,000 ids and
// SQLite's variable limit is lower on some supported builds.
func (c *Catalog) ExpandPairGroups(ids []int64) ([]int64, error) {
	tx, err := c.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("expanding pair groups: begin: %w", err)
	}
	defer tx.Rollback()

	expanded, err := expandPairGroups(tx, ids)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("expanding pair groups: commit: %w", err)
	}
	return expanded, nil
}

func expandPairGroups(q pairQueryer, ids []int64) ([]int64, error) {
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	if len(seen) == 0 {
		return []int64{}, nil
	}

	requested := make([]int64, 0, len(seen))
	for id := range seen {
		requested = append(requested, id)
	}
	sort.Slice(requested, func(i, j int) bool { return requested[i] < requested[j] })

	keysByFolder := make(map[string]map[string]struct{})
	for _, chunk := range chunkIDs(requested) {
		args := idsToAny(chunk)
		rows, err := q.Query(`SELECT folder_label, pair_key FROM files
			WHERE id IN (`+placeholders(len(chunk))+`) AND pair_key <> ''`, args...)
		if err != nil {
			return nil, fmt.Errorf("expanding pair groups: reading keys: %w", err)
		}
		for rows.Next() {
			var folder, key string
			if err := rows.Scan(&folder, &key); err != nil {
				rows.Close()
				return nil, fmt.Errorf("expanding pair groups: scanning keys: %w", err)
			}
			if keysByFolder[folder] == nil {
				keysByFolder[folder] = make(map[string]struct{})
			}
			keysByFolder[folder][key] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("expanding pair groups: reading keys: %w", err)
		}
		rows.Close()
	}

	folders := make([]string, 0, len(keysByFolder))
	for folder := range keysByFolder {
		folders = append(folders, folder)
	}
	sort.Strings(folders)
	for _, folder := range folders {
		keys := make([]string, 0, len(keysByFolder[folder]))
		for key := range keysByFolder[folder] {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for start := 0; start < len(keys); start += idChunkSize {
			end := start + idChunkSize
			if end > len(keys) {
				end = len(keys)
			}
			chunk := keys[start:end]
			args := make([]any, 0, len(chunk)+1)
			args = append(args, folder)
			for _, key := range chunk {
				args = append(args, key)
			}
			rows, err := q.Query(`SELECT id FROM files WHERE folder_label = ?
				AND pair_key IN (`+placeholders(len(chunk))+`)`, args...)
			if err != nil {
				return nil, fmt.Errorf("expanding pair groups: reading members: %w", err)
			}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return nil, fmt.Errorf("expanding pair groups: scanning members: %w", err)
				}
				seen[id] = struct{}{}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, fmt.Errorf("expanding pair groups: reading members: %w", err)
			}
			rows.Close()
		}
	}

	expanded := make([]int64, 0, len(seen))
	for id := range seen {
		expanded = append(expanded, id)
	}
	sort.Slice(expanded, func(i, j int) bool { return expanded[i] < expanded[j] })
	return expanded, nil
}

func idsToAny(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

// FilesByIDs returns existing rows for ids, deduplicated and ordered by id.
func (c *Catalog) FilesByIDs(ids []int64) ([]FileRecord, error) {
	seen := make(map[int64]FileRecord)
	for _, chunk := range chunkIDs(ids) {
		files, err := c.queryFiles(`SELECT `+fileColumns+` FROM files
			WHERE id IN (`+placeholders(len(chunk))+`)`, idsToAny(chunk)...)
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			seen[file.ID] = file
		}
	}
	files := make([]FileRecord, 0, len(seen))
	for _, file := range seen {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ID < files[j].ID })
	return files, nil
}

// CanonicalPairStem implements the deliberately narrow basename candidate
// rule. IMG_E1234 and IMG_1234 share IMG_1234; all comparisons are folded.
// Appended XMP names (photo.jpg.xmp) are reduced to the original's stem.
func CanonicalPairStem(filename string) string {
	lower := strings.ToLower(strings.TrimSpace(filename))
	ext := path.Ext(lower)
	stem := strings.TrimSuffix(lower, ext)
	if ext == ".xmp" {
		if inner := path.Ext(stem); isPairStillExtension(inner) {
			stem = strings.TrimSuffix(stem, inner)
		}
	}
	if strings.HasPrefix(stem, "img_e") {
		stem = "img_" + strings.TrimPrefix(stem, "img_e")
	}
	return stem
}

// PairCandidates returns rows in the same folder and directory whose folded,
// canonical stem matches the anchor. The returned set is a candidate cluster,
// never pairing authority; callers still compare content identifiers.
func (c *Catalog) PairCandidates(anchor FileRecord) ([]FileRecord, error) {
	stem := CanonicalPairStem(anchor.Filename)
	if stem == "" {
		return []FileRecord{}, nil
	}
	dir := path.Dir(anchor.RelativePath)
	if dir == "." {
		dir = ""
	}
	stems := []string{stem}
	if strings.HasPrefix(stem, "img_") {
		stems = append(stems, "img_e"+strings.TrimPrefix(stem, "img_"))
	}
	exts := []string{".mov", ".mp4", ".heic", ".heif", ".jpg", ".jpeg", ".aae", ".xmp"}
	paths := make(map[string]struct{})
	for _, candidateStem := range stems {
		for _, ext := range exts {
			paths[strings.ToLower(path.Join(dir, candidateStem+ext))] = struct{}{}
		}
		for _, stillExt := range []string{".heic", ".heif", ".jpg", ".jpeg"} {
			paths[strings.ToLower(path.Join(dir, candidateStem+stillExt+".xmp"))] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(paths))
	for rel := range paths {
		ordered = append(ordered, rel)
	}
	sort.Strings(ordered)
	args := make([]any, 0, len(ordered)+1)
	args = append(args, anchor.FolderLabel)
	for _, rel := range ordered {
		args = append(args, rel)
	}
	return c.queryFiles(`SELECT `+fileColumns+` FROM files
		WHERE folder_label = ? AND LOWER(relative_path) IN (`+placeholders(len(ordered))+`)
		ORDER BY id`, args...)
}

// PairCandidateRefs returns the stable rows which can participate in a Live
// Photo candidate cluster. The daemon calls it once per metadata pass and
// builds a folded-stem index in Go; doing LOWER(relative_path) once per file
// would turn a 50,000-photo backfill into an O(n^2) catalog scan.
func (c *Catalog) PairCandidateRefs() ([]PairCandidateRef, error) {
	rows, err := c.db.Query(`SELECT id, folder_label, relative_path, filename, extension
		FROM files WHERE status IN ('synced', 'archived')
		AND extension IN ('.mov', '.mp4', '.heic', '.heif', '.jpg', '.jpeg', '.aae', '.xmp')
		ORDER BY folder_label, relative_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := make([]PairCandidateRef, 0)
	for rows.Next() {
		var ref PairCandidateRef
		if err := rows.Scan(&ref.ID, &ref.FolderLabel, &ref.RelativePath,
			&ref.Filename, &ref.Extension); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// SetPairAssignments applies a candidate cluster in one transaction. AAE and
// XMP rows have no content identifier of their own; when they receive a CID
// key, content_id records the established group's authority rather than
// claiming the sidecar itself carried that evidence.
func (c *Catalog) SetPairAssignments(assignments []PairAssignment) error {
	if len(assignments) == 0 {
		return nil
	}
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("setting pair assignments: begin: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE files SET pair_key = ?, pair_source = ? WHERE id = ?
		AND ((? = 'content_id' AND pair_source IN ('', 'basename'))
		  OR (? = 'basename' AND pair_source = '' AND pair_key = ''))`)
	if err != nil {
		return fmt.Errorf("setting pair assignments: prepare: %w", err)
	}
	defer stmt.Close()
	for _, assignment := range assignments {
		if assignment.Key == "" || (assignment.Source != "content_id" && assignment.Source != "basename") {
			return fmt.Errorf("invalid pair assignment for file %d", assignment.ID)
		}
		if _, err := stmt.Exec(assignment.Key, assignment.Source, assignment.ID,
			assignment.Source, assignment.Source); err != nil {
			return fmt.Errorf("setting pair assignment for file %d: %w", assignment.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("setting pair assignments: commit: %w", err)
	}
	return nil
}

// SetDegradedPair replaces every non-manual authority in one candidate
// cluster with the same canonical stem key. This is the only operation allowed
// to demote a video's CID: when ExifTool is absent there is no still CID to
// compare it with, so the group is explicitly and uniformly a basename guess.
// A later successful metadata extraction promotes rows back to CID authority.
func (c *Catalog) SetDegradedPair(ids []int64, key string) error {
	if key == "" || len(ids) == 0 {
		return nil
	}
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("setting degraded pair: begin: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE files SET pair_key = ?, pair_source = 'basename'
		WHERE id = ? AND pair_source <> 'manual'`)
	if err != nil {
		return fmt.Errorf("setting degraded pair: prepare: %w", err)
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.Exec(key, id); err != nil {
			return fmt.Errorf("setting degraded pair for file %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("setting degraded pair: commit: %w", err)
	}
	return nil
}

func isPairStillExtension(ext string) bool {
	switch strings.ToLower(ext) {
	case ".heic", ".heif", ".jpg", ".jpeg":
		return true
	default:
		return false
	}
}

// orderedPairGroups makes unpaired rows one-member groups and orders complete
// groups by their newest member. Using the newest activity prevents an old MOV
// from pulling a recently edited still to the front of the quota queue.
func orderedPairGroups(files []FileRecord) []filePairGroup {
	byKey := make(map[string]*filePairGroup)
	for _, file := range files {
		identity := fmt.Sprintf("id:%d", file.ID)
		if file.PairKey != "" {
			identity = file.FolderLabel + "\x00" + file.PairKey
		}
		group := byKey[identity]
		if group == nil {
			group = &filePairGroup{identity: identity}
			byKey[identity] = group
		}
		group.files = append(group.files, file)
		activity := file.FileMtime
		if file.FileAtime.After(activity) {
			activity = file.FileAtime
		}
		if activity.After(group.activity) {
			group.activity = activity
		}
	}
	groups := make([]filePairGroup, 0, len(byKey))
	for _, group := range byKey {
		sort.Slice(group.files, func(i, j int) bool { return group.files[i].ID < group.files[j].ID })
		groups = append(groups, *group)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].activity.Equal(groups[j].activity) {
			return groups[i].identity < groups[j].identity
		}
		return groups[i].activity.Before(groups[j].activity)
	})
	return groups
}
