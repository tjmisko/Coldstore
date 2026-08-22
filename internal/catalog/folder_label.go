package catalog

import (
	"database/sql"
	"errors"
	"fmt"
)

// FolderLabelRenameResult reports the catalog rows moved by an explicit,
// offline folder-label migration. File tags and job items follow file/job IDs
// and therefore need no rewrite.
type FolderLabelRenameResult struct {
	Files           int64
	PersistentPaths int64
	Events          int64
	Jobs            int64
}

// ValidateFolderLabelRename checks that oldLabel is the only catalog identity
// backed by syncthingID and that no in-flight work could retain the old label.
// It performs no writes. RenameFolderLabel repeats these checks inside its
// transaction, so callers may safely use this as filesystem preflight.
func (c *Catalog) ValidateFolderLabelRename(oldLabel, newLabel, syncthingID string) error {
	return validateFolderLabelRename(c.db, oldLabel, newLabel, syncthingID)
}

// RenameFolderLabel deliberately changes a catalog identity. It is the narrow,
// explicit escape hatch for a real folder split or ownership change; ordinary
// startup reconciliation still refuses label edits.
//
// Every label-bearing table is updated in one transaction. The source policy,
// pins, tags, metadata and file IDs survive. The display name and enforcement
// latches are cleared so the new identity cannot inherit stale presentation or
// a stale quota warning. budgetBytes is explicit because policy normally belongs
// to SQLite and a migration must never accidentally take the TOML seed value.
func (c *Catalog) RenameFolderLabel(oldLabel, newLabel, syncthingID, syncthingPath string,
	budgetBytes int64) (FolderLabelRenameResult, error) {
	var summary FolderLabelRenameResult
	if syncthingPath == "" {
		return summary, errors.New("new Syncthing path is empty")
	}
	if budgetBytes <= 0 {
		return summary, fmt.Errorf("new budget must be positive, got %d", budgetBytes)
	}

	tx, err := c.db.Begin()
	if err != nil {
		return summary, fmt.Errorf("renaming folder label: begin: %w", err)
	}
	defer tx.Rollback()

	if err := validateFolderLabelRename(tx, oldLabel, newLabel, syncthingID); err != nil {
		return summary, err
	}
	if err := validateThumbnailPrefixes(tx, oldLabel); err != nil {
		return summary, err
	}

	result, err := tx.Exec(`UPDATE files
		SET folder_label = ?,
			thumbnail_path = CASE WHEN thumbnail_path = '' THEN ''
				ELSE ? || substr(thumbnail_path, length(?) + 1) END
		WHERE folder_label = ?`, newLabel, newLabel, oldLabel, oldLabel)
	if err != nil {
		return summary, fmt.Errorf("renaming file rows: %w", err)
	}
	if summary.Files, err = result.RowsAffected(); err != nil {
		return summary, fmt.Errorf("counting renamed file rows: %w", err)
	}

	result, err = tx.Exec(`UPDATE persistent_paths SET folder_label = ? WHERE folder_label = ?`,
		newLabel, oldLabel)
	if err != nil {
		return summary, fmt.Errorf("renaming persistent paths: %w", err)
	}
	if summary.PersistentPaths, err = result.RowsAffected(); err != nil {
		return summary, fmt.Errorf("counting renamed persistent paths: %w", err)
	}

	result, err = tx.Exec(`UPDATE events SET folder = ? WHERE folder = ?`, newLabel, oldLabel)
	if err != nil {
		return summary, fmt.Errorf("renaming event history: %w", err)
	}
	if summary.Events, err = result.RowsAffected(); err != nil {
		return summary, fmt.Errorf("counting renamed events: %w", err)
	}

	result, err = tx.Exec(`UPDATE jobs SET folder_label = ? WHERE folder_label = ?`,
		newLabel, oldLabel)
	if err != nil {
		return summary, fmt.Errorf("renaming job history: %w", err)
	}
	if summary.Jobs, err = result.RowsAffected(); err != nil {
		return summary, fmt.Errorf("counting renamed jobs: %w", err)
	}

	result, err = tx.Exec(`UPDATE folder_configs
		SET folder_label = ?, syncthing_id = ?, syncthing_path = ?, budget_bytes = ?,
			present_in_toml = 1, wedge_warned_at = NULL, last_enforced_at = NULL,
			display_name = ''
		WHERE folder_label = ?`,
		newLabel, syncthingID, syncthingPath, budgetBytes, oldLabel)
	if err != nil {
		return summary, fmt.Errorf("renaming folder policy: %w", err)
	}
	if rows, rowsErr := result.RowsAffected(); rowsErr != nil {
		return summary, fmt.Errorf("counting renamed folder policy: %w", rowsErr)
	} else if rows != 1 {
		return summary, fmt.Errorf("renaming folder policy affected %d rows, want 1", rows)
	}

	if _, err := tx.Exec(`INSERT INTO events (event_type, folder, path, details)
		VALUES ('folder_label_migrated', ?, '', ?)`, newLabel,
		fmt.Sprintf("renamed catalog identity from %q to %q", oldLabel, newLabel)); err != nil {
		return summary, fmt.Errorf("logging folder-label migration: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return summary, fmt.Errorf("renaming folder label: commit: %w", err)
	}
	return summary, nil
}

type folderLabelQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func validateFolderLabelRename(q folderLabelQueryer, oldLabel, newLabel, syncthingID string) error {
	if oldLabel == "" || newLabel == "" {
		return errors.New("old and new folder labels are required")
	}
	if oldLabel == newLabel {
		return fmt.Errorf("old and new folder labels are both %q", oldLabel)
	}
	if syncthingID == "" {
		return errors.New("the target declaration has an empty Syncthing ID")
	}

	var cataloguedID string
	if err := q.QueryRow(`SELECT syncthing_id FROM folder_configs WHERE folder_label = ?`,
		oldLabel).Scan(&cataloguedID); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("source folder %q is not catalogued", oldLabel)
	} else if err != nil {
		return fmt.Errorf("reading source folder %q: %w", oldLabel, err)
	}
	if cataloguedID != syncthingID {
		return fmt.Errorf("source folder %q has Syncthing ID %q, but target declaration has %q; "+
			"refusing to merge different Syncthing identities",
			oldLabel, cataloguedID, syncthingID)
	}
	var otherIDOwner string
	if err := q.QueryRow(`SELECT folder_label FROM folder_configs
		WHERE syncthing_id = ? AND folder_label <> ? LIMIT 1`,
		syncthingID, oldLabel).Scan(&otherIDOwner); err == nil {
		return fmt.Errorf("Syncthing ID %q is also catalogued under folder %q",
			syncthingID, otherIDOwner)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("checking Syncthing ID ownership: %w", err)
	}

	var exists int
	if err := q.QueryRow(`SELECT 1 FROM folder_configs WHERE folder_label = ?`,
		newLabel).Scan(&exists); err == nil {
		return fmt.Errorf("destination folder %q is already catalogued", newLabel)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("checking destination folder %q: %w", newLabel, err)
	}

	for _, check := range []struct {
		name  string
		query string
	}{
		{"file rows", `SELECT COUNT(*) FROM files WHERE folder_label = ?`},
		{"persistent paths", `SELECT COUNT(*) FROM persistent_paths WHERE folder_label = ?`},
		{"event history", `SELECT COUNT(*) FROM events WHERE folder = ?`},
		{"job history", `SELECT COUNT(*) FROM jobs WHERE folder_label = ?`},
	} {
		var count int64
		if err := q.QueryRow(check.query, newLabel).Scan(&count); err != nil {
			return fmt.Errorf("checking destination %s: %w", check.name, err)
		}
		if count != 0 {
			return fmt.Errorf("destination folder %q already owns %d %s", newLabel, count, check.name)
		}
	}

	var transient int64
	if err := q.QueryRow(`SELECT COUNT(*) FROM files
		WHERE folder_label = ? AND status IN ('archiving', 'restoring')`, oldLabel).Scan(&transient); err != nil {
		return fmt.Errorf("checking in-flight file moves: %w", err)
	}
	if transient != 0 {
		return fmt.Errorf("source folder %q has %d in-flight file move(s); start the old config "+
			"and let startup recovery finish before retrying", oldLabel, transient)
	}

	var unfinishedJobs int64
	if err := q.QueryRow(`SELECT COUNT(*) FROM jobs
		WHERE folder_label = ? AND state IN ('pending', 'running')`, oldLabel).Scan(&unfinishedJobs); err != nil {
		return fmt.Errorf("checking unfinished jobs: %w", err)
	}
	if unfinishedJobs != 0 {
		return fmt.Errorf("source folder %q has %d pending or running job(s); let them finish before retrying",
			oldLabel, unfinishedJobs)
	}

	return nil
}

func validateThumbnailPrefixes(q folderLabelQueryer, oldLabel string) error {
	var invalid int64
	if err := q.QueryRow(`SELECT COUNT(*) FROM files
		WHERE folder_label = ? AND thumbnail_path <> ''
		AND substr(thumbnail_path, 1, length(?) + 1) <> ? || '/'`,
		oldLabel, oldLabel, oldLabel).Scan(&invalid); err != nil {
		return fmt.Errorf("checking thumbnail paths: %w", err)
	}
	if invalid != 0 {
		return fmt.Errorf("source folder %q has %d thumbnail path(s) outside its label directory; "+
			"refusing an ambiguous filesystem migration", oldLabel, invalid)
	}
	return nil
}
