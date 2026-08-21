// seed-demo builds a complete, self-contained coldstore installation under one
// directory: a config file, four Syncthing-shaped folders on disk, a catalog
// with every state the control plane can render, and the thumbnails to go with
// it.
//
//	<root>/coldstore.toml   the config the daemon is started with
//	<root>/store/           cold storage: archive/, thumbnails/, catalog.db
//	<root>/sync/<Label>/    the managed folders, each with a .stfolder marker
//
// "Every state" is the point. A demo that shows only synced and archived files
// leaves the wedge banner, the pin badges, the disabled and orphaned folder
// rows, the conflict resolver and the thumbnail-failure glyph unreachable --
// so the UI that renders them is never once looked at before it ships, and the
// Playwright suite has nothing to assert against.
//
//	go run ./scripts/seed-demo                       # /tmp/coldstore-demo
//	go run ./scripts/seed-demo -root /tmp/x -addr 127.0.0.1:8099
//	./coldstore --config /tmp/coldstore-demo/coldstore.toml
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/daemon"
	"github.com/tjmisko/coldstore/internal/thumbnail"
)

// markerFile is what makes `-root` safe to wipe. seed-demo deletes its root and
// starts over on every run; without a marker, a mistyped -root pointed at a
// real directory would take the owner's files with it. Only a directory this
// program created -- or one that does not exist yet -- is ever removed.
const markerFile = ".coldstore-demo"

// storeDir is cold storage: the archive, the thumbnails and catalog.db.
func storeDir(root string) string { return filepath.Join(root, "store") }

const (
	folderCameraRoll = "CameraRoll"
	folderBackups    = "Backups"
	folderDocuments  = "Documents"
	folderOldPhone   = "OldPhone"
)

var logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

func main() {
	root := flag.String("root", "/tmp/coldstore-demo", "directory to build the demo installation in")
	addr := flag.String("addr", config.DefaultListenAddr, "listen_addr to write into the demo config")
	flag.Parse()

	if err := run(*root, *addr); err != nil {
		logger.Error("seeding failed", "error", err)
		os.Exit(1)
	}
}

func run(root, addr string) error {
	if err := resetRoot(root); err != nil {
		return err
	}

	configPath := filepath.Join(root, "coldstore.toml")
	if err := os.WriteFile(configPath, []byte(demoConfig(root, addr)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", configPath, err)
	}

	// Load what we just wrote rather than building a Config in memory: a demo
	// config the daemon would reject is worse than no demo at all, and this is
	// the same parser and the same validation the daemon runs.
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("the generated config does not load: %w", err)
	}

	for _, f := range cfg.Folders {
		if err := makeSyncthingFolder(f.Path); err != nil {
			return err
		}
	}
	// OldPhone is deliberately absent from the config; its directory still
	// exists so its files remain browsable and downloadable (design 3.4).
	oldPhonePath := filepath.Join(root, "sync", folderOldPhone)
	if err := makeSyncthingFolder(oldPhonePath); err != nil {
		return err
	}

	cat, err := catalog.Open(filepath.Join(storeDir(root), "catalog.db"))
	if err != nil {
		return fmt.Errorf("opening catalog: %w", err)
	}
	defer cat.Close()

	// The real reconciler, so folder_configs is seeded exactly as a first
	// daemon start would seed it -- including the persistent globs and the
	// folder_seeded events.
	if err := daemon.ReconcileFolders(cfg, cat, logger); err != nil {
		return fmt.Errorf("reconciling demo folders: %w", err)
	}
	if err := seedOrphanedFolder(cat, cfg, oldPhonePath); err != nil {
		return err
	}

	camera, err := seedCameraRoll(cat, root)
	if err != nil {
		return err
	}
	if err := seedBackups(cat, root); err != nil {
		return err
	}
	if err := seedDocuments(cat, root); err != nil {
		return err
	}
	if err := seedOldPhoneFiles(cat, root); err != nil {
		return err
	}
	if err := applyPolicy(cat); err != nil {
		return err
	}
	if err := generateThumbnails(cat, root, camera); err != nil {
		return err
	}
	if err := seedEvents(cat); err != nil {
		return err
	}

	logger.Info("demo installation ready",
		"config", configPath,
		"run", "./coldstore --config "+configPath,
		"url", "http://"+addr)
	return nil
}

