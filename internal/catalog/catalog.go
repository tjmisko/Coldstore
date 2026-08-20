package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ErrNoSuchFolder is returned by folder lookups when no folder_configs row
// carries the requested label.
var ErrNoSuchFolder = errors.New("no such folder")

// defaultOffloadCandidateLimit caps how many candidate rows one enforcement
// pass materialises. Candidates are consumed oldest-first and the loop stops
// at the budget, so this is always more than one pass needs; a pass that
// exhausts the list while still over budget is picked up by the next tick.
const defaultOffloadCandidateLimit = 5000

// maxThumbnailAttempts is how many times a file whose thumbnail generation
// failed is retried before it drops out of the queue for good.
const maxThumbnailAttempts = 3

// idChunkSize caps how many ids go into a single IN (...) clause.
const idChunkSize = 500

// fileColumns is the one select list for the files table. Every SELECT on
// files uses it and scanFile is its only consumer, so adding a column is two
// edits in one commit instead of eight coordinated ones.
const fileColumns = `id, folder_label, relative_path, filename, extension, mime_type,
	size_bytes, file_mtime, file_atime, first_seen, archived_at, status,
	thumbnail_path, content_hash, pinned, pinned_at, pin_source, last_seen,
	thumb_attempts, thumb_last_attempt, thumb_error`

type Catalog struct {
	db *sql.DB
}

func Open(dbPath string) (*Catalog, error) {
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating database directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// One connection for the whole process. The daemon tick, the job worker
	// and every HTTP handler share it, so SQLITE_BUSY and WAL snapshot upgrade
	// failures cannot occur in-process.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	// Advisory marker so install checks and humans can see what wrote this
	// catalog without opening SQLite. Never fatal.
	_ = writeSchemaMarker(filepath.Dir(dbPath), schemaVersion)

	return &Catalog{db: db}, nil
}

func (c *Catalog) Close() error {
	return c.db.Close()
}

// ---------------------------------------------------------------------------
// files
// ---------------------------------------------------------------------------

// UpsertFile inserts or updates a file record based on (folder_label,
// relative_path), stamping last_seen with the current time.
//
// Scanners must call UpsertFileSeen instead, so that every row they touch
// carries the generation of the walk that observed it.
func (c *Catalog) UpsertFile(f *FileRecord) error {
	return c.UpsertFileSeen(f, time.Now().Unix())
}

// UpsertFileSeen is UpsertFile with an explicit scan generation. scanGeneration
// is the epoch second at which the walk that observed this file started; the
// missing-detection sweep uses it to find rows the walk did not touch.
//
// The ON CONFLICT list deliberately does NOT mention pinned, pinned_at,
// pin_source or thumb_attempts: a rescan must never clear a pin or reset a
// thumbnail's failure count.
func (c *Catalog) UpsertFileSeen(f *FileRecord, scanGeneration int64) error {
	_, err := c.db.Exec(`
		INSERT INTO files (folder_label, relative_path, filename, extension, mime_type, size_bytes, file_mtime, file_atime, status, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(folder_label, relative_path) DO UPDATE SET
			filename = excluded.filename,
			size_bytes = excluded.size_bytes,
			file_mtime = excluded.file_mtime,
			file_atime = excluded.file_atime,
			mime_type = excluded.mime_type,
			last_seen = excluded.last_seen,
			status = CASE
				WHEN files.status IN ('missing', 'archived') THEN 'synced'
				ELSE files.status
			END
	`, f.FolderLabel, f.RelativePath, f.Filename, f.Extension, f.MimeType, f.SizeBytes, f.FileMtime, f.FileAtime, f.Status, scanGeneration)
	return err
}

// GetFile retrieves a file by folder and relative path.
func (c *Catalog) GetFile(folder, relPath string) (*FileRecord, error) {
	row := c.db.QueryRow(`SELECT `+fileColumns+`
		FROM files WHERE folder_label = ? AND relative_path = ?`, folder, relPath)
	return scanFile(row)
}

// GetFileByID retrieves a file by its ID.
func (c *Catalog) GetFileByID(id int64) (*FileRecord, error) {
	row := c.db.QueryRow(`SELECT `+fileColumns+` FROM files WHERE id = ?`, id)
	return scanFile(row)
}

