package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

// RecoverStartupState resolves every file row left in a transient state by a
// previous run: "archiving" (a move out that never committed) and "restoring"
// (a move in that never committed).
//
// It is a STARTUP-ONLY phase and it must stay one. It mutates the filesystem
// and writes file rows without holding any mover lock, so running it from a
// ticker -- which is where recoverArchiving used to live -- races an in-flight
// move and can delete the only copy of a file: recovery sees a copy that is
// "too short", deletes it and writes status='synced', while the move that is
// still running finishes into the unlinked inode and then removes the source.
// Calling it from main before the mover, the worker, the API server or the
// daemon exist is the whole reason it is allowed to touch the filesystem at
// all (invariant 8).
//
// A caller must treat an error as fatal: a daemon that could not resolve its
// transient rows enforces quotas against a catalog whose truth value is
// unknown. Rows whose folder cannot be located are logged and skipped rather
// than failing the whole pass -- a folder that has gone away is not a reason to
// refuse to start.
func RecoverStartupState(cfg *config.Config, cat *catalog.Catalog, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	r := &recovery{cfg: cfg, cat: cat, logger: logger}
	if err := r.recoverArchiving(); err != nil {
		return fmt.Errorf("recovering interrupted archives: %w", err)
	}
	if err := r.recoverRestoring(); err != nil {
		return fmt.Errorf("recovering interrupted restores: %w", err)
	}
	return nil
}

type recovery struct {
	cfg    *config.Config
	cat    *catalog.Catalog
	logger *slog.Logger
}

// folderPath resolves a folder label to its on-disk root, preferring the
// catalog (which knows about folders that have since disappeared from the
// TOML) and falling back to the TOML for a database that has not been
// reconciled yet.
func (r *recovery) folderPath(label string) (string, error) {
	if path, err := r.cat.FolderPath(label); err == nil && path != "" {
		return path, nil
	}
	for _, f := range r.cfg.Folders {
		if f.Label == label {
			return f.Path, nil
		}
	}
	return "", fmt.Errorf("no folder config for label %q", label)
}

func (r *recovery) archivePath(f *catalog.FileRecord) string {
	return filepath.Join(r.cfg.Storage.ColdStoragePath, "archive", f.FolderLabel, f.RelativePath)
}

// removeStaleTemp deletes the temp sibling a killed copy may have left next to
// a destination. It is never referenced by a row and is always an incomplete
// copy of something that still exists elsewhere.
func (r *recovery) removeStaleTemp(dst string) {
	if err := os.Remove(dst + tmpSuffix); err != nil && !os.IsNotExist(err) {
		r.logger.Warn("cannot remove stale temp file", "path", dst+tmpSuffix, "error", err)
	}
}

// setAside renames a complete copy out of the way instead of deleting it.
func (r *recovery) setAside(path string) string {
	aside := asidePath(path)
	if err := os.Rename(path, aside); err != nil {
		r.logger.Error("cannot set aside a superseded copy", "path", path, "error", err)
	}
	return aside
}

