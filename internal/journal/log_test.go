package journal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/fsx"
	"github.com/tjmisko/coldstore/internal/fsx/fsxtest"
)

var bg = context.Background()

func folderRec(name string) Record {
	return Record{By: "test", Type: TypeFolderDeclared, Folder: name, Kind: FolderKindCollection}
}

func openLog(t *testing.T, dir string, opts Options) *Log {
	t.Helper()
	l, err := Open(bg, fsx.OS(), dir, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func appendOne(t *testing.T, l *Log, name string) uint64 {
	t.Helper()
	seq, err := l.Append(bg, folderRec(name))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return seq
}

func replayAll(t *testing.T, l *Log, from uint64) []Record {
	t.Helper()
	var got []Record
	if err := l.Replay(bg, from, func(r Record) error { got = append(got, r); return nil }); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return got
}

func segmentPath(dir string, n int) string { return filepath.Join(dir, segmentName(n)) }

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// copyDir copies the flat journal directory src to a new temp directory.
func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "journal")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		writeFile(t, filepath.Join(dst, e.Name()), readFile(t, filepath.Join(src, e.Name())))
	}
	return dst
}

// encodeRecords builds the bytes of a segment from records with explicit
// Seq/Batch, for crafting damaged journals.
func encodeRecords(t *testing.T, recs ...Record) []byte {
	t.Helper()
	var buf []byte
	for _, rec := range recs {
		if rec.At.IsZero() {
			rec.At = time.Unix(1700000000, 0)
		}
		var err error
		if buf, err = appendLine(buf, rec); err != nil {
			t.Fatal(err)
		}
	}
	return buf
}

func seqRec(seq uint64) Record {
	r := folderRec("F")
	r.Seq, r.Batch = seq, seq
	return r
}

