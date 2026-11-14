package journal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tjmisko/coldstore/internal/crashpoint"
	"github.com/tjmisko/coldstore/internal/fsx"
)

const (
	defaultSegmentBytes = 16 << 20
	formatFileName      = "FORMAT"
	segmentSuffix       = ".jsonl"
)

var (
	cpBeforeWrite     = crashpoint.Register("journal.append.before_write")
	cpWritten         = crashpoint.Register("journal.append.written")
	cpSynced          = crashpoint.Register("journal.append.synced")
	cpSegmentCreated  = crashpoint.Register("journal.rotate.segment_created")
	cpRotateDirSynced = crashpoint.Register("journal.rotate.dir_synced")
)

func segmentName(n int) string { return fmt.Sprintf("%06d%s", n, segmentSuffix) }

// request is one Append call waiting for the leader to commit it.
type request struct {
	recs    []Record
	lastSeq uint64
	err     error
	done    chan struct{}
}

// Log is the journal. It implements Journal.
type Log struct {
	fsys fsx.FS
	dir  string
	opts Options

	// qmu guards the group-commit queue.
	qmu     sync.Mutex
	queue   []*request
	leading bool

	// wmu guards everything below: the open segment and the committed state.
	wmu      sync.Mutex
	file     fsx.File
	segNum   int
	segSize  int64
	lastSeq  uint64
	torn     *Torn
	poisoned error
	closed   bool
}

var _ Journal = (*Log)(nil)

// DiskFormatVersion reads journal/FORMAT without taking the lock. A missing
// directory or file reports 0.
func DiskFormatVersion(fsys fsx.FS, dir string) (int, error) {
	f, err := fsys.Open(filepath.Join(dir, formatFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) && errors.Is(pathErr.Err, syscall.ENOTDIR) {
			return 0, nil
		}
		return 0, fmt.Errorf("journal: open FORMAT: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 64))
	if err != nil {
		return 0, fmt.Errorf("journal: read FORMAT: %w", err)
	}
	version, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || version < 0 {
		return 0, fmt.Errorf("journal: FORMAT %q is not a version number", strings.TrimSpace(string(raw)))
	}
	return version, nil
}

// Open requires the caller to hold the Lock. It checks journal/FORMAT (creating
// it with FormatVersion in an empty directory), scans the last segment,
// truncates a torn tail, and returns what it dropped via Torn().
func Open(ctx context.Context, fsys fsx.FS, dir string, opts Options) (*Log, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.SegmentBytes <= 0 {
		opts.SegmentBytes = defaultSegmentBytes
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	version, err := DiskFormatVersion(fsys, dir)
	if err != nil {
		return nil, err
	}
	if version > FormatVersion {
		return nil, fmt.Errorf("%w: format %d, this binary reads %d", ErrFormatNewer, version, FormatVersion)
	}

	l := &Log{fsys: fsys, dir: dir, opts: opts}
	if err := l.ensureDir(); err != nil {
		return nil, err
	}
	if version == 0 {
		if err := l.writeFormat(); err != nil {
			return nil, err
		}
	}
	segments, err := l.listSegments()
	if err != nil {
		return nil, err
	}
	if len(segments) == 0 {
		return l.openFresh()
	}
	return l.openExisting(ctx, segments)
}

func (l *Log) ensureDir() error {
	_, err := l.fsys.Stat(l.dir)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("journal: stat %s: %w", l.dir, err)
	}
	if err := l.fsys.MkdirAll(l.dir, 0o755); err != nil {
		return fmt.Errorf("journal: create %s: %w", l.dir, err)
	}
	if err := l.fsys.SyncDir(filepath.Dir(l.dir)); err != nil {
		return fmt.Errorf("journal: sync parent of %s: %w", l.dir, err)
	}
	return nil
}

func (l *Log) writeFormat() error {
	final := filepath.Join(l.dir, formatFileName)
	tmp := final + ".tmp"
	f, err := l.fsys.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("journal: create FORMAT: %w", err)
	}
	if _, err := f.Write([]byte(strconv.Itoa(FormatVersion) + "\n")); err != nil {
		_ = f.Close()
		return fmt.Errorf("journal: write FORMAT: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("journal: sync FORMAT: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("journal: close FORMAT: %w", err)
	}
	if err := l.fsys.Rename(tmp, final); err != nil {
		return fmt.Errorf("journal: install FORMAT: %w", err)
	}
	if err := l.fsys.SyncDir(l.dir); err != nil {
		return fmt.Errorf("journal: sync journal directory: %w", err)
	}
	return nil
}

// listSegments returns the segment numbers present, which must be 1..n.
func (l *Log) listSegments() ([]int, error) {
	entries, err := l.fsys.ReadDir(l.dir)
	if err != nil {
		return nil, fmt.Errorf("journal: list %s: %w", l.dir, err)
	}
	var numbers []int
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != 6+len(segmentSuffix) || !strings.HasSuffix(name, segmentSuffix) {
			continue
		}
		n, err := strconv.Atoi(name[:6])
		if err != nil || n < 1 {
			continue
		}
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	for i, n := range numbers {
		if n != i+1 {
			return nil, &DamagedError{Segment: segmentName(i + 1), Line: 0, Reason: "seq_gap"}
		}
	}
	return numbers, nil
}

func (l *Log) openFresh() (*Log, error) {
	file, err := l.createSegment(1)
	if err != nil {
		return nil, err
	}
	l.file, l.segNum = file, 1
	return l, nil
}

// createSegment creates segment n, fsyncs it and fsyncs the directory.
func (l *Log) createSegment(n int) (fsx.File, error) {
	path := filepath.Join(l.dir, segmentName(n))
	f, err := l.fsys.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("journal: create segment %s: %w", segmentName(n), err)
	}
	cpSegmentCreated.Hit()
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("journal: sync segment %s: %w", segmentName(n), err)
	}
	if err := l.fsys.SyncDir(l.dir); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("journal: sync journal directory: %w", err)
	}
	cpRotateDirSynced.Hit()
	return f, nil
}

