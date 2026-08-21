package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"path"
	"strings"
)

// ErrRenameConflict means a catalog destination is already occupied or the
// source changed after rename preflight.
var ErrRenameConflict = errors.New("rename catalog conflict")

// FilesAtOrBelow returns the exact row and every descendant of rel. It is the
// catalog half of deciding whether a rename source is one file or a directory.
func (c *Catalog) FilesAtOrBelow(folder, rel string) ([]FileRecord, error) {
	return c.queryFiles(`SELECT `+fileColumns+` FROM files
		WHERE folder_label = ? AND (relative_path = ? OR
			substr(relative_path, 1, length(?) + 1) = ? || '/')
		ORDER BY relative_path, id`, folder, rel, rel, rel)
}

// AffectedPersistentPatterns returns owner-authored patterns whose literal
// prefix names the path being moved. Rename never rewrites these policy rows;
// the API warns so the owner can decide how the glob should evolve.
func (c *Catalog) AffectedPersistentPatterns(folder, rel string) ([]string, error) {
	paths, err := c.ListPersistentPaths(folder)
	if err != nil {
		return nil, err
	}
	warnings := make([]string, 0)
	for _, persistent := range paths {
		pattern := strings.TrimPrefix(strings.ReplaceAll(persistent.Pattern, "\\", "/"), "./")
		if strings.HasPrefix(pattern, rel) {
			warnings = append(warnings, persistent.Pattern)
		}
	}
	return warnings, nil
}

// RenameDirectoryRows performs the coordinate rewrite after all three trees
// have reached their destination. It is one transaction and uses prefix
// updates, so a directory containing thousands of files is not a per-row job.
func (c *Catalog) RenameDirectoryRows(folder, from, to string) (int64, error) {
	tx, err := c.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("renaming directory rows: begin: %w", err)
	}
	defer tx.Rollback()

	if err := rejectTransientRenameRows(tx, folder, from); err != nil {
		return 0, err
	}
	var occupied int
	err = tx.QueryRow(`SELECT COUNT(*) FROM files WHERE folder_label = ?
		AND (relative_path = ? OR substr(relative_path, 1, length(?) + 1) = ? || '/')`,
		folder, to, to, to).Scan(&occupied)
	if err != nil {
		return 0, err
	}
	if occupied != 0 {
		return 0, fmt.Errorf("%w: catalog destination %q is occupied", ErrRenameConflict, to)
	}

	fromThumb := path.Join(folder, from)
	toThumb := path.Join(folder, to)
	fromPair := "stem:" + from
	toPair := "stem:" + to
	res, err := tx.Exec(`UPDATE files SET
		relative_path = ? || substr(relative_path, length(?) + 1),
		thumbnail_path = CASE
			WHEN thumbnail_path = '' THEN ''
			WHEN thumbnail_path = ? OR
				substr(thumbnail_path, 1, length(?) + 1) = ? || '/'
				THEN ? || substr(thumbnail_path, length(?) + 1)
			ELSE thumbnail_path END,
		pair_key = CASE
			WHEN pair_source = 'basename' AND (pair_key = ? OR
				substr(pair_key, 1, length(?) + 1) = ? || '/')
				THEN ? || substr(pair_key, length(?) + 1)
			ELSE pair_key END
		WHERE folder_label = ? AND (relative_path = ? OR
			substr(relative_path, 1, length(?) + 1) = ? || '/')`,
		to, from,
		fromThumb, fromThumb, fromThumb, toThumb, fromThumb,
		fromPair, fromPair, fromPair, toPair, fromPair,
		folder, from, from, from)
	if err != nil {
		return 0, fmt.Errorf("renaming directory rows: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("renaming directory rows: commit: %w", err)
	}
	return n, nil
}

// FileRename is one member of a group-preserving single-file rename.
type FileRename struct {
	ID   int64
	From string
	To   string
}