func TestAppendAndReplay(t *testing.T) {
	t.Run("should create FORMAT in an empty directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "journal")
		openLog(t, dir, Options{})
		if got := string(readFile(t, filepath.Join(dir, "FORMAT"))); got != "1\n" {
			t.Fatalf("FORMAT = %q", got)
		}
		if v, err := DiskFormatVersion(fsx.OS(), dir); err != nil || v != FormatVersion {
			t.Fatalf("DiskFormatVersion = %d, %v", v, err)
		}
	})
	t.Run("should report version 0 when the directory does not exist", func(t *testing.T) {
		v, err := DiskFormatVersion(fsx.OS(), filepath.Join(t.TempDir(), "nope"))
		if err != nil || v != 0 {
			t.Fatalf("got %d, %v", v, err)
		}
	})
	t.Run("should assign consecutive sequence numbers when records are appended", func(t *testing.T) {
		l := openLog(t, filepath.Join(t.TempDir(), "j"), Options{})
		for i := 1; i <= 5; i++ {
			if got := appendOne(t, l, fmt.Sprintf("F%d", i)); got != uint64(i) {
				t.Fatalf("append %d returned seq %d", i, got)
			}
		}
		recs := replayAll(t, l, 0)
		for i, rec := range recs {
			if rec.Seq != uint64(i+1) {
				t.Fatalf("record %d has seq %d", i, rec.Seq)
			}
			if rec.At.IsZero() || rec.At.Location() != time.UTC {
				t.Fatalf("record %d has At %v", i, rec.At)
			}
		}
		if l.LastSeq() != 5 {
			t.Fatalf("LastSeq = %d", l.LastSeq())
		}
	})
	t.Run("should stamp every record of one Append with the same batch", func(t *testing.T) {
		l := openLog(t, filepath.Join(t.TempDir(), "j"), Options{})
		appendOne(t, l, "first")
		last, err := l.Append(bg, folderRec("a"), folderRec("b"), folderRec("c"))
		if err != nil || last != 4 {
			t.Fatalf("Append = %d, %v", last, err)
		}
		recs := replayAll(t, l, 0)
		if recs[0].Batch != 1 {
			t.Fatalf("single record batch = %d", recs[0].Batch)
		}
		for _, rec := range recs[1:] {
			if rec.Batch != 4 {
				t.Fatalf("seq %d has batch %d, want 4", rec.Seq, rec.Batch)
			}
		}
	})
	t.Run("should return every record after fromSeq in order when replayed", func(t *testing.T) {
		l := openLog(t, filepath.Join(t.TempDir(), "j"), Options{})
		for i := 0; i < 6; i++ {
			appendOne(t, l, "F")
		}
		recs := replayAll(t, l, 3)
		if len(recs) != 3 || recs[0].Seq != 4 || recs[2].Seq != 6 {
			t.Fatalf("got %+v", recs)
		}
		if got := replayAll(t, l, 6); len(got) != 0 {
			t.Fatalf("replay past the end returned %d records", len(got))
		}
	})
	t.Run("should keep the same records when the log is closed and reopened", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		l := openLog(t, dir, Options{})
		appendOne(t, l, "a")
		appendOne(t, l, "b")
		_ = l.Close()
		again := openLog(t, dir, Options{})
		if again.LastSeq() != 2 || again.Torn() != nil {
			t.Fatalf("LastSeq %d, Torn %+v", again.LastSeq(), again.Torn())
		}
		if seq := appendOne(t, again, "c"); seq != 3 {
			t.Fatalf("next seq %d", seq)
		}
	})
	t.Run("should reject the whole call and write nothing when one record is invalid", func(t *testing.T) {
		l := openLog(t, filepath.Join(t.TempDir(), "j"), Options{})
		_, err := l.Append(bg, folderRec("ok"), Record{By: "x", Type: "bogus"})
		if !errors.Is(err, ErrUnknownType) {
			t.Fatalf("got %v", err)
		}
		if l.LastSeq() != 0 || len(replayAll(t, l, 0)) != 0 {
			t.Fatal("a rejected call left records behind")
		}
	})
	t.Run("should return the current last sequence when Append is given no records", func(t *testing.T) {
		l := openLog(t, filepath.Join(t.TempDir(), "j"), Options{})
		appendOne(t, l, "a")
		if seq, err := l.Append(bg); err != nil || seq != 1 {
			t.Fatalf("got %d, %v", seq, err)
		}
	})
	t.Run("should return an error when appending after Close", func(t *testing.T) {
		l := openLog(t, filepath.Join(t.TempDir(), "j"), Options{})
		_ = l.Close()
		if _, err := l.Append(bg, folderRec("a")); err == nil {
			t.Fatal("append after close succeeded")
		}
	})
	t.Run("should keep a caller supplied timestamp when At is set", func(t *testing.T) {
		l := openLog(t, filepath.Join(t.TempDir(), "j"), Options{Now: func() time.Time { return time.Unix(5, 0) }})
		explicit := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
		r := folderRec("a")
		r.At = explicit
		if _, err := l.Append(bg, r, folderRec("b")); err != nil {
			t.Fatal(err)
		}
		recs := replayAll(t, l, 0)
		if !recs[0].At.Equal(explicit) || !recs[1].At.Equal(time.Unix(5, 0)) {
			t.Fatalf("At values %v %v", recs[0].At, recs[1].At)
		}
	})
}

