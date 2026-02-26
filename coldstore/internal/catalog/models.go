package catalog

import "time"

type FileStatus string

const (
	StatusSynced    FileStatus = "synced"
	StatusArchived  FileStatus = "archived"
	StatusRestoring FileStatus = "restoring"
	StatusMissing   FileStatus = "missing"
)

type FileRecord struct {
	ID            int64      `json:"id"`
	FolderLabel   string     `json:"folder_label"`
	RelativePath  string     `json:"relative_path"`
	Filename      string     `json:"filename"`
	Extension     string     `json:"extension"`
	MimeType      string     `json:"mime_type"`
	SizeBytes     int64      `json:"size_bytes"`
	FileMtime     time.Time  `json:"file_mtime"`
	FirstSeen     time.Time  `json:"first_seen"`
	ArchivedAt    *time.Time `json:"archived_at,omitempty"`
	Status        FileStatus `json:"status"`
	ThumbnailPath string     `json:"thumbnail_path,omitempty"`
	ContentHash   string     `json:"content_hash,omitempty"`
}

type FolderConfigRecord struct {
	ID            int64  `json:"id"`
	FolderLabel   string `json:"folder_label"`
	SyncthingID   string `json:"syncthing_id"`
	SyncthingPath string `json:"syncthing_path"`
	BudgetBytes   int64  `json:"budget_bytes"`
	GraceMinutes  int    `json:"grace_minutes"`
	Enabled       bool   `json:"enabled"`
}

type PersistentPath struct {
	ID          int64  `json:"id"`
	FolderLabel string `json:"folder_label"`
	Pattern     string `json:"pattern"`
}

type Event struct {
	ID        int64     `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	EventType string    `json:"event_type"`
	Folder    string    `json:"folder"`
	Path      string    `json:"path"`
	Details   string    `json:"details"`
}