func (l *Log) openExisting(ctx context.Context, segments []int) (*Log, error) {
	last := len(segments)
	name := segmentName(last)
	path := filepath.Join(l.dir, name)

	res, err := l.scanSegment(ctx, name, 0, false)
	if err != nil {
		return nil, err
	}
	if res.dropped() {
		if err := l.truncateTail(path, res); err != nil {
			return nil, err
		}
		l.torn = &Torn{Segment: name, Offset: res.intactEnd, DroppedBytes: res.size - res.intactEnd, DroppedRecs: res.droppedRecs}
		if l.opts.Logger != nil {
			l.opts.Logger.Warn("journal: dropped a torn tail", "segment", name, "offset", l.torn.Offset,
				"dropped_bytes", l.torn.DroppedBytes, "dropped_records", l.torn.DroppedRecs)
		}
	}

	lastSeq := res.lastSeq
	for earlier := last - 1; res.records == 0 && earlier >= 1; earlier-- {
		prev, err := l.scanSegment(ctx, segmentName(earlier), 0, true)
		if err != nil {
			return nil, err
		}
		res.records = prev.records
		lastSeq = prev.lastSeq
	}

	file, err := l.fsys.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("journal: open segment %s: %w", name, err)
	}
	if res.dropped() {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("journal: sync truncated segment %s: %w", name, err)
		}
	}
	l.file, l.segNum, l.segSize, l.lastSeq = file, last, res.intactEnd, lastSeq
	return l, nil
}

func (l *Log) truncateTail(path string, res scanResult) error {
	if err := l.fsys.Truncate(path, res.intactEnd); err != nil {
		return fmt.Errorf("journal: truncate torn tail of %s: %w", filepath.Base(path), err)
	}
	return nil
}

// scanResult is what scanning one segment found.
type scanResult struct {
	records     int
	lastSeq     uint64
	intactEnd   int64 // end of the intact prefix, incomplete trailing batch excluded
	size        int64 // bytes in the file
	droppedRecs int
}

func (r scanResult) dropped() bool { return r.size != r.intactEnd }