func TestRotation(t *testing.T) {
	t.Run("should start a new segment when the current one reaches SegmentBytes", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		l := openLog(t, dir, Options{SegmentBytes: 300})
		for i := 0; i < 10; i++ {
			appendOne(t, l, "F")
		}
		if _, err := os.Stat(segmentPath(dir, 2)); err != nil {
			t.Fatalf("no second segment: %v", err)
		}
		first := readFile(t, segmentPath(dir, 1))
		if len(first) < 300 || len(first) > 300+400 {
			t.Fatalf("first segment is %d bytes for SegmentBytes 300", len(first))
		}
	})
	t.Run("should never split a batch across segments", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		l := openLog(t, dir, Options{SegmentBytes: 1})
		for i := 0; i < 4; i++ {
			if _, err := l.Append(bg, folderRec("a"), folderRec("b"), folderRec("c")); err != nil {
				t.Fatal(err)
			}
		}
		for n := 1; n <= 4; n++ {
			lines := strings.Split(strings.TrimSuffix(string(readFile(t, segmentPath(dir, n))), "\n"), "\n")
			if len(lines) != 3 {
				t.Fatalf("segment %d holds %d lines, want one whole batch of 3", n, len(lines))
			}
			last, err := decodeLine([]byte(lines[2]))
			if err != nil || last.Seq != last.Batch {
				t.Fatalf("segment %d ends mid-batch: %+v %v", n, last, err)
			}
		}
	})
	t.Run("should replay across segment boundaries in order when the log has rotated", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		l := openLog(t, dir, Options{SegmentBytes: 1})
		for i := 0; i < 6; i++ {
			appendOne(t, l, "F")
		}
		recs := replayAll(t, l, 0)
		if len(recs) != 6 {
			t.Fatalf("got %d records", len(recs))
		}
		for i, rec := range recs {
			if rec.Seq != uint64(i+1) {
				t.Fatalf("record %d has seq %d", i, rec.Seq)
			}
		}
		_ = l.Close()
		again := openLog(t, dir, Options{SegmentBytes: 1})
		if again.LastSeq() != 6 || len(replayAll(t, again, 4)) != 2 {
			t.Fatalf("reopen: LastSeq %d", again.LastSeq())
		}
	})
	t.Run("should continue the sequence when the last segment is empty after a rotation", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		l := openLog(t, dir, Options{SegmentBytes: 1})
		appendOne(t, l, "a")
		appendOne(t, l, "b")
		_ = l.Close()
		writeFile(t, segmentPath(dir, 3), nil)
		again := openLog(t, dir, Options{SegmentBytes: 1})
		if again.LastSeq() != 2 {
			t.Fatalf("LastSeq = %d", again.LastSeq())
		}
		if seq := appendOne(t, again, "c"); seq != 3 {
			t.Fatalf("next seq %d", seq)
		}
		if got := replayAll(t, again, 0); len(got) != 3 {
			t.Fatalf("replayed %d", len(got))
		}
	})
}

func TestGroupCommit(t *testing.T) {
	t.Run("should give each concurrent caller its own lastSeq when appends are coalesced", func(t *testing.T) {
		l := openLog(t, filepath.Join(t.TempDir(), "j"), Options{SegmentBytes: 2000})
		const callers = 32
		var wg sync.WaitGroup
		seqs := make([]uint64, callers)
		errs := make([]error, callers)
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				seqs[i], errs[i] = l.Append(bg, folderRec("a"), folderRec("b"))
			}()
		}
		wg.Wait()
		seen := map[uint64]bool{}
		for i := range seqs {
			if errs[i] != nil {
				t.Fatal(errs[i])
			}
			if seqs[i]%2 != 0 || seen[seqs[i]] {
				t.Fatalf("caller %d got seq %d (duplicate or splits a pair)", i, seqs[i])
			}
			seen[seqs[i]] = true
		}
		recs := replayAll(t, l, 0)
		if len(recs) != callers*2 {
			t.Fatalf("replayed %d records", len(recs))
		}
		for i := 0; i < len(recs); i += 2 {
			if recs[i].Batch != recs[i+1].Batch || recs[i+1].Seq != recs[i+1].Batch {
				t.Fatalf("pair at %d is not one batch", i)
			}
		}
	})
	t.Run("should issue one fsync for several queued batches when callers overlap", func(t *testing.T) {
		gate := &gatedFS{FS: fsx.OS(), entered: make(chan struct{}), release: make(chan struct{})}
		faulty := fsxtest.NewFaulty(gate)
		l, err := Open(bg, faulty, filepath.Join(t.TempDir(), "j"), Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		base := countSegmentSyncs(faulty)
		gate.arm()

		var wg sync.WaitGroup
		appendAsync := func() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := l.Append(bg, folderRec("a")); err != nil {
					t.Error(err)
				}
			}()
		}
		appendAsync()
		<-gate.entered // the leader is now blocked inside its fsync
		const followers = 5
		for i := 0; i < followers; i++ {
			appendAsync()
		}
		deadline := time.Now().Add(10 * time.Second)
		for l.queueLen() != followers {
			if time.Now().After(deadline) {
				t.Fatalf("queue length %d, want %d", l.queueLen(), followers)
			}
			time.Sleep(time.Millisecond)
		}
		close(gate.release)
		wg.Wait()
		if got := countSegmentSyncs(faulty) - base; got != 2 {
			t.Fatalf("%d fsyncs for 1 leader batch plus %d queued batches, want 2", got, followers)
		}
		if l.LastSeq() != followers+1 {
			t.Fatalf("LastSeq = %d", l.LastSeq())
		}
	})
}

