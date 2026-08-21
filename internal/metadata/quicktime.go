package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// QuickTimeExtractor reads MOV/MP4 container metadata with ffprobe. It does
// not ask ffprobe to read still-image EXIF because ffprobe does not expose it.
type QuickTimeExtractor struct {
	binary  string
	timeout time.Duration
}

// NewQuickTimeExtractor constructs a cancellable ffprobe reader.
func NewQuickTimeExtractor(binary string, timeout time.Duration) *QuickTimeExtractor {
	if binary == "" {
		binary = "ffprobe"
	}
	if timeout <= 0 {
		timeout = defaultToolTimeout
	}
	return &QuickTimeExtractor{binary: binary, timeout: timeout}
}

type ffprobeDocument struct {
	Format struct {
		Tags map[string]string `json:"tags"`
	} `json:"format"`
	Streams []struct {
		Tags map[string]string `json:"tags"`
	} `json:"streams"`
}

// Extract implements Extractor for MOV and MP4 containers.
func (r *QuickTimeExtractor) Extract(ctx context.Context, path, ext string) (Extracted, error) {
	ext = normalizeExtension(path, ext)
	if ext != ".mov" && ext != ".mp4" {
		return Extracted{}, fmt.Errorf("%w: %q", ErrUnsupportedExtension, ext)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Extracted{}, fmt.Errorf("metadata: resolve ffprobe path %s: %w", path, err)
	}
	output, err := runTool(
		ctx,
		r.binary,
		r.timeout,
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		absolute,
	)
	if err != nil {
		return Extracted{}, err
	}
	var document ffprobeDocument
	if err := json.Unmarshal(output, &document); err != nil {
		return Extracted{}, fmt.Errorf("metadata: decode ffprobe JSON for %s: %w", path, err)
	}
	tags := normalizeQuickTimeTags(document)

	quicktime, err := parseQuickTimeCapture(tags["com.apple.quicktime.creationdate"])
	if err != nil {
		return Extracted{}, err
	}
	mvhd, err := parseMVHDCapture(tags["creation_time"], nil)
	if err != nil {
		return Extracted{}, err
	}
	capture := chooseCapture(captureCandidate{}, quicktime, captureCandidate{}, mvhd)
	result := Extracted{
		CapturedLocal:  capture.local,
		CapturedOffset: capture.offset,
		CapturedSource: capture.source,
		ContentID:      strings.TrimSpace(tags["com.apple.quicktime.content.identifier"]),
	}

	location := tags["com.apple.quicktime.location.iso6709"]
	if location == "" {
		location = tags["location"]
	}
	if location == "" {
		location = tags["location-eng"]
	}
	if location != "" {
		lat, lon, altitude, err := ParseISO6709(location)
		if err != nil {
			return Extracted{}, err
		}
		result.Lat = float64Pointer(lat)
		result.Lon = float64Pointer(lon)
		result.AltitudeM = altitude
		result.GPSSource = "quicktime"
	}
	return result, nil
}

func normalizeQuickTimeTags(document ffprobeDocument) map[string]string {
	tags := make(map[string]string)
	add := func(values map[string]string) {
		for key, value := range values {
			key = strings.ToLower(strings.TrimSpace(key))
			if _, exists := tags[key]; !exists {
				tags[key] = strings.TrimSpace(value)
			}
		}
	}
	// Format tags are the container authority. Stream tags are a fallback for
	// writers that attach creation_time to the first media stream.
	add(document.Format.Tags)
	for _, stream := range document.Streams {
		add(stream.Tags)
	}
	return tags
}
