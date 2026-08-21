package daemon

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// vanishOnFirstWrite makes the source disappear the moment the copy starts, so
// the temp sibling becomes the only copy of those bytes while the move is
// still in flight. That is exactly what a delete on another Syncthing device
// does to a file coldstore has already opened.
func vanishOnFirstWrite(t *testing.T, src string) {
	t.Helper()
	orig := destWriter
	t.Cleanup(func() { destWriter = orig })
	destWriter = func(f *os.File) io.Writer {
		return writerFunc(func(p []byte) (int, error) {
			if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
				t.Errorf("removing the source mid-copy: %v", err)
			}
			return f.Write(p)
		})
	}
}

// failDirSync makes every directory fsync fail, which aborts crossDeviceMove at
// step 4 -- after the bytes have been written, before anything is committed.
func failDirSync(t *testing.T, msg string) {
	t.Helper()
	orig := syncDirPath
	t.Cleanup(func() { syncDirPath = orig })
	syncDirPath = func(string) error { return errIO(msg) }
}

type errIO string

func (e errIO) Error() string { return string(e) }

func newMoveFixture(t *testing.T, content string) (src, dst string) {
	t.Helper()
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "sync")
	dstDir := filepath.Join(dir, "cold")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src = filepath.Join(srcDir, "photo.jpg")
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return src, filepath.Join(dstDir, "photo.jpg")
}

// Defect 10. Every failure exit after the copy unlinked the temp sibling
// unconditionally. In the one case where the source had vanished mid-copy that
// temp was the only copy left of the file, and coldstore deleted it -- the
// single place the move primitive broke invariant 9, which it otherwise keeps
// without exception.
func TestCrossDeviceMoveShouldKeepTheCopyWhenTheSourceVanishedAndTheMoveThenFails(t *testing.T) {
	const content = "the only remaining copy of this photograph"
	src, dst := newMoveFixture(t, content)

	vanishOnFirstWrite(t, src)
	failDirSync(t, "simulated fsync failure")

	err := crossDeviceMove(src, dst, testLogger())
	if err == nil {
		t.Fatal("the move was supposed to fail")
	}
	if !strings.Contains(err.Error(), "syncing destination directory") {
		t.Fatalf("err = %v, want the directory-fsync failure", err)
	}

	if _, statErr := os.Lstat(src); !os.IsNotExist(statErr) {
		t.Fatalf("the source was supposed to vanish during the copy, got %v", statErr)
	}
	if _, statErr := os.Lstat(dst + tmpSuffix); !os.IsNotExist(statErr) {
		t.Errorf("the temp name should not survive, got %v", statErr)
	}

	aside := dst + supersededSuffix
	got, readErr := os.ReadFile(aside)
	if readErr != nil {
		t.Fatalf("the last copy of the file was destroyed; expected it kept at %s: %v",
			aside, readErr)
	}
	if string(got) != content {
		t.Errorf("kept copy = %q, want %q", got, content)
	}
}

// The other half of the same rule, and the reason it is a check and not an
// unconditional keep: while the source is still there and unchanged the temp
// holds no bytes the source does not, so it is redundant and must go. A move
// that left one behind on every transient failure would litter cold storage
// with copies Syncthing then has an opinion about.
func TestCrossDeviceMoveShouldRemoveTheTempWhenTheMoveFailsAndTheSourceIsIntact(t *testing.T) {
	src, dst := newMoveFixture(t, "still safely in the folder")

	failDirSync(t, "simulated fsync failure")

	if err := crossDeviceMove(src, dst, testLogger()); err == nil {
		t.Fatal("the move was supposed to fail")
	}

	if _, err := os.Lstat(src); err != nil {
		t.Fatalf("the source must be untouched: %v", err)
	}
	if _, err := os.Lstat(dst + tmpSuffix); !os.IsNotExist(err) {
		t.Errorf("the temp sibling should be gone, got %v", err)
	}
	if _, err := os.Lstat(dst + supersededSuffix); !os.IsNotExist(err) {
		t.Errorf("nothing should be set aside when the source is intact, got %v", err)
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Errorf("nothing should have been committed, got %v", err)
	}
}

// The same rule at a different exit, so the fix is not one failure deep: a
// copy that dies part-way through with its source already gone still holds the
// only bytes anybody has. They are incomplete, and .coldstore-superseded is a
// name no catalog row references and no restore serves, so the owner decides
// whether a damaged file beats no file -- coldstore does not decide it for
// them by calling unlink.
func TestCrossDeviceMoveShouldKeepThePartialCopyWhenTheSourceVanishedAndTheCopyDies(t *testing.T) {
	content := strings.Repeat("photographic evidence, 1998. ", 8000)
	src, dst := newMoveFixture(t, content)

	orig := destWriter
	t.Cleanup(func() { destWriter = orig })
	writes := 0
	destWriter = func(f *os.File) io.Writer {
		return writerFunc(func(p []byte) (int, error) {
			writes++
			if writes == 1 {
				if err := os.Remove(src); err != nil {
					t.Errorf("removing the source mid-copy: %v", err)
				}
			}
			if writes > 2 {
				return 0, errIO("simulated read/write failure part-way through")
			}
			return f.Write(p)
		})
	}

	err := crossDeviceMove(src, dst, testLogger())
	if err == nil {
		t.Fatal("the move was supposed to fail")
	}
	if !strings.Contains(err.Error(), "copying") {
		t.Fatalf("err = %v, want the copy failure", err)
	}

	if _, statErr := os.Lstat(dst + tmpSuffix); !os.IsNotExist(statErr) {
		t.Errorf("the temp name should not survive, got %v", statErr)
	}
	got, readErr := os.ReadFile(dst + supersededSuffix)
	if readErr != nil {
		t.Fatalf("the only surviving bytes were destroyed: %v", readErr)
	}
	if len(got) == 0 {
		t.Fatal("the kept copy is empty; there were bytes to keep")
	}
	if !strings.HasPrefix(content, string(got)) {
		t.Error("the kept copy is not a prefix of the original")
	}
}