func countSegmentSyncs(f *fsxtest.Faulty) int {
	n := 0
	for _, call := range f.Calls() {
		if call.Op == fsxtest.OpSync && strings.HasSuffix(call.Path, segmentSuffix) {
			n++
		}
	}
	return n
}

// gatedFS blocks the first segment Sync after arm until release is closed.
type gatedFS struct {
	fsx.FS
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (g *gatedFS) arm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed = true
}

func (g *gatedFS) wrap(f fsx.File, err error) (fsx.File, error) {
	if err != nil {
		return nil, err
	}
	return &gatedFile{File: f, gate: g}, nil
}

func (g *gatedFS) OpenFile(name string, flag int, perm fs.FileMode) (fsx.File, error) {
	return g.wrap(g.FS.OpenFile(name, flag, perm))
}

type gatedFile struct {
	fsx.File
	gate *gatedFS
}

func (f *gatedFile) Sync() error {
	if strings.HasSuffix(f.Name(), segmentSuffix) {
		f.gate.mu.Lock()
		hold := f.gate.armed
		f.gate.armed = false
		f.gate.mu.Unlock()
		if hold {
			close(f.gate.entered)
			<-f.gate.release
		}
	}
	return f.File.Sync()
}

func TestTornJournal(t *testing.T) {
	t.Run("should drop the final record with a Torn report when the last line is cut mid-line", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "j")
		l := openLog(t, src, Options{})
		appendOne(t, l, "a")
		appendOne(t, l, "b")
		_ = l.Close()
		full := readFile(t, segmentPath(src, 1))
		lastStart := bytes.LastIndex(full[:len(full)-1], []byte("\n")) + 1

		for cut := lastStart; cut < len(full); cut++ {
			dir := copyDir(t, src)
			writeFile(t, segmentPath(dir, 1), full[:cut])
			got, err := Open(bg, fsx.OS(), dir, Options{})
			if err != nil {
				t.Fatalf("cut %d: Open: %v", cut, err)
			}
			if got.LastSeq() != 1 {
				t.Fatalf("cut %d: LastSeq %d, want 1", cut, got.LastSeq())
			}
			torn := got.Torn()
			switch {
			case cut == lastStart && torn != nil:
				t.Fatalf("cut %d on a line boundary reported torn %+v", cut, torn)
			case cut > lastStart && (torn == nil || torn.Offset != int64(lastStart) || torn.DroppedBytes != int64(cut-lastStart) || torn.Segment != segmentName(1)):
				t.Fatalf("cut %d: torn = %+v", cut, torn)
			}
			if recs := replayAll(t, got, 0); len(recs) != 1 {
				t.Fatalf("cut %d: replayed %d records", cut, len(recs))
			}
			_ = got.Close()
			if info, _ := os.Stat(segmentPath(dir, 1)); info.Size() != int64(lastStart) {
				t.Fatalf("cut %d: file is %d bytes after Open, want %d", cut, info.Size(), lastStart)
			}
		}
	})
	t.Run("should drop the final line when it is intact but lacks its newline", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "j")
		l := openLog(t, src, Options{})
		appendOne(t, l, "a")
		appendOne(t, l, "b")
		_ = l.Close()
		full := readFile(t, segmentPath(src, 1))
		writeFile(t, segmentPath(src, 1), full[:len(full)-1])
		got := openLog(t, src, Options{})
		if got.LastSeq() != 1 || got.Torn() == nil {
			t.Fatalf("LastSeq %d, Torn %+v", got.LastSeq(), got.Torn())
		}
	})
	t.Run("should drop the whole final batch when only its first record is intact", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "j")
		l := openLog(t, src, Options{})
		appendOne(t, l, "a")
		if _, err := l.Append(bg, folderRec("b"), folderRec("c"), folderRec("d")); err != nil {
			t.Fatal(err)
		}
		_ = l.Close()
		lines := bytes.SplitAfter(readFile(t, segmentPath(src, 1)), []byte("\n"))
		keep := len(lines[0]) + len(lines[1]) // record 1 and only the first record of the batch
		for _, extra := range []int{0, len(lines[2]) / 2} {
			dir := copyDir(t, src)
			writeFile(t, segmentPath(dir, 1), append(readFile(t, segmentPath(dir, 1))[:keep:keep], lines[2][:extra]...))
			got := openLog(t, dir, Options{})
			torn := got.Torn()
			if torn == nil || torn.DroppedRecs != 1 || torn.Offset != int64(len(lines[0])) {
				t.Fatalf("extra %d: torn = %+v", extra, torn)
			}
			if got.LastSeq() != 1 || len(replayAll(t, got, 0)) != 1 {
				t.Fatalf("extra %d: LastSeq %d", extra, got.LastSeq())
			}
		}
	})
	t.Run("should drop the whole journal tail when the first batch of the segment is incomplete", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		l := openLog(t, dir, Options{})
		if _, err := l.Append(bg, folderRec("a"), folderRec("b")); err != nil {
			t.Fatal(err)
		}
		_ = l.Close()
		lines := bytes.SplitAfter(readFile(t, segmentPath(dir, 1)), []byte("\n"))
		writeFile(t, segmentPath(dir, 1), lines[0])
		got := openLog(t, dir, Options{})
		if got.LastSeq() != 0 || got.Torn() == nil || got.Torn().DroppedRecs != 1 || len(replayAll(t, got, 0)) != 0 {
			t.Fatalf("LastSeq %d, Torn %+v", got.LastSeq(), got.Torn())
		}
		if seq := appendOne(t, got, "again"); seq != 1 {
			t.Fatalf("next seq %d, want 1", seq)
		}
	})
	t.Run("should truncate the torn tail so the next Append follows the last intact record", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		l := openLog(t, dir, Options{})
		appendOne(t, l, "a")
		appendOne(t, l, "b")
		_ = l.Close()
		full := readFile(t, segmentPath(dir, 1))
		writeFile(t, segmentPath(dir, 1), full[:len(full)-7])

		got := openLog(t, dir, Options{})
		if seq := appendOne(t, got, "c"); seq != 2 {
			t.Fatalf("next seq %d, want 2", seq)
		}
		_ = got.Close()
		again := openLog(t, dir, Options{})
		if again.Torn() != nil {
			t.Fatalf("second open still torn: %+v", again.Torn())
		}
		recs := replayAll(t, again, 0)
		if len(recs) != 2 || recs[1].Folder != "c" {
			t.Fatalf("records %+v", recs)
		}
	})
	t.Run("should return DamagedError when a line in the middle of a segment fails its checksum", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		l := openLog(t, dir, Options{})
		for i := 0; i < 3; i++ {
			appendOne(t, l, "F")
		}
		_ = l.Close()
		data := readFile(t, segmentPath(dir, 1))
		secondLine := bytes.IndexByte(data, '\n') + 1
		data[secondLine+10] ^= 0x01
		writeFile(t, segmentPath(dir, 1), data)

		_, err := Open(bg, fsx.OS(), dir, Options{})
		var damaged *DamagedError
		if !errors.As(err, &damaged) || damaged.Reason != "checksum" || damaged.Line != 2 {
			t.Fatalf("got %v, want checksum damage at line 2", err)
		}
		if after := readFile(t, segmentPath(dir, 1)); !bytes.Equal(after, data) {
			t.Fatal("Open modified a damaged segment")
		}
	})
	t.Run("should return DamagedError when a non-final segment ends mid-line", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		l := openLog(t, dir, Options{SegmentBytes: 1})
		for i := 0; i < 3; i++ {
			appendOne(t, l, "F")
		}
		_ = l.Close()
		first := readFile(t, segmentPath(dir, 1))
		writeFile(t, segmentPath(dir, 1), first[:len(first)-5])

		got := openLog(t, dir, Options{SegmentBytes: 1}) // the last segment is intact
		var damaged *DamagedError
		err := got.Replay(bg, 0, func(Record) error { return nil })
		if !errors.As(err, &damaged) || damaged.Segment != segmentName(1) {
			t.Fatalf("Replay = %v, want DamagedError in segment 1", err)
		}
	})
	t.Run("should return DamagedError when a sequence number is skipped", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		openLog(t, dir, Options{}).Close()
		writeFile(t, segmentPath(dir, 1), encodeRecords(t, seqRec(1), seqRec(3)))
		_, err := Open(bg, fsx.OS(), dir, Options{})
		var damaged *DamagedError
		if !errors.As(err, &damaged) || damaged.Reason != "seq_gap" {
			t.Fatalf("Open = %v", err)
		}
	})
	t.Run("should return DamagedError when a sequence number is skipped between segments", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		openLog(t, dir, Options{}).Close()
		writeFile(t, segmentPath(dir, 1), encodeRecords(t, seqRec(1), seqRec(2)))
		writeFile(t, segmentPath(dir, 2), encodeRecords(t, seqRec(4)))
		got := openLog(t, dir, Options{})
		var damaged *DamagedError
		if err := got.Replay(bg, 0, func(Record) error { return nil }); !errors.As(err, &damaged) || damaged.Reason != "seq_gap" {
			t.Fatalf("Replay = %v", err)
		}
	})
	t.Run("should return DamagedError when a segment is missing", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		openLog(t, dir, Options{}).Close()
		writeFile(t, segmentPath(dir, 3), encodeRecords(t, seqRec(1)))
		var damaged *DamagedError
		if _, err := Open(bg, fsx.OS(), dir, Options{}); !errors.As(err, &damaged) {
			t.Fatalf("Open = %v", err)
		}
	})
	t.Run("should return DamagedError and keep the bytes when a checksummed record has an unknown type", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		openLog(t, dir, Options{}).Close()
		future := Record{Seq: 2, Batch: 2, By: "newer", Type: "item_purged"}
		// appendLine does not validate; Append does.
		data := encodeRecords(t, seqRec(1), future)
		writeFile(t, segmentPath(dir, 1), data)
		_, err := Open(bg, fsx.OS(), dir, Options{})
		var damaged *DamagedError
		if !errors.As(err, &damaged) || damaged.Reason != "unknown_type" {
			t.Fatalf("Open = %v", err)
		}
		if !bytes.Equal(readFile(t, segmentPath(dir, 1)), data) {
			t.Fatal("Open truncated records it could not read")
		}
	})
	t.Run("should return DamagedError when a line is longer than the limit", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		openLog(t, dir, Options{}).Close()
		long := append(bytes.Repeat([]byte("x"), maxLineBytes+100), '\n')
		writeFile(t, segmentPath(dir, 1), append(long, encodeRecords(t, seqRec(1))...))
		var damaged *DamagedError
		if _, err := Open(bg, fsx.OS(), dir, Options{}); !errors.As(err, &damaged) || damaged.Reason != "line_too_long" {
			t.Fatalf("Open = %v", err)
		}
	})
	t.Run("should treat a trailing overlong line as torn when nothing intact follows it", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		openLog(t, dir, Options{}).Close()
		long := bytes.Repeat([]byte("x"), maxLineBytes+100)
		good := encodeRecords(t, seqRec(1))
		writeFile(t, segmentPath(dir, 1), append(good[:len(good):len(good)], long...))
		got := openLog(t, dir, Options{})
		if got.LastSeq() != 1 || got.Torn() == nil || got.Torn().DroppedBytes != int64(len(long)) {
			t.Fatalf("LastSeq %d, Torn %+v", got.LastSeq(), got.Torn())
		}
	})
	t.Run("should never deliver the records of an incomplete batch when replaying a live segment", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		openLog(t, dir, Options{}).Close()
		a, b := seqRec(1), seqRec(2)
		a.Batch, b.Batch = 2, 2
		writeFile(t, segmentPath(dir, 1), encodeRecords(t, a))
		// Open drops it; the log is then empty.
		got := openLog(t, dir, Options{})
		if len(replayAll(t, got, 0)) != 0 || got.Torn() == nil {
			t.Fatal("an incomplete batch was visible")
		}
	})
}