// resetRoot makes the seeder idempotent without making it dangerous.
func resetRoot(root string) error {
	info, err := os.Stat(root)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return fmt.Errorf("stat %s: %w", root, err)
	case !info.IsDir():
		return fmt.Errorf("%s is not a directory", root)
	default:
		if _, err := os.Stat(filepath.Join(root, markerFile)); err != nil {
			return fmt.Errorf("%s exists and was not created by seed-demo "+
				"(no %s marker); refusing to delete it", root, markerFile)
		}
		if err := os.RemoveAll(root); err != nil {
			return fmt.Errorf("clearing %s: %w", root, err)
		}
	}

	// The cold store lives in a subdirectory, never at the root: config.Validate
	// refuses a managed folder nested inside cold_storage_path, because
	// coldstore would then archive its own archive.
	for _, dir := range []string{root, storeDir(root),
		filepath.Join(storeDir(root), "archive"), filepath.Join(storeDir(root), "thumbnails")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	return os.WriteFile(filepath.Join(root, markerFile), []byte("seed-demo\n"), 0o644)
}

// makeSyncthingFolder creates a folder root WITH the .stfolder marker.
// FolderReady refuses to move anything into or out of a folder without it, so
// a demo folder missing the marker can neither archive nor restore.
func makeSyncthingFolder(path string) error {
	return os.MkdirAll(filepath.Join(path, ".stfolder"), 0o755)
}

func demoConfig(root, addr string) string {
	return fmt.Sprintf(`# Generated by scripts/seed-demo. Everything here lives under %[1]s.
[storage]
cold_storage_path = "%[1]s/store"

[daemon]
# One minute, not the shipped default of fifteen. Quota enforcement runs on the
# scan ticker, and enforcement is what stamps last_enforced_at -- at the real
# interval a demo would read "checked never" under every folder for the whole
# session. Nothing here moves on a tick: CameraRoll is under budget, Backups is
# wedged with no unpinned candidate, Documents is disabled and OldPhone is not
# declared, so the state a demo or a test set up is stable across ticks.
scan_interval_minutes = 1
catalog_sync_interval_minutes = 5
thumbnail_workers = 2
grace_period_minutes = 30

[server]
# Loopback, and no credentials: with basic auth on, the browser's own dialog
# intercepts the 401 the UI wants to render (design 7.10, dev-mode note).
listen_addr = %[2]q

[[folders]]
label = %[3]q
syncthing_id = "demo-camera"
path = "%[1]s/sync/%[3]s"
budget = "200kB"
persistent = ["Favorites/"]

[[folders]]
label = %[4]q
syncthing_id = "demo-backups"
path = "%[1]s/sync/%[4]s"
budget = "20kB"
persistent = []

[[folders]]
label = %[5]q
syncthing_id = "demo-docs"
path = "%[1]s/sync/%[5]s"
budget = "100kB"
persistent = ["Important/"]

# NOTE: %[6]q is deliberately NOT declared here. seed-demo puts it in the
# catalog and marks it absent from the TOML, which is the "folder you removed
# from coldstore.toml" state: greyed in the sidebar, never scanned or enforced,
# still browsable.
`, root, addr, folderCameraRoll, folderBackups, folderDocuments, folderOldPhone)
}

