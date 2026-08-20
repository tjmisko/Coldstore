package main

import (
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
	"github.com/tjmisko/coldstore/internal/thumbnail"
)

const demoRoot = "/tmp/coldstore-demo"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Create directory structure
	for _, dir := range []string{
		filepath.Join(demoRoot, "archive"),
		filepath.Join(demoRoot, "thumbnails", "CameraRoll"),
		filepath.Join(demoRoot, "thumbnails", "Documents"),
		filepath.Join(demoRoot, "sync", "CameraRoll"),
		filepath.Join(demoRoot, "sync", "Documents"),
		// FolderReady refuses to move anything into or out of a folder with no
		// .stfolder marker, so the demo folders need the one Syncthing would
		// have created or nothing in the demo can be archived or restored.
		filepath.Join(demoRoot, "sync", "CameraRoll", ".stfolder"),
		filepath.Join(demoRoot, "sync", "Documents", ".stfolder"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			logger.Error("mkdir failed", "path", dir, "error", err)
			os.Exit(1)
		}
	}

	// Open catalog
	dbPath := filepath.Join(demoRoot, "catalog.db")
	os.Remove(dbPath)
	cat, err := catalog.Open(dbPath)
	if err != nil {
		logger.Error("failed to open catalog", "error", err)
		os.Exit(1)
	}
	defer cat.Close()

	// Seed folder configs
	seedFolderConfigs(cat, logger)

	// Seed CameraRoll images
	cameraFiles := seedCameraRoll(cat, logger)

	// Seed Documents
	seedDocuments(cat, logger)

	// Mark some CameraRoll files as archived/missing
	markArchivedAndMissing(cat, cameraFiles, logger)

	// Generate thumbnails for synced images
	generateThumbnails(cat, cameraFiles, logger)

	// Add persistent paths
	cat.AddPersistentPath("CameraRoll", "Favorites/*")
	cat.AddPersistentPath("Documents", "Important/*")

	// Log some events
	cat.LogEvent("scan_complete", "CameraRoll", "", "scanned 30 files")
	cat.LogEvent("offload", "CameraRoll", "IMG_20240803_091544.jpg", "archived to cold storage")
	cat.LogEvent("scan_complete", "Documents", "", "scanned 15 files")

	logger.Info("demo data seeded successfully",
		"db", dbPath,
		"sync_root", filepath.Join(demoRoot, "sync"))
}

func seedFolderConfigs(cat *catalog.Catalog, logger *slog.Logger) {
	configs := []catalog.FolderConfigRecord{
		{
			FolderLabel:   "CameraRoll",
			SyncthingID:   "demo-camera",
			SyncthingPath: filepath.Join(demoRoot, "sync", "CameraRoll"),
			BudgetBytes:   16_000, // 16KB -- synced images fill ~80%
			GraceMinutes:  1,
			Enabled:       true,
		},
		{
			FolderLabel:   "Documents",
			SyncthingID:   "demo-docs",
			SyncthingPath: filepath.Join(demoRoot, "sync", "Documents"),
			BudgetBytes:   340_000, // 340KB -- docs fill ~40%
			GraceMinutes:  1,
			Enabled:       true,
		},
	}
	for _, fc := range configs {
		if err := cat.UpsertFolderConfig(&fc); err != nil {
			logger.Error("failed to upsert folder config", "label", fc.FolderLabel, "error", err)
			os.Exit(1)
		}
	}
	logger.Info("folder configs seeded")
}

type seededFile struct {
	record catalog.FileRecord
	path   string
}

