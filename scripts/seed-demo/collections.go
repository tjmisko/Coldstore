package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math/rand"
	"time"

	"github.com/tjmisko/coldstore/internal/archive"
	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/ingest"
	"github.com/tjmisko/coldstore/internal/thumbnail"
)

// folderScans is the demo's archive-of-record collection. Unlike the four
// folders above it has no path and no budget: its bytes live in the
// content-addressed store, and the daemon browses it through the same sidebar.
const folderScans = "Scans"

// deriveWait bounds how long the seeder waits for the collection's thumbnails.
const deriveWait = 60 * time.Second

type scanFile struct {
	path  string
	body  []byte
	mtime time.Time
}

// seedCollections puts a few files into the Scans collection through the real
// archive service -- the same Open and Ingest the daemon runs -- and closes it
// again, so the daemon that later takes the journal lock finds a finished
// archive.
func seedCollections(cfg *config.Config, cat *catalog.Catalog) error {
	ctx := context.Background()
	opts := archive.Options{
		Root:        cfg.Storage.ColdStoragePath,
		Collections: collectionNames(cfg),
		Logger:      logger,
		Render:      thumbnail.NewGenerator(cfg.Storage.ColdStoragePath, cat, logger).RenderTo,
	}
	service, err := archive.Open(ctx, opts)
	if err != nil {
		return fmt.Errorf("opening the archive: %w", err)
	}
	service.Start(ctx)

	files := scanFiles()
	for _, file := range files {
		arrival := ingest.Arrival{
			Folder: folderScans, Path: file.path, Mtime: file.mtime,
			Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(file.body)), nil },
		}
		if _, err := service.Ingest(ctx, "seed-demo", arrival); err != nil {
			_ = service.Close()
			return fmt.Errorf("seeding %s/%s: %w", folderScans, file.path, err)
		}
	}

	if err := waitForDerive(ctx, service); err != nil {
		_ = service.Close()
		return err
	}
	if err := service.Close(); err != nil {
		return fmt.Errorf("closing the archive: %w", err)
	}
	logger.Info("seeded", "collection", folderScans, "files", len(files))
	return nil
}

func collectionNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Collections))
	for _, collection := range cfg.Collections {
		names = append(names, collection.Name)
	}
	return names
}

// waitForDerive blocks until every blob has been sniffed and thumbnailed, so the
// first page load already has its thumbnails.
func waitForDerive(ctx context.Context, service *archive.Service) error {
	deadline := time.Now().Add(deriveWait)
	for time.Now().Before(deadline) {
		pending, err := service.Index().BlobsToDerive(1)
		if err != nil {
			return fmt.Errorf("checking derived metadata: %w", err)
		}
		if len(pending) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("the %s thumbnails were not derived within %s", folderScans, deriveWait)
}

// scanFiles is five photographs, two of them in a subfolder, and one note with
// no thumbnail. Every path is deterministic and every body is distinct.
func scanFiles() []scanFile {
	rng := rand.New(rand.NewSource(11))
	base := time.Date(2024, 4, 2, 9, 30, 0, 0, time.UTC)
	paths := []string{
		"Passport-Scan.jpg", "Lease-Page-1.jpg", "Lease-Page-2.jpg",
		"Receipts/2024-03-Groceries.jpg", "Receipts/2024-04-Hardware.jpg",
	}
	files := make([]scanFile, 0, len(paths)+1)
	for i, path := range paths {
		files = append(files, scanFile{path: path, body: jpegBytes(56+rng.Intn(40), rng), mtime: base.Add(time.Duration(i) * 36 * time.Hour)})
	}
	files = append(files, scanFile{
		path: "Notes/filing-index.txt", body: []byte("Where each scan came from.\n"), mtime: base.Add(10 * 24 * time.Hour),
	})
	return files
}

func jpegBytes(side int, rng *rand.Rand) []byte {
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	c := color.RGBA{R: uint8(rng.Intn(256)), G: uint8(rng.Intn(256)), B: uint8(rng.Intn(256)), A: 255}
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8(int(c.R)*(side-x)/side + x*40/side),
				G: uint8(int(c.G)*(side-y)/side + y*40/side),
				B: c.B, A: 255,
			})
		}
	}
	var out bytes.Buffer
	_ = jpeg.Encode(&out, img, &jpeg.Options{Quality: 80})
	return out.Bytes()
}
