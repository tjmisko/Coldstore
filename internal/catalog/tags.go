package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxTagSegmentRunes = 64
	maxTagPathRunes    = 255
	maxTagDepth        = 6
	MaxBulkTagIDs      = 5000
	MaxBulkTagNames    = 16
)

var ErrNoSuchTag = errors.New("no such tag")

type InvalidTagNameError struct {
	Name   string
	Reason string
}

func (e *InvalidTagNameError) Error() string {
	return fmt.Sprintf("invalid tag %q: %s", e.Name, e.Reason)
}

type TagHasDescendantsError struct {
	Name        string
	Descendants []string
}

func (e *TagHasDescendantsError) Error() string {
	return fmt.Sprintf("tag %q has descendants: %s", e.Name, strings.Join(e.Descendants, ", "))
}

type Tag struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Count           int64  `json:"count"`
	DescendantCount int64  `json:"descendant_count"`
	nameFold        string
}

type TagMutationResult struct {
	Updated int64    `json:"updated"`
	Created []string `json:"created"`
	Undo    TagUndo  `json:"undo"`
}

// TagFileMutation is one uniform mutation over a set of file ids. A bulk
// request normally contains one of these; an exact undo may contain several,
// because the files did not necessarily have the same tags beforehand.
type TagFileMutation struct {
	IDs    []int64  `json:"ids"`
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

// TagUndo is directly replayable as POST /api/v1/files/tags {"changes": ...}.
// Changes are grouped by tag and direction instead of repeating every tag name
// once per file, but name exactly the file associations actually changed.
type TagUndo struct {
	Changes []TagFileMutation `json:"changes"`
}

type TagMutationLimitError struct {
	Field string
	Limit int
}

func (e *TagMutationLimitError) Error() string {
	return fmt.Sprintf("at most %d unique %s per request", e.Limit, e.Field)
}

type TagRenameResult struct {
	Merged        bool  `json:"merged"`
	FilesAffected int64 `json:"files_affected"`
}

type TagDeleteResult struct {
	Deleted       int64 `json:"deleted"`
	FilesAffected int64 `json:"files_affected"`
}

type normalizedTag struct {
	name string
	fold string
}

// normalizeTagName applies the vocabulary rules before a tag reaches SQL.
// strings.ToLower is Unicode-aware; SQLite's built-in NOCASE is ASCII-only.
func normalizeTagName(raw string) (normalizedTag, error) {
	invalid := func(reason string) (normalizedTag, error) {
		return normalizedTag{}, &InvalidTagNameError{Name: raw, Reason: reason}
	}
	if raw == "" {
		return invalid("name is empty")
	}
	if strings.HasPrefix(raw, "/") || strings.HasSuffix(raw, "/") {
		return invalid("leading or trailing slash is not allowed")
	}
	parts := strings.Split(raw, "/")
	if len(parts) > maxTagDepth {
		return invalid(fmt.Sprintf("at most %d segments are allowed", maxTagDepth))
	}

	normalized := make([]string, 0, len(parts))
	for _, part := range parts {
		var b strings.Builder
		pendingSpace := false
		wrote := false
		for _, r := range part {
			if unicode.IsControl(r) {
				return invalid("control characters are not allowed")
			}
			if unicode.IsSpace(r) {
				if wrote {
					pendingSpace = true
				}
				continue
			}
			if pendingSpace {
				b.WriteByte(' ')
				pendingSpace = false
			}
			b.WriteRune(r)
			wrote = true
		}
		segment := b.String()
		if segment == "" {
			return invalid("segments may not be empty")
		}
		if utf8.RuneCountInString(segment) > maxTagSegmentRunes {
			return invalid(fmt.Sprintf("segments may not exceed %d characters", maxTagSegmentRunes))
		}
		normalized = append(normalized, segment)
	}

	name := strings.Join(normalized, "/")
	if utf8.RuneCountInString(name) > maxTagPathRunes {
		return invalid(fmt.Sprintf("path may not exceed %d characters", maxTagPathRunes))
	}
	return normalizedTag{name: name, fold: strings.ToLower(name)}, nil
}

func normalizeTagNames(raw []string) ([]normalizedTag, error) {
	result := make([]normalizedTag, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, value := range raw {
		normalized, err := normalizeTagName(value)
		if err != nil {
			return nil, err
		}
		if seen[normalized.fold] {
			continue
		}
		seen[normalized.fold] = true
		result = append(result, normalized)
	}
	return result, nil
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}

func tagSubtreePattern(fold string) string { return escapeLike(fold) + `/%` }

func findTagByFold(s fileScanner, fold string) (Tag, error) {
	var tag Tag
	err := s.Scan(&tag.ID, &tag.Name, &tag.nameFold)
	if errors.Is(err, sql.ErrNoRows) {
		return Tag{}, ErrNoSuchTag
	}
	return tag, err
}

// resolveTagTx creates every missing ancestor and returns the requested leaf.
// Existing ancestor display spelling wins, keeping child display paths
// consistent when a request differs only by case.
func resolveTagTx(tx *sql.Tx, normalized normalizedTag, now int64) (Tag, []string, error) {
	nameParts := strings.Split(normalized.name, "/")
	foldParts := strings.Split(normalized.fold, "/")
	canonicalName := ""
	created := make([]string, 0, len(nameParts))
	var leaf Tag

	for i, segment := range nameParts {
		fold := strings.Join(foldParts[:i+1], "/")
		name := segment
		if canonicalName != "" {
			name = canonicalName + "/" + segment
		}
		res, err := tx.Exec(`INSERT OR IGNORE INTO tags (name, name_fold, created_at)
			VALUES (?, ?, ?)`, name, fold, now)
		if err != nil {
			return Tag{}, nil, err
		}
		if inserted, err := res.RowsAffected(); err != nil {
			return Tag{}, nil, err
		} else if inserted == 1 {
			created = append(created, name)
		}
		leaf, err = findTagByFold(tx.QueryRow(`SELECT id, name, name_fold FROM tags
			WHERE name_fold = ?`, fold), fold)
		if err != nil {
			return Tag{}, nil, err
		}
		canonicalName = leaf.Name
	}
	return leaf, created, nil
}

// ResolveOrCreateTags materialises all ancestors and returns the requested
// leaf tags. Created contains every row this call brought into existence.
func (c *Catalog) ResolveOrCreateTags(names []string, now int64) ([]Tag, []string, error) {
	normalized, err := normalizeTagNames(names)
	if err != nil {
		return nil, nil, err
	}
	tx, err := c.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	tags := make([]Tag, 0, len(normalized))
	created := make([]string, 0)
	for _, name := range normalized {
		tag, added, err := resolveTagTx(tx, name, now)
		if err != nil {
			return nil, nil, err
		}
		tags = append(tags, tag)
		created = append(created, added...)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return tags, created, nil
}

func uniqueIDs(ids []int64) []int64 {
	result := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, id)
	}
	return result
}

func existingFileIDsTx(tx *sql.Tx, ids []int64) ([]int64, error) {
	exists := make(map[int64]bool, len(ids))
	for _, chunk := range chunkIDs(ids) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := tx.Query(`SELECT id FROM files WHERE id IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			exists[id] = true
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	result := make([]int64, 0, len(exists))
	for _, id := range ids {
		if exists[id] {
			result = append(result, id)
		}
	}
	return result, nil
}

func assignedFileIDsTx(tx *sql.Tx, tagID int64, ids []int64) (map[int64]bool, error) {
	assigned := make(map[int64]bool)
	for _, chunk := range chunkIDs(ids) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, tagID)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := tx.Query(`SELECT file_id FROM file_tags WHERE tag_id = ?
			AND file_id IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			assigned[id] = true
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return assigned, nil
}

func insertTagAssignmentsTx(tx *sql.Tx, tagID int64, ids []int64, at int64) error {
	for _, chunk := range chunkIDs(ids) {
		values := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk)*3)
		for _, id := range chunk {
			values = append(values, "(?, ?, ?)")
			args = append(args, id, tagID, at)
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO file_tags
			(file_id, tag_id, applied_at) VALUES `+strings.Join(values, ","), args...); err != nil {
			return err
		}
	}
	return nil
}

func deleteTagAssignmentsTx(tx *sql.Tx, tagID int64, ids []int64) error {
	for _, chunk := range chunkIDs(ids) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, tagID)
		for _, id := range chunk {
			args = append(args, id)
		}
		if _, err := tx.Exec(`DELETE FROM file_tags WHERE tag_id = ? AND file_id IN (`+
			placeholders(len(chunk))+`)`, args...); err != nil {
			return err
		}
	}
	return nil
}

type preparedTagMutations struct {
	ids         []int64
	addNames    []normalizedTag
	removeNames []normalizedTag
	addIDs      map[string][]int64
	removeIDs   map[string][]int64
}

func prepareTagMutations(mutations []TagFileMutation, enforceBulkLimits bool) (preparedTagMutations, error) {
	prepared := preparedTagMutations{
		addIDs:    make(map[string][]int64),
		removeIDs: make(map[string][]int64),
	}
	idSeen := make(map[int64]bool)
	nameSeen := make(map[string]bool)
	addSeen := make(map[string]bool)
	removeSeen := make(map[string]bool)
	// One intent per (file, folded tag). Repeating an intent is harmless;
	// opposing intents for the same association are an ambiguous request.
	intents := make(map[int64]map[string]int8)

	for _, mutation := range mutations {
		add, err := normalizeTagNames(mutation.Add)
		if err != nil {
			return prepared, err
		}
		remove, err := normalizeTagNames(mutation.Remove)
		if err != nil {
			return prepared, err
		}
		addInMutation := make(map[string]bool, len(add))
		for _, name := range add {
			addInMutation[name.fold] = true
		}
		for _, name := range remove {
			if addInMutation[name.fold] {
				return prepared, &InvalidTagNameError{
					Name: name.name, Reason: "cannot be both added and removed",
				}
			}
		}

		for _, name := range add {
			if !addSeen[name.fold] {
				prepared.addNames = append(prepared.addNames, name)
				addSeen[name.fold] = true
			}
			nameSeen[name.fold] = true
		}
		for _, name := range remove {
			if !removeSeen[name.fold] {
				prepared.removeNames = append(prepared.removeNames, name)
				removeSeen[name.fold] = true
			}
			nameSeen[name.fold] = true
		}

		for _, id := range uniqueIDs(mutation.IDs) {
			if !idSeen[id] {
				idSeen[id] = true
				prepared.ids = append(prepared.ids, id)
			}
			if intents[id] == nil {
				intents[id] = make(map[string]int8)
			}
			for _, name := range add {
				if intents[id][name.fold] == -1 {
					return prepared, &InvalidTagNameError{
						Name: name.name, Reason: "cannot be both added and removed for the same file",
					}
				}
				if intents[id][name.fold] == 0 {
					intents[id][name.fold] = 1
					prepared.addIDs[name.fold] = append(prepared.addIDs[name.fold], id)
				}
			}
			for _, name := range remove {
				if intents[id][name.fold] == 1 {
					return prepared, &InvalidTagNameError{
						Name: name.name, Reason: "cannot be both added and removed for the same file",
					}
				}
				if intents[id][name.fold] == 0 {
					intents[id][name.fold] = -1
					prepared.removeIDs[name.fold] = append(prepared.removeIDs[name.fold], id)
				}
			}
		}
	}

	if enforceBulkLimits {
		if len(nameSeen) > MaxBulkTagNames {
			return prepared, &TagMutationLimitError{Field: "tag names", Limit: MaxBulkTagNames}
		}
	}
	return prepared, nil
}

func existingIDSet(ids []int64) map[int64]bool {
	result := make(map[int64]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result
}

func onlyExistingIDs(ids []int64, existing map[int64]bool) []int64 {
	result := make([]int64, 0, len(ids))
	for _, id := range ids {
		if existing[id] {
			result = append(result, id)
		}
	}
	return result
}

func undoChange(ids []int64, add, remove string) TagFileMutation {
	change := TagFileMutation{IDs: ids, Add: []string{}, Remove: []string{}}
	if add != "" {
		change.Add = append(change.Add, add)
	}
	if remove != "" {
		change.Remove = append(change.Remove, remove)
	}
	return change
}

func (c *Catalog) applyTagMutations(mutations []TagFileMutation, at int64, enforceBulkLimits bool) (TagMutationResult, error) {
	result := TagMutationResult{Created: []string{}, Undo: TagUndo{Changes: []TagFileMutation{}}}
	prepared, err := prepareTagMutations(mutations, enforceBulkLimits)
	if err != nil {
		return result, err
	}
	if len(prepared.ids) == 0 || len(prepared.addNames)+len(prepared.removeNames) == 0 {
		return result, nil
	}

	tx, err := c.db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	existingIDs, err := existingFileIDsTx(tx, prepared.ids)
	if err != nil {
		return result, err
	}
	if enforceBulkLimits && len(existingIDs) > MaxBulkTagIDs {
		return result, &TagMutationLimitError{Field: "file ids", Limit: MaxBulkTagIDs}
	}
	if len(existingIDs) == 0 {
		if err := tx.Commit(); err != nil {
			return result, err
		}
		return result, nil
	}
	existing := existingIDSet(existingIDs)
	changedIDs := make(map[int64]bool)

	for _, name := range prepared.addNames {
		wanted := onlyExistingIDs(prepared.addIDs[name.fold], existing)
		if len(wanted) == 0 {
			continue
		}
		tag, created, err := resolveTagTx(tx, name, at)
		if err != nil {
			return result, err
		}
		result.Created = append(result.Created, created...)
		assigned, err := assignedFileIDsTx(tx, tag.ID, wanted)
		if err != nil {
			return result, err
		}
		changed := make([]int64, 0, len(wanted)-len(assigned))
		for _, id := range wanted {
			if !assigned[id] {
				changed = append(changed, id)
				changedIDs[id] = true
			}
		}
		if err := insertTagAssignmentsTx(tx, tag.ID, changed, at); err != nil {
			return result, err
		}
		if len(changed) > 0 {
			result.Undo.Changes = append(result.Undo.Changes, undoChange(changed, "", tag.Name))
		}
	}

	for _, name := range prepared.removeNames {
		wanted := onlyExistingIDs(prepared.removeIDs[name.fold], existing)
		if len(wanted) == 0 {
			continue
		}
		tag, err := findTagByFold(tx.QueryRow(`SELECT id, name, name_fold FROM tags
			WHERE name_fold = ?`, name.fold), name.fold)
		if errors.Is(err, ErrNoSuchTag) {
			continue
		}
		if err != nil {
			return result, err
		}
		assigned, err := assignedFileIDsTx(tx, tag.ID, wanted)
		if err != nil {
			return result, err
		}
		changed := make([]int64, 0, len(assigned))
		for _, id := range wanted {
			if assigned[id] {
				changed = append(changed, id)
				changedIDs[id] = true
			}
		}
		if err := deleteTagAssignmentsTx(tx, tag.ID, changed); err != nil {
			return result, err
		}
		if len(changed) > 0 {
			result.Undo.Changes = append(result.Undo.Changes, undoChange(changed, tag.Name, ""))
		}
	}

	result.Updated = int64(len(changedIDs))
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

// ApplyTagMutations atomically applies one or more heterogeneous tag changes.
// Unknown ids are ignored, matching SetPinnedBulk; the undo names only
// existing files whose associations actually changed.
func (c *Catalog) ApplyTagMutations(mutations []TagFileMutation, at int64) (TagMutationResult, error) {
	return c.applyTagMutations(mutations, at, true)
}

// ApplyTags atomically adds and removes leaf tags. Ancestors are materialised
// but are never applied. Updated counts distinct files whose associations
// actually changed; unknown file ids are ignored like the bulk pin primitive.
func (c *Catalog) ApplyTags(ids []int64, add, remove []string, at int64) (TagMutationResult, error) {
	// The 16-name cap belongs only to the bulk endpoint. The existing
	// single-file API predates it and remains source-compatible.
	return c.applyTagMutations([]TagFileMutation{{IDs: ids, Add: add, Remove: remove}}, at, false)
}

func (c *Catalog) hydrateFileTags(files []FileRecord) error {
	if len(files) == 0 {
		return nil
	}
	byID := make(map[int64]int, len(files))
	ids := make([]int64, 0, len(files))
	for i := range files {
		files[i].Tags = make([]string, 0)
		byID[files[i].ID] = i
		ids = append(ids, files[i].ID)
	}
	for _, chunk := range chunkIDs(ids) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := c.db.Query(`SELECT ft.file_id, t.name
			FROM file_tags ft JOIN tags t ON t.id = ft.tag_id
			WHERE ft.file_id IN (`+placeholders(len(chunk))+`)
			ORDER BY ft.file_id, t.name_fold`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var fileID int64
			var name string
			if err := rows.Scan(&fileID, &name); err != nil {
				rows.Close()
				return err
			}
			if index, ok := byID[fileID]; ok {
				files[index].Tags = append(files[index].Tags, name)
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) hydrateFile(file *FileRecord) (*FileRecord, error) {
	files := []FileRecord{*file}
	if err := c.hydrateFileTags(files); err != nil {
		return nil, err
	}
	return &files[0], nil
}

// ListTags returns exact and subtree counts in one GROUP BY query. With a
// folder filter, unused tags are omitted from that folder's vocabulary.
func (c *Catalog) ListTags(folder, prefix string, limit int) ([]Tag, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	for _, r := range prefix {
		if unicode.IsControl(r) {
			return nil, &InvalidTagNameError{Name: prefix, Reason: "control characters are not allowed"}
		}
	}
	foldedPrefix := strings.ToLower(strings.TrimSpace(prefix))
	if utf8.RuneCountInString(foldedPrefix) > maxTagPathRunes {
		return nil, &InvalidTagNameError{Name: prefix, Reason: "prefix is too long"}
	}
	pattern := escapeLike(foldedPrefix) + "%"

	rows, err := c.db.Query(`
		SELECT t.id, t.name, t.name_fold,
		       COUNT(DISTINCT CASE WHEN d.id = t.id THEN f.id END) AS exact_count,
		       COUNT(DISTINCT f.id) AS descendant_count
		FROM tags t
		LEFT JOIN tags d ON d.name_fold = t.name_fold
			OR substr(d.name_fold, 1, length(t.name_fold) + 1) = t.name_fold || '/'
		LEFT JOIN file_tags ft ON ft.tag_id = d.id
		LEFT JOIN files f ON f.id = ft.file_id AND (? = '' OR f.folder_label = ?)
		WHERE t.name_fold LIKE ? ESCAPE '\'
		GROUP BY t.id, t.name, t.name_fold
		HAVING ? = '' OR COUNT(DISTINCT f.id) > 0
		ORDER BY exact_count DESC, t.name_fold ASC
		LIMIT ?`, folder, folder, pattern, folder, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tags := make([]Tag, 0)
	for rows.Next() {
		var tag Tag
		if err := rows.Scan(&tag.ID, &tag.Name, &tag.nameFold, &tag.Count, &tag.DescendantCount); err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

type tagRow struct {
	ID   int64
	Name string
	Fold string
}

func subtreeRowsTx(tx *sql.Tx, fold string) ([]tagRow, error) {
	rows, err := tx.Query(`SELECT id, name, name_fold FROM tags
		WHERE name_fold = ? OR name_fold LIKE ? ESCAPE '\'
		ORDER BY length(name_fold), name_fold`, fold, tagSubtreePattern(fold))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]tagRow, 0)
	for rows.Next() {
		var row tagRow
		if err := rows.Scan(&row.ID, &row.Name, &row.Fold); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func filesAffectedByTagsTx(tx *sql.Tx, rows []tagRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	args := make([]any, len(rows))
	for i, row := range rows {
		args[i] = row.ID
	}
	var count int64
	err := tx.QueryRow(`SELECT COUNT(DISTINCT file_id) FROM file_tags WHERE tag_id IN (`+
		placeholders(len(rows))+`)`, args...).Scan(&count)
	return count, err
}

// RenameTag renames a whole materialised subtree. Destination collisions are
// merged transactionally, preserving the union of every file association.
func (c *Catalog) RenameTag(oldName, newName string, now int64) (TagRenameResult, error) {
	var result TagRenameResult
	oldNormalized, err := normalizeTagName(oldName)
	if err != nil {
		return result, err
	}
	newNormalized, err := normalizeTagName(newName)
	if err != nil {
		return result, err
	}
	if strings.HasPrefix(newNormalized.fold, oldNormalized.fold+"/") {
		return result, &InvalidTagNameError{Name: newName, Reason: "a tag cannot be renamed into its own subtree"}
	}

	tx, err := c.db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	source, err := findTagByFold(tx.QueryRow(`SELECT id, name, name_fold FROM tags
		WHERE name_fold = ?`, oldNormalized.fold), oldNormalized.fold)
	if err != nil {
		return result, err
	}
	rows, err := subtreeRowsTx(tx, source.nameFold)
	if err != nil {
		return result, err
	}
	result.FilesAffected, err = filesAffectedByTagsTx(tx, rows)
	if err != nil {
		return result, err
	}

	newParts := strings.Split(newNormalized.name, "/")
	newFoldParts := strings.Split(newNormalized.fold, "/")
	newRootName := newParts[len(newParts)-1]
	if len(newParts) > 1 {
		parent := normalizedTag{
			name: strings.Join(newParts[:len(newParts)-1], "/"),
			fold: strings.Join(newFoldParts[:len(newFoldParts)-1], "/"),
		}
		parentTag, _, err := resolveTagTx(tx, parent, now)
		if err != nil {
			return result, err
		}
		newRootName = parentTag.Name + "/" + newParts[len(newParts)-1]
	}
	if destination, err := findTagByFold(tx.QueryRow(`SELECT id, name, name_fold FROM tags
		WHERE name_fold = ?`, newNormalized.fold), newNormalized.fold); err == nil {
		if destination.ID != source.ID {
			newRootName = destination.Name
		}
	} else if !errors.Is(err, ErrNoSuchTag) {
		return result, err
	}

	if source.nameFold == newNormalized.fold {
		for _, row := range rows {
			suffix := strings.TrimPrefix(row.Name, source.Name)
			if _, err := tx.Exec(`UPDATE tags SET name = ? WHERE id = ?`, newRootName+suffix, row.ID); err != nil {
				return result, err
			}
		}
		if err := tx.Commit(); err != nil {
			return result, err
		}
		return result, nil
	}

	for _, row := range rows {
		suffixFold := strings.TrimPrefix(row.Fold, source.nameFold)
		suffixName := strings.TrimPrefix(row.Name, source.Name)
		destinationFold := newNormalized.fold + suffixFold
		destinationName := newRootName + suffixName
		destination, findErr := findTagByFold(tx.QueryRow(`SELECT id, name, name_fold FROM tags
			WHERE name_fold = ?`, destinationFold), destinationFold)
		switch {
		case findErr == nil && destination.ID != row.ID:
			result.Merged = true
			if _, err := tx.Exec(`INSERT OR IGNORE INTO file_tags (file_id, tag_id, applied_at)
				SELECT file_id, ?, applied_at FROM file_tags WHERE tag_id = ?`, destination.ID, row.ID); err != nil {
				return result, err
			}
			if _, err := tx.Exec(`DELETE FROM file_tags WHERE tag_id = ?`, row.ID); err != nil {
				return result, err
			}
			if _, err := tx.Exec(`DELETE FROM tags WHERE id = ?`, row.ID); err != nil {
				return result, err
			}
		case errors.Is(findErr, ErrNoSuchTag):
			if _, err := tx.Exec(`UPDATE tags SET name = ?, name_fold = ? WHERE id = ?`,
				destinationName, destinationFold, row.ID); err != nil {
				return result, err
			}
		case findErr != nil:
			return result, findErr
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

// DeleteTag removes associations before tag rows because the schema
// deliberately has no inert REFERENCES clauses. Descendants require an
// explicit cascade request.
func (c *Catalog) DeleteTag(name string, cascade bool) (TagDeleteResult, error) {
	var result TagDeleteResult
	normalized, err := normalizeTagName(name)
	if err != nil {
		return result, err
	}
	tx, err := c.db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	root, err := findTagByFold(tx.QueryRow(`SELECT id, name, name_fold FROM tags
		WHERE name_fold = ?`, normalized.fold), normalized.fold)
	if err != nil {
		return result, err
	}
	rows, err := subtreeRowsTx(tx, root.nameFold)
	if err != nil {
		return result, err
	}
	if len(rows) > 1 && !cascade {
		descendants := make([]string, 0, len(rows)-1)
		for _, row := range rows[1:] {
			descendants = append(descendants, row.Name)
		}
		return result, &TagHasDescendantsError{Name: root.Name, Descendants: descendants}
	}
	result.FilesAffected, err = filesAffectedByTagsTx(tx, rows)
	if err != nil {
		return result, err
	}
	args := make([]any, len(rows))
	for i, row := range rows {
		args[i] = row.ID
	}
	if _, err := tx.Exec(`DELETE FROM file_tags WHERE tag_id IN (`+placeholders(len(rows))+`)`, args...); err != nil {
		return result, err
	}
	res, err := tx.Exec(`DELETE FROM tags WHERE id IN (`+placeholders(len(rows))+`)`, args...)
	if err != nil {
		return result, err
	}
	result.Deleted, err = res.RowsAffected()
	if err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}
