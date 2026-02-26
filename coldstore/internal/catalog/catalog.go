package catalog

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Catalog struct {
	db *sql.DB
}

func Open(dbPath string) (*Catalog, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}

	return &Catalog{db: db}, nil
}

func (c *Catalog) Close() error {
	return c.db.Close()
}

// UpsertFile inserts or updates a file record based on (folder_label, relative_path).
func (c *Catalog) UpsertFile(f *FileRecord) error {
	_, err := c.db.Exec(`
		INSERT INTO files (folder_label, relative_path, filename, extension, mime_type, size_bytes, file_mtime, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(folder_label, relative_path) DO UPDATE SET
			filename = excluded.filename,
			size_bytes = excluded.size_bytes,
			file_mtime = excluded.file_mtime,
			mime_type = excluded.mime_type,
			status = CASE
				WHEN files.status = 'missing' THEN 'synced'
				ELSE files.status
			END
	`, f.FolderLabel, f.RelativePath, f.Filename, f.Extension, f.MimeType, f.SizeBytes, f.FileMtime, f.Status)
	return err
}

// GetFile retrieves a file by folder and relative path.
func (c *Catalog) GetFile(folder, relPath string) (*FileRecord, error) {
	row := c.db.QueryRow(`
		SELECT id, folder_label, relative_path, filename, extension, mime_type,
		       size_bytes, file_mtime, first_seen, archived_at, status, thumbnail_path, content_hash
		FROM files WHERE folder_label = ? AND relative_path = ?
	`, folder, relPath)
	return scanFileRecord(row)
}

// GetFileByID retrieves a file by its ID.
func (c *Catalog) GetFileByID(id int64) (*FileRecord, error) {
	row := c.db.QueryRow(`
		SELECT id, folder_label, relative_path, filename, extension, mime_type,
		       size_bytes, file_mtime, first_seen, archived_at, status, thumbnail_path, content_hash
		FROM files WHERE id = ?
	`, id)
	return scanFileRecord(row)
}

type FileQuery struct {
	Folder    string
	Status    FileStatus
	Extension string
	Search    string
	SortBy    string
	Offset    int
	Limit     int
}

// ListFiles queries files with filtering, sorting, and pagination.
func (c *Catalog) ListFiles(q FileQuery) ([]FileRecord, error) {
	query := `SELECT id, folder_label, relative_path, filename, extension, mime_type,
	                 size_bytes, file_mtime, first_seen, archived_at, status, thumbnail_path, content_hash
	          FROM files WHERE 1=1`
	var args []any

	if q.Folder != "" {
		query += " AND folder_label = ?"
		args = append(args, q.Folder)
	}
	if q.Status != "" {
		query += " AND status = ?"
		args = append(args, q.Status)
	}
	if q.Extension != "" {
		query += " AND extension = ?"
		args = append(args, q.Extension)
	}
	if q.Search != "" {
		query += " AND filename LIKE ?"
		args = append(args, "%"+q.Search+"%")
	}

	switch q.SortBy {
	case "size":
		query += " ORDER BY size_bytes DESC"
	case "name":
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

	rows, err := c.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []FileRecord
	for rows.Next() {
		f, err := scanFileRecordRows(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, *f)
	}
	return files, rows.Err()
}

// UpdateFileStatus sets a file's status and optionally archived_at.
func (c *Catalog) UpdateFileStatus(id int64, status FileStatus) error {
	if status == StatusArchived {
		_, err := c.db.Exec(`UPDATE files SET status = ?, archived_at = ? WHERE id = ?`,
			status, time.Now().UTC(), id)
		return err
	}
	_, err := c.db.Exec(`UPDATE files SET status = ?, archived_at = NULL WHERE id = ?`, status, id)
	return err
}

// MarkMissingSyncedFiles marks all synced files in a folder as missing,
// used before a scan to detect files that have disappeared.
func (c *Catalog) MarkMissingSyncedFiles(folder string) error {
	_, err := c.db.Exec(`UPDATE files SET status = 'missing' WHERE folder_label = ? AND status = 'synced'`, folder)
	return err
}

// FolderSyncedSize returns the total size of synced files in a folder.
func (c *Catalog) FolderSyncedSize(folder string) (int64, error) {
	var size sql.NullInt64
	err := c.db.QueryRow(`SELECT SUM(size_bytes) FROM files WHERE folder_label = ? AND status = 'synced'`, folder).Scan(&size)
	if err != nil {
		return 0, err
	}
	return size.Int64, nil
}

// OffloadCandidates returns synced files in a folder, sorted by mtime ascending (oldest first),
// skipping any files whose relative path matches persistent patterns.
func (c *Catalog) OffloadCandidates(folder string) ([]FileRecord, error) {
	rows, err := c.db.Query(`
		SELECT id, folder_label, relative_path, filename, extension, mime_type,
		       size_bytes, file_mtime, first_seen, archived_at, status, thumbnail_path, content_hash
		FROM files
		WHERE folder_label = ? AND status = 'synced'
		ORDER BY file_mtime ASC
	`, folder)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []FileRecord
	for rows.Next() {
		f, err := scanFileRecordRows(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, *f)
	}
	return files, rows.Err()
}

// UpsertFolderConfig inserts or updates a folder configuration.
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

// ListFolderConfigs returns all folder configs.
func (c *Catalog) ListFolderConfigs() ([]FolderConfigRecord, error) {
	rows, err := c.db.Query(`SELECT id, folder_label, syncthing_id, syncthing_path, budget_bytes, grace_minutes, enabled FROM folder_configs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []FolderConfigRecord
	for rows.Next() {
		var fc FolderConfigRecord
		if err := rows.Scan(&fc.ID, &fc.FolderLabel, &fc.SyncthingID, &fc.SyncthingPath, &fc.BudgetBytes, &fc.GraceMinutes, &fc.Enabled); err != nil {
			return nil, err
		}
		configs = append(configs, fc)
	}
	return configs, rows.Err()
}

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

// LogEvent records an event in the audit log.
func (c *Catalog) LogEvent(eventType, folder, path, details string) error {
	_, err := c.db.Exec(`INSERT INTO events (event_type, folder, path, details) VALUES (?, ?, ?, ?)`,
		eventType, folder, path, details)
	return err
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

func scanFileRecord(row *sql.Row) (*FileRecord, error) {
	var f FileRecord
	var archivedAt sql.NullTime
	err := row.Scan(&f.ID, &f.FolderLabel, &f.RelativePath, &f.Filename, &f.Extension,
		&f.MimeType, &f.SizeBytes, &f.FileMtime, &f.FirstSeen, &archivedAt,
		&f.Status, &f.ThumbnailPath, &f.ContentHash)
	if err != nil {
		return nil, err
	}
	if archivedAt.Valid {
		f.ArchivedAt = &archivedAt.Time
	}
	return &f, nil
}

func scanFileRecordRows(rows *sql.Rows) (*FileRecord, error) {
	var f FileRecord
	var archivedAt sql.NullTime
	err := rows.Scan(&f.ID, &f.FolderLabel, &f.RelativePath, &f.Filename, &f.Extension,
		&f.MimeType, &f.SizeBytes, &f.FileMtime, &f.FirstSeen, &archivedAt,
		&f.Status, &f.ThumbnailPath, &f.ContentHash)
	if err != nil {
		return nil, err
	}
	if archivedAt.Valid {
		f.ArchivedAt = &archivedAt.Time
	}
	return &f, nil
}