// RenameFileRows applies every pair-member coordinate in one transaction.
// Extension changes reset both derived queues and clear non-manual extracted
// values; the underlying content-sniffed type remains valid because a rename
// does not change bytes.
func (c *Catalog) RenameFileRows(folder string, renames []FileRename) error {
	if len(renames) == 0 {
		return nil
	}
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("renaming file rows: begin: %w", err)
	}
	defer tx.Rollback()

	ids := make(map[int64]struct{}, len(renames))
	for _, rename := range renames {
		ids[rename.ID] = struct{}{}
	}
	for _, rename := range renames {
		var current string
		var status FileStatus
		err := tx.QueryRow(`SELECT relative_path, status FROM files WHERE id = ? AND folder_label = ?`,
			rename.ID, folder).Scan(&current, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: source row %d disappeared", ErrRenameConflict, rename.ID)
		}
		if err != nil {
			return err
		}
		if current != rename.From {
			// A resumed job may find the complete mapping already committed.
			if current == rename.To {
				continue
			}
			return fmt.Errorf("%w: file %d moved from %q", ErrRenameConflict, rename.ID, rename.From)
		}
		if status == StatusArchiving || status == StatusRestoring {
			return fmt.Errorf("%w: file %d is %s", ErrRenameConflict, rename.ID, status)
		}
		var occupant int64
		err = tx.QueryRow(`SELECT id FROM files WHERE folder_label = ? AND relative_path = ?`,
			folder, rename.To).Scan(&occupant)
		if err == nil {
			if _, partOfRename := ids[occupant]; !partOfRename || occupant != rename.ID {
				return fmt.Errorf("%w: catalog destination %q is occupied", ErrRenameConflict, rename.To)
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}

	stmt, err := tx.Prepare(`UPDATE files SET
		relative_path = ?, filename = ?, extension = ?, mime_type = ?,
		thumbnail_path = CASE WHEN ? THEN '' WHEN thumbnail_path = '' THEN '' ELSE ? END,
		thumb_attempts = CASE WHEN ? THEN 0 ELSE thumb_attempts END,
		thumb_last_attempt = CASE WHEN ? THEN NULL ELSE thumb_last_attempt END,
		thumb_error = CASE WHEN ? THEN '' ELSE thumb_error END,
		meta_scanned_at = CASE WHEN ? THEN NULL ELSE meta_scanned_at END,
		meta_attempts = CASE WHEN ? THEN 0 ELSE meta_attempts END,
		meta_last_attempt = CASE WHEN ? THEN NULL ELSE meta_last_attempt END,
		meta_error = CASE WHEN ? THEN '' ELSE meta_error END,
		captured_local = CASE WHEN ? AND captured_source <> 'manual' THEN '' ELSE captured_local END,
		captured_offset_minutes = CASE WHEN ? AND captured_source <> 'manual' THEN NULL ELSE captured_offset_minutes END,
		captured_source = CASE WHEN ? AND captured_source <> 'manual' THEN '' ELSE captured_source END,
		gps_lat = CASE WHEN ? AND gps_source <> 'manual' THEN NULL ELSE gps_lat END,
		gps_lon = CASE WHEN ? AND gps_source <> 'manual' THEN NULL ELSE gps_lon END,
		gps_altitude_m = CASE WHEN ? AND gps_source <> 'manual' THEN NULL ELSE gps_altitude_m END,
		gps_accuracy_m = CASE WHEN ? AND gps_source <> 'manual' THEN NULL ELSE gps_accuracy_m END,
		gps_source = CASE WHEN ? AND gps_source <> 'manual' THEN '' ELSE gps_source END,
		pair_key = CASE WHEN pair_source = 'basename' THEN ? ELSE pair_key END
		WHERE id = ? AND folder_label = ? AND relative_path = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, rename := range renames {
		oldExt := strings.ToLower(path.Ext(rename.From))
		newExt := strings.ToLower(path.Ext(rename.To))
		extChanged := oldExt != newExt
		thumb := path.Join(folder, rename.To) + ".thumb.jpg"
		dir := path.Dir(rename.To)
		if dir == "." {
			dir = ""
		}
		pairKey := "stem:" + path.Join(dir, CanonicalPairStem(path.Base(rename.To)))
		if _, err := stmt.Exec(
			rename.To, path.Base(rename.To), newExt, mime.TypeByExtension(newExt),
			extChanged, thumb,
			extChanged, extChanged, extChanged,
			extChanged, extChanged, extChanged, extChanged,
			extChanged, extChanged, extChanged,
			extChanged, extChanged, extChanged, extChanged, extChanged,
			pairKey, rename.ID, folder, rename.From,
		); err != nil {
			return fmt.Errorf("renaming file row %d: %w", rename.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("renaming file rows: commit: %w", err)
	}
	return nil
}

func rejectTransientRenameRows(tx *sql.Tx, folder, rel string) error {
	var id int64
	var status string
	err := tx.QueryRow(`SELECT id, status FROM files WHERE folder_label = ?
		AND (relative_path = ? OR substr(relative_path, 1, length(?) + 1) = ? || '/')
		AND status IN ('archiving','restoring') ORDER BY relative_path LIMIT 1`,
		folder, rel, rel, rel).Scan(&id, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: file %d is %s", ErrRenameConflict, id, status)
}