// scanSegment reads a whole segment without delivering records. When strict is
// false (the last segment) a damaged tail is tolerated: the intact prefix ends
// at the first bad line provided nothing intact follows it, and records of an
// incomplete trailing batch are excluded. Every other damage is a
// *DamagedError. limit > 0 bounds how many bytes are read.
func (l *Log) scanSegment(ctx context.Context, name string, limit int64, strict bool) (scanResult, error) {
	var res scanResult
	f, err := l.fsys.Open(filepath.Join(l.dir, name))
	if err != nil {
		return res, fmt.Errorf("journal: open segment %s: %w", name, err)
	}
	defer f.Close()
	var src io.Reader = f
	if limit > 0 {
		src = io.LimitReader(f, limit)
	}
	lines := newLineReader(src)

	var (
		offset, badAt   int64 = 0, -1
		badLine, lineNo int
		badReason       string
		prev            Record
		batchStart      int64
		batchRecs       int
		batchFirstSeq   uint64
	)
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		line, complete, tooLong, size, err := lines.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, fmt.Errorf("journal: read segment %s: %w", name, err)
		}
		lineNo++

		var rec Record
		var derr error
		switch {
		case tooLong:
			derr = &decodeError{"line_too_long", errors.New("line exceeds the limit")}
		case !complete:
			derr = &decodeError{"checksum", errors.New("line has no newline")}
		default:
			rec, derr = decodeLine(line)
		}
		if derr != nil {
			var de *decodeError
			errors.As(derr, &de)
			tolerable := !strict && (de.Reason == "checksum" || de.Reason == "line_too_long")
			if !tolerable {
				return res, &DamagedError{Segment: name, Line: lineNo, Reason: de.Reason}
			}
			if badAt < 0 {
				badAt, badLine, badReason = offset, lineNo, de.Reason
			}
			offset += size
			continue
		}
		if badAt >= 0 {
			return res, &DamagedError{Segment: name, Line: badLine, Reason: badReason}
		}
		if res.records > 0 && rec.Seq != prev.Seq+1 {
			return res, &DamagedError{Segment: name, Line: lineNo, Reason: "seq_gap"}
		}
		if rec.Batch < rec.Seq || (res.records > 0 && prev.Seq != prev.Batch && rec.Batch != prev.Batch) {
			return res, &DamagedError{Segment: name, Line: lineNo, Reason: "seq_gap"}
		}
		if res.records == 0 || prev.Seq == prev.Batch {
			batchStart, batchRecs, batchFirstSeq = offset, 0, rec.Seq
		}
		batchRecs++
		res.records++
		prev = rec
		res.lastSeq = rec.Seq
		offset += size
	}

	res.size = offset
	res.intactEnd = offset
	if badAt >= 0 {
		res.intactEnd = badAt
	}
	if res.records == 0 || prev.Seq == prev.Batch {
		return res, nil
	}
	if strict {
		return res, &DamagedError{Segment: name, Line: lineNo, Reason: "seq_gap"}
	}
	res.intactEnd = batchStart
	res.droppedRecs = batchRecs
	res.records -= batchRecs
	res.lastSeq = batchFirstSeq - 1
	if res.records == 0 {
		res.lastSeq = 0
	}
	return res, nil
}

// LastSeq is the sequence number of the last durable record, 0 when empty.
func (l *Log) LastSeq() uint64 {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	return l.lastSeq
}

// Torn reports what Open dropped, or nil when the tail was intact.
func (l *Log) Torn() *Torn { return l.torn }

// Close closes the open segment. Appends after Close fail.
func (l *Log) Close() error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.file == nil {
		return nil
	}
	if err := l.file.Close(); err != nil {
		return fmt.Errorf("journal: close segment: %w", err)
	}
	return nil
}