// recoverArchiving resolves rows stuck in "archiving". dst is the archive path
// and src is the syncthing path; see the decision table in design section 5.6.
//
// The destination is only ever renamed into place complete (section 5.2b), so
// "dst exists" means "a whole copy is in cold storage" for anything this binary
// wrote. A short dst can still be found in a database written by an older
// binary, and is handled explicitly.
func (r *recovery) recoverArchiving() error {
	files, err := r.cat.ArchivingFiles()
	if err != nil {
		return err
	}

	for i := range files {
		f := &files[i]

		syncthingPath, err := r.folderPath(f.FolderLabel)
		if err != nil {
			r.logger.Warn("cannot recover interrupted archive, folder is unknown",
				"folder", f.FolderLabel, "path", f.RelativePath, "error", err)
			continue
		}

		// Every branch below reads "the source is not there" as evidence. On an
		// unmounted root nothing is there, so that evidence is worthless: the
		// complete-copy branch would promote a file to archived while it is
		// still sitting on the device, and the scanner would then report it as
		// a conflict the owner has to resolve by hand. Leave the row in
		// archiving instead; the next recovery sees a mounted folder.
		if err := FolderReady(syncthingPath); err != nil {
			r.logger.Warn("cannot recover interrupted archive, folder is unavailable",
				"folder", f.FolderLabel, "path", f.RelativePath, "error", err)
			continue
		}

		dst := r.archivePath(f)
		src := filepath.Join(syncthingPath, f.RelativePath)

		dstInfo, dstErr := os.Lstat(dst)
		srcInfo, srcErr := os.Lstat(src)

		switch {
		case dstErr != nil && srcErr == nil:
			// Nothing happened. The copy never reached the commit rename.
			r.removeStaleTemp(dst)
			r.claim(f, catalog.StatusArchiving, catalog.StatusSynced, "no archive copy, source intact")

		case dstErr != nil && srcErr != nil:
			// The source can only be gone here because something else deleted
			// it: the unlink runs strictly after the commit rename.
			r.removeStaleTemp(dst)
			r.claim(f, catalog.StatusArchiving, catalog.StatusMissing, "neither copy exists")
			r.cat.LogEvent("source_vanished", f.FolderLabel, f.RelativePath,
				"the file disappeared while an archive was in flight")

		case dstInfo.Size() == f.SizeBytes && srcErr != nil:
			// The normal kill-between-unlink-and-commit case. Deliberately
			// promoted to archived rather than treated as a user deletion:
			// guessing wrong the other way destroys the last copy of a file.
			r.markArchived(f, "archive copy complete, source already removed")

		case dstInfo.Size() == f.SizeBytes && r.sourceUnchanged(f, srcInfo):
			// Same bytes in both places; only the unlink never ran.
			if err := os.Remove(src); err != nil {
				r.logger.Error("cannot remove the source of a completed archive",
					"path", src, "error", err)
				continue
			}
			r.markArchived(f, "archive copy complete, removed the leftover source")

		case dstInfo.Size() == f.SizeBytes:
			// The device copy is a different, newer version. Keep both.
			aside := r.setAside(dst)
			r.claim(f, catalog.StatusArchiving, catalog.StatusSynced, "device copy is newer")
			r.cat.LogEvent("conflict", f.FolderLabel, f.RelativePath,
				fmt.Sprintf("the device copy changed during an archive; the archived copy was kept as %s",
					filepath.Base(aside)))

		case srcErr == nil:
			// A short archive copy with the source present. If it is a prefix
			// of the source it is an interrupted copy and holds no bytes the
			// source does not; anything else is a different file and is kept.
			partial, cmpErr := isPrefixOf(dst, src)
			if cmpErr != nil {
				r.logger.Warn("cannot compare a short archive copy against its source",
					"path", f.RelativePath, "error", cmpErr)
			}
			if partial && cmpErr == nil {
				os.Remove(dst)
				r.claim(f, catalog.StatusArchiving, catalog.StatusSynced, "removed a partial archive copy")
				break
			}
			aside := r.setAside(dst)
			r.claim(f, catalog.StatusArchiving, catalog.StatusSynced, "archive copy does not match the source")
			r.cat.LogEvent("conflict", f.FolderLabel, f.RelativePath,
				fmt.Sprintf("archive copy is %d bytes against a catalogued %d; kept as %s",
					dstInfo.Size(), f.SizeBytes, filepath.Base(aside)))

		default:
			// Short archive copy and no source at all. Invariant 9: keep the
			// bytes and keep them referenced, and say so loudly.
			r.markArchived(f, "archive copy is the only copy, but its size is unexpected")
			r.cat.LogEvent("archive_unconfirmed", f.FolderLabel, f.RelativePath,
				fmt.Sprintf("archive copy is %d bytes against a catalogued %d, and no copy remains on the device",
					dstInfo.Size(), f.SizeBytes))
		}
	}
	return nil
}

