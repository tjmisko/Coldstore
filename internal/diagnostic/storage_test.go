package diagnostic

import (
	"errors"
	"testing"
)

func TestInspectBtrfsShouldClassifyMissingDevicesWithoutRepeatingDeviceOutput(t *testing.T) {
	output := []byte("Total devices 2 FS bytes used 1\n*** Some devices missing\ndevid 2 path /private/device")
	got := classifyBtrfsOutput(output, errors.New("exit status 1"))
	if got.Status != "degraded" {
		t.Fatalf("status = %q, want degraded", got.Status)
	}
	if got.Detail == "" || got.Detail == string(output) {
		t.Errorf("detail = %q, want a generic redacted explanation", got.Detail)
	}
}

func TestInspectBtrfsShouldReportUnknownWhenTheToolCannotRun(t *testing.T) {
	got := classifyBtrfsOutput(nil, errors.New("executable file not found"))
	if got.Status != "unknown" {
		t.Errorf("status = %q, want unknown", got.Status)
	}
}