// seedOrphanedFolder creates a folder row and then marks it absent, which is
// exactly the sequence a real deployment goes through when a [[folders]] block
// is deleted from coldstore.toml.
func seedOrphanedFolder(cat *catalog.Catalog, cfg *config.Config, path string) error {
	if _, err := cat.ReconcileFolderFromTOML(folderOldPhone, "demo-oldphone", path, 50_000, 30); err != nil {
		return fmt.Errorf("seeding %s: %w", folderOldPhone, err)
	}
	declared := make([]string, 0, len(cfg.Folders))
	for _, f := range cfg.Folders {
		declared = append(declared, f.Label)
	}
	if err := cat.MarkFoldersAbsentFromTOML(declared); err != nil {
		return fmt.Errorf("marking %s absent: %w", folderOldPhone, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// file seeding

type seeded struct {
	id   int64
	name string
	path string
}

// seedFile writes a file, catalogues it as synced, and returns the row id.
func seedFile(cat *catalog.Catalog, folder, dir, relPath string, mtime time.Time,
	write func(path string) error) (seeded, error) {

	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return seeded{}, err
	}
	if err := write(full); err != nil {
		return seeded{}, fmt.Errorf("writing %s: %w", full, err)
	}
	if err := os.Chtimes(full, mtime, mtime); err != nil {
		return seeded{}, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return seeded{}, err
	}

	rec := catalog.FileRecord{
		FolderLabel:  folder,
		RelativePath: relPath,
		Filename:     filepath.Base(relPath),
		Extension:    filepath.Ext(relPath),
		MimeType:     mimeFor(filepath.Ext(relPath)),
		SizeBytes:    info.Size(),
		FileMtime:    mtime,
		FileAtime:    mtime,
		Status:       catalog.StatusSynced,
	}
	if _, err := cat.UpsertFile(&rec, time.Now().UnixNano()); err != nil {
		return seeded{}, fmt.Errorf("cataloguing %s: %w", relPath, err)
	}
	saved, err := cat.GetFile(folder, relPath)
	if err != nil {
		return seeded{}, err
	}
	return seeded{id: saved.ID, name: relPath, path: full}, nil
}

func mimeFor(ext string) string {
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".pdf":
		return "application/pdf"
	case ".txt", ".md":
		return "text/plain"
	case ".csv":
		return "text/csv"
	default:
		return "application/octet-stream"
	}
}

// archiveRow performs the filesystem half of an offload as well as the catalog
// half, so a restore in the demo really has bytes to move back.
func archiveRow(cat *catalog.Catalog, root, folder string, f seeded) error {
	dst := filepath.Join(storeDir(root), "archive", folder, f.name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(f.path, dst); err != nil {
		return fmt.Errorf("archiving %s: %w", f.name, err)
	}
	return cat.UpdateFileStatus(f.id, catalog.StatusArchived)
}

func seedCameraRoll(cat *catalog.Catalog, root string) ([]seeded, error) {
	dir := filepath.Join(root, "sync", folderCameraRoll)
	rng := rand.New(rand.NewSource(42))
	base := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)

	names := []string{
		"IMG_20260105_143022.jpg", "IMG_20260112_091015.jpg", "IMG_20260205_170833.jpg",
		"IMG_20260214_083201.jpg", "IMG_20260301_112445.jpg", "IMG_20260315_194522.jpg",
		"IMG_20260328_080155.jpg", "IMG_20260410_153311.jpg", "IMG_20260425_064730.jpg",
		"IMG_20260502_121900.jpg", "IMG_20260518_175244.jpg", "IMG_20260601_090322.jpg",
		"IMG_20260615_142011.jpg", "IMG_20260620_083544.jpg", "IMG_20260701_110233.jpg",
		"concert_night.jpg", "hiking_trail.jpg", "birthday_cake.jpg", "city_skyline.jpg",
		"garden_flowers.jpg", "snow_mountain.jpg", "lake_sunrise.jpg", "autumn_leaves.jpg",
		"family_dinner.jpg", "sunset_beach.jpg",
		"Favorites/wedding_first_dance.jpg", "Favorites/newborn_hands.jpg",
		"Favorites/graduation.jpg",
		"raw_untouched.cr2", "screen_recording.mkv",
	}

	files := make([]seeded, 0, len(names))
	for i, name := range names {
		mtime := base.Add(time.Duration(i) * 5 * 24 * time.Hour)
		f, err := seedFile(cat, folderCameraRoll, dir, name, mtime, func(path string) error {
			if ext := filepath.Ext(path); ext != ".jpg" && ext != ".png" {
				// Deliberately not an image: these are the rows the thumbnail
				// pipeline gives up on, and the tile that renders the failure
				// glyph.
				return os.WriteFile(path, []byte("not an image: "+filepath.Base(path)), 0o644)
			}
			return writeColoredJPEG(path, 48+rng.Intn(48), rng)
		})
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}

	// Eight archived, two missing, in the oldest slice of the roll.
	for _, f := range files[:8] {
		if err := archiveRow(cat, root, folderCameraRoll, f); err != nil {
			return nil, err
		}
	}
	for _, f := range files[8:10] {
		if err := os.Remove(f.path); err != nil {
			return nil, err
		}
		if err := cat.UpdateFileStatus(f.id, catalog.StatusMissing); err != nil {
			return nil, err
		}
	}

	// Pins, both kinds. The three Favorites are what an owner pins on purpose;
	// the restore pins are the ones a restore applied silently months ago and
	// that the wedge banner exists to send them back to.
	now := time.Now().Unix()
	for i, f := range files[25:28] {
		if err := cat.SetPinned(f.id, true, catalog.PinSourceUser, now-int64(i)*86400); err != nil {
			return nil, err
		}
	}
	for i, f := range files[10:14] {
		// Staggered and old, so sort=pinned_at has something to order.
		if err := cat.SetPinned(f.id, true, catalog.PinSourceRestore, now-int64(200-i*7)*86400); err != nil {
			return nil, err
		}
	}

	// Thumbnail failures, at the attempt ceiling. The ceiling is what stops the
	// daemon retrying them: without it the two non-image files below would
	// make every thumbnail pass shell out to ffmpeg and log its entire build
	// configuration on each failure, which buries the demo log.
	failed := append(append([]seeded{}, files[22:25]...), files[len(files)-2:]...)
	for _, f := range failed {
		for attempt := 0; attempt < 3; attempt++ {
			if err := cat.RecordThumbnailFailure(f.id,
				"ffmpeg: Invalid data found when processing input", now); err != nil {
				return nil, err
			}
		}
	}

	// One conflict: an archived row whose path is occupied again. The daemon's
	// first scan re-reports it; the event is seeded so the Conflicts tab is
	// populated before the daemon has ever run.
	conflicted := files[0]
	body := []byte("the copy the phone brought back -- larger than the archived one\n")
	if err := os.WriteFile(conflicted.path, body, 0o644); err != nil {
		return nil, err
	}
	if err := cat.LogEvent("conflict", folderCameraRoll, conflicted.name, fmt.Sprintf(
		"a file has reappeared at the path of an archived record: %d bytes on the device",
		len(body))); err != nil {
		return nil, err
	}

	// A file in cold storage that no row references. Nothing surfaces it yet:
	// design 5.11's stray-archive sweep is NOT implemented, so this is here to
	// prove the daemon ignores it, not to light up a panel.
	stray := filepath.Join(storeDir(root), "archive", folderCameraRoll, "stray-not-in-catalog.jpg")
	if err := writeColoredJPEG(stray, 64, rng); err != nil {
		return nil, err
	}

	logger.Info("seeded", "folder", folderCameraRoll, "files", len(files))
	return files, nil
}

// seedBackups is the wedged folder: its pinned bytes alone exceed its budget,
// so no amount of offloading can converge and the daemon has nothing to do.
// That state is stable -- enforcement finds no unpinned candidate and stops --
// which is what makes it usable as a fixture.
func seedBackups(cat *catalog.Catalog, root string) error {
	dir := filepath.Join(root, "sync", folderBackups)
	base := time.Date(2025, 9, 1, 8, 0, 0, 0, time.UTC)
	now := time.Now().Unix()

	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("backup-2025-%02d.tar", i+1)
		f, err := seedFile(cat, folderBackups, dir, name, base.Add(time.Duration(i)*48*time.Hour),
			func(path string) error { return writeFiller(path, 4_000) })
		if err != nil {
			return err
		}
		if err := cat.SetPinned(f.id, true, catalog.PinSourceRestore, now-int64(300-i*5)*86400); err != nil {
			return err
		}
	}

	// Three archived, unpinned rows. Restoring them is what raises the budget
	// pre-flight's 409 and the confirm dialog it feeds.
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("archive-2024-%02d.tar", i+1)
		f, err := seedFile(cat, folderBackups, dir, name, base.Add(-time.Duration(i+1)*720*time.Hour),
			func(path string) error { return writeFiller(path, 6_000) })
		if err != nil {
			return err
		}
		if err := archiveRow(cat, root, folderBackups, f); err != nil {
			return err
		}
	}

	logger.Info("seeded", "folder", folderBackups, "state", "wedged")
	return nil
}

