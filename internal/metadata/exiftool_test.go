package metadata

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestExifToolProbeUsesOneResidentProcessForRealHEIC(t *testing.T) {
	if _, err := exec.LookPath("exiftool"); err != nil {
		t.Skip("exiftool not installed")
	}
	probe := NewExifToolProbe("exiftool", 2*time.Second)
	t.Cleanup(func() {
		if err := probe.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	first, err := probe.Probe(context.Background(), "testdata/iphone.heic")
	if err != nil {
		t.Fatal(err)
	}
	pid := probe.cmd.Process.Pid
	second, err := probe.Probe(context.Background(), "testdata/iphone.heic")
	if err != nil {
		t.Fatal(err)
	}
	if probe.cmd.Process.Pid != pid {
		t.Fatalf("ExifTool PID changed from %d to %d", pid, probe.cmd.Process.Pid)
	}
	for _, got := range []Extracted{first, second} {
		if got.ContentID != "F61FDA97-19FB-4D2E-9173-3B6128AD0FD4" {
			t.Errorf("ContentID = %q", got.ContentID)
		}
		if got.CapturedLocal != "2026-02-19 14:22:37.146" {
			t.Errorf("CapturedLocal = %q", got.CapturedLocal)
		}
	}
}

func TestDecodeExifToolResponsePreservesLeadingZeroAndCoordinateZero(t *testing.T) {
	got, err := decodeExifToolResponse([]byte(`[{"DateTimeOriginal":"2024:06:14 18:32:07","SubSecTimeOriginal":"05","GPSLatitude":0,"GPSLongitude":0}]`))
	if err != nil {
		t.Fatal(err)
	}
	if got.CapturedLocal != "2024-06-14 18:32:07.05" {
		t.Errorf("CapturedLocal = %q", got.CapturedLocal)
	}
	if got.Lat == nil || got.Lon == nil || *got.Lat != 0 || *got.Lon != 0 {
		t.Fatalf("zero coordinates lost: %v/%v", got.Lat, got.Lon)
	}
}

func TestExifToolProbeKillsResidentProcessAtDeadline(t *testing.T) {
	binary := writeExecutable(t, `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    -execute*) sleep 10 ;;
  esac
done
`)
	probe := NewExifToolProbe(binary, 50*time.Millisecond)
	started := time.Now()
	_, err := probe.Probe(context.Background(), "testdata/iphone.heic")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("deadline took %s; resident process group was not killed", elapsed)
	}
	if probe.cmd != nil {
		t.Fatal("cancelled resident process was retained")
	}
}

func TestExifToolProbeHonorsDeadlineWhileQueued(t *testing.T) {
	binary := writeExecutable(t, `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    -execute*)
      id=${line#-execute}
      sleep 0.3
      printf '[]\n{ready%s}\n' "$id"
      ;;
  esac
done
`)
	probe := NewExifToolProbe(binary, 2*time.Second)
	t.Cleanup(func() { _ = probe.Close() })
	firstDone := make(chan error, 1)
	go func() {
		_, err := probe.Probe(context.Background(), "testdata/iphone.heic")
		firstDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for len(probe.gate) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(probe.gate) == 0 {
		t.Fatal("first probe never acquired resident process")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := probe.Probe(ctx, "testdata/iphone.heic")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("queued deadline took %s", elapsed)
	}
	<-firstDone
}
