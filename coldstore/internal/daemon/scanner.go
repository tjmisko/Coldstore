package daemon

import (
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
// It first recovers any files left in the transient "archiving" state from a
// previous crash, then scans each folder.
func (s *Scanner) ScanAll() error {
	s.recoverArchiving()
	for _, folder := range s.cfg.Folders {
		if err := s.ScanFolder(folder); err != nil {
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
	s.logger.Info("scanning folder", "label", folder.Label, "path", folder.Path)

	persistentPatterns, err := s.cat.ListPersistentPaths(folder.Label)
	if err != nil {
		return err
	}

	// Merge config persistent patterns with DB patterns
	allPatterns := make([]string, 0, len(folder.Persistent)+len(persistentPatterns))
	allPatterns = append(allPatterns, folder.Persistent...)
	for _, p := range persistentPatterns {
		allPatterns = append(allPatterns, p.Pattern)
	}

	// Mark all synced files as missing; re-found files get set back to synced
	if err := s.cat.MarkMissingSyncedFiles(folder.Label); err != nil {
		return err
	}

	scanned := 0
	err = filepath.WalkDir(folder.Path, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			s.logger.Warn("walk error", "path", path, "error", err)
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
			return nil
		}

		// Skip syncthing internal files
		if strings.HasPrefix(relPath, ".st") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
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

		if err := s.cat.UpsertFile(record); err != nil {
			s.logger.Warn("upsert failed", "path", relPath, "error", err)
		}

		scanned++
		return nil
	})

	s.logger.Info("folder scan complete", "label", folder.Label, "files_scanned", scanned)
	return err
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
