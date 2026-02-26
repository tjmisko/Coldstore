package thumbnail

import (
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	_ "golang.org/x/image/webp"

	"github.com/disintegration/imaging"
	"github.com/tjmisko/coldstore/internal/catalog"
)

const (
	maxEdge    = 400
	jpegQ      = 80
	thumbSuffix = ".thumb.jpg"
)

// imageExts are extensions handled by the Go imaging pipeline.
var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
}

// heifExts need ffmpeg for decoding.
var heifExts = map[string]bool{
	".heif": true, ".heic": true,
}

// videoExts need ffmpeg frame extraction.
var videoExts = map[string]bool{
	".mp4": true, ".mov": true, ".avi": true, ".mkv": true, ".webm": true,
}

// CanThumbnail returns true if we support generating a thumbnail for this extension.
func CanThumbnail(ext string) bool {
	ext = strings.ToLower(ext)
	return imageExts[ext] || heifExts[ext] || videoExts[ext]
}

// Generator produces thumbnails and stores them in the configured thumbnail directory.
type Generator struct {
	thumbDir string
	cat      *catalog.Catalog
	logger   *slog.Logger
}

func NewGenerator(coldStoragePath string, cat *catalog.Catalog, logger *slog.Logger) *Generator {
	return &Generator{
		thumbDir: filepath.Join(coldStoragePath, "thumbnails"),
		cat:      cat,
		logger:   logger,
	}
}

// ThumbPath returns the filesystem path where a thumbnail would be stored.
func (g *Generator) ThumbPath(folderLabel, relativePath string) string {
	return filepath.Join(g.thumbDir, folderLabel, relativePath+thumbSuffix)
}

// ThumbRelPath returns the relative path component used in the catalog and API.
func ThumbRelPath(folderLabel, relativePath string) string {
	return filepath.Join(folderLabel, relativePath+thumbSuffix)
}

// Generate creates a thumbnail for the given source file.
// sourcePath is the absolute path to the original file (synced or archived).
func (g *Generator) Generate(file *catalog.FileRecord, sourcePath string) error {
	ext := strings.ToLower(file.Extension)
	outPath := g.ThumbPath(file.FolderLabel, file.RelativePath)

	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return fmt.Errorf("creating thumb dir: %w", err)
	}

	var err error
	switch {
	case imageExts[ext]:
		err = g.generateImage(sourcePath, outPath)
	case heifExts[ext]:
		err = g.generateHEIF(sourcePath, outPath)
	case videoExts[ext]:
		err = g.generateVideo(sourcePath, outPath)
	default:
		return fmt.Errorf("unsupported extension: %s", ext)
	}

	if err != nil {
		return err
	}

	// Update catalog with thumbnail path
	thumbRel := ThumbRelPath(file.FolderLabel, file.RelativePath)
	return g.cat.UpdateThumbnailPath(file.ID, thumbRel)
}

func (g *Generator) generateImage(src, dst string) error {
	img, err := imaging.Open(src, imaging.AutoOrientation(true))
	if err != nil {
		return fmt.Errorf("opening image: %w", err)
	}
	return saveThumb(img, dst)
}

func (g *Generator) generateHEIF(src, dst string) error {
	// Use ffmpeg to decode HEIF -> pipe to stdout as PNG, then resize in Go
	cmd := exec.Command("ffmpeg",
		"-i", src,
		"-vframes", "1",
		"-f", "image2pipe",
		"-vcodec", "png",
		"-",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}

	img, _, err := image.Decode(stdout)
	if err != nil {
		cmd.Wait()
		return fmt.Errorf("decoding ffmpeg output: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}

	resized := imaging.Fit(img, maxEdge, maxEdge, imaging.Lanczos)
	return saveThumb(resized, dst)
}

func (g *Generator) generateVideo(src, dst string) error {
	// ffmpeg: extract a representative frame
	cmd := exec.Command("ffmpeg",
		"-i", src,
		"-vf", "thumbnail,scale='min("+fmt.Sprintf("%d", maxEdge)+"\\,iw):-1'",
		"-frames:v", "1",
		"-y",
		dst,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg video thumb: %w: %s", err, string(out))
	}
	return nil
}

func saveThumb(img image.Image, dst string) error {
	resized := imaging.Fit(img, maxEdge, maxEdge, imaging.Lanczos)

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	return jpeg.Encode(f, resized, &jpeg.Options{Quality: jpegQ})
}

// ProcessQueue generates thumbnails for a batch of files using a worker pool.
func (g *Generator) ProcessQueue(files []catalog.FileRecord, sourcePathFn func(*catalog.FileRecord) string, workers int) {
	if workers <= 0 {
		workers = 2
	}

	ch := make(chan catalog.FileRecord, len(files))
	for _, f := range files {
		ch <- f
	}
	close(ch)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for file := range ch {
				src := sourcePathFn(&file)
				if err := g.Generate(&file, src); err != nil {
					g.logger.Warn("thumbnail generation failed",
						"folder", file.FolderLabel,
						"path", file.RelativePath,
						"error", err)
				} else {
					g.logger.Debug("thumbnail generated",
						"folder", file.FolderLabel,
						"path", file.RelativePath)
				}
			}
		}()
	}
	wg.Wait()
}