// recoverRestoring resolves rows stuck in "restoring". src is the cold-storage
// copy and dst is the syncthing path; see the decision table in design
// section 5.6.
//
// The rule is NOT "does the destination exist". Before section 5.2b a killed
// restore always left a partial file at the destination AND an intact copy in
// cold storage, so an existence test would commit the truncation, let Syncthing
// propagate it to every device, and pin the row so nothing ever looked at the
// good copy again. The cold-storage copy is authoritative while a restore is in
// flight.
func (r *recovery) recoverRestoring() error {
	files, err := r.cat.RestoringFiles()
	if err != nil {
		return err
	}

	for i := range files {
		f := &files[i]

		syncthingPath, err := r.folderPath(f.FolderLabel)
		if err != nil {
			r.logger.Warn("cannot recover interrupted restore, folder is unknown",
				"folder", f.FolderLabel, "path", f.RelativePath, "error", err)
			continue
		}

		// Same reasoning as recoverArchiving: an unmounted root makes the
		// destination look absent, and "nothing on the device" is one of the
		// branches below. Deciding on that is deciding on noise.
		if err := FolderReady(syncthingPath); err != nil {
			r.logger.Warn("cannot recover interrupted restore, folder is unavailable",
				"folder", f.FolderLabel, "path", f.RelativePath, "error", err)
			continue
		}

		src := r.archivePath(f)
		dst := filepath.Join(syncthingPath, f.RelativePath)

		_, srcErr := os.Lstat(src)
		dstInfo, dstErr := os.Lstat(dst)

		switch {
		case srcErr == nil && dstErr != nil:
			// The restore never committed. Simply retry it later.
			r.removeStaleTemp(dst)
			r.claim(f, catalog.StatusRestoring, catalog.StatusArchived, "cold copy intact, nothing on the device")

		case srcErr == nil && r.sourceUnchanged(f, dstInfo):
			// The commit rename landed but the cold copy was never removed.
			//
			// Size AND mtime, mirroring the archiving branch. Size alone is
			// not proof the restore committed: a same-length rewrite at the
			// destination -- an EXIF-orientation rewrite of a JPEG is exactly
			// byte-length-preserving -- would pass it, and this branch then
			// DELETES the cold copy, which is the only copy of the archived
			// bytes. A completed restore preserves mtime (crossDeviceMove
			// step 6 chtimes it), so anything else falls through to the
			// set-aside branch below and keeps both copies.
			if err := os.Remove(src); err != nil {
				r.logger.Error("cannot remove the cold copy of a completed restore",
					"path", src, "error", err)
				continue
			}
			r.markRestored(f, "restore had completed, removed the leftover cold copy")

		case srcErr == nil:
			// Both exist and the device copy is the wrong size. A prefix of
			// the cold copy is an interrupted restore and is safe to delete;
			// anything else is a different file and both copies are kept.
			partial, cmpErr := isPrefixOf(dst, src)
			if cmpErr != nil {
				r.logger.Warn("cannot compare a short restored file against the cold copy",
					"path", f.RelativePath, "error", cmpErr)
			}
			if partial && cmpErr == nil {
				os.Remove(dst)
				r.removeStaleTemp(dst)
				r.claim(f, catalog.StatusRestoring, catalog.StatusArchived,
					"removed a truncated restore, kept the cold copy")
				break
			}
			aside := r.setAside(src)
			r.claim(f, catalog.StatusRestoring, catalog.StatusSynced, "device copy does not match the cold copy")
			r.cat.LogEvent("conflict", f.FolderLabel, f.RelativePath,
				fmt.Sprintf("the device copy is %d bytes against a catalogued %d; the cold copy was kept as %s",
					dstInfo.Size(), f.SizeBytes, filepath.Base(aside)))

		case dstErr != nil:
			r.claim(f, catalog.StatusRestoring, catalog.StatusMissing, "neither copy exists")
			r.cat.LogEvent("source_vanished", f.FolderLabel, f.RelativePath,
				"the file disappeared while a restore was in flight")

		case dstInfo.Size() == f.SizeBytes:
			// The move completed; only the commit was lost.
			r.markRestored(f, "restore had completed, only the catalog write was lost")

		default:
			// On the device with an unexpected size and no cold copy left.
			// Adopt what is there, but do not trust the size.
			r.claim(f, catalog.StatusRestoring, catalog.StatusSynced, "device copy has an unexpected size")
			r.cat.LogEvent("conflict", f.FolderLabel, f.RelativePath,
				fmt.Sprintf("the restored file is %d bytes against a catalogued %d, and no cold copy remains",
					dstInfo.Size(), f.SizeBytes))
		}
	}
	return nil
}

// sourceUnchanged reports whether the file on the device still matches what the
// catalog recorded. Mtimes are compared at second resolution because that is
// what the catalog stores.
func (r *recovery) sourceUnchanged(f *catalog.FileRecord, info os.FileInfo) bool {
	return info.Size() == f.SizeBytes && info.ModTime().Unix() == f.FileMtime.Unix()
}

func (r *recovery) claim(f *catalog.FileRecord, from, to catalog.FileStatus, why string) {
	ok, err := r.cat.ClaimFile(f.ID, from, to)
	if err != nil {
		r.logger.Error("recovery could not write a file status",
			"id", f.ID, "path", f.RelativePath, "to", to, "error", err)
		return
	}
	if !ok {
		r.logger.Error("recovery lost a status transition it should have owned",
			"id", f.ID, "path", f.RelativePath, "from", from, "to", to)
		return
	}
	r.logger.Info("recovered interrupted move",
		"path", f.RelativePath, "folder", f.FolderLabel, "status", to, "reason", why)
}

func (r *recovery) markArchived(f *catalog.FileRecord, why string) {
	ok, err := r.cat.MarkArchived(f.ID, time.Now())
	if err != nil {
		r.logger.Error("recovery could not mark a file archived",
			"id", f.ID, "path", f.RelativePath, "error", err)
		return
	}
	if !ok {
		r.logger.Error("recovery lost a transition to archived it should have owned",
			"id", f.ID, "path", f.RelativePath)
		return
	}
	r.logger.Info("recovered interrupted move",
		"path", f.RelativePath, "folder", f.FolderLabel, "status", catalog.StatusArchived, "reason", why)
}

// markRestored commits through MarkRestored so a recovered restore is pinned,
// exactly as a completed one would have been (D2). Without the pin the next
// quota tick takes straight back the file the owner asked to get back.
func (r *recovery) markRestored(f *catalog.FileRecord, why string) {
	ok, err := r.cat.MarkRestored(f.ID, time.Now().Unix())
	if err != nil {
		r.logger.Error("recovery could not mark a file restored",
			"id", f.ID, "path", f.RelativePath, "error", err)
		return
	}
	if !ok {
		r.logger.Error("recovery lost a transition to synced it should have owned",
			"id", f.ID, "path", f.RelativePath)
		return
	}
	r.logger.Info("recovered interrupted move",
		"path", f.RelativePath, "folder", f.FolderLabel, "status", catalog.StatusSynced, "reason", why)
}
