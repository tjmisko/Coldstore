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
	"sync"
	"syscall"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

// ErrScanInProgress is returned when a scan is asked for while one is already
// running. Two overlapping walks of the same tree do no useful work and their
// generation stamps interleave, so the second caller is told to come back
// rather than being queued behind the first.
var ErrScanInProgress = errors.New("a scan is already in progress")

type Scanner struct {
	cfg    *config.Config
	cat    *catalog.Catalog
	logger *slog.Logger

	// mu admits one scan at a time across every caller: the daemon's catalog
	// tick, the initial scan and the job worker's scan job all take it with
	// TryLock (design section 5.9). The API never scans inline any more, so
	// this is the only guard that has to hold.
	mu sync.Mutex
}

func NewScanner(cfg *config.Config, cat *catalog.Catalog, logger *slog.Logger) *Scanner {
	return &Scanner{cfg: cfg, cat: cat, logger: logger}
}

// ScanAll walks every folder the daemon currently manages and populates the
// catalog.
func (s *Scanner) ScanAll() error {
	return s.ScanAllContext(context.Background())
}

// ScanAllContext is ScanAll with cancellation. A folder whose scan fails is
// logged and skipped; a cancelled context stops the whole pass and is returned
// to the caller.
//
// It does NOT recover files left in a transient state: that is
// RecoverStartupState, which main runs once before anything else exists. A
// scan runs on a ticker alongside in-flight moves, and recovery must never do
// that (design section 5.6).
//
// The folder list comes from folder_configs, not from cfg.Folders (D1): a
// folder the user has disabled in the UI, or one that has disappeared from
// coldstore.toml, must not be walked at all. Walking a folder whose declared
// path is stale is how a whole catalog gets marked missing.
func (s *Scanner) ScanAllContext(ctx context.Context) error {
	if !s.mu.TryLock() {
		return ErrScanInProgress
	}
	defer s.mu.Unlock()
	return s.scanAll(ctx)
}

func (s *Scanner) scanAll(ctx context.Context) error {
	folders, err := s.cat.ActiveFolderConfigs()
	if err != nil {
		return fmt.Errorf("reading folder policy: %w", err)
	}

	for _, folder := range folders {
		if err := ctx.Err(); err != nil {
			s.logger.Info("scan interrupted", "reason", err)
			return err
		}
		if err := s.scanFolder(ctx, folder); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			s.logger.Error("scan failed for folder", "folder", folder.FolderLabel, "error", err)
			continue
		}
	}
	return nil
}

// ScanFolder walks a single syncthing folder, upserting files and marking missing ones.
func (s *Scanner) ScanFolder(folder catalog.FolderConfigRecord) error {
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
func (s *Scanner) ScanFolderContext(ctx context.Context, folder catalog.FolderConfigRecord) error {
	if !s.mu.TryLock() {
		return ErrScanInProgress
	}
	defer s.mu.Unlock()
	return s.scanFolder(ctx, folder)
}

// scanFolder is ScanFolderContext with the scan lock already held. ScanAll
// holds it for the whole pass, so the per-folder entry point cannot take it
// again -- sync.Mutex is not reentrant, and a TryLock here would turn every
// folder after the first into ErrScanInProgress.
func (s *Scanner) scanFolder(ctx context.Context, folder catalog.FolderConfigRecord) error {
	if err := verifyFolderRoot(folder.SyncthingPath); err != nil {
		return fmt.Errorf("folder root unavailable, skipping scan of %q at %s: %w",
			folder.FolderLabel, folder.SyncthingPath, err)
	}

	s.logger.Info("scanning folder", "label", folder.FolderLabel, "path", folder.SyncthingPath)

	// Nanosecond resolution, not seconds: two scans inside the same wall-clock
	// second must not share a generation, or the second one sweeps nothing.
	generation := time.Now().UnixNano()

	scanned := 0
	incomplete := 0
	conflicts := 0

	err := filepath.WalkDir(folder.SyncthingPath, func(path string, d os.DirEntry, err error) error {
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
			if strings.HasPrefix(d.Name(), ".") && path != folder.SyncthingPath {
				return filepath.SkipDir
			}
			return nil
		}

		// On the NAME, at every depth. The old test was on the relative path
		// with a ".st" prefix, so it only ever fired at the folder root and it
		// never matched Syncthing's actual temp file, ".syncthing.NAME.tmp".
		if skipScanEntry(d.Name()) {
			return nil
		}

		relPath, err := filepath.Rel(folder.SyncthingPath, path)
		if err != nil {
			incomplete++
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
			FolderLabel:  folder.FolderLabel,
			RelativePath: relPath,
			Filename:     info.Name(),
			Extension:    ext,
			MimeType:     mimeType,
			SizeBytes:    info.Size(),
			FileMtime:    info.ModTime(),
			FileAtime:    atime,
			Status:       catalog.StatusSynced,
		}

		conflict, err := s.cat.UpsertFile(record, generation)
		if err != nil {
			// The row keeps its old generation, so the missing sweep would
			// wrongly claim it. Suppress the sweep instead.
			s.logger.Warn("upsert failed", "path", relPath, "error", err)
			incomplete++
			return nil
		}
		if conflict {
			conflicts++
			s.reportConflict(folder.FolderLabel, relPath, info.Size())
		}

		scanned++
		return nil
	})

	if err != nil {
		return fmt.Errorf("walking %s: %w", folder.SyncthingPath, err)
	}

	if incomplete > 0 {
		s.logger.Warn("folder scan incomplete, not marking any file missing",
			"label", folder.FolderLabel, "files_scanned", scanned, "errors", incomplete)
		return nil
	}

	missing, err := s.cat.MarkFilesMissingBeforeGeneration(folder.FolderLabel, generation)
	if err != nil {
		return fmt.Errorf("marking missing files in %s: %w", folder.FolderLabel, err)
	}

	s.logger.Info("folder scan complete",
		"label", folder.FolderLabel, "files_scanned", scanned,
		"marked_missing", missing, "conflicts", conflicts)
	return nil
}