type FileQuery struct {
	Folder    string
	Status    FileStatus
	Extension string
	Search    string
	Pinned    *bool
	PinSource string
	IDs       []int64
	SortBy    string
	Offset    int
	Limit     int
}

// filter builds the WHERE tail shared by ListFiles and ListFileIDs. When IDs
// is non-empty every other filter is ignored, so a caller resolving a known
// selection gets exactly those rows that still exist.
func (q FileQuery) filter() (string, []any) {
	var clause strings.Builder
	var args []any

	if len(q.IDs) > 0 {
		clause.WriteString(" AND id IN (" + placeholders(len(q.IDs)) + ")")
		for _, id := range q.IDs {
			args = append(args, id)
		}
		return clause.String(), args
	}

	if q.Folder != "" {
		clause.WriteString(" AND folder_label = ?")
		args = append(args, q.Folder)
	}
	if q.Status != "" {
		clause.WriteString(" AND status = ?")
		args = append(args, q.Status)
	}
	if q.Extension != "" {
		clause.WriteString(" AND extension = ?")
		args = append(args, q.Extension)
	}
	if q.Search != "" {
		clause.WriteString(" AND filename LIKE ?")
		args = append(args, "%"+q.Search+"%")
	}
	if q.Pinned != nil {
		clause.WriteString(" AND pinned = ?")
		args = append(args, boolToInt(*q.Pinned))
	}
	if q.PinSource != "" {
		clause.WriteString(" AND pin_source = ?")
		args = append(args, q.PinSource)
	}
	return clause.String(), args
}

