package catalog

import (
	"fmt"
	"time"
)

// BeginTrash records the source tree before any bytes move. That source is
// durable crash-recovery information: a trashed archived file returns to the
// archive, while a trashed synced file returns to its Syncthing folder.
func (c *Catalog) BeginTrash(id int64, from FileStatus) (bool, error) {
	if from != StatusSynced && from != StatusArchived {
		return false, fmt.Errorf("invalid trash source status %q", from)
	}
	res, err := c.db.Exec(`UPDATE files
		SET status = 'trashing', trashed_from = ?, trashed_at = NULL
		WHERE id = ? AND status = ?`, from, id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (c *Catalog) AbortTrash(id int64, to FileStatus) (bool, error) {
	if to != StatusSynced && to != StatusArchived {
		return false, fmt.Errorf("invalid trash rollback status %q", to)
	}
	res, err := c.db.Exec(`UPDATE files SET status = ?, trashed_from = '', trashed_at = NULL,
		last_seen = CASE WHEN ? = 'synced' THEN ? ELSE last_seen END
		WHERE id = ? AND status = 'trashing' AND trashed_from = ?`,
		to, to, time.Now().UnixNano(), id, to)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (c *Catalog) MarkTrashed(id int64, at time.Time) (bool, error) {
	res, err := c.db.Exec(`UPDATE files SET status = 'trashed', trashed_at = ?
		WHERE id = ? AND status = 'trashing'
		AND trashed_from IN ('synced','archived')`, at.UTC(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (c *Catalog) BeginUntrash(id int64) (bool, error) {
	res, err := c.db.Exec(`UPDATE files SET status = 'untrashing'
		WHERE id = ? AND status = 'trashed'
		AND trashed_from IN ('synced','archived')`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (c *Catalog) AbortUntrash(id int64) (bool, error) {
	res, err := c.db.Exec(`UPDATE files SET status = 'trashed'
		WHERE id = ? AND status = 'untrashing'`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// MarkUntrashed commits to the recorded source state and then clears the
// recovery fields. SQLite evaluates every SET expression against the old row,
// so each CASE below still sees trashed_from before it is blanked.
func (c *Catalog) MarkUntrashed(id int64) (bool, error) {
	res, err := c.db.Exec(`UPDATE files SET
		status = trashed_from,
		archived_at = CASE WHEN trashed_from = 'synced' THEN NULL ELSE archived_at END,
		last_seen = CASE WHEN trashed_from = 'synced' THEN ? ELSE last_seen END,
		trashed_at = NULL,
		trashed_from = ''
		WHERE id = ? AND status = 'untrashing'
		AND trashed_from IN ('synced','archived')`, time.Now().UnixNano(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// MarkTrashMissing is the last-copy-gone recovery outcome. It deliberately
// keeps trashed_from for diagnosis while taking the row out of the trash bin.
func (c *Catalog) MarkTrashMissing(id int64, from FileStatus) (bool, error) {
	res, err := c.db.Exec(`UPDATE files SET status = 'missing', trashed_at = NULL
		WHERE id = ? AND status IN ('trashing','untrashing') AND trashed_from = ?`, id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeleteTrashedFile removes the catalog record and its tag links in one
// transaction after the mover has removed the bytes. Job history is retained:
// job_items intentionally has no foreign key to files.
func (c *Catalog) DeleteTrashedFile(id int64) (bool, error) {
	tx, err := c.db.Begin()
	if err != nil {
		return false, fmt.Errorf("deleting trashed file: begin: %w", err)
	}
	defer tx.Rollback()

	var status FileStatus
	if err := tx.QueryRow(`SELECT status FROM files WHERE id = ?`, id).Scan(&status); err != nil {
		return false, err
	}
	if status != StatusTrashed {
		return false, nil
	}
	if _, err := tx.Exec(`DELETE FROM file_tags WHERE file_id = ?`, id); err != nil {
		return false, fmt.Errorf("deleting file tags: %w", err)
	}
	res, err := tx.Exec(`DELETE FROM files WHERE id = ? AND status = 'trashed'`, id)
	if err != nil {
		return false, fmt.Errorf("deleting file row: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("deleting trashed file: commit: %w", err)
	}
	return n == 1, nil
}
