package daemon

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withDestWriter installs a temp-file writer wrapper for the duration of a
// test. It is the seam that lets a test simulate a filesystem which
// acknowledges bytes it does not keep, and a process that dies mid-copy.
func withDestWriter(t *testing.T, wrap func(f *os.File) io.Writer) {
	t.Helper()
	orig := destWriter
	t.Cleanup(func() { destWriter = orig })
	destWriter = wrap
}

// siblings lists the base names next to path, so a test can prove that the
// only thing a failed move left behind is a temp file nothing references.
func siblings(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestMoveFileShouldRefuseTheMoveWhenTheDestinationIsOccupied(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	os.WriteFile(src, []byte("the version coldstore archived in september"), 0o644)
	os.WriteFile(dst, []byte("a newer copy syncthing brought back"), 0o644)

	err := moveFile(src, dst, testLogger())
	if !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("error = %v, want ErrDestinationExists", err)
	}

	got, _ := os.ReadFile(dst)
	if string(got) != "a newer copy syncthing brought back" {
		t.Errorf("the occupant was modified: %q", got)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("the source must survive a refused move: %v", err)
	}
}

func TestCrossDeviceMoveShouldLeaveNothingAtTheDestinationWhenTheProcessDiesMidCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "photo.jpg")
	dst := filepath.Join(dir, "archived", "photo.jpg")
	os.MkdirAll(filepath.Dir(dst), 0o755)

	content := make([]byte, 512<<10)
	for i := range content {
		content[i] = byte(i)
	}
	os.WriteFile(src, content, 0o644)

	// A hard kill is a process that stops between two instructions: no
	// deferred cleanup, no error path, no catalog write. A panic from inside
	// the copy is the closest a test can get, and it is strictly harsher than
	// SIGKILL because it also proves nothing in the unwinding path is needed
	// to keep the destination path clean.
	withDestWriter(t, func(f *os.File) io.Writer {
		return &killAfterWriter{f: f, after: 3}
	})

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the injected kill did not fire")
			}
		}()
		crossDeviceMove(src, dst, testLogger())
	}()

	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("a partially written file is visible at the destination path; " +
			"syncthing would propagate that truncation to every device")
	}
	got, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("the source must still be there: %v", err)
	}
	if len(got) != len(content) {
		t.Errorf("source is %d bytes, want %d", len(got), len(content))
	}

	// Whatever landed is at the temp sibling, which no catalog row references
	// and which startup recovery deletes.
	for _, name := range siblings(t, dst) {
		if !strings.HasSuffix(name, tmpSuffix) {
			t.Errorf("unexpected leftover %q next to the destination", name)
		}
	}
}

// killAfterWriter panics after a number of writes, simulating a process that
// dies part-way through a copy.
type killAfterWriter struct {
	f     *os.File
	after int
	seen  int
}

func (k *killAfterWriter) Write(p []byte) (int, error) {
	k.seen++
	if k.seen > k.after {
		panic("simulated hard kill mid-copy")
	}
	return k.f.Write(p)
}

func TestCrossDeviceMoveShouldFailWhenTheFilesystemKeepsFewerBytesThanItAcknowledged(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")
	content := []byte("every byte of this matters, especially the last one")
	os.WriteFile(src, content, 0o644)

	withDestWriter(t, func(f *os.File) io.Writer { return &dropLastByteWriter{f: f} })

	err := crossDeviceMove(src, dst, testLogger())
	if err == nil {
		t.Fatal("a copy that did not land intact must fail the move")
	}

	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("the unverified copy must not be committed to the destination")
	}
	if _, err := os.Stat(dst + tmpSuffix); !os.IsNotExist(err) {
		t.Error("the temp file must be cleaned up when verification fails")
	}
	got, readErr := os.ReadFile(src)
	if readErr != nil || string(got) != string(content) {
		t.Errorf("the source must survive intact, got %q err %v", got, readErr)
	}
}

// dropLastByteWriter reports every byte as written but never stores the last
// one it was given, which is exactly what the re-read verification exists to
// catch: a hash computed on the way past cannot see this.
type dropLastByteWriter struct {
	f       *os.File
	pending []byte
}

func (d *dropLastByteWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(d.pending) > 0 {
		if _, err := d.f.Write(d.pending); err != nil {
			return 0, err
		}
	}
	if _, err := d.f.Write(p[:len(p)-1]); err != nil {
		return 0, err
	}
	d.pending = append(d.pending[:0], p[len(p)-1])
	return len(p), nil
}

