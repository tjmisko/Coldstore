// Package metadata extracts capture and location metadata without changing the
// source file. Still images use permissively licensed pure-Go readers; video
// containers and Live Photo candidate probes use external tools as separate
// processes.
package metadata

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Extracted is the normalized metadata persisted by later catalog work.
// Optional numeric fields are pointers because zero is a valid coordinate,
// altitude, accuracy, and UTC offset.
type Extracted struct {
	CapturedLocal  string
	CapturedOffset *int
	CapturedSource string
	Lat            *float64
	Lon            *float64
	AltitudeM      *float64
	AccuracyM      *float64
	GPSSource      string
	ContentID      string
}

// Extractor is the pure package boundary used by the derived-data pass.
type Extractor interface {
	Extract(ctx context.Context, path, ext string) (Extracted, error)
}

var supportedExtensions = []string{
	".jpg", ".jpeg", ".heic", ".heif", ".png", ".webp", ".avif",
	".tif", ".tiff", ".dng", ".cr2", ".nef", ".arw", ".pef",
	".mov", ".mp4",
}

// SupportedExtensions returns the extension allowlist for the background
// extraction queue. Return a copy so a caller cannot silently change which
// files HybridExtractor accepts.
func SupportedExtensions() []string {
	return append([]string(nil), supportedExtensions...)
}

// ErrUnsupportedExtension reports a file outside the metadata queue's
// extension allowlist.
var ErrUnsupportedExtension = errors.New("metadata: unsupported extension")

const defaultToolTimeout = 30 * time.Second

// Options controls external tools. Empty paths select the normal executable
// names. A non-positive timeout selects the 30-second per-file default.
type Options struct {
	FFprobePath  string
	ExifToolPath string
	ToolTimeout  time.Duration
}

// HybridExtractor dispatches bulk reads by extension. Its normal Extract path
// never invokes ExifTool; ProbeStillCandidate is the deliberately narrow Live
// Photo candidate path.
type HybridExtractor struct {
	still    *StillExtractor
	video    *QuickTimeExtractor
	exiftool *ExifToolProbe
}

// New returns an extractor using ffprobe and exiftool from PATH.
func New() *HybridExtractor {
	return NewWithOptions(Options{})
}

// NewWithOptions returns an extractor with configurable external tools.
func NewWithOptions(opts Options) *HybridExtractor {
	timeout := opts.ToolTimeout
	if timeout <= 0 {
		timeout = defaultToolTimeout
	}
	ffprobe := opts.FFprobePath
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	exiftool := opts.ExifToolPath
	if exiftool == "" {
		exiftool = "exiftool"
	}
	return &HybridExtractor{
		still:    NewStillExtractor(),
		video:    NewQuickTimeExtractor(ffprobe, timeout),
		exiftool: NewExifToolProbe(exiftool, timeout),
	}
}

// Extract reads metadata using the bulk reader for the extension.
func (e *HybridExtractor) Extract(ctx context.Context, path, ext string) (Extracted, error) {
	ext = normalizeExtension(path, ext)
	switch ext {
	case ".jpg", ".jpeg", ".heic", ".heif", ".png", ".webp", ".avif",
		".tif", ".tiff", ".dng", ".cr2", ".nef", ".arw", ".pef":
		return e.still.Extract(ctx, path, ext)
	case ".mov", ".mp4":
		return e.video.Extract(ctx, path, ext)
	default:
		return Extracted{}, fmt.Errorf("%w: %q", ErrUnsupportedExtension, ext)
	}
}

// ProbeStillCandidate asks the resident ExifTool process for a Live Photo
// still candidate. It is intentionally separate from Extract so ExifTool can
// never become the bulk metadata path accidentally.
func (e *HybridExtractor) ProbeStillCandidate(ctx context.Context, path, ext string) (Extracted, error) {
	ext = normalizeExtension(path, ext)
	switch ext {
	case ".jpg", ".jpeg", ".heic", ".heif":
		return e.exiftool.Probe(ctx, path)
	default:
		return Extracted{}, fmt.Errorf("%w for ExifTool candidate probe: %q", ErrUnsupportedExtension, ext)
	}
}

// Close shuts down the resident ExifTool process. It is safe to call more than
// once and does not start ExifTool if it has never been needed.
func (e *HybridExtractor) Close() error {
	return e.exiftool.Close()
}

func normalizeExtension(path, ext string) string {
	if ext == "" {
		ext = filepath.Ext(path)
	}
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return ext
}
