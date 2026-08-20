package catalog

import "time"

type FileStatus string

const (
	StatusSynced    FileStatus = "synced"
	StatusArchiving FileStatus = "archiving"
	StatusArchived  FileStatus = "archived"
	StatusRestoring FileStatus = "restoring"
	StatusMissing   FileStatus = "missing"
)

// Pin sources. A pin is either explicit (the user pressed the pin toggle) or
// implicit (a restore auto-pinned the file so the next quota tick cannot take
// it straight back). Keeping them apart is what makes "unpin the ones I did
// not choose" a possible action later on.
const (
	PinSourceNone    = ""
	PinSourceUser    = "user"
	PinSourceRestore = "restore"
)

// Job actions and states.
const (
	JobActionRestore = "restore"
	JobActionArchive = "archive"
	JobActionScan    = "scan"

	JobStatePending = "pending"
	JobStateRunning = "running"
	JobStateDone    = "done"
	JobStateFailed  = "failed"
)

type FileRecord struct {
	ID               int64      `json:"id"`
	FolderLabel      string     `json:"folder_label"`
	RelativePath     string     `json:"relative_path"`
	Filename         string     `json:"filename"`
	Extension        string     `json:"extension"`
	MimeType         string     `json:"mime_type"`
	SizeBytes        int64      `json:"size_bytes"`
	FileMtime        time.Time  `json:"file_mtime"`
	FileAtime        time.Time  `json:"file_atime"`
	FirstSeen        time.Time  `json:"first_seen"`
	ArchivedAt       *time.Time `json:"archived_at,omitempty"`
	Status           FileStatus `json:"status"`
	ThumbnailPath    string     `json:"thumbnail_path,omitempty"`
	ContentHash      string     `json:"content_hash,omitempty"`
	Pinned           bool       `json:"pinned"`
	PinnedAt         *int64     `json:"pinned_at"`  // null when unpinned; never omitted
	PinSource        string     `json:"pin_source"` // "" | "user" | "restore"
	LastSeen         int64      `json:"-"`          // internal scan generation
	ThumbAttempts    int        `json:"thumb_attempts"`
	ThumbLastAttempt *int64     `json:"thumb_last_attempt,omitempty"`
	ThumbError       string     `json:"thumb_error,omitempty"`
}

type FolderConfigRecord struct {
	ID             int64  `json:"id"`
	FolderLabel    string `json:"folder_label"`
	SyncthingID    string `json:"syncthing_id"`
	SyncthingPath  string `json:"syncthing_path"`
	BudgetBytes    int64  `json:"budget_bytes"`
	GraceMinutes   int    `json:"grace_minutes"`
	Enabled        bool   `json:"enabled"`
	PresentInTOML  bool   `json:"present_in_toml"`
	LastEnforcedAt *int64 `json:"last_enforced_at"` // null, never omitted
	WedgeWarnedAt  *int64 `json:"-"`                // internal latch; not exposed
}

// FolderBudget is the single place the over-budget and wedged predicates are
// defined, so the enforcement epilogue and the status handler cannot drift.
type FolderBudget struct {
	BudgetBytes int64
	TotalBytes  int64
	PinnedBytes int64
}

// OverBudget reports the honest "this folder does not fit" condition. It is
// legitimately true for a tick when every remaining candidate is inside its
// grace window or matches a persistent glob.
func (b FolderBudget) OverBudget() bool { return b.BudgetBytes > 0 && b.TotalBytes > b.BudgetBytes }

// Wedged reports that pinned bytes ALONE exceed the budget, so even offloading
// every unpinned file in the folder cannot converge (D3).
func (b FolderBudget) Wedged() bool { return b.BudgetBytes > 0 && b.PinnedBytes > b.BudgetBytes }

// FolderCounts is the per-folder file tally the status API reports.
type FolderCounts struct {
	Total    int64 `json:"total"`
	Synced   int64 `json:"synced"`
	Archived int64 `json:"archived"`
	Missing  int64 `json:"missing"`
	Pinned   int64 `json:"pinned"`
}

// Job is the jobs row. It is NOT the wire type for GET /jobs/{id}: the row's
// counter column is `failed` and that response's `failed` field is the failure
// detail array, so they cannot share a struct.
type Job struct {
	ID          int64  `json:"id"`
	Action      string `json:"action"`
	FolderLabel string `json:"folder_label"`
	State       string `json:"state"`
	Total       int    `json:"total"`
	Done        int    `json:"done"`
	FailedCount int    `json:"failed_count"` // column `failed`
	Error       string `json:"error"`        // job-level reason, "" normally
	CreatedAt   int64  `json:"created_at"`
	StartedAt   *int64 `json:"started_at"`  // null, never omitted
	FinishedAt  *int64 `json:"finished_at"` // null, never omitted
}

type JobFailure struct {
	ID    int64  `json:"id"`
	Error string `json:"error"`
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