func TestCrossDeviceMoveShouldPreserveMtimeAndAtimeWhenTheFileIsCopied(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old.jpg")
	dst := filepath.Join(dir, "cold", "old.jpg")
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.WriteFile(src, []byte("a photo from 2019"), 0o644)

	mtime := time.Date(2019, 6, 12, 9, 30, 0, 0, time.UTC)
	atime := time.Date(2020, 1, 2, 8, 0, 0, 0, time.UTC)
	if err := os.Chtimes(src, atime, mtime); err != nil {
		t.Fatal(err)
	}

	if err := crossDeviceMove(src, dst, testLogger()); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().UTC().Equal(mtime) {
		// A re-dated copy is propagated by syncthing and then written over
		// files.file_mtime by the next scan, destroying the only record of
		// when the photo was taken.
		t.Errorf("mtime = %v, want %v", info.ModTime().UTC(), mtime)
	}
	if got := identityFromInfo(info).atime.UTC(); !got.Equal(atime) {
		// atime is half of OffloadCandidates' ordering key.
		t.Errorf("atime = %v, want %v", got, atime)
	}
}

func TestCrossDeviceMoveShouldKeepBothCopiesWhenTheSourceIsRewrittenDuringTheMove(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "video.mp4")
	dst := filepath.Join(dir, "cold", "video.mp4")
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.WriteFile(src, []byte("the version being archived"), 0o644)

	// Syncthing updates a file by renaming a temp over it, so the path can be
	// rebound to a different inode at any moment during a long copy. Rebind it
	// from inside the fsync hook: the copy is finished but the source has not
	// been re-stat'd yet.
	newer := filepath.Join(dir, ".syncthing.video.mp4.tmp")
	os.WriteFile(newer, []byte("the edit made on the phone this morning"), 0o644)
	orig := syncFile
	t.Cleanup(func() { syncFile = orig })
	syncFile = func(f *os.File) error {
		if err := orig(f); err != nil {
			return err
		}
		return os.Rename(newer, src)
	}

	err := crossDeviceMove(src, dst, testLogger())
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("error = %v, want ErrSourceChanged", err)
	}

	got, readErr := os.ReadFile(src)
	if readErr != nil {
		t.Fatalf("the rewritten source must not be deleted: %v", readErr)
	}
	if string(got) != "the edit made on the phone this morning" {
		t.Errorf("source content = %q, want the newer version", got)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("the stale copy must not stay at the destination path")
	}
	aside, err := os.ReadFile(dst + supersededSuffix)
	if err != nil {
		t.Fatalf("the copied bytes must be kept aside, not deleted: %v", err)
	}
	if string(aside) != "the version being archived" {
		t.Errorf("superseded copy = %q", aside)
	}
}

func TestCrossDeviceMoveShouldSucceedWhenAPreviousKillLeftATempFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	os.WriteFile(src, []byte("complete content"), 0o644)
	os.WriteFile(dst+tmpSuffix, []byte("half a fi"), 0o644)

	if err := crossDeviceMove(src, dst, testLogger()); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "complete content" {
		t.Errorf("destination = %q, err %v", got, err)
	}
	if _, err := os.Stat(dst + tmpSuffix); !os.IsNotExist(err) {
		t.Error("the stale temp file should have been replaced, not left behind")
	}
}

func TestCrossDeviceMoveShouldNotCreateTheDestinationPathUntilTheCopyIsVerified(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")
	os.WriteFile(src, make([]byte, 256<<10), 0o644)

	sawDestination := false
	withDestWriter(t, func(f *os.File) io.Writer {
		return &observingWriter{f: f, onWrite: func() {
			if _, err := os.Lstat(dst); err == nil {
				sawDestination = true
			}
		}}
	})

	if err := crossDeviceMove(src, dst, testLogger()); err != nil {
		t.Fatal(err)
	}
	if sawDestination {
		t.Error("the destination path existed while the copy was still running")
	}
}

type observingWriter struct {
	f       *os.File
	onWrite func()
}

func (o *observingWriter) Write(p []byte) (int, error) {
	o.onWrite()
	return o.f.Write(p)
}

func TestIsPrefixOfShouldTellATruncatedCopyFromADifferentFile(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "full")
	partial := filepath.Join(dir, "partial")
	other := filepath.Join(dir, "other")
	os.WriteFile(full, []byte("abcdefghij"), 0o644)
	os.WriteFile(partial, []byte("abcd"), 0o644)
	os.WriteFile(other, []byte("zzzz"), 0o644)

	if ok, err := isPrefixOf(partial, full); err != nil || !ok {
		t.Errorf("truncated copy: ok = %v, err = %v", ok, err)
	}
	if ok, err := isPrefixOf(other, full); err != nil || ok {
		t.Errorf("different file: ok = %v, err = %v", ok, err)
	}
	if ok, err := isPrefixOf(full, partial); err != nil || ok {
		t.Errorf("longer than the original: ok = %v, err = %v", ok, err)
	}
}

func TestSetAsideShouldNotOverwriteACopyThatWasAlreadySetAside(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "photo.jpg")
	os.WriteFile(dst+supersededSuffix, []byte("the copy set aside last week"), 0o644)

	aside := asidePath(dst)
	if aside == dst+supersededSuffix {
		t.Fatalf("asidePath returned the occupied slot %q", aside)
	}
	mustStillSay(t, dst+supersededSuffix, "the copy set aside last week")
}

