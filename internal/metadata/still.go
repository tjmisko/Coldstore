package metadata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	crossmeta "github.com/bep/imagemeta"
	primarymeta "github.com/evanoberholster/imagemeta"
	primaryexif "github.com/evanoberholster/imagemeta/meta/exif"
)

type stillReadResult struct {
	extracted     Extracted
	tagCount      int
	hasAppleMaker bool
}

// StillExtractor uses evanoberholster/imagemeta as the primary reader and the
// complementary bep/imagemeta parser only for suspicious HEIC results.
type StillExtractor struct {
	primary    func(context.Context, string) (stillReadResult, error)
	crosscheck func(context.Context, string, string) (stillReadResult, error)
}

// NewStillExtractor constructs the pure-Go bulk still-image reader.
func NewStillExtractor() *StillExtractor {
	reader := &StillExtractor{}
	reader.primary = reader.readPrimary
	reader.crosscheck = reader.readCrosscheck
	return reader
}

// Extract implements Extractor for still formats.
func (r *StillExtractor) Extract(ctx context.Context, path, ext string) (Extracted, error) {
	if err := ctx.Err(); err != nil {
		return Extracted{}, fmt.Errorf("metadata: still read cancelled: %w", err)
	}
	ext = normalizeExtension(path, ext)
	primary, err := r.primary(ctx, path)
	if err != nil {
		return Extracted{}, err
	}
	if ext != ".heic" && ext != ".heif" {
		return primary.extracted, nil
	}

	suspicious := primary.tagCount == 0 || extractedEmpty(primary.extracted)
	if primary.hasAppleMaker && coordinatesAreZero(primary.extracted) {
		suspicious = true
	}
	if !suspicious {
		return primary.extracted, nil
	}

	fallback, err := r.crosscheck(ctx, path, ext)
	if err != nil {
		return Extracted{}, fmt.Errorf("metadata: HEIC cross-check: %w", err)
	}
	if fallback.tagCount > 0 {
		return fallback.extracted, nil
	}
	return primary.extracted, nil
}

func (r *StillExtractor) readPrimary(ctx context.Context, path string) (stillReadResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return stillReadResult{}, fmt.Errorf("metadata: open %s: %w", path, err)
	}
	defer file.Close()

	decoded, err := primarymeta.Decode(contextReadSeeker{ctx: ctx, ReadSeeker: file})
	if err != nil {
		if errors.Is(err, primarymeta.ErrNoExif) {
			return stillReadResult{}, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stillReadResult{}, fmt.Errorf("metadata: still read cancelled: %w", ctxErr)
		}
		return stillReadResult{}, fmt.Errorf("metadata: decode %s: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return stillReadResult{}, fmt.Errorf("metadata: still read cancelled: %w", err)
	}

	extracted, err := extractedFromPrimary(decoded)
	if err != nil {
		return stillReadResult{}, fmt.Errorf("metadata: normalize %s: %w", path, err)
	}
	return stillReadResult{
		extracted:     extracted,
		tagCount:      approximatePrimaryTagCount(decoded),
		hasAppleMaker: decoded.MakerNote.Apple != nil,
	}, nil
}

func extractedFromPrimary(decoded primaryexif.Exif) (Extracted, error) {
	gpsUTC := decoded.GPS.GPSTimestamp()
	original, err := parseEXIFCapture(
		formatEXIFTime(decoded.ExifIFD.DateTimeOriginal),
		decoded.ExifIFD.ExifToolSubSecTimeOriginal(),
		locationOffset(decoded.ExifIFD.OffsetTimeOriginal),
		"exif",
		gpsUTC,
	)
	if err != nil {
		return Extracted{}, err
	}
	digitized, err := parseEXIFCapture(
		formatEXIFTime(decoded.ExifIFD.CreateDate),
		decoded.ExifIFD.ExifToolSubSecTimeDigitized(),
		locationOffset(decoded.ExifIFD.OffsetTimeDigitized),
		"exif_digitized",
		gpsUTC,
	)
	if err != nil {
		return Extracted{}, err
	}
	capture := chooseCapture(original, captureCandidate{}, digitized, captureCandidate{})

	result := Extracted{
		CapturedLocal:  capture.local,
		CapturedOffset: capture.offset,
		CapturedSource: capture.source,
	}
	lat := decoded.GPS.Latitude()
	lon := decoded.GPS.Longitude()
	if decoded.GPS.GPSVersion.String() != "" || lat != 0 || lon != 0 {
		result.Lat = float64Pointer(lat)
		result.Lon = float64Pointer(lon)
		result.GPSSource = "exif"
	}
	// The primary library does not expose presence bits for optional scalar
	// GPS tags. Do not turn an absent tag into a claimed zero-valued fact.
	if altitude := float64(decoded.GPS.Altitude()); altitude != 0 {
		result.AltitudeM = float64Pointer(altitude)
	}
	if accuracy := decoded.GPS.HPositioningError(); accuracy != 0 {
		result.AccuracyM = float64Pointer(accuracy)
	}
	return result, nil
}

