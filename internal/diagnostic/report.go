package diagnostic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/daemon"
)

type Severity string

const (
	SeverityWarning Severity = "warning"
	SeverityFailure Severity = "failure"

	ExitHealthy = 0
	ExitWarning = 1
	ExitFailure = 2
)

type Finding struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Message  string   `json:"message"`
}

type CatalogReport struct {
	Exists           bool `json:"exists"`
	SupportedSchema  int  `json:"supported_schema"`
	OnDiskSchema     int  `json:"on_disk_schema"`
	SchemaCompatible bool `json:"schema_compatible"`
}

type StorageReport struct {
	Path         string      `json:"path"`
	Exists       bool        `json:"exists"`
	IsMountPoint bool        `json:"is_mount_point"`
	FSType       string      `json:"filesystem_type,omitempty"`
	FreeBytes    uint64      `json:"free_bytes,omitempty"`
	FreePercent  float64     `json:"free_percent,omitempty"`
	Btrfs        BtrfsReport `json:"btrfs"`
}

type BtrfsReport struct {
	Status string `json:"status"` // healthy, degraded, unknown, not_applicable
	Detail string `json:"detail,omitempty"`
}

type FileCounts struct {
	Total     int64 `json:"total"`
	Synced    int64 `json:"synced"`
	Archiving int64 `json:"archiving"`
	Archived  int64 `json:"archived"`
	Restoring int64 `json:"restoring"`
	Missing   int64 `json:"missing"`
}

type FolderReport struct {
	Label          string               `json:"label"`
	Path           string               `json:"path"`
	Declared       bool                 `json:"declared"`
	Catalogued     bool                 `json:"catalogued"`
	Enabled        bool                 `json:"enabled"`
	PresentInTOML  bool                 `json:"present_in_toml"`
	Ready          bool                 `json:"ready"`
	Readiness      string               `json:"readiness"`
	BudgetBytes    int64                `json:"budget_bytes"`
	OnDeviceBytes  int64                `json:"on_device_bytes"`
	PinnedBytes    int64                `json:"pinned_bytes"`
	OverBudget     bool                 `json:"over_budget"`
	Wedged         bool                 `json:"wedged"`
	LastEnforcedAt *int64               `json:"last_enforced_at"`
	Counts         catalog.FolderCounts `json:"counts"`
}

type JobReport struct {
	ID          int64  `json:"id"`
	Action      string `json:"action"`
	FolderLabel string `json:"folder_label,omitempty"`
	State       string `json:"state"`
	Total       int    `json:"total"`
	Done        int    `json:"done"`
	Failed      int    `json:"failed"`
	CreatedAt   int64  `json:"created_at"`
}

type SafeEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Folder    string    `json:"folder,omitempty"`
}

type DerivedReport struct {
	ThumbnailFailures int64  `json:"thumbnail_failures"`
	DetectionBacklog  int64  `json:"detection_backlog"`
	MetadataFailures  *int64 `json:"metadata_failures"`
}

type Report struct {
	Status       string         `json:"status"`
	ExitCode     int            `json:"exit_code"`
	GeneratedAt  time.Time      `json:"generated_at"`
	BuildVersion string         `json:"build_version"`
	DaemonStatus string         `json:"daemon_status"`
	Catalog      CatalogReport  `json:"catalog"`
	Storage      StorageReport  `json:"storage"`
	Files        FileCounts     `json:"files"`
	Folders      []FolderReport `json:"folders"`
	ActiveJobs   []JobReport    `json:"active_jobs"`
	FailedJobs   []JobReport    `json:"failed_jobs"`
	Derived      DerivedReport  `json:"derived"`
	RecentEvents []SafeEvent    `json:"recent_warning_events"`
	LastScan     *time.Time     `json:"last_successful_scan"`
	LastOffload  *time.Time     `json:"last_successful_offload"`
	Findings     []Finding      `json:"findings"`
	Limitations  []string       `json:"limitations"`
}

type Builder struct {
	now           func() time.Time
	describeMount func(string) (config.MountState, error)
	space         func(string) (uint64, float64, error)
	btrfs         func(context.Context, string) BtrfsReport
}

func NewBuilder() *Builder {
	return &Builder{
		now:           time.Now,
		describeMount: config.DescribeMount,
		space: func(path string) (uint64, float64, error) {
			bytes, err := daemon.AvailableSpaceBytes(path)
			if err != nil {
				return 0, 0, err
			}
			percent, err := daemon.AvailableSpacePercent(path)
			return bytes, percent, err
		},
		btrfs: inspectBtrfs,
	}
}