func seedCameraRoll(cat *catalog.Catalog, logger *slog.Logger) []seededFile {
	syncDir := filepath.Join(demoRoot, "sync", "CameraRoll")
	baseTime := time.Date(2024, 7, 1, 10, 0, 0, 0, time.UTC)
	rng := rand.New(rand.NewSource(42))

	type imgSpec struct {
		name string
		ext  string
		mime string
	}

	specs := []imgSpec{
		{"IMG_20240115_143022.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240122_091015.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240205_170833.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240214_083201.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240301_112445.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240315_194522.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240328_080155.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240410_153311.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240425_064730.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240502_121900.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240518_175244.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240601_090322.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240615_142011.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240620_083544.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240701_110233.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240715_165422.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240722_094811.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240803_091544.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240815_173022.jpg", ".jpg", "image/jpeg"},
		{"IMG_20240901_081155.jpg", ".jpg", "image/jpeg"},
		{"sunset_beach.png", ".png", "image/png"},
		{"family_dinner.jpg", ".jpg", "image/jpeg"},
		{"concert_night.jpg", ".jpg", "image/jpeg"},
		{"hiking_trail.jpg", ".jpg", "image/jpeg"},
		{"birthday_cake.jpg", ".jpg", "image/jpeg"},
		{"city_skyline.jpg", ".jpg", "image/jpeg"},
		{"garden_flowers.jpg", ".jpg", "image/jpeg"},
		{"snow_mountain.jpg", ".jpg", "image/jpeg"},
		{"lake_sunrise.jpg", ".jpg", "image/jpeg"},
		{"autumn_leaves.jpg", ".jpg", "image/jpeg"},
	}

	var files []seededFile
	for i, spec := range specs {
		filePath := filepath.Join(syncDir, spec.name)
		mtime := baseTime.Add(time.Duration(i) * 5 * 24 * time.Hour) // ~5 days apart

		// Generate a small colored JPEG (different color per image)
		if err := writeColoredJPEG(filePath, rng); err != nil {
			logger.Error("failed to create image", "path", filePath, "error", err)
			os.Exit(1)
		}
		os.Chtimes(filePath, mtime, mtime)

		info, _ := os.Stat(filePath)
		rec := catalog.FileRecord{
			FolderLabel:  "CameraRoll",
			RelativePath: spec.name,
			Filename:     spec.name,
			Extension:    spec.ext,
			MimeType:     spec.mime,
			SizeBytes:    info.Size(),
			FileMtime:    mtime,
			FileAtime:    mtime,
			Status:       catalog.StatusSynced,
		}
		if _, err := cat.UpsertFile(&rec, time.Now().UnixNano()); err != nil {
			logger.Error("failed to upsert file", "path", spec.name, "error", err)
			os.Exit(1)
		}
		files = append(files, seededFile{record: rec, path: filePath})
	}
	logger.Info("CameraRoll seeded", "count", len(files))
	return files
}

func seedDocuments(cat *catalog.Catalog, logger *slog.Logger) {
	syncDir := filepath.Join(demoRoot, "sync", "Documents")
	baseTime := time.Date(2024, 3, 1, 9, 0, 0, 0, time.UTC)

	type docSpec struct {
		name string
		ext  string
		mime string
		size int
	}

	specs := []docSpec{
		{"2024-Q1-Report.pdf", ".pdf", "application/pdf", 15000},
		{"2024-Q2-Report.pdf", ".pdf", "application/pdf", 18200},
		{"Meeting-Notes-Jan.txt", ".txt", "text/plain", 2400},
		{"Meeting-Notes-Feb.txt", ".txt", "text/plain", 3100},
		{"Meeting-Notes-Mar.txt", ".txt", "text/plain", 1800},
		{"Budget-2024.xlsx", ".xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", 8500},
		{"Project-Proposal.docx", ".docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", 12300},
		{"Team-Roster.csv", ".csv", "text/csv", 950},
		{"Architecture-Diagram.pdf", ".pdf", "application/pdf", 22000},
		{"Release-Notes-v2.md", ".md", "text/markdown", 4200},
		{"Invoice-0042.pdf", ".pdf", "application/pdf", 6700},
		{"Contract-Draft.docx", ".docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", 9800},
		{"README.txt", ".txt", "text/plain", 1200},
		{"Changelog.txt", ".txt", "text/plain", 5600},
		{"Presentation-Q3.pptx", ".pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation", 25000},
	}

	for i, spec := range specs {
		filePath := filepath.Join(syncDir, spec.name)
		mtime := baseTime.Add(time.Duration(i) * 10 * 24 * time.Hour)

		// Write stub file with random bytes
		data := make([]byte, spec.size)
		for j := range data {
			data[j] = byte(j % 256)
		}
		if err := os.WriteFile(filePath, data, 0644); err != nil {
			logger.Error("failed to create doc", "path", filePath, "error", err)
			os.Exit(1)
		}
		os.Chtimes(filePath, mtime, mtime)

		rec := catalog.FileRecord{
			FolderLabel:  "Documents",
			RelativePath: spec.name,
			Filename:     spec.name,
			Extension:    spec.ext,
			MimeType:     spec.mime,
			SizeBytes:    int64(spec.size),
			FileMtime:    mtime,
			FileAtime:    mtime,
			Status:       catalog.StatusSynced,
		}
		if _, err := cat.UpsertFile(&rec, time.Now().UnixNano()); err != nil {
			logger.Error("failed to upsert file", "path", spec.name, "error", err)
			os.Exit(1)
		}
	}
	logger.Info("Documents seeded", "count", len(specs))
}

