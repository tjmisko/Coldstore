package metadata

import (
	"testing"
	"time"
)

func TestEXIFCapturePreservesLeadingSubsecondAndUnknownOffset(t *testing.T) {
	got, err := parseEXIFCapture("2024:06:14 18:32:07", "05", "", "exif", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got.local != "2024-06-14 18:32:07.05" {
		t.Fatalf("local = %q", got.local)
	}
	if got.offset != nil {
		t.Fatalf("offset = %d, want nil", *got.offset)
	}
}

func TestEXIFCaptureDerivesOffsetFromGPSUTC(t *testing.T) {
	gpsUTC := time.Date(2024, time.June, 14, 16, 32, 8, 0, time.UTC)
	got, err := parseEXIFCapture("2024:06:14 18:32:07", "", "", "exif", gpsUTC)
	if err != nil {
		t.Fatal(err)
	}
	if got.offset == nil || *got.offset != 120 {
		t.Fatalf("offset = %v, want 120", got.offset)
	}
}

func TestQuickTimeCreationDateAcceptsAppleAndRFC3339Offsets(t *testing.T) {
	for _, value := range []string{
		"2026-03-14T15:09:26.007-0700",
		"2026-03-14T15:09:26.007-07:00",
	} {
		got, err := parseQuickTimeCapture(value)
		if err != nil {
			t.Fatal(err)
		}
		if got.local != "2026-03-14 15:09:26.007" {
			t.Errorf("%q local = %q", value, got.local)
		}
		if got.offset == nil || *got.offset != -420 {
			t.Errorf("%q offset = %v", value, got.offset)
		}
	}
}

func TestCapturePrecedence(t *testing.T) {
	candidates := []captureCandidate{
		{local: "original", source: "exif"},
		{local: "quicktime", source: "quicktime"},
		{local: "digitized", source: "exif_digitized"},
		{local: "mvhd", source: "quicktime_mvhd"},
	}
	for first := range candidates {
		values := make([]captureCandidate, len(candidates))
		copy(values[first:], candidates[first:])
		got := chooseCapture(values[0], values[1], values[2], values[3])
		if got.local != candidates[first].local {
			t.Fatalf("first populated candidate %d selected %q", first, got.local)
		}
	}
}

func TestMVHDFallbackRecordsUTCInsteadOfInventingLocalTime(t *testing.T) {
	got, err := parseMVHDCapture("2026-03-14T22:09:26Z", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.local != "2026-03-14 22:09:26" || got.offset == nil || *got.offset != 0 {
		t.Fatalf("mvhd = %+v", got)
	}
}