// ListFiles queries files with filtering, sorting, and pagination.
func (c *Catalog) ListFiles(q FileQuery) ([]FileRecord, error) {
	where, args := q.filter()
	query := `SELECT ` + fileColumns + ` FROM files WHERE 1=1` + where

	switch {
	case len(q.IDs) > 0:
		query += " ORDER BY id ASC"
	case q.SortBy == "size":
		query += " ORDER BY size_bytes DESC"
	case q.SortBy == "name":
		query += " ORDER BY filename ASC"
	default:
		query += " ORDER BY file_mtime DESC"
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	query += " LIMIT ? OFFSET ?"
	args = append(args, limit, q.Offset)

	return c.queryFiles(query, args...)
}

// ListFileIDs backs "select all in folder". It applies the same filters as
// ListFiles, ignores sort/offset/limit, orders by id ASC, and returns at most
// capacity ids together with the true total. ids is always non-nil.
func (c *Catalog) ListFileIDs(q FileQuery, capacity int) (ids []int64, total int64, err error) {
	if capacity <= 0 {
		capacity = 20000
	}
	where, args := q.filter()

	if err := c.db.QueryRow(`SELECT COUNT(*) FROM files WHERE 1=1`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting files: %w", err)
	}

	rows, err := c.db.Query(`SELECT id FROM files WHERE 1=1`+where+` ORDER BY id LIMIT ?`,
		append(args, capacity)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	ids = []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	return ids, total, rows.Err()
}

// UpdateFileStatus sets a file's status and optionally archived_at.
//
// It is for the transient and rollback transitions only, and must never touch
// pinned: a pin flag written on a transient flip would be silently set or
// cleared by every rollback.
func (c *Catalog) UpdateFileStatus(id int64, status FileStatus) error {
	if status == StatusArchived {
		_, err := c.db.Exec(`UPDATE files SET status = ?, archived_at = ? WHERE id = ?`,
			status, time.Now().UTC(), id)
		return err
	}
	_, err := c.db.Exec(`UPDATE files SET status = ?, archived_at = NULL WHERE id = ?`, status, id)
	return err
}

// ClaimFile is the generic compare-and-swap status transition. A false result
// means another actor got there first; it is never an error, but no caller may
// discard it and proceed to move a file.
func (c *Catalog) ClaimFile(id int64, from, to FileStatus) (bool, error) {
	res, err := c.db.Exec(`UPDATE files SET status = ? WHERE id = ? AND status = ?`, to, id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ClaimUnpinnedFile is the CAS the AUTOMATIC offload path uses, and the only
// one it may use. A pin that commits before the claim makes the claim fail,
// which is the authoritative third layer of D2's pin protection.
func (c *Catalog) ClaimUnpinnedFile(id int64) (bool, error) {
	res, err := c.db.Exec(
		`UPDATE files SET status = 'archiving' WHERE id = ? AND status = 'synced' AND pinned = 0`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// MarkArchived commits a completed offload. It deliberately does NOT touch
// pinned: a pin that lands mid-move survives as a latent instruction that
// takes effect when the file is restored, instead of being silently discarded
// by the move the owner was trying to stop.
func (c *Catalog) MarkArchived(id int64, at time.Time) (bool, error) {
	res, err := c.db.Exec(
		`UPDATE files SET status = 'archived', archived_at = ? WHERE id = ? AND status = 'archiving'`,
		at.UTC(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// MarkRestored commits a completed restore: status, archived_at and the pin in
// ONE statement, so a crash can never leave a restored file unpinned and
// immediately re-archivable (D2).
//
// It sets pin_source='restore' only if the file was not already pinned by the
// user; a user pin is never downgraded to an auto-pin.
func (c *Catalog) MarkRestored(id int64, at int64) (bool, error) {
	res, err := c.db.Exec(`
		UPDATE files SET status = 'synced', archived_at = NULL, pinned = 1,
			pinned_at = COALESCE(pinned_at, ?),
			pin_source = CASE WHEN pin_source = 'user' THEN 'user' ELSE 'restore' END
		WHERE id = ? AND status = 'restoring'`, at, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// SetPinned writes a single file's pin.
//
// Callers: this is a Mover primitive. Every pin write must go through
// daemon.Mover so that it holds the mover mutex and cannot commit while a move
// of that file is in flight. It is exported only because the Mover lives in
// another package.
func (c *Catalog) SetPinned(id int64, pinned bool, source string, at int64) error {
	if !pinned {
		_, err := c.db.Exec(
			`UPDATE files SET pinned = 0, pinned_at = NULL, pin_source = '' WHERE id = ?`, id)
		return err
	}
	if err := validPinSource(source); err != nil {
		return err
	}
	_, err := c.db.Exec(
		`UPDATE files SET pinned = 1, pinned_at = ?, pin_source = ? WHERE id = ?`, at, source, id)
	return err
}

// SetPinnedBulk is SetPinned over many ids, chunked so the IN clause stays
// inside SQLite's parameter limit. It returns the number of rows written.
// See SetPinned on why this is exported.
func (c *Catalog) SetPinnedBulk(ids []int64, pinned bool, source string, at int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if pinned {
		if err := validPinSource(source); err != nil {
			return 0, err
		}
	}

	var updated int64
	for _, chunk := range chunkIDs(ids) {
		var query string
		var args []any
		if pinned {
			query = `UPDATE files SET pinned = 1, pinned_at = ?, pin_source = ? WHERE id IN (` +
				placeholders(len(chunk)) + `)`
			args = append(args, at, source)
		} else {
			query = `UPDATE files SET pinned = 0, pinned_at = NULL, pin_source = '' WHERE id IN (` +
				placeholders(len(chunk)) + `)`
		}
		for _, id := range chunk {
			args = append(args, id)
		}
		res, err := c.db.Exec(query, args...)
		if err != nil {
			return updated, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return updated, err
		}
		updated += n
	}
	return updated, nil
}

func validPinSource(source string) error {
	if source == PinSourceUser || source == PinSourceRestore {
		return nil
	}
	return fmt.Errorf("invalid pin source %q", source)
}

// ArchivingFiles returns all files stuck in the transient "archiving" state.
func (c *Catalog) ArchivingFiles() ([]FileRecord, error) {
	return c.queryFiles(`SELECT ` + fileColumns + ` FROM files WHERE status = 'archiving'`)
}

// RestoringFiles returns all files stuck in the transient "restoring" state.
func (c *Catalog) RestoringFiles() ([]FileRecord, error) {
	return c.queryFiles(`SELECT ` + fileColumns + ` FROM files WHERE status = 'restoring'`)
}

// MarkMissingSyncedFiles marks all synced files in a folder as missing.
//
// Deprecated: this is the mark-then-restore sweep. It blinds every CAS and
// every size aggregate for the whole duration of a walk. Use UpsertFileSeen
// with a scan generation and then MarkFilesMissingBeforeGeneration.
func (c *Catalog) MarkMissingSyncedFiles(folder string) error {
	_, err := c.db.Exec(`UPDATE files SET status = 'missing' WHERE folder_label = ? AND status = 'synced'`, folder)
	return err
}

// MarkFilesMissingBeforeGeneration marks as missing every synced file in a
// folder that the scan identified by scanGeneration did not touch. It must be
// called only after a walk that COMPLETED without error; an aborted or
// unmounted walk simply never runs it.
//
// Rows in archiving or restoring are never swept, so a move in flight cannot
// be flipped to missing behind the mover's back.
func (c *Catalog) MarkFilesMissingBeforeGeneration(folder string, scanGeneration int64) (int64, error) {
	res, err := c.db.Exec(`
		UPDATE files SET status = 'missing'
		WHERE folder_label = ? AND status = 'synced' AND last_seen < ?`, folder, scanGeneration)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FolderSyncedSize returns the total size of synced files in a folder.
//
// Deprecated: use FolderSizes, which reports the pinned subset alongside the
// total and counts the two transient on-device states.
func (c *Catalog) FolderSyncedSize(folder string) (int64, error) {
	total, _, err := c.FolderSizes(folder)
	return total, err
}

// FolderSizes returns the folder's on-device footprint and the pinned subset
// of it, in one pass.
//
// Both figures count status IN ('synced','archiving','restoring'):
//   - synced    : on device now
//   - archiving : still on device, a move out is in flight
//   - restoring : a move in is in flight; the bytes are about to land
//
// 'archived' and 'missing' are not on the device and are excluded from both.
//
// pinned is a SUBSET of total, never an addend. Per D3 the budget is a hard cap
// on the on-device footprint whether pinned or not.
func (c *Catalog) FolderSizes(folder string) (total, pinned int64, err error) {
	err = c.db.QueryRow(`
		SELECT COALESCE(SUM(size_bytes), 0),
		       COALESCE(SUM(CASE WHEN pinned = 1 THEN size_bytes ELSE 0 END), 0)
		FROM files
		WHERE folder_label = ? AND status IN ('synced','archiving','restoring')
	`, folder).Scan(&total, &pinned)
	return total, pinned, err
}

// FolderSizesForIDs sums the archived bytes the given ids would bring back,
// grouped by folder. It backs the restore budget pre-flight.
func (c *Catalog) FolderSizesForIDs(ids []int64) (map[string]int64, error) {
	sizes := make(map[string]int64)
	if len(ids) == 0 {
		return sizes, nil
	}

	for _, chunk := range chunkIDs(ids) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := c.db.Query(`
			SELECT folder_label, COALESCE(SUM(size_bytes), 0)
			FROM files WHERE id IN (`+placeholders(len(chunk))+`) AND status = 'archived'
			GROUP BY folder_label`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var label string
			var size int64
			if err := rows.Scan(&label, &size); err != nil {
				rows.Close()
				return nil, err
			}
			sizes[label] += size
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return sizes, nil
}

// CountUnpinnedSyncedOldestUpTo reports how many files the daemon would have to
// offload, oldest first, to free the given number of bytes. capped is true when
// the scan hit its 50,000-row ceiling before reaching the target.
func (c *Catalog) CountUnpinnedSyncedOldestUpTo(folder string, bytes int64) (count int, capped bool, err error) {
	if bytes <= 0 {
		return 0, false, nil
	}
	const scanCap = 50000

	rows, err := c.db.Query(`
		SELECT size_bytes FROM files
		WHERE folder_label = ? AND status = 'synced' AND pinned = 0
		ORDER BY MAX(file_mtime, file_atime) ASC
		LIMIT ?`, folder, scanCap)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()

	var running int64
	var scanned int
	for rows.Next() {
		var size int64
		if err := rows.Scan(&size); err != nil {
			return 0, false, err
		}
		scanned++
		running += size
		count++
		if running >= bytes {
			return count, false, rows.Err()
		}
	}
	return count, scanned == scanCap, rows.Err()
}

// FolderCounts returns the per-status file tally for a folder.
func (c *Catalog) FolderCounts(folder string) (FolderCounts, error) {
	var fc FolderCounts
	err := c.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN status = 'synced' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'archived' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'missing' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN pinned = 1 THEN 1 ELSE 0 END), 0)
		FROM files WHERE folder_label = ?`, folder).
		Scan(&fc.Total, &fc.Synced, &fc.Archived, &fc.Missing, &fc.Pinned)
	return fc, err
}

// OffloadCandidates returns unpinned synced files in a folder, oldest first by
// max(mtime, atime), capped at defaultOffloadCandidateLimit rows.
//
// Persistent-glob filtering and the grace cutoff happen in the caller, in Go:
// file_mtime and file_atime are TEXT columns, so comparing a bound Go time
// against them is a string comparison with a timezone-dependent result.
func (c *Catalog) OffloadCandidates(folder string) ([]FileRecord, error) {
	return c.OffloadCandidatesLimited(folder, defaultOffloadCandidateLimit)
}

// OffloadCandidatesLimited is OffloadCandidates with an explicit row cap. A
// limit of zero or less means the default. The cap is mandatory: without it a
// 50,000-file camera roll materialises 50,000 records on every tick.
func (c *Catalog) OffloadCandidatesLimited(folder string, limit int) ([]FileRecord, error) {
	if limit <= 0 {
		limit = defaultOffloadCandidateLimit
	}
	return c.queryFiles(`SELECT `+fileColumns+`
		FROM files
		WHERE folder_label = ? AND status = 'synced' AND pinned = 0
		ORDER BY MAX(file_mtime, file_atime) ASC
		LIMIT ?`, folder, limit)
}

// FileCount returns the total number of files matching the given status (or all if empty).
func (c *Catalog) FileCount(status FileStatus) (int64, error) {
	var count int64
	var err error
	if status == "" {
		err = c.db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&count)
	} else {
		err = c.db.QueryRow(`SELECT COUNT(*) FROM files WHERE status = ?`, status).Scan(&count)
	}
	return count, err
}

// ---------------------------------------------------------------------------
// thumbnails
// ---------------------------------------------------------------------------

// UpdateThumbnailPath sets the thumbnail_path for a file and clears any
// recorded failure.
func (c *Catalog) UpdateThumbnailPath(id int64, thumbPath string) error {
	_, err := c.db.Exec(
		`UPDATE files SET thumbnail_path = ?, thumb_error = '' WHERE id = ?`, thumbPath, id)
	return err
}

// FilesNeedingThumbnails returns files that have a thumbnailable extension but
// no thumbnail_path set, capped at 200 rows per call.
func (c *Catalog) FilesNeedingThumbnails(extensions []string) ([]FileRecord, error) {
	return c.FilesNeedingThumbnailsLimited(extensions, 200)
}

// FilesNeedingThumbnailsLimited is FilesNeedingThumbnails with an explicit row
// cap. Files that have already failed maxThumbnailAttempts times are excluded,
// so a permanently unreadable file is not retried forever.
func (c *Catalog) FilesNeedingThumbnailsLimited(extensions []string, limit int) ([]FileRecord, error) {
	if len(extensions) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}

	args := make([]any, 0, len(extensions)+2)
	for _, ext := range extensions {
		args = append(args, ext)
	}
	args = append(args, maxThumbnailAttempts, limit)

	return c.queryFiles(`SELECT `+fileColumns+`
		FROM files
		WHERE thumbnail_path = '' AND extension IN (`+placeholders(len(extensions))+`)
		AND status IN ('synced', 'archived')
		AND thumb_attempts < ?
		ORDER BY file_mtime DESC
		LIMIT ?`, args...)
}

// RecordThumbnailFailure increments a file's attempt counter and records why
// the last attempt failed. A file that reaches maxThumbnailAttempts drops out
// of FilesNeedingThumbnails.
func (c *Catalog) RecordThumbnailFailure(id int64, errMsg string, at int64) error {
	_, err := c.db.Exec(`
		UPDATE files SET thumb_attempts = thumb_attempts + 1,
			thumb_last_attempt = ?, thumb_error = ?
		WHERE id = ?`, at, errMsg, id)
	return err
}

// ResetThumbnailFailures clears the failure state for a folder so its files
// re-enter the queue. Pass an empty label for every folder.
func (c *Catalog) ResetThumbnailFailures(folder string) (int64, error) {
	query := `UPDATE files SET thumb_attempts = 0, thumb_last_attempt = NULL, thumb_error = ''
		WHERE thumb_attempts > 0`
	var args []any
	if folder != "" {
		query += ` AND folder_label = ?`
		args = append(args, folder)
	}
	res, err := c.db.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// folder_configs
// ---------------------------------------------------------------------------

const folderConfigColumns = `id, folder_label, syncthing_id, syncthing_path, budget_bytes,
	grace_minutes, enabled, present_in_toml, wedge_warned_at, last_enforced_at`

// UpsertFolderConfig inserts or updates a folder configuration.
//
// Deprecated: it overwrites every column including the three the user edits, so
// calling it from a startup reconciler would reset a UI-set budget to the TOML
// value on every restart. Use ReconcileFolderFromTOML or UpdateFolderPolicy.
func (c *Catalog) UpsertFolderConfig(fc *FolderConfigRecord) error {
	_, err := c.db.Exec(`
		INSERT INTO folder_configs (folder_label, syncthing_id, syncthing_path, budget_bytes, grace_minutes, enabled)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(folder_label) DO UPDATE SET
			syncthing_id = excluded.syncthing_id,
			syncthing_path = excluded.syncthing_path,
			budget_bytes = excluded.budget_bytes,
			grace_minutes = excluded.grace_minutes,
			enabled = excluded.enabled
	`, fc.FolderLabel, fc.SyncthingID, fc.SyncthingPath, fc.BudgetBytes, fc.GraceMinutes, fc.Enabled)
	return err
}

// ReconcileFolderFromTOML upserts the infrastructure fields of a folder
// declared in coldstore.toml. budget/grace/enabled are written ONLY when the
// row is created; on an existing row they are left exactly as the user set them
// in the UI. Returns true if a new row was inserted.
func (c *Catalog) ReconcileFolderFromTOML(label, syncthingID, syncthingPath string,
	seedBudget int64, seedGrace int) (inserted bool, err error) {

	res, err := c.db.Exec(`
		INSERT INTO folder_configs
			(folder_label, syncthing_id, syncthing_path, budget_bytes,
			 grace_minutes, enabled, present_in_toml)
		VALUES (?, ?, ?, ?, ?, 1, 1)
		ON CONFLICT(folder_label) DO NOTHING
	`, label, syncthingID, syncthingPath, seedBudget, seedGrace)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}

	// Always refresh the TOML-owned fields, never the policy fields.
	if _, err := c.db.Exec(`
		UPDATE folder_configs
		SET syncthing_id = ?, syncthing_path = ?, present_in_toml = 1
		WHERE folder_label = ?
	`, syncthingID, syncthingPath, label); err != nil {
		return false, err
	}
	return n == 1, nil
}

// MarkFoldersAbsentFromTOML sets present_in_toml = 0 for every row whose label
// is not in the given set. Rows are never deleted: dropping one would throw
// away the budget, the grace setting and the persistent globs, so a typo in
// coldstore.toml followed by a restart would destroy the owner's tuning.
func (c *Catalog) MarkFoldersAbsentFromTOML(declared []string) error {
	if len(declared) == 0 {
		_, err := c.db.Exec(`UPDATE folder_configs SET present_in_toml = 0`)
		return err
	}
	args := make([]any, len(declared))
	for i, label := range declared {
		args[i] = label
	}
	_, err := c.db.Exec(`UPDATE folder_configs SET present_in_toml = 0
		WHERE folder_label NOT IN (`+placeholders(len(declared))+`)`, args...)
	return err
}

// UpdateFolderPolicy writes ONLY the UI-tunable fields. It never inserts and
// never touches syncthing_id or syncthing_path. The SET clause is built from
// the non-nil pointers only, so a request that omits grace_minutes leaves
// grace_minutes alone. Returns false if no row with that label exists.
func (c *Catalog) UpdateFolderPolicy(label string, budgetBytes *int64, graceMinutes *int,
	enabled *bool) (found bool, err error) {

	var exists int
	err = c.db.QueryRow(`SELECT 1 FROM folder_configs WHERE folder_label = ?`, label).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	var sets []string
	var args []any
	if budgetBytes != nil {
		sets = append(sets, "budget_bytes = ?")
		args = append(args, *budgetBytes)
	}
	if graceMinutes != nil {
		sets = append(sets, "grace_minutes = ?")
		args = append(args, *graceMinutes)
	}
	if enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolToInt(*enabled))
	}
	if len(sets) == 0 {
		return true, nil
	}

	args = append(args, label)
	if _, err := c.db.Exec(`UPDATE folder_configs SET `+strings.Join(sets, ", ")+
		` WHERE folder_label = ?`, args...); err != nil {
		return false, err
	}
	return true, nil
}

// ActiveFolderConfigs returns folders the daemon should enforce and scan:
// enabled and still declared in TOML.
func (c *Catalog) ActiveFolderConfigs() ([]FolderConfigRecord, error) {
	return c.queryFolderConfigs(`SELECT ` + folderConfigColumns + ` FROM folder_configs
		WHERE enabled = 1 AND present_in_toml = 1 ORDER BY folder_label`)
}

// ListFolderConfigs returns every row, including disabled and orphaned ones.
func (c *Catalog) ListFolderConfigs() ([]FolderConfigRecord, error) {
	return c.queryFolderConfigs(`SELECT ` + folderConfigColumns + ` FROM folder_configs
		ORDER BY folder_label`)
}

// FolderConfigByLabel returns one folder's row, or ErrNoSuchFolder.
func (c *Catalog) FolderConfigByLabel(label string) (*FolderConfigRecord, error) {
	row := c.db.QueryRow(`SELECT `+folderConfigColumns+` FROM folder_configs
		WHERE folder_label = ?`, label)
	fc, err := scanFolderConfig(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchFolder, label)
	}
	return fc, err
}

// FolderPath returns the syncthing_path of any folder_configs row, including
// disabled and orphaned (present_in_toml = 0) ones. It is the single runtime
// source of a folder's on-disk location, which is what keeps an orphaned
// folder's files browsable and downloadable.
func (c *Catalog) FolderPath(label string) (string, error) {
	var path string
	err := c.db.QueryRow(`SELECT syncthing_path FROM folder_configs WHERE folder_label = ?`, label).
		Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrNoSuchFolder, label)
	}
	if err != nil {
		return "", err
	}
	return path, nil
}

// SetWedgeWarnedAt sets or clears the wedge warning latch. Pass nil to clear
// it. The latch lives in the database rather than in memory so that a restart
// loop cannot re-flood the events table with the same warning.
func (c *Catalog) SetWedgeWarnedAt(label string, at *int64) error {
	_, err := c.db.Exec(`UPDATE folder_configs SET wedge_warned_at = ? WHERE folder_label = ?`, at, label)
	return err
}

// SetLastEnforcedAt records that the daemon looked at this folder, whatever it
// decided to do. It is stamped even for a zero-budget or unavailable folder, so
// that "last checked N minutes ago" never looks like a dead daemon.
func (c *Catalog) SetLastEnforcedAt(label string, at int64) error {
	_, err := c.db.Exec(`UPDATE folder_configs SET last_enforced_at = ? WHERE folder_label = ?`, at, label)
	return err
}

// ---------------------------------------------------------------------------
// persistent paths
// ---------------------------------------------------------------------------

// AddPersistentPath adds a persistent path pattern for a folder.
func (c *Catalog) AddPersistentPath(folder, pattern string) error {
	_, err := c.db.Exec(`INSERT OR IGNORE INTO persistent_paths (folder_label, pattern) VALUES (?, ?)`, folder, pattern)
	return err
}

// ListPersistentPaths returns all persistent path patterns for a folder.
func (c *Catalog) ListPersistentPaths(folder string) ([]PersistentPath, error) {
	rows, err := c.db.Query(`SELECT id, folder_label, pattern FROM persistent_paths WHERE folder_label = ?`, folder)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var paths []PersistentPath
	for rows.Next() {
		var p PersistentPath
		if err := rows.Scan(&p.ID, &p.FolderLabel, &p.Pattern); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// DeletePersistentPath removes a persistent path by ID.
func (c *Catalog) DeletePersistentPath(id int64) error {
	_, err := c.db.Exec(`DELETE FROM persistent_paths WHERE id = ?`, id)
	return err
}

// ---------------------------------------------------------------------------
// events
// ---------------------------------------------------------------------------

// LogEvent records an event in the audit log.
func (c *Catalog) LogEvent(eventType, folder, path, details string) error {
	_, err := c.db.Exec(`INSERT INTO events (event_type, folder, path, details) VALUES (?, ?, ?, ?)`,
		eventType, folder, path, details)
	return err
}

// LogEventDeduped inserts an event only if no event with the same type and
// folder was recorded within the last windowMinutes.
//
// The window is evaluated entirely inside SQLite so that both sides of the
// comparison are CURRENT_TIMESTAMP-format text; never bind a Go time.Time here.
//
// Because suppression keys on (event_type, folder), every deduped event type
// must be specific. Two unrelated conditions sharing a coarse type would
// suppress each other.
func (c *Catalog) LogEventDeduped(eventType, folder, path, details string, windowMinutes int) error {
	_, err := c.db.Exec(`
		INSERT INTO events (event_type, folder, path, details)
		SELECT ?, ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM events
			WHERE event_type = ? AND folder = ? AND timestamp > datetime('now', ?)
		)`,
		eventType, folder, path, details,
		eventType, folder, fmt.Sprintf("-%d minutes", windowMinutes))
	return err
}

// ---------------------------------------------------------------------------
// scanning helpers
// ---------------------------------------------------------------------------

type fileScanner interface{ Scan(dest ...any) error }

func scanFile(s fileScanner) (*FileRecord, error) {
	var f FileRecord
	var archivedAt sql.NullTime
	var pinnedAt, lastAttempt sql.NullInt64
	err := s.Scan(&f.ID, &f.FolderLabel, &f.RelativePath, &f.Filename, &f.Extension,
		&f.MimeType, &f.SizeBytes, &f.FileMtime, &f.FileAtime, &f.FirstSeen, &archivedAt,
		&f.Status, &f.ThumbnailPath, &f.ContentHash, &f.Pinned, &pinnedAt, &f.PinSource,
		&f.LastSeen, &f.ThumbAttempts, &lastAttempt, &f.ThumbError)
	if err != nil {
		return nil, err
	}
	if archivedAt.Valid {
		f.ArchivedAt = &archivedAt.Time
	}
	if pinnedAt.Valid {
		f.PinnedAt = &pinnedAt.Int64
	}
	if lastAttempt.Valid {
		f.ThumbLastAttempt = &lastAttempt.Int64
	}
	return &f, nil
}

func (c *Catalog) queryFiles(query string, args ...any) ([]FileRecord, error) {
	rows, err := c.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []FileRecord
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, *f)
	}
	return files, rows.Err()
}

func scanFolderConfig(s fileScanner) (*FolderConfigRecord, error) {
	var fc FolderConfigRecord
	var wedgeWarnedAt, lastEnforcedAt sql.NullInt64
	err := s.Scan(&fc.ID, &fc.FolderLabel, &fc.SyncthingID, &fc.SyncthingPath, &fc.BudgetBytes,
		&fc.GraceMinutes, &fc.Enabled, &fc.PresentInTOML, &wedgeWarnedAt, &lastEnforcedAt)
	if err != nil {
		return nil, err
	}
	if wedgeWarnedAt.Valid {
		fc.WedgeWarnedAt = &wedgeWarnedAt.Int64
	}
	if lastEnforcedAt.Valid {
		fc.LastEnforcedAt = &lastEnforcedAt.Int64
	}
	return &fc, nil
}

func (c *Catalog) queryFolderConfigs(query string, args ...any) ([]FolderConfigRecord, error) {
	rows, err := c.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []FolderConfigRecord
	for rows.Next() {
		fc, err := scanFolderConfig(rows)
		if err != nil {
			return nil, err
		}
		configs = append(configs, *fc)
	}
	return configs, rows.Err()
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func chunkIDs(ids []int64) [][]int64 {
	var chunks [][]int64
	for start := 0; start < len(ids); start += idChunkSize {
		end := start + idChunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunks = append(chunks, ids[start:end])
	}
	return chunks
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