func mustStillSay(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}

// The invariant-10 hole a reviewer reproduced: moveFile's os.Lstat runs once,
// before the copy starts, and the commit used to be an unconditional
// os.Rename. On a real deployment the sync folder and the cold pool are
// different filesystems, so a restore of a 2 GB video leaves a window as long
// as the copy in which Syncthing can deliver a new version of that filename --
// and those bytes were destroyed silently, with no .coldstore-superseded
// sibling, no event and a success return.
func TestCrossDeviceMoveShouldNotDestroyAFileThatAppearsAtTheDestinationDuringTheCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jpg")
	dst := filepath.Join(dir, "photo.jpg")
	const archived = "the version coldstore archived in september"
	const delivered = "the newer copy syncthing just delivered"
	os.WriteFile(src, []byte(archived), 0o644)

	// The interloper lands after the copy has begun and before the commit,
	// which is exactly where the real window is.
	withDestWriter(t, func(f *os.File) io.Writer {
		return writerFunc(func(p []byte) (int, error) {
			if _, err := os.Lstat(dst); os.IsNotExist(err) {
				os.WriteFile(dst, []byte(delivered), 0o644)
			}
			return f.Write(p)
		})
	})

	err := crossDeviceMove(src, dst, testLogger())
	if !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("error = %v, want ErrDestinationExists", err)
	}

	got, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("reading the destination: %v", readErr)
	}
	if string(got) != delivered {
		t.Fatalf("DATA LOSS: the destination holds %q, want the delivered copy %q", got, delivered)
	}
	// The source never moved, so the archived bytes are still referenced and
	// the temp sibling holds nothing the source does not.
	kept, err := os.ReadFile(src)
	if err != nil || string(kept) != archived {
		t.Fatalf("the source must survive a refused commit: %v %q", err, kept)
	}
	for _, name := range siblings(t, dst) {
		if strings.HasSuffix(name, tmpSuffix) {
			t.Errorf("a temp sibling was left behind: %s", name)
		}
	}
}

// The same collision, but the source is gone by the time the commit fails --
// somebody deleted it mid-copy. Now the temp file is the ONLY copy of those
// bytes, and invariant 9 says keep them rather than tidy them away.
func TestCrossDeviceMoveShouldKeepTheCopyAsideWhenTheDestinationIsTakenAndTheSourceIsGone(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jpg")
	dst := filepath.Join(dir, "photo.jpg")
	const archived = "the only remaining copy of this photograph"
	os.WriteFile(src, []byte(archived), 0o644)

	withDestWriter(t, func(f *os.File) io.Writer {
		return writerFunc(func(p []byte) (int, error) {
			if _, err := os.Lstat(dst); os.IsNotExist(err) {
				os.WriteFile(dst, []byte("a different file entirely"), 0o644)
				os.Remove(src)
			}
			return f.Write(p)
		})
	})

	if err := crossDeviceMove(src, dst, testLogger()); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("error = %v, want ErrDestinationExists", err)
	}

	var found string
	for _, name := range siblings(t, dst) {
		if strings.Contains(name, supersededSuffix) {
			found = name
		}
		if strings.HasSuffix(name, tmpSuffix) {
			t.Errorf("a temp sibling was left behind: %s", name)
		}
	}
	if found == "" {
		t.Fatalf("the only copy of the bytes was deleted; siblings = %v", siblings(t, dst))
	}
	got, _ := os.ReadFile(filepath.Join(dir, found))
	if string(got) != archived {
		t.Errorf("the set-aside copy holds %q, want %q", got, archived)
	}
}

// The same-device fast path has the same hole with a microsecond window. It is
// closed the same way -- link(2) rather than rename(2) -- so assert it here
// rather than trusting that nobody puts the rename back.
func TestMoveFileShouldNotReplaceAnOccupantOnTheSameDevice(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jpg")
	dst := filepath.Join(dir, "photo.jpg")
	os.WriteFile(src, []byte("archived"), 0o644)
	os.WriteFile(dst, []byte("on the device"), 0o644)

	if err := moveFile(src, dst, testLogger()); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("error = %v, want ErrDestinationExists", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "on the device" {
		t.Errorf("the occupant was replaced: %q", got)
	}
}

// A successful same-device move must still leave exactly one name for the
// file: link-then-unlink is not allowed to forget the unlink.
func TestMoveFileShouldLeaveOnlyTheDestinationAfterASameDeviceMove(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jpg")
	dst := filepath.Join(dir, "photo.jpg")
	os.WriteFile(src, []byte("bytes"), 0o644)

	if err := moveFile(src, dst, testLogger()); err != nil {
		t.Fatalf("moveFile: %v", err)
	}
	if _, err := os.Lstat(src); !os.IsNotExist(err) {
		t.Errorf("the source name survived the move: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "bytes" {
		t.Errorf("destination = %q, %v", got, err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
