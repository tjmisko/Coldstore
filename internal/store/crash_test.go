package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/tjmisko/coldstore/internal/crashpoint"
	"github.com/tjmisko/coldstore/internal/fsx"
)

const (
	envHelperRoot   = "COLDSTORE_STORE_HELPER_ROOT"
	envHelperSource = "COLDSTORE_STORE_HELPER_SOURCE"
	// envHelperSeedCorrupt makes the helper first store a damaged blob for the
	// source bytes, so the Put it then runs takes the repair path.
	envHelperSeedCorrupt = "COLDSTORE_STORE_HELPER_SEED_CORRUPT"
)

// seedCorruptBlob stores the source bytes, then flips one byte of the blob.
func seedCorruptBlob(t *testing.T, root, sourcePath string) {
	t.Helper()
	content, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := Open(fsx.OS(), root)
	if err != nil {
		t.Fatal(err)
	}
	h := mustPut(t, disk, content)
	damage(t, disk.Path(h), func(b []byte) []byte { b[len(b)/2] ^= 0xff; return b })
}

// TestHelperPutProcess is the child side of the crash tests. It does nothing
// unless the parent set the helper environment.
func TestHelperPutProcess(t *testing.T) {
	root := os.Getenv(envHelperRoot)
	if root == "" {
		t.Skip("helper process only")
	}
	if os.Getenv(envHelperSeedCorrupt) != "" {
		seedCorruptBlob(t, root, os.Getenv(envHelperSource))
	}
	if _, err := crashpoint.ArmFromEnv(); err != nil {
		t.Fatalf("ArmFromEnv: %v", err)
	}
	disk, err := Open(fsx.OS(), root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	src, err := os.Open(os.Getenv(envHelperSource))
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	defer src.Close()
	if _, _, err := disk.Put(context.Background(), src, nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Reaching here means the armed point was never hit: the parent fails the case.
}

const repairPointPrefix = "store.repair."

func storePutPoints() []string {
	var points []string
	for _, name := range crashpoint.Names() {
		if strings.HasPrefix(name, "store.put.") {
			points = append(points, name)
		}
	}
	return points
}

func TestPutCrashPoints(t *testing.T) {
	points := storePutPoints()
	if len(points) != 7 {
		t.Fatalf("registered store.put points = %v, want the 7 of contract C2", points)
	}
	if repair := repairPoints(); len(repair) != 3 {
		t.Fatalf("registered %s points = %v, want 3", repairPointPrefix, repair)
	}
	content := bytes.Repeat([]byte("crash me "), 1000)
	wantHash := hashOf(content)

	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			root := t.TempDir()
			sourcePath := filepath.Join(t.TempDir(), "source.bin")
			if err := os.WriteFile(sourcePath, content, 0o644); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperPutProcess$", "-test.v")
			cmd.Env = append(os.Environ(),
				envHelperRoot+"="+root,
				envHelperSource+"="+sourcePath,
				crashpoint.EnvPoint+"="+point,
			)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("child did not die (point %s never reached?): err=%v\n%s", point, err, output)
			}
			status, ok := exitErr.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child did not die by SIGKILL: %v\n%s", exitErr, output)
			}

			t.Run("should leave either no blob or a blob that verifies when the process dies at "+point, func(t *testing.T) {
				disk, err := Open(fsx.OS(), root)
				if err != nil {
					t.Fatalf("Open after crash: %v", err)
				}
				blobs := walkAll(t, disk)
				if len(blobs) > 1 {
					t.Fatalf("%d blobs", len(blobs))
				}
				for h := range blobs {
					if h != wantHash {
						t.Errorf("unexpected blob %s", h)
					}
					if err := disk.Verify(context.Background(), h); err != nil {
						t.Errorf("surviving blob does not verify: %v", err)
					}
				}
				if names := listDir(t, filepath.Join(root, "tmp")); len(names) != 0 {
					t.Errorf("Open left staging files: %v", names)
				}
			})

			t.Run("should complete and verify when Put is re-run after a crash at "+point, func(t *testing.T) {
				disk, err := Open(fsx.OS(), root)
				if err != nil {
					t.Fatalf("Open after crash: %v", err)
				}
				h, size, err := disk.Put(context.Background(), bytes.NewReader(content), nil)
				if err != nil || h != wantHash || size != int64(len(content)) {
					t.Fatalf("re-run Put = %s, %d, %v", h, size, err)
				}
				if err := disk.Verify(context.Background(), h); err != nil {
					t.Errorf("Verify: %v", err)
				}
				if n := len(walkAll(t, disk)); n != 1 {
					t.Errorf("%d blobs, want 1", n)
				}
				if names := listDir(t, filepath.Join(root, "tmp")); len(names) != 0 {
					t.Errorf("tmp not empty: %v", names)
				}
			})

			t.Run("should not have altered the source file when the process dies at "+point, func(t *testing.T) {
				got, err := os.ReadFile(sourcePath)
				if err != nil || !bytes.Equal(got, content) {
					t.Errorf("source changed (%v)", err)
				}
			})
		})
	}
}

func repairPoints() []string {
	var points []string
	for _, name := range crashpoint.Names() {
		if strings.HasPrefix(name, repairPointPrefix) {
			points = append(points, name)
		}
	}
	return points
}

func TestPutRepairCrashPoints(t *testing.T) {
	content := bytes.Repeat([]byte("repair me "), 1000)
	wantHash := hashOf(content)

	for _, point := range repairPoints() {
		t.Run(point, func(t *testing.T) {
			root := t.TempDir()
			sourcePath := filepath.Join(t.TempDir(), "source.bin")
			if err := os.WriteFile(sourcePath, content, 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperPutProcess$", "-test.v")
			cmd.Env = append(os.Environ(),
				envHelperRoot+"="+root,
				envHelperSource+"="+sourcePath,
				envHelperSeedCorrupt+"=1",
				crashpoint.EnvPoint+"="+point,
			)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("child did not die (point %s never reached?): err=%v\n%s", point, err, output)
			}
			status, ok := exitErr.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child did not die by SIGKILL: %v\n%s", exitErr, output)
			}

			t.Run("should leave the old bad blob or the new good one, never nothing, when the process dies at "+point, func(t *testing.T) {
				disk, err := Open(fsx.OS(), root)
				if err != nil {
					t.Fatalf("Open after crash: %v", err)
				}
				blobs := walkAll(t, disk)
				if len(blobs) != 1 {
					t.Fatalf("%d blobs, want exactly 1", len(blobs))
				}
				verifyErr := disk.Verify(context.Background(), wantHash)
				var corrupt *CorruptError
				if verifyErr != nil && !errors.As(verifyErr, &corrupt) {
					t.Errorf("blob is neither good nor corrupt: %v", verifyErr)
				}
				if point != repairPointPrefix+"begin" && verifyErr != nil {
					t.Errorf("rename had happened but the blob does not verify: %v", verifyErr)
				}
			})

			t.Run("should end with a verified blob when Put is re-run after a crash at "+point, func(t *testing.T) {
				disk, err := Open(fsx.OS(), root)
				if err != nil {
					t.Fatalf("Open after crash: %v", err)
				}
				if _, _, err := disk.Put(context.Background(), bytes.NewReader(content), nil); err != nil {
					t.Fatalf("re-run Put: %v", err)
				}
				if err := disk.Verify(context.Background(), wantHash); err != nil {
					t.Errorf("Verify: %v", err)
				}
				if names := listDir(t, filepath.Join(root, "tmp")); len(names) != 0 {
					t.Errorf("tmp not empty: %v", names)
				}
			})
		})
	}
}