func seedDocuments(cat *catalog.Catalog, root string) error {
	dir := filepath.Join(root, "sync", folderDocuments)
	base := time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)

	// Every size below is load-bearing: the folder's total has to stay above
	// its 100 kB budget so the demo shows the over-budget state, and below the
	// 500 kB the Playwright budget test raises it to. The real documents are
	// padded to hit them.
	docs := []struct {
		name string
		size int
		// blankFirstPage makes a PDF whose page 1 is an empty cover sheet, so
		// the "first page with content" heuristic has a case to work on that
		// is not a synthetic unit-test fixture.
		blankFirstPage bool
		corrupt        bool
	}{
		{name: "2026-Q1-Report.pdf", size: 15_000},
		{name: "2026-Q2-Report.pdf", size: 18_200, blankFirstPage: true},
		{name: "Meeting-Notes-Jan.txt", size: 2_400},
		{name: "Meeting-Notes-Feb.txt", size: 3_100},
		{name: "Meeting-Notes-Mar.txt", size: 1_800},
		{name: "Budget-2026.csv", size: 8_500},
		{name: "Project-Proposal.pdf", size: 12_300},
		{name: "Team-Roster.csv", size: 950},
		// Announces itself as a PDF and is not one, on purpose: without a file
		// that fails, the thumbnail-failure glyph, the thumb_error text and
		// the retry action are unreachable in the demo.
		{name: "Architecture-Diagram.pdf", size: 22_000, corrupt: true},
		{name: "Release-Notes-v2.md", size: 4_200},
		{name: "Pocket-Guide.epub", size: 6_700},
		{name: "Contract-Draft.pdf", size: 9_800},
		{name: "README.txt", size: 1_200},
		{name: "bookmarks", size: 5_600},
		{name: "Important/Passport-Scan.pdf", size: 25_000},
	}
	for i, d := range docs {
		write := documentWriter(d.name, d.size, d.blankFirstPage, d.corrupt)
		if _, err := seedFile(cat, folderDocuments, dir, d.name,
			base.Add(time.Duration(i)*10*24*time.Hour), write); err != nil {
			return err
		}
	}

	logger.Info("seeded", "folder", folderDocuments, "state", "disabled, over budget")
	return nil
}

