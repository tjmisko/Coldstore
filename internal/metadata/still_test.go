package metadata

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestStillExtractorReadsRealIPhoneHEIC(t *testing.T) {
	got, err := NewStillExtractor().Extract(context.Background(), "testdata/iphone.heic", ".heic")
	if err != nil {
		t.Fatal(err)
	}
	if got.CapturedLocal != "2026-02-19 14:22:37.146" {
		t.Errorf("CapturedLocal = %q", got.CapturedLocal)
	}
	if got.CapturedOffset == nil || *got.CapturedOffset != -480 {
		t.Errorf("CapturedOffset = %v", got.CapturedOffset)
	}
	if got.CapturedSource != "exif" {
		t.Errorf("CapturedSource = %q", got.CapturedSource)
	}
	if got.Lat == nil || got.Lon == nil {
		t.Fatal("GPS coordinates are absent")
	}
	assertClose(t, "latitude", *got.Lat, 37.9363333333333)
	assertClose(t, "longitude", *got.Lon, -122.365288888889)
	if got.AltitudeM == nil || got.AccuracyM == nil {
		t.Fatalf("altitude/accuracy = %v/%v, want both", got.AltitudeM, got.AccuracyM)
	}
	// The primary reader normalizes altitude through float32.
	assertCloseWithin(t, "altitude", *got.AltitudeM, 13.20880719, 1e-5)
	assertCloseWithin(t, "accuracy", *got.AccuracyM, 31.92358577, 1e-8)
	if got.ContentID != "" {
		t.Errorf("bulk pure-Go path returned ContentID %q; candidate probe must own it", got.ContentID)
	}
}

func TestStillExtractorTreatsNoMetadataAsANormalEmptyResult(t *testing.T) {
	got, err := NewStillExtractor().Extract(context.Background(), "testdata/no-exif.jpg", ".jpg")
	if err != nil {
		t.Fatal(err)
	}
	if !extractedEmpty(got) || got.CapturedSource != "" || got.GPSSource != "" {
		t.Fatalf("empty fixture metadata = %+v", got)
	}
}

func TestStillExtractorCrossChecksSuspiciousHEICResults(t *testing.T) {
	want := Extracted{CapturedLocal: "2024-01-02 03:04:05", CapturedSource: "exif"}
	reader := NewStillExtractor()
	reader.primary = func(context.Context, string) (stillReadResult, error) {
		return stillReadResult{}, nil
	}
	calls := 0
	reader.crosscheck = func(context.Context, string, string) (stillReadResult, error) {
		calls++
		return stillReadResult{extracted: want, tagCount: 4}, nil
	}
	got, err := reader.Extract(context.Background(), "broken-order.heic", ".heic")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || got.CapturedLocal != want.CapturedLocal {
		t.Fatalf("calls/result = %d/%+v", calls, got)
	}
}

func TestStillExtractorCrossChecksRealHEICWithILOCBeforeIINF(t *testing.T) {
	path := heicWithILOCBeforeIINF(t, "testdata/iphone.heic")
	reader := NewStillExtractor()
	primary, err := reader.readPrimary(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if primary.tagCount != 0 {
		t.Fatalf("primary tag count = %d, want 0 for the known box-order failure", primary.tagCount)
	}

	got, err := reader.Extract(context.Background(), path, ".heic")
	if err != nil {
		t.Fatal(err)
	}
	if got.CapturedLocal != "2026-02-19 14:22:37.146" || got.Lat == nil || got.Lon == nil {
		t.Fatalf("cross-checked metadata = %+v", got)
	}
	assertClose(t, "latitude", *got.Lat, 37.9363333333333)
	assertClose(t, "longitude", *got.Lon, -122.365288888889)
}

func TestStillExtractorDistrustsZeroCoordinatesFromAppleMakerNote(t *testing.T) {
	zero := 0.0
	reader := NewStillExtractor()
	reader.primary = func(context.Context, string) (stillReadResult, error) {
		return stillReadResult{
			extracted:     Extracted{Lat: &zero, Lon: &zero, GPSSource: "exif"},
			tagCount:      10,
			hasAppleMaker: true,
		}, nil
	}
	calls := 0
	reader.crosscheck = func(context.Context, string, string) (stillReadResult, error) {
		calls++
		return stillReadResult{extracted: Extracted{Lat: &zero, Lon: &zero, GPSSource: "exif"}, tagCount: 12}, nil
	}
	got, err := reader.Extract(context.Background(), "gulf.heic", ".heic")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || got.Lat == nil || got.Lon == nil {
		t.Fatalf("cross-check did not preserve real zero coordinates: calls=%d result=%+v", calls, got)
	}
}

func TestStillExtractorHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewStillExtractor().Extract(ctx, "testdata/iphone.heic", ".heic")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

type bmffBox struct {
	start int
	end   int
	type4 string
}

func heicWithILOCBeforeIINF(t *testing.T, source string) string {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	top, err := bmffBoxes(data, 0, len(data))
	if err != nil {
		t.Fatal(err)
	}
	var meta bmffBox
	for _, box := range top {
		if box.type4 == "meta" {
			meta = box
			break
		}
	}
	if meta.end == 0 {
		t.Fatal("fixture has no top-level meta box")
	}
	header := 8
	if binary.BigEndian.Uint32(data[meta.start:meta.start+4]) == 1 {
		header = 16
	}
	childrenStart := meta.start + header + 4 // meta is a FullBox.
	children, err := bmffBoxes(data, childrenStart, meta.end)
	if err != nil {
		t.Fatal(err)
	}
	iinf, iloc := -1, -1
	for index, box := range children {
		switch box.type4 {
		case "iinf":
			iinf = index
		case "iloc":
			iloc = index
		}
	}
	if iinf < 0 || iloc < 0 {
		t.Fatalf("fixture meta children omit iinf or iloc: iinf=%d iloc=%d", iinf, iloc)
	}
	if iloc > iinf {
		children[iinf], children[iloc] = children[iloc], children[iinf]
	}

	reordered := make([]byte, 0, len(data))
	reordered = append(reordered, data[:childrenStart]...)
	for _, box := range children {
		reordered = append(reordered, data[box.start:box.end]...)
	}
	reordered = append(reordered, data[meta.end:]...)
	path := filepath.Join(t.TempDir(), "iloc-before-iinf.heic")
	if err := os.WriteFile(path, reordered, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func bmffBoxes(data []byte, start, end int) ([]bmffBox, error) {
	var boxes []bmffBox
	for cursor := start; cursor < end; {
		if end-cursor < 8 {
			return nil, fmt.Errorf("short BMFF box header at %d", cursor)
		}
		size := uint64(binary.BigEndian.Uint32(data[cursor : cursor+4]))
		header := 8
		if size == 1 {
			if end-cursor < 16 {
				return nil, fmt.Errorf("short extended BMFF box header at %d", cursor)
			}
			size = binary.BigEndian.Uint64(data[cursor+8 : cursor+16])
			header = 16
		} else if size == 0 {
			size = uint64(end - cursor)
		}
		if size < uint64(header) || size > uint64(end-cursor) {
			return nil, fmt.Errorf("invalid BMFF box size %d at %d", size, cursor)
		}
		next := cursor + int(size)
		boxes = append(boxes, bmffBox{start: cursor, end: next, type4: string(data[cursor+4 : cursor+8])})
		cursor = next
	}
	return boxes, nil
}
