package metadata

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestQuickTimeExtractorReadsTaggedMOVFixture(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	got, err := NewQuickTimeExtractor("ffprobe", 5*time.Second).Extract(context.Background(), "testdata/quicktime.mov", ".mov")
	if err != nil {
		t.Fatal(err)
	}
	if got.CapturedLocal != "2026-03-14 15:09:26" || got.CapturedOffset == nil || *got.CapturedOffset != -420 {
		t.Errorf("capture = %q offset %v", got.CapturedLocal, got.CapturedOffset)
	}
	if got.CapturedSource != "quicktime" {
		t.Errorf("CapturedSource = %q", got.CapturedSource)
	}
	if got.ContentID != "F61FDA97-19FB-4D2E-9173-3B6128AD0FD4" {
		t.Errorf("ContentID = %q", got.ContentID)
	}
	if got.Lat == nil || got.Lon == nil || got.AltitudeM == nil {
		t.Fatalf("location = %v/%v/%v", got.Lat, got.Lon, got.AltitudeM)
	}
	assertClose(t, "latitude", *got.Lat, 37.7749)
	assertClose(t, "longitude", *got.Lon, -122.4194)
	assertClose(t, "altitude", *got.AltitudeM, 52.3)
}

func TestQuickTimeExtractorKillsProcessGroupAtDeadline(t *testing.T) {
	binary := writeExecutable(t, "#!/bin/sh\nsleep 10\n")
	reader := NewQuickTimeExtractor(binary, 50*time.Millisecond)
	started := time.Now()
	_, err := reader.Extract(context.Background(), "ignored.mov", ".mov")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("deadline took %s; child process was not killed", elapsed)
	}
}

func TestQuickTimeExtractorHonorsCallerCancellation(t *testing.T) {
	binary := writeExecutable(t, "#!/bin/sh\nsleep 10\n")
	reader := NewQuickTimeExtractor(binary, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	started := time.Now()
	_, err := reader.Extract(ctx, "ignored.mov", ".mov")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancellation took %s; child process was not killed", elapsed)
	}
}

func writeExecutable(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