func markArchivedAndMissing(cat *catalog.Catalog, files []seededFile, logger *slog.Logger) {
	archiveDir := filepath.Join(demoRoot, "archive", "CameraRoll")
	os.MkdirAll(archiveDir, 0755)

	// Archive the 8 oldest files (indices 0-7)
	for i := 0; i < 8 && i < len(files); i++ {
		f, err := cat.GetFile(files[i].record.FolderLabel, files[i].record.RelativePath)
		if err != nil {
			logger.Error("failed to get file for archiving", "error", err)
			continue
		}

		// Move file to archive
		archivePath := filepath.Join(archiveDir, f.Filename)
		os.Rename(files[i].path, archivePath)
		files[i].path = archivePath

		cat.UpdateFileStatus(f.ID, catalog.StatusArchived)
		logger.Debug("archived", "file", f.Filename)
	}

	// Mark 2 files as missing (indices 8-9)
	for i := 8; i < 10 && i < len(files); i++ {
		f, err := cat.GetFile(files[i].record.FolderLabel, files[i].record.RelativePath)
		if err != nil {
			logger.Error("failed to get file for missing mark", "error", err)
			continue
		}
		// Remove from sync dir to simulate missing
		os.Remove(files[i].path)
		cat.UpdateFileStatus(f.ID, catalog.StatusMissing)
		logger.Debug("marked missing", "file", f.Filename)
	}

	logger.Info("marked archived and missing files")
}

func generateThumbnails(cat *catalog.Catalog, files []seededFile, logger *slog.Logger) {
	gen := thumbnail.NewGenerator(demoRoot, cat, logger)

	// Only generate thumbnails for files that still exist on disk (synced or archived)
	var toProcess []catalog.FileRecord
	pathMap := make(map[string]string)

	for _, sf := range files {
		if _, err := os.Stat(sf.path); err != nil {
			continue // file was removed (missing)
		}
		f, err := cat.GetFile(sf.record.FolderLabel, sf.record.RelativePath)
		if err != nil {
			continue
		}
		if !thumbnail.CanThumbnail(f.Extension) {
			continue
		}
		toProcess = append(toProcess, *f)
		key := fmt.Sprintf("%s/%s", f.FolderLabel, f.RelativePath)
		pathMap[key] = sf.path
	}

	gen.ProcessQueue(toProcess, func(f *catalog.FileRecord) string {
		return pathMap[fmt.Sprintf("%s/%s", f.FolderLabel, f.RelativePath)]
	}, 2)

	logger.Info("thumbnails generated", "count", len(toProcess))
}

func writeColoredJPEG(path string, rng *rand.Rand) error {
	width, height := 64, 64
	img := image.NewRGBA(image.Rect(0, 0, width, height))

	// Random color for each image
	c := color.RGBA{
		R: uint8(rng.Intn(256)),
		G: uint8(rng.Intn(256)),
		B: uint8(rng.Intn(256)),
		A: 255,
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, c)
		}
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return jpeg.Encode(f, img, &jpeg.Options{Quality: 80})
}
