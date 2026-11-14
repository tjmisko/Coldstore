package thumbnail

import (
	"context"
	"fmt"
	"image"
	_ "image/gif" // some older EPUB covers are GIF, and .gif is thumbnailable
	"image/jpeg"
	_ "image/png"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/disintegration/imaging"
	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/filetype"
)

const (
	maxEdge     = 400
	jpegQ       = 80
	thumbSuffix = ".thumb.jpg"
)

// imageExts are extensions handled by the Go imaging pipeline.
var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true,
}

// heifExts need ffmpeg for decoding.
var heifExts = map[string]bool{
	".heif": true, ".heic": true,
}

// videoExts need ffmpeg frame extraction.
var videoExts = map[string]bool{
	".mp4": true, ".mov": true, ".avi": true, ".mkv": true, ".webm": true,
}

// pdfExts need poppler.
var pdfExts = map[string]bool{
	".pdf": true,
}

// epubExts are handled entirely in Go: an EPUB is a zip with an XML manifest
// in it, and archive/zip plus encoding/xml read both.
var epubExts = map[string]bool{
	".epub": true,
}

// extCategories is the ONE place a new thumbnailable category is registered.
// CanThumbnail, SupportedExtensions and Generate's dispatch switch all read
// from it, so the queue predicate and the generator cannot drift apart. The
// daemon used to keep a second hand-written copy of this list, and adding a
// format to one and not the other made the generator support a type the queue
// never selected -- silently, because nothing tied the two together.
var extCategories = []map[string]bool{imageExts, heifExts, videoExts, pdfExts, epubExts}

// detectedTypes maps a content-sniffed type onto the same categories.
//
// It is the second way into the generator, and the only one that works for a
// file with no extension: extension and mime_type are both derived from the
// filename, so a bookmark export called "reading" is invisible to either. This
// list has to stay in step with extCategories by hand -- there is no
// mechanical relation between ".mov" and "video/quicktime" -- which is what
// the round-trip test in thumbnail_test.go checks.
var detectedImageTypes = map[string]bool{
	filetype.TypeJPEG: true,
	filetype.TypePNG:  true,
	filetype.TypeWebP: true,
	filetype.TypeGIF:  true,
}

var detectedHEIFTypes = map[string]bool{
	filetype.TypeHEIC: true,
	filetype.TypeHEIF: true,
	filetype.TypeAVIF: true,
}

var detectedVideoTypes = map[string]bool{
	filetype.TypeMP4:       true,
	filetype.TypeQuickTime: true,
	filetype.TypeMatroska:  true,
	filetype.TypeWebM:      true,
	filetype.TypeAVI:       true,
}

var detectedPDFTypes = map[string]bool{
	filetype.TypePDF: true,
}

var detectedEPUBTypes = map[string]bool{
	filetype.TypeEPUB: true,
}

var typeCategories = []map[string]bool{
	detectedImageTypes, detectedHEIFTypes, detectedVideoTypes,
	detectedPDFTypes, detectedEPUBTypes,
}

// CanThumbnailType reports whether a content-sniffed type is one the generator
// can render.
func CanThumbnailType(detected string) bool {
	for _, set := range typeCategories {
		if set[detected] {
			return true
		}
	}
	return false
}

// SupportedDetectedTypes returns every type CanThumbnailType accepts, sorted,
// as the second arm to hand to catalog.FilesNeedingThumbnailsByType.
func SupportedDetectedTypes() []string {
	var types []string
	for _, set := range typeCategories {
		for t := range set {
			types = append(types, t)
		}
	}
	sort.Strings(types)
	return types
}

// CanThumbnail returns true if we support generating a thumbnail for this extension.
func CanThumbnail(ext string) bool {
	ext = strings.ToLower(ext)
	for _, set := range extCategories {
		if set[ext] {
			return true
		}
	}
	return false
}

// SupportedExtensions returns every extension CanThumbnail accepts, sorted, as
// the list to hand to catalog.FilesNeedingThumbnails.
func SupportedExtensions() []string {
	var exts []string
	for _, set := range extCategories {
		for ext := range set {
			exts = append(exts, ext)
		}
	}
	sort.Strings(exts)
	return exts
}

// Generator produces thumbnails and stores them in the configured thumbnail directory.
type Generator struct {
	thumbDir string
	cat      *catalog.Catalog
	logger   *slog.Logger

	ffmpeg tool
	pdf    tool
}

func NewGenerator(coldStoragePath string, cat *catalog.Catalog, logger *slog.Logger) *Generator {
	return &Generator{
		thumbDir: filepath.Join(coldStoragePath, "thumbnails"),
		cat:      cat,
		logger:   logger,
		ffmpeg:   newTool("ffmpeg", "ffmpeg"),
		// pdftocairo, NOT pdftoppm. pdftoppm cannot write to stdout: given "-"
		// as its output root it creates a file literally named "-.jpg". Every
		// snippet online that pipes it is wrong.
		pdf: newTool("pdftocairo", "pdftocairo"),
	}
}

// SetFFmpeg overrides the ffmpeg binary and the per-invocation timeout. A
// non-positive timeout restores the default.
func (g *Generator) SetFFmpeg(binary string, timeout time.Duration) {
	g.ffmpeg.set(binary, timeout)
}

// SetPDFRenderer overrides the PDF renderer and its per-invocation timeout,
// the same way SetFFmpeg does. It exists so the fake-binary test pattern
// extends to PDFs and unit tests do not need poppler installed.
func (g *Generator) SetPDFRenderer(binary string, timeout time.Duration) {
	g.pdf.set(binary, timeout)
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
	return g.GenerateContext(context.Background(), file, sourcePath)
}

