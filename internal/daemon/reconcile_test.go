package daemon

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

func openCatalog(t *testing.T) (*catalog.Catalog, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	return cat, dbPath
}

// countEvents reads the events table directly. The catalog has no list-events
// method yet, and the point of this assertion is that restarts do not pile up
// rows there.
func countEvents(t *testing.T, dbPath, eventType string) int {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE event_type = ?`, eventType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func tomlConfig(folders ...config.FolderConfig) *config.Config {
	return &config.Config{
		Daemon:  config.DaemonConfig{GracePeriodMinutes: 45},
		Folders: folders,
	}
}

func TestReconcileFoldersShouldSeedPolicyWhenTheFolderIsFirstDeclared(t *testing.T) {
	cat, _ := openCatalog(t)
	cfg := tomlConfig(config.FolderConfig{
		Label: "Photos", SyncthingID: "abcd-1234", Path: "/srv/sync/Photos",
		BudgetBytes: 20000, Persistent: []string{"Starred/*", "Favourites/*"},
	})

	if err := ReconcileFolders(cfg, cat, testLogger()); err != nil {
		t.Fatal(err)
	}

	fc, err := cat.FolderConfigByLabel("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if fc.BudgetBytes != 20000 {
		t.Errorf("budget_bytes = %d, want the TOML seed 20000", fc.BudgetBytes)
	}
	if fc.GraceMinutes != 45 {
		t.Errorf("grace_minutes = %d, want the [daemon] seed 45", fc.GraceMinutes)
	}
	if fc.SyncthingID != "abcd-1234" || fc.SyncthingPath != "/srv/sync/Photos" {
		t.Errorf("infrastructure = (%q, %q), want the TOML values", fc.SyncthingID, fc.SyncthingPath)
	}
	if !fc.Enabled || !fc.PresentInTOML {
		t.Errorf("enabled = %v, present_in_toml = %v, want both true", fc.Enabled, fc.PresentInTOML)
	}

	globs, err := cat.ListPersistentPaths("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if len(globs) != 2 {
		t.Errorf("seeded %d persistent globs, want 2", len(globs))
	}
}

func TestReconcileFoldersShouldKeepTheEditedPolicyWhenTheSameTOMLIsReadAgain(t *testing.T) {
	cat, _ := openCatalog(t)
	cfg := tomlConfig(config.FolderConfig{
		Label: "Photos", SyncthingID: "abcd-1234", Path: "/srv/sync/Photos", BudgetBytes: 20000,
	})

	if err := ReconcileFolders(cfg, cat, testLogger()); err != nil {
		t.Fatal(err)
	}

	// The owner edits policy in the UI.
	budget := int64(999)
	grace := 5
	enabled := false
	if found, err := cat.UpdateFolderPolicy("Photos", &budget, &grace, &enabled); err != nil || !found {
		t.Fatalf("UpdateFolderPolicy = %v, %v", found, err)
	}

	// A restart: same TOML, and TOML has since been edited to move the folder.
	cfg.Folders[0].Path = "/srv/sync/Pictures"
	cfg.Folders[0].SyncthingID = "efgh-5678"
	if err := ReconcileFolders(cfg, cat, testLogger()); err != nil {
		t.Fatal(err)
	}

	fc, err := cat.FolderConfigByLabel("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if fc.BudgetBytes != 999 {
		t.Errorf("budget_bytes = %d, want 999: TOML seeds a budget once and never again", fc.BudgetBytes)
	}
	if fc.GraceMinutes != 5 {
		t.Errorf("grace_minutes = %d, want 5", fc.GraceMinutes)
	}
	if fc.Enabled {
		t.Error("enabled = true, want the user's false")
	}
	if fc.SyncthingPath != "/srv/sync/Pictures" || fc.SyncthingID != "efgh-5678" {
		t.Errorf("infrastructure = (%q, %q), want it refreshed from TOML",
			fc.SyncthingID, fc.SyncthingPath)
	}
}

func TestReconcileFoldersShouldNotReseedPersistentGlobsWhenTheUserHasDeletedThem(t *testing.T) {
	cat, _ := openCatalog(t)
	cfg := tomlConfig(config.FolderConfig{
		Label: "Photos", Path: "/srv/sync/Photos", BudgetBytes: 20000,
		Persistent: []string{"Starred/*"},
	})

	if err := ReconcileFolders(cfg, cat, testLogger()); err != nil {
		t.Fatal(err)
	}
	globs, err := cat.ListPersistentPaths("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.DeletePersistentPath(globs[0].ID); err != nil {
		t.Fatal(err)
	}

	if err := ReconcileFolders(cfg, cat, testLogger()); err != nil {
		t.Fatal(err)
	}

	globs, err = cat.ListPersistentPaths("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if len(globs) != 0 {
		t.Errorf("persistent globs = %v, want none: a glob deleted in the UI must stay deleted", globs)
	}
}

func TestReconcileFoldersShouldRetainThePolicyWhenAFolderDisappearsFromTOML(t *testing.T) {
	cat, _ := openCatalog(t)
	cfg := tomlConfig(
		config.FolderConfig{Label: "Photos", Path: "/srv/sync/Photos", BudgetBytes: 20000},
		config.FolderConfig{Label: "Docs", Path: "/srv/sync/Docs", BudgetBytes: 10000},
	)
	if err := ReconcileFolders(cfg, cat, testLogger()); err != nil {
		t.Fatal(err)
	}
	budget := int64(4242)
	if _, err := cat.UpdateFolderPolicy("Docs", &budget, nil, nil); err != nil {
		t.Fatal(err)
	}

	// Docs is deleted from coldstore.toml.
	cfg.Folders = cfg.Folders[:1]
	if err := ReconcileFolders(cfg, cat, testLogger()); err != nil {
		t.Fatal(err)
	}

	docs, err := cat.FolderConfigByLabel("Docs")
	if err != nil {
		t.Fatalf("the row must survive: %v", err)
	}
	if docs.PresentInTOML {
		t.Error("present_in_toml = true, want false")
	}
	if docs.BudgetBytes != 4242 {
		t.Errorf("budget_bytes = %d, want the retained 4242", docs.BudgetBytes)
	}

	active, err := cat.ActiveFolderConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].FolderLabel != "Photos" {
		t.Errorf("active folders = %+v, want only Photos", active)
	}

	// Its files stay resolvable, which is what keeps them browsable.
	if path, err := cat.FolderPath("Docs"); err != nil || path != "/srv/sync/Docs" {
		t.Errorf("FolderPath(Docs) = %q, %v, want the retained path", path, err)
	}

	// Re-declaring it brings it back with the owner's tuning intact.
	cfg.Folders = append(cfg.Folders,
		config.FolderConfig{Label: "Docs", Path: "/srv/sync/Docs", BudgetBytes: 10000})
	if err := ReconcileFolders(cfg, cat, testLogger()); err != nil {
		t.Fatal(err)
	}
	docs, err = cat.FolderConfigByLabel("Docs")
	if err != nil {
		t.Fatal(err)
	}
	if !docs.PresentInTOML || docs.BudgetBytes != 4242 {
		t.Errorf("redeclared Docs = (present %v, budget %d), want (true, 4242)",
			docs.PresentInTOML, docs.BudgetBytes)
	}
}

func TestReconcileFoldersShouldWriteOneFolderSeededEventWhenAFolderIsNew(t *testing.T) {
	cat, dbPath := openCatalog(t)
	cfg := tomlConfig(config.FolderConfig{Label: "Photos", Path: "/srv/sync/Photos", BudgetBytes: 20000})

	for i := 0; i < 3; i++ {
		if err := ReconcileFolders(cfg, cat, testLogger()); err != nil {
			t.Fatal(err)
		}
	}

	if seeded := countEvents(t, dbPath, "folder_seeded"); seeded != 1 {
		t.Errorf("folder_seeded events = %d, want exactly 1 across three startups", seeded)
	}
}

func TestReconcileFoldersShouldRefuseToStartWhenADeclarationIsUnusable(t *testing.T) {
	cases := []struct {
		name   string
		folder config.FolderConfig
		want   string
	}{
		{"no label", config.FolderConfig{Path: "/srv/sync/x", BudgetBytes: 1}, "missing label or path"},
		{"no path", config.FolderConfig{Label: "X", BudgetBytes: 1}, "missing label or path"},
		{"zero budget", config.FolderConfig{Label: "X", Path: "/srv/sync/x"}, "non-positive budget"},
		{"negative budget", config.FolderConfig{Label: "X", Path: "/srv/sync/x", BudgetBytes: -1}, "non-positive budget"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cat, _ := openCatalog(t)
			err := ReconcileFolders(tomlConfig(tc.folder), cat, testLogger())
			if err == nil {
				t.Fatal("want an error: a daemon that cannot establish its policy table must not start")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			all, err := cat.ListFolderConfigs()
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != 0 {
				t.Errorf("wrote %d folder rows despite the error, want 0", len(all))
			}
		})
	}
}

func TestReconcileFoldersShouldRejectALabelRenameBeforeWritingAnything(t *testing.T) {
	cat, dbPath := openCatalog(t)
	if err := ReconcileFolders(tomlConfig(config.FolderConfig{
		Label: "Photos", Path: "/srv/sync/Photos", BudgetBytes: 20_000,
	}), cat, testLogger()); err != nil {
		t.Fatal(err)
	}
	seededEvents := countEvents(t, dbPath, "folder_seeded")

	// The first declaration would be a valid insert if reconciliation reached
	// its write loop. The second is an attempted cosmetic rename of Photos.
	err := ReconcileFolders(tomlConfig(
		config.FolderConfig{Label: "Docs", Path: "/srv/sync/Docs", BudgetBytes: 10_000},
		config.FolderConfig{Label: "Pictures", Path: "/srv/sync/Photos", BudgetBytes: 20_000},
	), cat, testLogger())
	if err == nil {
		t.Fatal("renaming Photos to Pictures while retaining its path must be rejected")
	}
	for _, want := range []string{"Pictures", "/srv/sync/Photos", "Photos", "permanent", "display_name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want recovery detail %q", err, want)
		}
	}

	all, err := cat.ListFolderConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].FolderLabel != "Photos" || !all[0].PresentInTOML {
		t.Errorf("folder rows after refusal = %+v, want the untouched Photos row only", all)
	}
	if got := countEvents(t, dbPath, "folder_seeded"); got != seededEvents {
		t.Errorf("folder_seeded events = %d, want unchanged %d", got, seededEvents)
	}
}

func TestReconcileFoldersShouldRejectSwappedLabels(t *testing.T) {
	cat, _ := openCatalog(t)
	if err := ReconcileFolders(tomlConfig(
		config.FolderConfig{Label: "Photos", Path: "/srv/sync/Photos", BudgetBytes: 20_000},
		config.FolderConfig{Label: "Docs", Path: "/srv/sync/Docs", BudgetBytes: 10_000},
	), cat, testLogger()); err != nil {
		t.Fatal(err)
	}

	err := ReconcileFolders(tomlConfig(
		config.FolderConfig{Label: "Photos", Path: "/srv/sync/Docs", BudgetBytes: 20_000},
		config.FolderConfig{Label: "Docs", Path: "/srv/sync/Photos", BudgetBytes: 10_000},
	), cat, testLogger())
	if err == nil || !strings.Contains(err.Error(), "already catalogued under label \"Docs\"") {
		t.Fatalf("swapped label reconciliation error = %v, want the Docs ownership conflict", err)
	}
}

func TestReconcileFoldersShouldCompareCleanedPaths(t *testing.T) {
	cat, _ := openCatalog(t)
	if err := ReconcileFolders(tomlConfig(config.FolderConfig{
		Label: "Photos", Path: "/srv/sync/Photos", BudgetBytes: 20_000,
	}), cat, testLogger()); err != nil {
		t.Fatal(err)
	}

	err := ReconcileFolders(tomlConfig(config.FolderConfig{
		Label: "Pictures", Path: "/srv/sync/Albums/../Photos/", BudgetBytes: 20_000,
	}), cat, testLogger())
	if err == nil {
		t.Fatal("a cleaned path with a trailing slash bypassed the permanent-label guard")
	}
	if !strings.Contains(err.Error(), `path "/srv/sync/Photos"`) {
		t.Errorf("error = %q, want the cleaned path", err)
	}
}
