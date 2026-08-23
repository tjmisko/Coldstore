package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func (m *Mover) trashRoot(folder string) string {
	return filepath.Join(m.cfg.Storage.ColdStoragePath, "trash", folder)
}

func (m *Mover) trashPath(file *catalog.FileRecord) string {
	return filepath.Join(m.trashRoot(file.FolderLabel), file.RelativePath)
}

func (m *Mover) originalPath(file *catalog.FileRecord, status catalog.FileStatus) (string, error) {
	switch status {
	case catalog.StatusSynced:
		fc, err := m.folderForMove(file.FolderLabel)
		if err != nil {
			return "", err
		}
		return filepath.Join(fc.SyncthingPath, file.RelativePath), nil
	case catalog.StatusArchived:
		return m.restorer.ColdPath(file.FolderLabel, file.RelativePath), nil
	default:
		return "", fmt.Errorf("%w: file %d has invalid trash source %q",
			ErrFileStateChanged, file.ID, status)
	}
}

// TrashByID moves a synced or archived file into the recoverable trash tree.
// Pair groups move as a unit, matching archive/restore and keeping Live Photo
// companions together.
func (m *Mover) TrashByID(id int64) (*catalog.FileRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, err := m.cat.GetFileByID(id); err != nil {
		return nil, err
	}
	expanded, err := m.cat.ExpandPairGroups([]int64{id})
	if err != nil {
		return nil, err
	}
	files, err := m.cat.FilesByIDs(expanded)
	if err != nil {
		return nil, err
	}
	if len(files) != len(expanded) {
		return nil, fmt.Errorf("%w: pair group contains a file that no longer exists", ErrFileStateChanged)
	}

	for i := range files {
		file := &files[i]
		switch file.Status {
		case catalog.StatusTrashed:
			continue
		case catalog.StatusSynced:
			if err := m.coldStorageReady(); err != nil {
				return nil, err
			}
			src, err := m.originalPath(file, file.Status)
			if err != nil {
				return nil, err
			}
			if err := m.sourceUnchanged(src, file); err != nil {
				return nil, err
			}
		case catalog.StatusArchived:
			src, err := m.originalPath(file, file.Status)
			if err != nil {
				return nil, err
			}
			if _, err := os.Lstat(src); err != nil {
				return nil, fmt.Errorf("%w: cannot stat %s: %v", ErrFileStateChanged, src, err)
			}
		default:
			return nil, fmt.Errorf("%w: paired file %d is %s", ErrFileStateChanged, file.ID, file.Status)
		}
	}

	completed := make([]catalog.FileRecord, 0, len(files))
	for i := range files {
		if files[i].Status == catalog.StatusTrashed {
			continue
		}
		if _, err := m.trashLocked(files[i].ID); err != nil {
			original := fmt.Errorf("trashing pair member %d: %w", files[i].ID, err)
			rollbackErrs := make([]error, 0)
			for j := len(completed) - 1; j >= 0; j-- {
				if _, rollbackErr := m.restoreTrashLocked(completed[j].ID); rollbackErr != nil {
					rollbackErrs = append(rollbackErrs,
						fmt.Errorf("restoring trashed pair member %d: %w", completed[j].ID, rollbackErr))
				}
			}
			if len(rollbackErrs) == 0 {
				return nil, original
			}
			return nil, errors.Join(append([]error{original}, rollbackErrs...)...)
		}
		completed = append(completed, files[i])
	}
	return m.cat.GetFileByID(id)
}