// GenerateContext is Generate with cancellation. Every ffmpeg invocation it
// makes carries its own deadline on top of ctx.
func (g *Generator) GenerateContext(ctx context.Context, file *catalog.FileRecord, sourcePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	outPath := g.ThumbPath(file.FolderLabel, file.RelativePath)
	if err := g.RenderTo(ctx, sourcePath, file.Extension, file.DetectedType, outPath); err != nil {
		return err
	}

	// Update catalog with thumbnail path
	thumbRel := ThumbRelPath(file.FolderLabel, file.RelativePath)
	return g.cat.UpdateThumbnailPath(file.ID, thumbRel)
}

// RenderTo renders a thumbnail of sourcePath at outPath without touching the
// catalog. ext is the file's extension with its dot (case-folded here) and
// detected its sniffed type; either one may be empty.
func (g *Generator) RenderTo(ctx context.Context, sourcePath, ext, detected, outPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	ext = strings.ToLower(ext)

	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return fmt.Errorf("creating thumb dir: %w", err)
	}

	// Extension first, detected type second. The extension is what the owner
	// named the file and is right almost always; the sniffed type is the way
	// in for the files that have no extension at all, which is the only reason
	// it exists.
	var err error
	switch {
	case imageExts[ext], detectedImageTypes[detected]:
		err = g.generateImage(sourcePath, outPath)
	case heifExts[ext], detectedHEIFTypes[detected]:
		err = g.generateHEIF(ctx, sourcePath, outPath)
	case videoExts[ext], detectedVideoTypes[detected]:
		err = g.generateVideo(ctx, sourcePath, outPath)
	case pdfExts[ext], detectedPDFTypes[detected]:
		err = g.generatePDF(ctx, sourcePath, outPath)
	case epubExts[ext], detectedEPUBTypes[detected]:
		err = g.generateEPUB(sourcePath, outPath)
	default:
		return fmt.Errorf("unsupported extension: %s", ext)
	}

	if err != nil {
		// A partial or empty output would otherwise be served as a broken
		// thumbnail forever.
		os.Remove(outPath)
		return err
	}
	return nil
}

func (g *Generator) generateImage(src, dst string) error {
	img, err := imaging.Open(src, imaging.AutoOrientation(true))
	if err != nil {
		return fmt.Errorf("opening image: %w", err)
	}
	return saveThumb(img, dst)
}

func (g *Generator) generateHEIF(ctx context.Context, src, dst string) error {
	cctx, cancel, timeout := g.ffmpeg.deadline(ctx)
	defer cancel()

	// Use ffmpeg to decode HEIF -> pipe to stdout as PNG, then resize in Go
	cmd := g.ffmpeg.command(cctx,
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

	img, _, decodeErr := image.Decode(stdout)
	if waitErr := cmd.Wait(); waitErr != nil {
		return g.ffmpeg.fail(cctx, timeout, src, waitErr, "")
	}
	if decodeErr != nil {
		return fmt.Errorf("decoding ffmpeg output: %w", decodeErr)
	}

	resized := imaging.Fit(img, maxEdge, maxEdge, imaging.Lanczos)
	return saveThumb(resized, dst)
}

func (g *Generator) generateVideo(ctx context.Context, src, dst string) error {
	cctx, cancel, timeout := g.ffmpeg.deadline(ctx)
	defer cancel()

	// ffmpeg: extract a representative frame
	cmd := g.ffmpeg.command(cctx,
		"-i", src,
		"-vf", "thumbnail,scale='min("+fmt.Sprintf("%d", maxEdge)+"\\,iw):-1'",
		"-frames:v", "1",
		"-y",
		dst,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return g.ffmpeg.fail(cctx, timeout, src, err, string(out))
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
	g.ProcessQueueContext(context.Background(), files, sourcePathFn, workers)
}

// ProcessQueueContext is ProcessQueue with cancellation and failure tracking.
//
// Workers stop picking up files as soon as ctx is cancelled. A file that fails
// for its own reasons has the failure recorded against it, so a corrupt or
// unreadable file drops out of the queue after a few attempts instead of being
// retried on every sync forever. A failure caused by shutdown is NOT recorded:
// stopping the daemon must not burn a file's attempts.
func (g *Generator) ProcessQueueContext(ctx context.Context, files []catalog.FileRecord,
	sourcePathFn func(*catalog.FileRecord) string, workers int) {

	if len(files) == 0 {
		return
	}
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
				if ctx.Err() != nil {
					return
				}

				src := sourcePathFn(&file)
				err := g.GenerateContext(ctx, &file, src)
				if err == nil {
					g.logger.Debug("thumbnail generated",
						"folder", file.FolderLabel,
						"path", file.RelativePath)
					continue
				}

				if ctx.Err() != nil {
					// Shutdown, not a bad file.
					return
				}

				g.logger.Warn("thumbnail generation failed",
					"folder", file.FolderLabel,
					"path", file.RelativePath,
					"attempts", file.ThumbAttempts+1,
					"error", err)

				if recErr := g.cat.RecordThumbnailFailure(file.ID, err.Error(), time.Now().Unix()); recErr != nil {
					g.logger.Error("recording thumbnail failure",
						"path", file.RelativePath, "error", recErr)
				}
			}
		}()
	}
	wg.Wait()
}
