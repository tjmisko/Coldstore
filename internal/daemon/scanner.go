package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

type Scanner struct {
	cfg    *config.Config
	cat    *catalog.Catalog
	logger *slog.Logger
}

func NewScanner(cfg *config.Config, cat *catalog.Catalog, logger *slog.Logger) *Scanner {
	return &Scanner{cfg: cfg, cat: cat, logger: logger}
}

// ScanAll walks all configured syncthing folders and populates the catalog.
func (s *Scanner) ScanAll() error {
	return s.ScanAllContext(context.Background())
}

// ScanAllContext is ScanAll with cancellation. It first recovers any files left
// in the transient "archiving" state from a previous crash, then scans each
// folder. A folder whose scan fails is logged and skipped; a cancelled context
// stops the whole pass and is returned to the caller.
func (s *Scanner) ScanAllContext(ctx context.Context) error {
	s.recoverArchiving()
	for _, folder := range s.cfg.Folders {
		if err := ctx.Err(); err != nil {
			s.logger.Info("scan interrupted", "reason", err)
			return err
		}
		if err := s.ScanFolderContext(ctx, folder); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			s.logger.Error("scan failed for folder", "folder", folder.Label, "error", err)
			continue
		}
	}
	return nil
}

// recoverArchiving resolves files stuck in "archiving" state after a crash.
// If the file exists in the archive directory, it's marked "archived".
// Otherwise, it's reverted to "synced" (the source was never moved).
func (s *Scanner) recoverArchiving() {
	files, err := s.cat.ArchivingFiles()
	if err != nil {
		s.logger.Error("querying archiving files for recovery", "error", err)
		return
	}
	for _, f := range files {
		archivePath := filepath.Join(s.cfg.Storage.ColdStoragePath, "archive", f.FolderLabel, f.RelativePath)
		if _, err := os.Stat(archivePath); err == nil {
			s.cat.UpdateFileStatus(f.ID, catalog.StatusArchived)
			s.logger.Info("recovered archiving file as archived", "path", f.RelativePath)
		} else {
			s.cat.UpdateFileStatus(f.ID, catalog.StatusSynced)
			s.logger.Info("recovered archiving file as synced", "path", f.RelativePath)
		}
	}
}

// ScanFolder walks a single syncthing folder, upserting files and marking missing ones.
func (s *Scanner) ScanFolder(folder config.FolderConfig) error {
	return s.ScanFolderContext(context.Background(), folder)
}

// ScanFolderContext is ScanFolder with cancellation.
//
// It refuses to touch the catalog at all unless the folder root is present and
// readable: an unmounted or renamed root would otherwise leave every file in
// the folder reading "missing", because WalkDir reports the error at the root
// and the walk finds nothing to put back.
//
// Files are stamped with a scan generation as they are found, and only a walk
// that completed cleanly is allowed to mark the untouched rows missing. A walk
// that hit ANY error mid-tree makes no missing claim: a permission error on one
// subdirectory must not archive-and-forget the files under it.
func (s *Scanner) ScanFolderContext(ctx context.Context, folder config.FolderConfig) error {
	if err := verifyFolderRoot(folder.Path); err != nil {
		return fmt.Errorf("folder root unavailable, skipping scan of %q at %s: %w",
			folder.Label, folder.Path, err)
	}

	s.logger.Info("scanning folder", "label", folder.Label, "path", folder.Path)

	// Nanosecond resolution, not seconds: two scans inside the same wall-clock
	// second must not share a generation, or the second one sweeps nothing.
	generation := time.Now().UnixNano()

	scanned := 0
	incomplete := 0

	err := filepath.WalkDir(folder.Path, func(path string, d os.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			s.logger.Warn("walk error", "path", path, "error", err)
			incomplete++
			return nil
		}
		if d.IsDir() {
			// Skip hidden directories (like .stfolder, .stversions)
			if strings.HasPrefix(d.Name(), ".") && path != folder.Path {
				return filepath.SkipDir
			}
			return nil
		}

		relPath, err := filepath.Rel(folder.Path, path)
		if err != nil {
			incomplete++
			return nil
		}

		// Skip syncthing internal files
		if strings.HasPrefix(relPath, ".st") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			s.logger.Warn("cannot stat entry", "path", path, "error", err)
			incomplete++
			return nil
		}

		ext := strings.ToLower(filepath.Ext(info.Name()))
		mimeType := mime.TypeByExtension(ext)

		// Read atime from the underlying syscall stat.
		// Accuracy depends on mount options (relatime, noatime, etc.).
		var atime time.Time
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			atime = time.Unix(stat.Atim.Sec, stat.Atim.Nsec)
		}

		record := &catalog.FileRecord{
			FolderLabel:  folder.Label,
			RelativePath: relPath,
			Filename:     info.Name(),
			Extension:    ext,
			MimeType:     mimeType,
			SizeBytes:    info.Size(),
			FileMtime:    info.ModTime(),
			FileAtime:    atime,
			Status:       catalog.StatusSynced,
		}

		if err := s.cat.UpsertFileSeen(record, generation); err != nil {
			// The row keeps its old generation, so the missing sweep would
			// wrongly claim it. Suppress the sweep instead.
			s.logger.Warn("upsert failed", "path", relPath, "error", err)
			incomplete++
			return nil
		}

		scanned++
		return nil
	})

	if err != nil {
		return fmt.Errorf("walking %s: %w", folder.Path, err)
	}

	if incomplete > 0 {
		s.logger.Warn("folder scan incomplete, not marking any file missing",
			"label", folder.Label, "files_scanned", scanned, "errors", incomplete)
		return nil
	}

	missing, err := s.cat.MarkFilesMissingBeforeGeneration(folder.Label, generation)
	if err != nil {
		return fmt.Errorf("marking missing files in %s: %w", folder.Label, err)
	}

	s.logger.Info("folder scan complete",
		"label", folder.Label, "files_scanned", scanned, "marked_missing", missing)
	return nil
}

// verifyFolderRoot reports whether a syncthing folder root is present and
// readable. A missing or unmounted root is an error worth logging, never a
// signal that every file in the folder was deleted.
func verifyFolderRoot(path string) error {
	if path == "" {
		return errors.New("folder path is empty")
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stating folder root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("folder root %s is not a directory", path)
	}

	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening folder root: %w", err)
	}
	defer dir.Close()

	if _, err := dir.ReadDir(1); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading folder root: %w", err)
	}
	return nil
}

// IsPersistent checks if a relative path matches any persistent glob pattern.
// Uses filepath.Match semantics: "*" matches a single path segment, not "/".
func IsPersistent(relPath string, patterns []string) bool {
	for _, pattern := range patterns {
		if matched, err := filepath.Match(pattern, relPath); err == nil && matched {
			return true
		}
	}
	return false
}