func (r *StillExtractor) readCrosscheck(ctx context.Context, path, ext string) (stillReadResult, error) {
	format, ok := crosscheckFormat(ext)
	if !ok {
		return stillReadResult{}, fmt.Errorf("%w: %q", ErrUnsupportedExtension, ext)
	}
	file, err := os.Open(path)
	if err != nil {
		return stillReadResult{}, fmt.Errorf("metadata: open %s: %w", path, err)
	}
	defer file.Close()

	var tags crossmeta.Tags
	_, err = crossmeta.Decode(crossmeta.Options{
		R:           contextReadSeeker{ctx: ctx, ReadSeeker: file},
		ImageFormat: format,
		Sources:     crossmeta.EXIF,
		HandleTag: func(tag crossmeta.TagInfo) error {
			tags.Add(tag)
			return nil
		},
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stillReadResult{}, fmt.Errorf("metadata: still cross-check cancelled: %w", ctxErr)
		}
		return stillReadResult{}, fmt.Errorf("metadata: cross-check decode %s: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return stillReadResult{}, fmt.Errorf("metadata: still cross-check cancelled: %w", err)
	}

	extracted, err := extractedFromCrosscheck(tags.EXIF())
	if err != nil {
		return stillReadResult{}, err
	}
	return stillReadResult{extracted: extracted, tagCount: len(tags.EXIF())}, nil
}

func extractedFromCrosscheck(tags map[string]crossmeta.TagInfo) (Extracted, error) {
	gpsUTC := crosscheckGPSTime(tags)
	original, err := parseEXIFCapture(
		tagString(tags, "DateTimeOriginal"),
		tagString(tags, "SubSecTimeOriginal"),
		tagString(tags, "OffsetTimeOriginal"),
		"exif",
		gpsUTC,
	)
	if err != nil {
		return Extracted{}, err
	}
	digitized, err := parseEXIFCapture(
		tagString(tags, "CreateDate"),
		tagString(tags, "SubSecTimeDigitized"),
		tagString(tags, "OffsetTimeDigitized"),
		"exif_digitized",
		gpsUTC,
	)
	if err != nil {
		return Extracted{}, err
	}
	capture := chooseCapture(original, captureCandidate{}, digitized, captureCandidate{})
	result := Extracted{
		CapturedLocal:  capture.local,
		CapturedOffset: capture.offset,
		CapturedSource: capture.source,
	}

	lat, hasLat := tagFloat(tags, "GPSLatitude")
	lon, hasLon := tagFloat(tags, "GPSLongitude")
	if hasLat && hasLon {
		if strings.EqualFold(tagString(tags, "GPSLatitudeRef"), "S") {
			lat = -math.Abs(lat)
		}
		if strings.EqualFold(tagString(tags, "GPSLongitudeRef"), "W") {
			lon = -math.Abs(lon)
		}
		result.Lat = float64Pointer(lat)
		result.Lon = float64Pointer(lon)
		result.GPSSource = "exif"
	}
	if altitude, ok := tagFloat(tags, "GPSAltitude"); ok {
		if ref, refOK := tagFloat(tags, "GPSAltitudeRef"); refOK && int(ref) == 1 {
			altitude = -math.Abs(altitude)
		}
		result.AltitudeM = float64Pointer(altitude)
	}
	if accuracy, ok := tagFloat(tags, "GPSHPositioningError"); ok {
		result.AccuracyM = float64Pointer(accuracy)
	}
	return result, nil
}

func crosscheckFormat(ext string) (crossmeta.ImageFormat, bool) {
	switch ext {
	case ".heic", ".heif":
		return crossmeta.HEIF, true
	case ".jpg", ".jpeg":
		return crossmeta.JPEG, true
	case ".png":
		return crossmeta.PNG, true
	case ".webp":
		return crossmeta.WebP, true
	case ".avif":
		return crossmeta.AVIF, true
	case ".tif", ".tiff":
		return crossmeta.TIFF, true
	case ".dng":
		return crossmeta.DNG, true
	case ".cr2":
		return crossmeta.CR2, true
	case ".nef":
		return crossmeta.NEF, true
	case ".arw":
		return crossmeta.ARW, true
	case ".pef":
		return crossmeta.PEF, true
	default:
		return 0, false
	}
}

func crosscheckGPSTime(tags map[string]crossmeta.TagInfo) time.Time {
	date := tagString(tags, "GPSDateStamp")
	clock := tagString(tags, "GPSTimeStamp")
	if date == "" || clock == "" {
		return time.Time{}
	}
	parsed, err := time.ParseInLocation("2006:01:02 15:04:05", date+" "+clock, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func tagString(tags map[string]crossmeta.TagInfo, name string) string {
	tag, ok := tags[name]
	if !ok || tag.Value == nil {
		return ""
	}
	switch value := tag.Value.(type) {
	case string:
		return strings.TrimSpace(value)
	case fmt.Stringer:
		return strings.TrimSpace(value.String())
	default:
		return strings.TrimSpace(fmt.Sprint(value))
	}
}

func tagFloat(tags map[string]crossmeta.TagInfo, name string) (float64, bool) {
	tag, ok := tags[name]
	if !ok {
		return 0, false
	}
	switch value := tag.Value.(type) {
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int8:
		return float64(value), true
	case int16:
		return float64(value), true
	case int32:
		return float64(value), true
	case int64:
		return float64(value), true
	case uint:
		return float64(value), true
	case uint8:
		return float64(value), true
	case uint16:
		return float64(value), true
	case uint32:
		return float64(value), true
	case uint64:
		return float64(value), true
	default:
		return 0, false
	}
}

func approximatePrimaryTagCount(decoded primaryexif.Exif) int {
	count := 0
	if decoded.IFD0.Make != "" || decoded.IFD0.Model != "" || decoded.IFD0.Orientation != 0 {
		count++
	}
	if !decoded.ExifIFD.DateTimeOriginal.IsZero() || !decoded.ExifIFD.CreateDate.IsZero() {
		count++
	}
	if decoded.GPS.GPSVersion.String() != "" || decoded.GPS.Latitude() != 0 || decoded.GPS.Longitude() != 0 {
		count++
	}
	if decoded.MakerNote.Apple != nil {
		count++
	}
	return count
}

func formatEXIFTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format("2006:01:02 15:04:05")
}

func locationOffset(location *time.Location) string {
	if location == nil {
		return ""
	}
	now := time.Now().In(location)
	_, seconds := now.Zone()
	minutes := seconds / 60
	sign := "+"
	if minutes < 0 {
		sign = "-"
		minutes = -minutes
	}
	return fmt.Sprintf("%s%02d:%02d", sign, minutes/60, minutes%60)
}

func extractedEmpty(value Extracted) bool {
	return value.CapturedLocal == "" && value.Lat == nil && value.Lon == nil &&
		value.AltitudeM == nil && value.AccuracyM == nil && value.ContentID == ""
}

func coordinatesAreZero(value Extracted) bool {
	return value.Lat != nil && value.Lon != nil && *value.Lat == 0 && *value.Lon == 0
}

func float64Pointer(value float64) *float64 {
	return &value
}

type contextReadSeeker struct {
	ctx context.Context
	io.ReadSeeker
}

func (r contextReadSeeker) Read(value []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReadSeeker.Read(value)
}

func (r contextReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReadSeeker.Seek(offset, whence)
}