func seedOldPhoneFiles(cat *catalog.Catalog, root string) error {
	dir := filepath.Join(root, "sync", folderOldPhone)
	base := time.Date(2023, 6, 1, 12, 0, 0, 0, time.UTC)
	rng := rand.New(rand.NewSource(7))

	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("OLD_2023%02d_%03d.jpg", i+1, i)
		f, err := seedFile(cat, folderOldPhone, dir, name, base.Add(time.Duration(i)*72*time.Hour),
			func(path string) error { return writeColoredJPEG(path, 40, rng) })
		if err != nil {
			return err
		}
		if i < 2 {
			if err := archiveRow(cat, root, folderOldPhone, f); err != nil {
				return err
			}
		}
	}

	logger.Info("seeded", "folder", folderOldPhone, "state", "not in coldstore.toml")
	return nil
}

// applyPolicy is the half of folder policy that belongs to the database rather
// than the TOML: what the owner has since changed in the UI.
func applyPolicy(cat *catalog.Catalog) error {
	disabled := false
	if found, err := cat.UpdateFolderPolicy(folderDocuments, nil, nil, &disabled); err != nil {
		return fmt.Errorf("disabling %s: %w", folderDocuments, err)
	} else if !found {
		return fmt.Errorf("%s has no folder_configs row", folderDocuments)
	}
	return nil
}

func seedEvents(cat *catalog.Catalog) error {
	events := []struct{ typ, folder, path, details string }{
		{"bulk_eviction", folderCameraRoll, "",
			"2483 files (13.2 GB) were archived in one enforcement pass"},
		{"folder_wedged", folderBackups, "",
			"48 kB pinned exceeds the 20 kB budget - nothing left to offload"},
		{"cold_storage_low", "", "", "cold storage is below 10% free"},
		{"archive", folderCameraRoll, "IMG_20260105_143022.jpg", "archived to cold storage"},
		{"restore", folderCameraRoll, "IMG_20260518_175244.jpg", "restored from cold storage"},
	}
	for _, e := range events {
		if err := cat.LogEvent(e.typ, e.folder, e.path, e.details); err != nil {
			return err
		}
	}
	return nil
}

func generateThumbnails(cat *catalog.Catalog, root string, camera []seeded) error {
	gen := thumbnail.NewGenerator(storeDir(root), cat, logger)

	paths := make(map[string]string, len(camera))
	var queue []catalog.FileRecord
	for _, s := range camera {
		rec, err := cat.GetFileByID(s.id)
		if err != nil {
			continue
		}
		if !thumbnail.CanThumbnail(rec.Extension) || rec.ThumbAttempts > 0 {
			continue
		}
		src := s.path
		if rec.Status == catalog.StatusArchived {
			src = filepath.Join(storeDir(root), "archive", rec.FolderLabel, rec.RelativePath)
		}
		if _, err := os.Stat(src); err != nil {
			continue // the missing ones
		}
		queue = append(queue, *rec)
		paths[rec.FolderLabel+"/"+rec.RelativePath] = src
	}

	gen.ProcessQueue(queue, func(f *catalog.FileRecord) string {
		return paths[f.FolderLabel+"/"+f.RelativePath]
	}, 2)

	logger.Info("thumbnails generated", "count", len(queue))
	return nil
}

func writeFiller(path string, size int) error {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	return os.WriteFile(path, data, 0o644)
}

func writeColoredJPEG(path string, side int, rng *rand.Rand) error {
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	c := color.RGBA{R: uint8(rng.Intn(256)), G: uint8(rng.Intn(256)), B: uint8(rng.Intn(256)), A: 255}
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			// A soft gradient rather than a flat fill, so thumbnails of
			// different files are visibly different at tile size.
			img.Set(x, y, color.RGBA{
				R: uint8(int(c.R)*(side-x)/side + x*40/side),
				G: uint8(int(c.G)*(side-y)/side + y*40/side),
				B: c.B, A: 255,
			})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return jpeg.Encode(f, img, &jpeg.Options{Quality: 80})
}