func (b *Builder) Build(ctx context.Context, cfg *config.Config, dbPath string) Report {
	now := b.now().UTC()
	r := Report{
		GeneratedAt:  now,
		BuildVersion: buildVersion(),
		DaemonStatus: "not_probed",
		Catalog:      CatalogReport{SupportedSchema: catalog.SchemaVersion()},
		Folders:      []FolderReport{},
		ActiveJobs:   []JobReport{},
		FailedJobs:   []JobReport{},
		RecentEvents: []SafeEvent{},
		Findings:     []Finding{},
		Limitations: []string{
			"daemon liveness/version is not probed; this report works without the HTTP listener",
			"Syncthing folder mode and peer health require the Syncthing API and are not checked",
			"storage and folder writability are not probed because status performs no writes; use --check and daemon preflight",
			"metadata extraction failures are unavailable in catalog schema 6",
			"btrfs device health is best-effort and may require privileges; verify unknown results with btrfs filesystem show",
		},
	}

	b.inspectStorage(ctx, cfg, &r)

	version, err := catalog.DatabaseSchemaVersion(dbPath)
	if err != nil {
		r.failure("catalog_unreadable", fmt.Sprintf("cannot read catalog schema: %v", err))
		b.addConfiguredFolders(cfg, &r)
		return r.finish()
	}
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			r.failure("catalog_missing", "catalog.db does not exist at the configured cold-storage path")
		} else {
			r.failure("catalog_unreadable", fmt.Sprintf("cannot stat catalog.db: %v", err))
		}
		b.addConfiguredFolders(cfg, &r)
		return r.finish()
	}
	r.Catalog.Exists = true
	r.Catalog.OnDiskSchema = version
	r.Catalog.SchemaCompatible = version == catalog.SchemaVersion()
	if !r.Catalog.SchemaCompatible {
		r.failure("schema_mismatch", fmt.Sprintf("catalog schema is %d; this binary supports %d; no schema-dependent queries were run",
			version, catalog.SchemaVersion()))
		b.addConfiguredFolders(cfg, &r)
		return r.finish()
	}

	cat, err := catalog.OpenReadOnly(dbPath)
	if err != nil {
		r.failure("catalog_unreadable", fmt.Sprintf("cannot open catalog read-only: %v", err))
		b.addConfiguredFolders(cfg, &r)
		return r.finish()
	}
	defer cat.Close()

	if err := b.inspectCatalog(cfg, cat, &r); err != nil {
		r.failure("catalog_query_failed", err.Error())
	}
	return r.finish()
}

func (b *Builder) inspectStorage(ctx context.Context, cfg *config.Config, r *Report) {
	path := cfg.Storage.ColdStoragePath
	r.Storage.Path = path
	r.Storage.Btrfs.Status = "unknown"
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			r.failure("storage_missing", fmt.Sprintf("cold storage %q does not exist", path))
		} else {
			r.failure("storage_unreadable", fmt.Sprintf("cannot stat cold storage %q: %v", path, err))
		}
		return
	}
	if !info.IsDir() {
		r.failure("storage_not_directory", fmt.Sprintf("cold storage %q is not a directory", path))
		return
	}
	r.Storage.Exists = true

	mount, err := b.describeMount(path)
	if err != nil {
		r.failure("mount_unknown", fmt.Sprintf("cannot inspect cold-storage mount: %v", err))
	} else {
		r.Storage.IsMountPoint = mount.IsMountPoint
		r.Storage.FSType = mount.FSType
		if cfg.Storage.RequireMountpoint && !mount.IsMountPoint {
			r.failure("mount_missing", "cold storage is not a mount point although require_mountpoint is enabled")
		} else if !mount.IsMountPoint {
			r.warning("mount_not_separate", "cold storage is not a separate mount; confirm this plain-directory layout is intentional")
		}
	}
	if free, percent, err := b.space(path); err != nil {
		r.warning("free_space_unknown", fmt.Sprintf("cannot inspect cold-storage free space: %v", err))
	} else {
		r.Storage.FreeBytes = free
		r.Storage.FreePercent = percent
		if percent < 5 {
			r.failure("cold_storage_low", fmt.Sprintf("cold storage has %.1f%% free space", percent))
		}
	}

	if r.Storage.FSType != "btrfs" {
		r.Storage.Btrfs = BtrfsReport{Status: "not_applicable"}
		return
	}
	r.Storage.Btrfs = b.btrfs(ctx, path)
	switch r.Storage.Btrfs.Status {
	case "degraded":
		r.failure("btrfs_degraded", "btrfs reports missing filesystem devices")
	case "unknown":
		r.warning("btrfs_unknown", "btrfs membership could not be verified without additional tool access or privileges")
	}
}