func (m *Mover) trashLocked(id int64) (*catalog.FileRecord, error) {
	rec, err := m.cat.GetFileByID(id)
	if err != nil {
		return nil, err
	}
	if rec.Status == catalog.StatusTrashed {
		return rec, nil
	}
	from := rec.Status
	if from != catalog.StatusSynced && from != catalog.StatusArchived {
		return nil, fmt.Errorf("%w: file %d is %s", ErrFileStateChanged, id, rec.Status)
	}
	src, err := m.originalPath(rec, from)
	if err != nil {
		return nil, err
	}
	dst := m.trashPath(rec)

	claimed, err := m.cat.BeginTrash(id, from)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, fmt.Errorf("%w: file %d could not be claimed for trash", ErrFileStateChanged, id)
	}

	if err := mkdirAllSync(filepath.Dir(dst), 0o755); err != nil {
		_, _ = m.cat.AbortTrash(id, from)
		return nil, fmt.Errorf("creating trash directory: %w", err)
	}
	if err := moveFile(src, dst, m.logger); err != nil {
		_, _ = m.cat.AbortTrash(id, from)
		return nil, fmt.Errorf("moving %s -> %s: %w", src, dst, err)
	}
	if from == catalog.StatusArchived {
		m.restorer.cleanEmptyParents(filepath.Dir(src), m.restorer.ArchiveRoot(rec.FolderLabel))
	}
	marked, err := m.cat.MarkTrashed(id, time.Now())
	if err != nil {
		return nil, fmt.Errorf("recording trash move: %w", err)
	}
	if !marked {
		m.cat.LogEvent("trash_unconfirmed", rec.FolderLabel, rec.RelativePath,
			"file moved into trash but the catalog row was not in 'trashing'")
		return nil, fmt.Errorf("trash of %d completed but could not be recorded", id)
	}
	m.cat.LogEvent("trash", rec.FolderLabel, rec.RelativePath,
		fmt.Sprintf("moved %d bytes to recoverable trash from %s", rec.SizeBytes, from))
	return m.cat.GetFileByID(id)
}

// RestoreTrashByID returns trashed files to the exact tree they came from.
// Unlike an archive restore it does not auto-pin a synced file.
func (m *Mover) RestoreTrashByID(id int64) (*catalog.FileRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, err := m.cat.GetFileByID(id); err != nil {
		return nil, err
	}
	expanded, err := m.cat.ExpandPairGroups([]int64{id})
	if err != nil {
		return nil, err
	}
	files, err := m.cat.FilesByIDs(expanded)
	if err != nil {
		return nil, err
	}
	if len(files) != len(expanded) {
		return nil, fmt.Errorf("%w: pair group contains a file that no longer exists", ErrFileStateChanged)
	}

	for i := range files {
		file := &files[i]
		switch file.Status {
		case catalog.StatusTrashed:
			if file.TrashedFrom != catalog.StatusSynced && file.TrashedFrom != catalog.StatusArchived {
				return nil, fmt.Errorf("%w: file %d has no valid trash source", ErrFileStateChanged, file.ID)
			}
			if _, err := os.Lstat(m.trashPath(file)); err != nil {
				return nil, fmt.Errorf("%w: cannot stat trash copy: %v", ErrFileStateChanged, err)
			}
			if _, err := m.originalPath(file, file.TrashedFrom); err != nil {
				return nil, err
			}
		case catalog.StatusSynced, catalog.StatusArchived:
			// A previous pair item already restored the complete group.
			continue
		default:
			return nil, fmt.Errorf("%w: paired file %d is %s", ErrFileStateChanged, file.ID, file.Status)
		}
	}

	completed := make([]catalog.FileRecord, 0, len(files))
	for i := range files {
		if files[i].Status != catalog.StatusTrashed {
			continue
		}
		if _, err := m.restoreTrashLocked(files[i].ID); err != nil {
			original := fmt.Errorf("restoring trashed pair member %d: %w", files[i].ID, err)
			rollbackErrs := make([]error, 0)
			for j := len(completed) - 1; j >= 0; j-- {
				if _, rollbackErr := m.trashLocked(completed[j].ID); rollbackErr != nil {
					rollbackErrs = append(rollbackErrs,
						fmt.Errorf("moving restored pair member %d back to trash: %w", completed[j].ID, rollbackErr))
				}
			}
			if len(rollbackErrs) == 0 {
				return nil, original
			}
			return nil, errors.Join(append([]error{original}, rollbackErrs...)...)
		}
		completed = append(completed, files[i])
	}
	return m.cat.GetFileByID(id)
}

