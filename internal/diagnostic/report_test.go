package diagnostic

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

var reportNow = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

type reportFixture struct {
	cfg     *config.Config
	dbPath  string
	cat     *catalog.Catalog
	builder *Builder
	folder  string
	cold    string
}

func newReportFixture(t *testing.T) *reportFixture {
	t.Helper()
	cold := t.TempDir()
	folder := filepath.Join(t.TempDir(), "photos")
	if err := os.MkdirAll(filepath.Join(folder, ".stfolder"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Storage: config.StorageConfig{ColdStoragePath: cold, RequireMountpoint: true},
		Daemon:  config.DaemonConfig{CatalogSyncIntervalMinutes: 30},
		Server: config.ServerConfig{
			Username: "server-user-secret", Password: "server-password-secret",
			TailscaleAllowedUsers: []string{"private-tailscale-login"},
		},
		Syncthing: config.SyncthingConfig{APIURL: "http://private-device:8384", APIKey: "api-key-secret"},
		Folders: []config.FolderConfig{{
			Label: "Photos", SyncthingID: "syncthing-device-folder-id", Path: folder,
			Budget: "100B", BudgetBytes: 100,
		}},
	}
	dbPath := filepath.Join(cold, "catalog.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	if _, err := cat.ReconcileFolderFromTOML("Photos", "catalog-device-id", folder, 100, 30); err != nil {
		t.Fatal(err)
	}
	builder := NewBuilder()
	builder.now = func() time.Time { return reportNow }
	builder.describeMount = func(string) (config.MountState, error) {
		return config.MountState{IsMountPoint: true, FSType: "ext2/3/4"}, nil
	}
	builder.space = func(string) (uint64, float64, error) { return 1_000_000, 50, nil }
	return &reportFixture{cfg: cfg, dbPath: dbPath, cat: cat, builder: builder, folder: folder, cold: cold}
}

func (f *reportFixture) closeCatalog(t *testing.T) {
	t.Helper()
	if f.cat != nil {
		if err := f.cat.Close(); err != nil {
			t.Fatal(err)
		}
		f.cat = nil
	}
}

func hasFinding(r Report, code string) bool {
	for _, finding := range r.Findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func TestReportShouldBeHealthyFromACompleteCurrentCatalog(t *testing.T) {
	f := newReportFixture(t)
	if err := f.cat.LogEvent("scan_completed", "Photos", "", "complete"); err != nil {
		t.Fatal(err)
	}
	f.closeCatalog(t)

	r := f.builder.Build(context.Background(), f.cfg, f.dbPath)
	if r.ExitCode != ExitHealthy || r.Status != "healthy" {
		t.Fatalf("status = %s/%d, findings = %+v", r.Status, r.ExitCode, r.Findings)
	}
	if !r.Catalog.SchemaCompatible || len(r.Folders) != 1 || !r.Folders[0].Ready {
		t.Errorf("healthy report lost catalog/folder state: %+v", r)
	}
}

func TestReportShouldFailForMissingStorageAndCatalogWithoutCreatingEither(t *testing.T) {
	root := t.TempDir()
	cold := filepath.Join(root, "unmounted")
	cfg := &config.Config{
		Storage: config.StorageConfig{ColdStoragePath: cold, RequireMountpoint: true},
		Daemon:  config.DaemonConfig{CatalogSyncIntervalMinutes: 30},
		Folders: []config.FolderConfig{{Label: "Photos", Path: filepath.Join(root, "photos"), BudgetBytes: 100}},
	}
	r := NewBuilder().Build(context.Background(), cfg, filepath.Join(cold, "catalog.db"))
	if r.ExitCode != ExitFailure || !hasFinding(r, "storage_missing") || !hasFinding(r, "catalog_missing") {
		t.Fatalf("missing archive report = %+v", r)
	}
	if _, err := os.Stat(cold); !os.IsNotExist(err) {
		t.Errorf("status created missing cold-storage path: %v", err)
	}
}

func TestReportShouldFailWhenBtrfsReportsMissingDevices(t *testing.T) {
	f := newReportFixture(t)
	if err := f.cat.LogEvent("scan_completed", "Photos", "", "complete"); err != nil {
		t.Fatal(err)
	}
	f.closeCatalog(t)
	f.builder.describeMount = func(string) (config.MountState, error) {
		return config.MountState{IsMountPoint: true, FSType: "btrfs"}, nil
	}
	f.builder.btrfs = func(context.Context, string) BtrfsReport {
		return BtrfsReport{Status: "degraded", Detail: "one or more filesystem devices are missing"}
	}
	r := f.builder.Build(context.Background(), f.cfg, f.dbPath)
	if r.ExitCode != ExitFailure || !hasFinding(r, "btrfs_degraded") {
		t.Fatalf("degraded btrfs report = %+v", r)
	}
}

func TestReportShouldFailWhenRequiredMountIsOnlyAPlainDirectory(t *testing.T) {
	f := newReportFixture(t)
	if err := f.cat.LogEvent("scan_completed", "Photos", "", "complete"); err != nil {
		t.Fatal(err)
	}
	f.closeCatalog(t)
	f.builder.describeMount = func(string) (config.MountState, error) {
		return config.MountState{IsMountPoint: false, FSType: "ext2/3/4"}, nil
	}
	r := f.builder.Build(context.Background(), f.cfg, f.dbPath)
	if r.ExitCode != ExitFailure || !hasFinding(r, "mount_missing") {
		t.Fatalf("required-mount report = %+v", r)
	}
}

func TestReportShouldLeaveANewerSchemaByteForByteUnchanged(t *testing.T) {
	f := newReportFixture(t)
	f.closeCatalog(t)
	raw, err := sql.Open("sqlite3", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}

	r := f.builder.Build(context.Background(), f.cfg, f.dbPath)
	after, err := os.ReadFile(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if r.ExitCode != ExitFailure || !hasFinding(r, "schema_mismatch") || r.Catalog.OnDiskSchema != 99 {
		t.Fatalf("newer-schema report = %+v", r)
	}
	if !bytes.Equal(before, after) {
		t.Error("status changed a newer catalog")
	}
}

func TestReportShouldWarnWhenFolderMarkerIsMissing(t *testing.T) {
	f := newReportFixture(t)
	if err := os.Remove(filepath.Join(f.folder, ".stfolder")); err != nil {
		t.Fatal(err)
	}
	if err := f.cat.LogEvent("scan_completed", "Photos", "", "complete"); err != nil {
		t.Fatal(err)
	}
	f.closeCatalog(t)
	r := f.builder.Build(context.Background(), f.cfg, f.dbPath)
	if r.ExitCode != ExitWarning || !hasFinding(r, "folder_not_ready") || r.Folders[0].Ready {
		t.Fatalf("missing-marker report = %+v", r)
	}
}

func TestReportShouldSurfaceStaleScanWedgeJobsFailuresAndSafeEvents(t *testing.T) {
	f := newReportFixture(t)
	now := reportNow.Add(-2 * time.Hour)
	rec := &catalog.FileRecord{
		FolderLabel: "Photos", RelativePath: "secret-file.jpg", Filename: "secret-file.jpg",
		Extension: ".jpg", SizeBytes: 200, FileMtime: now, Status: catalog.StatusSynced,
	}
	if _, err := f.cat.UpsertFile(rec, now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	stored, err := f.cat.GetFile("Photos", "secret-file.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.cat.SetPinned(stored.ID, true, catalog.PinSourceUser, now.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := f.cat.SetDetectedType(stored.ID, "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	if err := f.cat.RecordThumbnailFailure(stored.ID, "thumbnail-secret-detail", now.Unix()); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite3", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE files SET meta_attempts = 3,
		meta_error = 'metadata-secret-detail' WHERE id = ?`, stored.ID); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	active, err := f.cat.CreateJob(catalog.JobActionScan, "Photos", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.cat.MarkJobRunning(active, now.Unix()); err != nil {
		t.Fatal(err)
	}
	failed, err := f.cat.CreateJob(catalog.JobActionScan, "Photos", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.cat.FailJob(failed, now.Unix(), "job-secret-detail"); err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct{ typ, details string }{
		{"scan_completed", "scan secret"},
		{"scan_failed", "scan failure secret"},
		{"archive", "offload secret"},
		{"conflict", "conflict secret"},
		{"source_vanished", "data safety secret"},
	} {
		if err := f.cat.LogEvent(event.typ, "Photos", "private/path/"+event.typ, event.details); err != nil {
			t.Fatal(err)
		}
	}
	f.closeCatalog(t)
	f.builder.now = func() time.Time { return time.Now().UTC().Add(2 * time.Hour) }

	r := f.builder.Build(context.Background(), f.cfg, f.dbPath)
	for _, code := range []string{"folder_wedged", "active_job", "failed_job", "thumbnail_failures", "metadata_failures", "scan_stale", "recent_scan_failed", "recent_conflict", "recent_source_vanished"} {
		if !hasFinding(r, code) {
			t.Errorf("report missing finding %q: %+v", code, r.Findings)
		}
	}
	if r.ExitCode != ExitFailure || r.LastScan == nil || r.LastOffload == nil {
		t.Errorf("combined unhealthy report status/timestamps = %+v", r)
	}

	var human, structured bytes.Buffer
	if err := WriteHuman(&human, r); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(&structured, r); err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(structured.Bytes(), &decoded); err != nil {
		t.Fatalf("JSON output is not structured: %v\n%s", err, structured.String())
	}
	combined := human.String() + structured.String()
	for _, secret := range []string{
		"server-user-secret", "server-password-secret", "private-tailscale-login", "api-key-secret", "private-device",
		"syncthing-device-folder-id", "catalog-device-id", "thumbnail-secret-detail",
		"metadata-secret-detail", "job-secret-detail", "scan failure secret", "conflict secret",
		"private/path", "data safety secret",
	} {
		if strings.Contains(combined, secret) {
			t.Errorf("status output leaked %q:\n%s", secret, combined)
		}
	}
}