func TestFormatVersion(t *testing.T) {
	t.Run("should refuse to open and change nothing when FORMAT is newer", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "FORMAT"), []byte("2\n"))
		writeFile(t, segmentPath(dir, 1), encodeRecords(t, seqRec(1)))
		writeFile(t, segmentPath(dir, 2), []byte("partial line, no newline"))
		before := hashTree(t, dir)

		_, err := Open(bg, fsx.OS(), dir, Options{})
		if !errors.Is(err, ErrFormatNewer) {
			t.Fatalf("Open = %v, want ErrFormatNewer", err)
		}
		if after := hashTree(t, dir); !equalMaps(before, after) {
			t.Fatalf("directory changed:\nbefore %v\nafter  %v", before, after)
		}
	})
	t.Run("should open normally when FORMAT is the current version", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		openLog(t, dir, Options{}).Close()
		openLog(t, dir, Options{})
	})
	t.Run("should return an error when FORMAT is not a number", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "j")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "FORMAT"), []byte("banana\n"))
		if _, err := Open(bg, fsx.OS(), dir, Options{}); err == nil {
			t.Fatal("garbage FORMAT accepted")
		}
	})
}

// hashTree maps every file under dir to its SHA-256 (and lists directories).
func hashTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if d.IsDir() {
			out[rel] = "dir"
			return nil
		}
		sum := sha256.Sum256(readFile(t, path))
		out[rel] = fmt.Sprintf("%x", sum)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestReplayMemory(t *testing.T) {
	t.Run("should replay a long journal without holding more than one line when it streams", func(t *testing.T) {
		const records = 40000
		dir := filepath.Join(t.TempDir(), "j")
		openLog(t, dir, Options{}).Close()
		f, err := os.Create(segmentPath(dir, 1))
		if err != nil {
			t.Fatal(err)
		}
		var line []byte
		for seq := uint64(1); seq <= records; seq++ {
			line, _ = appendLine(line[:0], withAt(seqRec(seq)))
			if _, err := f.Write(line); err != nil {
				t.Fatal(err)
			}
		}
		_ = f.Close()
		info, _ := os.Stat(segmentPath(dir, 1))

		l := openLog(t, dir, Options{})
		runtime.GC()
		var base runtime.MemStats
		runtime.ReadMemStats(&base)
		var peak uint64
		count := 0
		err = l.Replay(bg, 0, func(Record) error {
			count++
			if count%5000 == 0 {
				runtime.GC()
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak {
					peak = m.HeapAlloc
				}
			}
			return nil
		})
		if err != nil || count != records {
			t.Fatalf("Replay: %d records, %v", count, err)
		}
		growth := int64(peak) - int64(base.HeapAlloc)
		if growth > info.Size()/4 {
			t.Fatalf("heap grew %d bytes while replaying a %d byte journal", growth, info.Size())
		}
	})
}

func withAt(r Record) Record {
	r.At = time.Unix(1700000000, 0)
	return r
}

func TestReplayContext(t *testing.T) {
	t.Run("should stop and return the context error when the context is cancelled mid-replay", func(t *testing.T) {
		l := openLog(t, filepath.Join(t.TempDir(), "j"), Options{})
		for i := 0; i < 5; i++ {
			appendOne(t, l, "F")
		}
		ctx, cancel := context.WithCancel(bg)
		n := 0
		err := l.Replay(ctx, 0, func(Record) error {
			n++
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) || n != 1 {
			t.Fatalf("n=%d err=%v", n, err)
		}
	})
	t.Run("should return the callback error when fn fails", func(t *testing.T) {
		l := openLog(t, filepath.Join(t.TempDir(), "j"), Options{})
		appendOne(t, l, "F")
		boom := errors.New("boom")
		if err := l.Replay(bg, 0, func(Record) error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("got %v", err)
		}
	})
}