// scanSkipSuffixes and scanSkipNames are design section 5.9's skip table.
//
// Two of these are coldstore's own scratch names. Without them the temp
// sibling a killed cross-device restore leaves behind, and the set-aside copy
// a source-changed move writes, are both catalogued as ordinary photos:
// counted against the budget, offered for download, and eventually archived
// into cold storage as files called "IMG_0031.jpg.coldstore-tmp".
var (
	scanSkipSuffixes = []string{
		".tmp",           // conservative; a .tmp in a photo folder is not a photo
		tmpSuffix,        // *.coldstore-tmp
		supersededSuffix, // *.coldstore-superseded
	}
	scanSkipNames = []string{".stfolder", ".stignore", ".stversions"}
)

// skipScanEntry reports whether a file NAME must never be catalogued.
func skipScanEntry(name string) bool {
	for _, suffix := range scanSkipSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	for _, exact := range scanSkipNames {
		if name == exact {
			return true
		}
	}
	// Syncthing's own naming, both variants, and anything else it hides at the
	// root: ".stignore.d", ".stversions" copies, and so on.
	if strings.HasPrefix(name, ".syncthing.") || strings.HasPrefix(name, "~syncthing~") {
		return true
	}
	return strings.HasPrefix(name, ".st")
}

// reportConflict makes a section 4.1b collision loud: an archived row whose
// path has reappeared in the folder. One event per path per scan, naming both
// sizes, because the owner cannot resolve what they cannot see.
func (s *Scanner) reportConflict(label, relPath string, onDisk int64) {
	details := fmt.Sprintf("a file has reappeared at the path of an archived record: "+
		"%d bytes on the device", onDisk)
	if rec, err := s.cat.GetFile(label, relPath); err == nil {
		details = fmt.Sprintf("a file has reappeared at the path of an archived record: "+
			"%d bytes on the device against %d bytes in cold storage",
			onDisk, rec.SizeBytes)
	}
	s.cat.LogEvent("conflict", label, relPath, details)
	s.logger.Warn("archived file's path is occupied again",
		"folder", label, "path", relPath, "size", onDisk)
}

// verifyFolderRoot reports whether a syncthing folder root is present and
// readable. A missing or unmounted root is an error worth logging, never a
// signal that every file in the folder was deleted.
//
// FolderReady comes first because a bare os.Stat cannot tell an empty folder
// from an unmounted mountpoint, and the two demand opposite responses: an
// empty folder really does mean every file is gone, while an unmounted one
// means nothing is known. Reading the second as the first marks the whole
// folder missing (design section 3.6). The readability check below still
// runs, since a mounted-but-unreadable root is a different failure again.
func verifyFolderRoot(path string) error {
	if path == "" {
		return errors.New("folder path is empty")
	}

	if err := FolderReady(path); err != nil {
		return err
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