// Append validates recs, assigns consecutive Seq numbers, At and Batch, writes
// them as one atomic batch and returns only after the fsync. Concurrent callers
// are coalesced: one leader writes every queued batch with a single fsync.
func (l *Log) Append(ctx context.Context, recs ...Record) (uint64, error) {
	if len(recs) == 0 {
		return l.LastSeq(), nil
	}
	for i, rec := range recs {
		if err := rec.Validate(); err != nil {
			return 0, fmt.Errorf("journal: record %d of %d: %w", i+1, len(recs), err)
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	req := &request{recs: append([]Record(nil), recs...), done: make(chan struct{})}

	l.qmu.Lock()
	l.queue = append(l.queue, req)
	lead := !l.leading
	l.leading = true
	l.qmu.Unlock()

	if lead {
		l.lead()
	}
	<-req.done
	return req.lastSeq, req.err
}

// lead commits queued requests until the queue is empty.
func (l *Log) lead() {
	for {
		l.qmu.Lock()
		group := l.queue
		l.queue = nil
		if len(group) == 0 {
			l.leading = false
			l.qmu.Unlock()
			return
		}
		l.qmu.Unlock()
		l.commit(group)
	}
}

func (l *Log) queueLen() int {
	l.qmu.Lock()
	defer l.qmu.Unlock()
	return len(l.queue)
}

// commit writes every request of group with one write and one fsync.
func (l *Log) commit(group []*request) {
	l.wmu.Lock()
	defer l.wmu.Unlock()

	err := l.commitLocked(group)
	for _, req := range group {
		if err != nil {
			req.lastSeq, req.err = 0, err
		}
		close(req.done)
	}
}

func (l *Log) commitLocked(group []*request) error {
	if l.closed {
		return errClosed
	}
	if l.poisoned != nil {
		return l.poisoned
	}
	if l.segSize >= l.opts.SegmentBytes {
		if err := l.rotate(); err != nil {
			l.poisoned = err
			return err
		}
	}

	now := l.opts.Now().UTC()
	seq := l.lastSeq
	var buf []byte
	for _, req := range group {
		batch := seq + uint64(len(req.recs))
		for i := range req.recs {
			seq++
			rec := &req.recs[i]
			rec.Seq, rec.Batch = seq, batch
			if rec.At.IsZero() {
				rec.At = now
			}
			var err error
			if buf, err = appendLine(buf, *rec); err != nil {
				return err // nothing written yet: the log is still sound
			}
		}
		req.lastSeq = batch
	}

	cpBeforeWrite.Hit()
	n, err := l.file.Write(buf)
	if err == nil && n != len(buf) {
		err = io.ErrShortWrite
	}
	if err != nil {
		l.poisoned = fmt.Errorf("journal: write segment: %w", err)
		return l.poisoned
	}
	cpWritten.Hit()
	if err := l.file.Sync(); err != nil {
		l.poisoned = fmt.Errorf("journal: sync segment: %w", err)
		return l.poisoned
	}
	cpSynced.Hit()
	l.segSize += int64(len(buf))
	l.lastSeq = seq
	return nil
}

// rotate starts the next segment. A batch is never split because rotation only
// happens between commits.
func (l *Log) rotate() error {
	next := l.segNum + 1
	file, err := l.createSegment(next)
	if err != nil {
		return err
	}
	old := l.file
	l.file, l.segNum, l.segSize = file, next, 0
	// Everything in the old segment is already synced; a close error loses nothing.
	_ = old.Close()
	return nil
}

// Replay streams every intact record with Seq > fromSeq in order. It never
// delivers records of an incomplete batch. Damage is a *DamagedError.
func (l *Log) Replay(ctx context.Context, fromSeq uint64, fn func(Record) error) error {
	l.wmu.Lock()
	if l.closed {
		l.wmu.Unlock()
		return errClosed
	}
	lastSegment, committed := l.segNum, l.segSize
	l.wmu.Unlock()

	var expected uint64
	for n := 1; n <= lastSegment; n++ {
		var limit int64
		if n == lastSegment {
			limit = committed
			if limit == 0 {
				continue
			}
		}
		var err error
		if expected, err = l.replaySegment(ctx, segmentName(n), limit, expected, fromSeq, fn); err != nil {
			return err
		}
	}
	return nil
}

func (l *Log) replaySegment(ctx context.Context, name string, limit int64, expected, fromSeq uint64, fn func(Record) error) (uint64, error) {
	f, err := l.fsys.Open(filepath.Join(l.dir, name))
	if err != nil {
		return expected, fmt.Errorf("journal: open segment %s: %w", name, err)
	}
	defer f.Close()
	var src io.Reader = f
	if limit > 0 {
		src = io.LimitReader(f, limit)
	}
	lines := newLineReader(src)

	var pending []Record // the records of the batch being read
	lineNo := 0
	for {
		if err := ctx.Err(); err != nil {
			return expected, err
		}
		line, complete, tooLong, _, err := lines.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return expected, fmt.Errorf("journal: read segment %s: %w", name, err)
		}
		lineNo++
		if tooLong {
			return expected, &DamagedError{Segment: name, Line: lineNo, Reason: "line_too_long"}
		}
		if !complete {
			return expected, &DamagedError{Segment: name, Line: lineNo, Reason: "checksum"}
		}
		rec, err := decodeLine(line)
		if err != nil {
			var de *decodeError
			errors.As(err, &de)
			return expected, &DamagedError{Segment: name, Line: lineNo, Reason: de.Reason}
		}
		if expected != 0 && rec.Seq != expected {
			return expected, &DamagedError{Segment: name, Line: lineNo, Reason: "seq_gap"}
		}
		if rec.Batch < rec.Seq || (len(pending) > 0 && rec.Batch != pending[0].Batch) {
			return expected, &DamagedError{Segment: name, Line: lineNo, Reason: "seq_gap"}
		}
		expected = rec.Seq + 1
		pending = append(pending, rec)
		if rec.Seq != rec.Batch {
			continue
		}
		for _, delivered := range pending {
			if delivered.Seq <= fromSeq {
				continue
			}
			if err := fn(delivered); err != nil {
				return expected, err
			}
		}
		pending = pending[:0]
	}
	if len(pending) > 0 && limit == 0 {
		// A non-final segment ended inside a batch.
		return expected, &DamagedError{Segment: name, Line: lineNo, Reason: "seq_gap"}
	}
	return expected, nil
}