func (b *Builder) inspectCatalog(cfg *config.Config, cat *catalog.Catalog, r *Report) error {
	statuses := []struct {
		status catalog.FileStatus
		dst    *int64
	}{
		{"", &r.Files.Total},
		{catalog.StatusSynced, &r.Files.Synced},
		{catalog.StatusArchiving, &r.Files.Archiving},
		{catalog.StatusArchived, &r.Files.Archived},
		{catalog.StatusRestoring, &r.Files.Restoring},
		{catalog.StatusMissing, &r.Files.Missing},
	}
	for _, item := range statuses {
		count, err := cat.FileCount(item.status)
		if err != nil {
			return fmt.Errorf("counting files with status %q: %w", item.status, err)
		}
		*item.dst = count
	}
	if r.Files.Archiving > 0 || r.Files.Restoring > 0 {
		r.warning("transient_files", fmt.Sprintf("%d files remain in transient move states", r.Files.Archiving+r.Files.Restoring))
	}
	if r.Files.Missing > 0 {
		r.warning("missing_files", fmt.Sprintf("%d catalogued files are missing", r.Files.Missing))
	}

	configs, err := cat.ListFolderConfigs()
	if err != nil {
		return fmt.Errorf("listing folder configs: %w", err)
	}
	declared := make(map[string]config.FolderConfig, len(cfg.Folders))
	for _, folder := range cfg.Folders {
		declared[folder.Label] = folder
	}
	for _, fc := range configs {
		_, inConfig := declared[fc.FolderLabel]
		fr := FolderReport{
			Label: fc.FolderLabel, Path: fc.SyncthingPath, Declared: inConfig,
			Catalogued: true, Enabled: fc.Enabled, PresentInTOML: fc.PresentInTOML,
			BudgetBytes: fc.BudgetBytes, LastEnforcedAt: fc.LastEnforcedAt,
		}
		fr.Ready, fr.Readiness = inspectFolderReadOnly(fr.Path)
		if !fr.Ready {
			r.warning("folder_not_ready", fmt.Sprintf("folder %q is not ready: %s", fr.Label, fr.Readiness))
		}
		if !inConfig {
			r.warning("folder_not_declared", fmt.Sprintf("catalogued folder %q is absent from the current config", fr.Label))
		}
		onDevice, pinned, err := cat.FolderSizes(fc.FolderLabel)
		if err != nil {
			return fmt.Errorf("sizing folder %q: %w", fc.FolderLabel, err)
		}
		fr.OnDeviceBytes, fr.PinnedBytes = onDevice, pinned
		budget := catalog.FolderBudget{BudgetBytes: fc.BudgetBytes, TotalBytes: onDevice, PinnedBytes: pinned}
		fr.OverBudget, fr.Wedged = budget.OverBudget(), budget.Wedged()
		if fr.Wedged {
			r.failure("folder_wedged", fmt.Sprintf("folder %q is wedged: pinned bytes exceed its budget", fr.Label))
		} else if fr.OverBudget {
			r.warning("folder_over_budget", fmt.Sprintf("folder %q is over budget", fr.Label))
		}
		fr.Counts, err = cat.FolderCounts(fc.FolderLabel)
		if err != nil {
			return fmt.Errorf("counting folder %q: %w", fc.FolderLabel, err)
		}
		r.Folders = append(r.Folders, fr)
		delete(declared, fc.FolderLabel)
	}
	for _, folder := range declared {
		fr := configuredFolderReport(folder)
		r.Folders = append(r.Folders, fr)
		r.warning("folder_not_catalogued", fmt.Sprintf("configured folder %q has no catalog policy row", folder.Label))
		if !fr.Ready {
			r.warning("folder_not_ready", fmt.Sprintf("folder %q is not ready: %s", fr.Label, fr.Readiness))
		}
	}
	sort.Slice(r.Folders, func(i, j int) bool { return r.Folders[i].Label < r.Folders[j].Label })

	jobs, err := cat.ListJobs(50)
	if err != nil {
		return fmt.Errorf("listing jobs: %w", err)
	}
	for _, job := range jobs {
		jr := safeJob(job)
		switch job.State {
		case catalog.JobStatePending, catalog.JobStateRunning:
			r.ActiveJobs = append(r.ActiveJobs, jr)
			r.warning("active_job", fmt.Sprintf("job %d is %s", job.ID, job.State))
		case catalog.JobStateFailed:
			r.FailedJobs = append(r.FailedJobs, jr)
			r.warning("failed_job", fmt.Sprintf("job %d failed", job.ID))
		}
	}

	derived, err := cat.DerivedFailureCounts()
	if err != nil {
		return fmt.Errorf("counting derived-data failures: %w", err)
	}
	r.Derived.ThumbnailFailures = derived.ThumbnailFailures
	r.Derived.DetectionBacklog = derived.DetectionBacklog
	if derived.ThumbnailFailures > 0 {
		r.warning("thumbnail_failures", fmt.Sprintf("%d files have exhausted or recorded thumbnail failures", derived.ThumbnailFailures))
	}
	if derived.DetectionBacklog > 0 {
		r.warning("detection_backlog", fmt.Sprintf("%d files still need content-type detection", derived.DetectionBacklog))
	}

	if err := b.inspectEvents(cfg, cat, r); err != nil {
		return err
	}
	return nil
}