func (m *Mover) restoreTrashLocked(id int64) (*catalog.FileRecord, error) {
	rec, err := m.cat.GetFileByID(id)
	if err != nil {
		return nil, err
	}
	if rec.Status == catalog.StatusSynced || rec.Status == catalog.StatusArchived {
		return rec, nil
	}
	if rec.Status != catalog.StatusTrashed {
		return nil, fmt.Errorf("%w: file %d is %s", ErrFileStateChanged, id, rec.Status)
	}
	src := m.trashPath(rec)
	dst, err := m.originalPath(rec, rec.TrashedFrom)
	if err != nil {
		return nil, err
	}
	claimed, err := m.cat.BeginUntrash(id)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, fmt.Errorf("%w: file %d could not be claimed for trash restore", ErrFileStateChanged, id)
	}
	if err := mkdirAllSync(filepath.Dir(dst), 0o755); err != nil {
		_, _ = m.cat.AbortUntrash(id)
		return nil, fmt.Errorf("creating restore directory: %w", err)
	}
	if err := moveFile(src, dst, m.logger); err != nil {
		_, _ = m.cat.AbortUntrash(id)
		return nil, fmt.Errorf("moving %s -> %s: %w", src, dst, err)
	}
	m.restorer.cleanEmptyParents(filepath.Dir(src), m.trashRoot(rec.FolderLabel))
	marked, err := m.cat.MarkUntrashed(id)
	if err != nil {
		return nil, fmt.Errorf("recording trash restore: %w", err)
	}
	if !marked {
		m.cat.LogEvent("untrash_unconfirmed", rec.FolderLabel, rec.RelativePath,
			"file left trash but the catalog row was not in 'untrashing'")
		return nil, fmt.Errorf("trash restore of %d completed but could not be recorded", id)
	}
	m.cat.LogEvent("untrash", rec.FolderLabel, rec.RelativePath,
		fmt.Sprintf("restored from trash to %s", rec.TrashedFrom))
	return m.cat.GetFileByID(id)
}

// DeleteByID permanently removes a settled trash entry, its thumbnail, tag
// links and catalog row. A missing trash file is accepted so a crash after the
// unlink but before the database commit can be retried safely.
func (m *Mover) DeleteByID(id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, err := m.cat.GetFileByID(id)
	if err != nil {
		return err
	}
	if rec.Status != catalog.StatusTrashed {
		return fmt.Errorf("%w: file %d is %s, not trashed", ErrFileStateChanged, id, rec.Status)
	}
	trashPath := m.trashPath(rec)
	if err := os.Remove(trashPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("permanently deleting %s: %w", trashPath, err)
	}

	if rec.ThumbnailPath != "" {
		thumbRoot := filepath.Join(m.cfg.Storage.ColdStoragePath, "thumbnails")
		thumbPath, pathErr := safeTreePath(thumbRoot, rec.ThumbnailPath)
		if pathErr != nil {
			return fmt.Errorf("resolving thumbnail for permanent deletion: %w", pathErr)
		} else if removeErr := os.Remove(thumbPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("removing thumbnail during permanent deletion: %w", removeErr)
		}
	}

	deleted, err := m.cat.DeleteTrashedFile(id)
	if err != nil {
		return err
	}
	if !deleted {
		return fmt.Errorf("%w: file %d left trash before permanent deletion committed", ErrFileStateChanged, id)
	}
	m.restorer.cleanEmptyParents(filepath.Dir(trashPath), m.trashRoot(rec.FolderLabel))
	m.cat.LogEvent("delete", rec.FolderLabel, rec.RelativePath,
		fmt.Sprintf("permanently deleted %d bytes from trash", rec.SizeBytes))
	return nil
}
