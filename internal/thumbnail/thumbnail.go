package thumbnail

import (
	"context"
	"errors"
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
	"syscall"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/disintegration/imaging"
	"github.com/tjmisko/coldstore/internal/catalog"
)

const (
	maxEdge     = 400
	jpegQ       = 80
	thumbSuffix = ".thumb.jpg"

	// defaultFFmpegTimeout bounds a single ffmpeg invocation. A truncated or
	// malformed video can make ffmpeg sit forever; without a deadline one such
	// file stalls the whole thumbnail backlog.
	defaultFFmpegTimeout = 30 * time.Second
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

	ffmpegBin     string
	ffmpegTimeout time.Duration
}

func NewGenerator(coldStoragePath string, cat *catalog.Catalog, logger *slog.Logger) *Generator {
	return &Generator{
		thumbDir:      filepath.Join(coldStoragePath, "thumbnails"),
		cat:           cat,
		logger:        logger,
		ffmpegBin:     "ffmpeg",
		ffmpegTimeout: defaultFFmpegTimeout,
	}
}

// SetFFmpeg overrides the ffmpeg binary and the per-invocation timeout. A
// non-positive timeout restores the default.
func (g *Generator) SetFFmpeg(binary string, timeout time.Duration) {
	if binary != "" {
		g.ffmpegBin = binary
	}
	if timeout > 0 {
		g.ffmpegTimeout = timeout
		return
	}
	g.ffmpegTimeout = defaultFFmpegTimeout
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
		err = g.generateHEIF(ctx, sourcePath, outPath)
	case videoExts[ext]:
		err = g.generateVideo(ctx, sourcePath, outPath)
	default:
		return fmt.Errorf("unsupported extension: %s", ext)
	}

	if err != nil {
		// A partial or empty output would otherwise be served as a broken
		// thumbnail forever.
		os.Remove(outPath)
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

// ffmpegWaitDelay bounds how long Wait blocks after the process has been
// killed. Without it a killed ffmpeg that left a child holding the output pipe
// open would hang the worker anyway, which is the very failure the timeout
// exists to prevent.
const ffmpegWaitDelay = 2 * time.Second

// newFFmpegCmd builds a cancellable ffmpeg invocation. The process gets its own
// process group so cancellation kills any child it spawned, not just ffmpeg
// itself.
func (g *Generator) newFFmpegCmd(cctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(cctx, g.ffmpegBin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = ffmpegWaitDelay
	return cmd
}

// ffmpegContext derives the deadline for one ffmpeg invocation.
func (g *Generator) ffmpegContext(ctx context.Context) (context.Context, context.CancelFunc, time.Duration) {
	timeout := g.ffmpegTimeout
	if timeout <= 0 {
		timeout = defaultFFmpegTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	return cctx, cancel, timeout
}

// ffmpegError turns a killed-by-deadline exec failure into a legible timeout
// error, so the recorded thumb_error says why the file was given up on.
func ffmpegError(cctx context.Context, timeout time.Duration, src string, err error, output string) error {
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("ffmpeg timed out after %s on %s", timeout, src)
	}
	if errors.Is(cctx.Err(), context.Canceled) {
		return fmt.Errorf("ffmpeg cancelled on %s: %w", src, context.Canceled)
	}
	if output == "" {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return fmt.Errorf("ffmpeg: %w: %s", err, output)
}

func (g *Generator) generateHEIF(ctx context.Context, src, dst string) error {
	cctx, cancel, timeout := g.ffmpegContext(ctx)
	defer cancel()

	// Use ffmpeg to decode HEIF -> pipe to stdout as PNG, then resize in Go
	cmd := g.newFFmpegCmd(cctx,
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
		return ffmpegError(cctx, timeout, src, waitErr, "")
	}
	if decodeErr != nil {
		return fmt.Errorf("decoding ffmpeg output: %w", decodeErr)
	}

	resized := imaging.Fit(img, maxEdge, maxEdge, imaging.Lanczos)
	return saveThumb(resized, dst)
}

func (g *Generator) generateVideo(ctx context.Context, src, dst string) error {
	cctx, cancel, timeout := g.ffmpegContext(ctx)
	defer cancel()

	// ffmpeg: extract a representative frame
	cmd := g.newFFmpegCmd(cctx,
		"-i", src,
		"-vf", "thumbnail,scale='min("+fmt.Sprintf("%d", maxEdge)+"\\,iw):-1'",
		"-frames:v", "1",
		"-y",
		dst,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ffmpegError(cctx, timeout, src, err, string(out))
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