func (b *Builder) inspectEvents(cfg *config.Config, cat *catalog.Catalog, r *Report) error {
	scans, err := cat.ListEvents("", "scan_completed", "", 1)
	if err != nil {
		return fmt.Errorf("reading last scan: %w", err)
	}
	if len(scans) == 0 {
		r.warning("scan_never_completed", "no successful scan completion is recorded")
	} else {
		at := scans[0].Timestamp.UTC()
		r.LastScan = &at
		maxAge := time.Duration(cfg.Daemon.CatalogSyncIntervalMinutes*2) * time.Minute
		if maxAge <= 0 {
			maxAge = time.Hour
		}
		if b.now().Sub(at) > maxAge {
			r.warning("scan_stale", fmt.Sprintf("last successful scan is older than %s", maxAge))
		}
	}
	offloads, err := cat.ListEvents("", "archive", "", 1)
	if err != nil {
		return fmt.Errorf("reading last offload: %w", err)
	}
	if len(offloads) > 0 {
		at := offloads[0].Timestamp.UTC()
		r.LastOffload = &at
	}

	events, err := cat.ListEvents("", "", catalog.EventClassWarning, 50)
	if err != nil {
		return fmt.Errorf("reading recent warnings: %w", err)
	}
	cutoff := b.now().Add(-7 * 24 * time.Hour)
	for _, event := range events {
		if event.Timestamp.Before(cutoff) {
			continue
		}
		r.RecentEvents = append(r.RecentEvents, SafeEvent{
			Timestamp: event.Timestamp.UTC(), Type: event.EventType, Folder: event.Folder,
		})
		severity := SeverityWarning
		if event.EventType == "source_vanished" || event.EventType == "archive_unconfirmed" {
			severity = SeverityFailure
		}
		r.add(severity, "recent_"+event.EventType,
			fmt.Sprintf("recent %s event for folder %q", event.EventType, event.Folder))
	}
	return nil
}

func (b *Builder) addConfiguredFolders(cfg *config.Config, r *Report) {
	for _, folder := range cfg.Folders {
		fr := configuredFolderReport(folder)
		r.Folders = append(r.Folders, fr)
		if !fr.Ready {
			r.warning("folder_not_ready", fmt.Sprintf("folder %q is not ready: %s", fr.Label, fr.Readiness))
		}
	}
}

func configuredFolderReport(folder config.FolderConfig) FolderReport {
	ready, readiness := inspectFolderReadOnly(folder.Path)
	return FolderReport{
		Label: folder.Label, Path: folder.Path, Declared: true, Catalogued: false,
		Enabled: true, PresentInTOML: true, Ready: ready, Readiness: readiness,
		BudgetBytes: folder.BudgetBytes,
	}
}

func inspectFolderReadOnly(path string) (bool, string) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, "path does not exist"
	}
	if err != nil {
		return false, "path is not readable"
	}
	if !info.IsDir() {
		return false, "path is not a directory"
	}
	if _, err := os.Stat(filepath.Join(path, ".stfolder")); err != nil {
		return false, ".stfolder marker is missing"
	}
	dir, err := os.Open(path)
	if err != nil {
		return false, "folder root cannot be opened"
	}
	defer dir.Close()
	if _, err := dir.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return false, "folder root cannot be read"
	}
	return true, "ready"
}

func safeJob(job catalog.Job) JobReport {
	return JobReport{
		ID: job.ID, Action: job.Action, FolderLabel: job.FolderLabel, State: job.State,
		Total: job.Total, Done: job.Done, Failed: job.FailedCount, CreatedAt: job.CreatedAt,
	}
}

func (r *Report) add(severity Severity, code, message string) {
	r.Findings = append(r.Findings, Finding{Severity: severity, Code: code, Message: message})
}

func (r *Report) warning(code, message string) { r.add(SeverityWarning, code, message) }
func (r *Report) failure(code, message string) { r.add(SeverityFailure, code, message) }

func (r Report) finish() Report {
	r.Status = "healthy"
	r.ExitCode = ExitHealthy
	for _, finding := range r.Findings {
		if finding.Severity == SeverityFailure {
			r.Status, r.ExitCode = "failure", ExitFailure
			return r
		}
		if finding.Severity == SeverityWarning {
			r.Status, r.ExitCode = "warning", ExitWarning
		}
	}
	return r
}

func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	revision := ""
	dirty := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if revision == "" {
		return "development"
	}
	if dirty {
		return revision + "-dirty"
	}
	return revision
}
