// Package fsxtest provides a fault-injecting fsx.FS for tests.
package fsxtest

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"

	"github.com/tjmisko/coldstore/internal/fsx"
)

type Op string

const (
	OpOpen     Op = "open"
	OpCreate   Op = "create"
	OpRead     Op = "read"
	OpWrite    Op = "write"
	OpSync     Op = "sync"
	OpSyncDir  Op = "syncdir"
	OpClose    Op = "close"
	OpRename   Op = "rename"
	OpLink     Op = "link"
	OpRemove   Op = "remove"
	OpMkdirAll Op = "mkdirall"
	OpStat     Op = "stat"
	OpLstat    Op = "lstat"
	OpReadDir  Op = "readdir"
	OpChmod    Op = "chmod"
	OpTruncate Op = "truncate"
)

// ErrInjected is returned by a fault whose Err is nil (and by FailCall(n, nil)).
var ErrInjected = errors.New("fsxtest: injected fault")

// Call is one recorded filesystem call. For OpRename and OpLink, Path is the
// destination (the name being created); a Fault's PathContains matches either
// the source or the destination. File methods (Read, ReadAt, Write, Sync,
// Close, Stat) record the file's name. File.Seek is not recorded.
type Call struct {
	Op   Op
	Path string
}

// Fault fires on the Nth (1-based) call that matches Op and, if non-empty,
// whose path contains PathContains. Short applies to OpRead and OpWrite only:
// half the requested bytes are transferred, then Err is returned (Err == nil
// means a silent short read, or io.ErrShortWrite for a write).
type Fault struct {
	Op           Op
	PathContains string
	Nth          int
	Err          error
	Short        bool
}

type armedFault struct {
	Fault
	matched int
}

// Faulty wraps an fsx.FS, records every call and injects faults. It is safe
// for concurrent use.
type Faulty struct {
	inner fsx.FS

	mu       sync.Mutex
	calls    []Call
	faults   []*armedFault
	failCall map[int]error
}

var _ fsx.FS = (*Faulty)(nil)

func NewFaulty(inner fsx.FS) *Faulty {
	return &Faulty{inner: inner, failCall: map[int]error{}}
}

// Inject arms a fault. Each fault counts its own matches and fires once.
func (f *Faulty) Inject(fault Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, &armedFault{Fault: fault})
}

// FailCall fails the n-th call overall (1-based), whatever its op.
func (f *Faulty) FailCall(n int, err error) {
	if err == nil {
		err = ErrInjected
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCall[n] = err
}

// Calls returns a copy of the full trace so far, in order.
func (f *Faulty) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Reset clears faults, FailCall registrations and the trace.
func (f *Faulty) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
	f.faults = nil
	f.failCall = map[int]error{}
}

// decision is what a call must do: pass through, fail, or fail after a short
// transfer.
type decision struct {
	fire  bool
	err   error
	short bool
}

// begin records the call and decides its fate. paths[0] is the recorded path.
func (f *Faulty) begin(op Op, paths ...string) decision {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Op: op, Path: paths[0]})
	overall := len(f.calls)

	if err, ok := f.failCall[overall]; ok {
		delete(f.failCall, overall)
		return decision{fire: true, err: err}
	}
	for _, fault := range f.faults {
		if !fault.matches(op, paths) {
			continue
		}
		fault.matched++
		if fault.matched != fault.Nth {
			continue
		}
		return decision{fire: true, err: fault.Err, short: fault.Short && (op == OpRead || op == OpWrite)}
	}
	return decision{}
}

func (a *armedFault) matches(op Op, paths []string) bool {
	if a.Op != op {
		return false
	}
	if a.PathContains == "" {
		return true
	}
	for _, path := range paths {
		if strings.Contains(path, a.PathContains) {
			return true
		}
	}
	return false
}

// failure is the error a non-short fired decision returns.
func (d decision) failure() error {
	if d.err == nil {
		return ErrInjected
	}
	return d.err
}

func (f *Faulty) OpenFile(name string, flag int, perm fs.FileMode) (fsx.File, error) {
	op := OpOpen
	if flag&os.O_CREATE != 0 {
		op = OpCreate
	}
	if d := f.begin(op, name); d.fire {
		return nil, d.failure()
	}
	file, err := f.inner.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return &faultyFile{faulty: f, inner: file}, nil
}

func (f *Faulty) Open(name string) (fsx.File, error) {
	if d := f.begin(OpOpen, name); d.fire {
		return nil, d.failure()
	}
	file, err := f.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &faultyFile{faulty: f, inner: file}, nil
}

func (f *Faulty) Rename(oldpath, newpath string) error {
	if d := f.begin(OpRename, newpath, oldpath); d.fire {
		return d.failure()
	}
	return f.inner.Rename(oldpath, newpath)
}

func (f *Faulty) Link(oldname, newname string) error {
	if d := f.begin(OpLink, newname, oldname); d.fire {
		return d.failure()
	}
	return f.inner.Link(oldname, newname)
}

func (f *Faulty) Remove(name string) error {
	if d := f.begin(OpRemove, name); d.fire {
		return d.failure()
	}
	return f.inner.Remove(name)
}

func (f *Faulty) MkdirAll(path string, perm fs.FileMode) error {
	if d := f.begin(OpMkdirAll, path); d.fire {
		return d.failure()
	}
	return f.inner.MkdirAll(path, perm)
}

func (f *Faulty) Stat(name string) (fs.FileInfo, error) {
	if d := f.begin(OpStat, name); d.fire {
		return nil, d.failure()
	}
	return f.inner.Stat(name)
}

func (f *Faulty) Lstat(name string) (fs.FileInfo, error) {
	if d := f.begin(OpLstat, name); d.fire {
		return nil, d.failure()
	}
	return f.inner.Lstat(name)
}

func (f *Faulty) ReadDir(name string) ([]fs.DirEntry, error) {
	if d := f.begin(OpReadDir, name); d.fire {
		return nil, d.failure()
	}
	return f.inner.ReadDir(name)
}

func (f *Faulty) Chmod(name string, mode fs.FileMode) error {
	if d := f.begin(OpChmod, name); d.fire {
		return d.failure()
	}
	return f.inner.Chmod(name, mode)
}

func (f *Faulty) Truncate(name string, size int64) error {
	if d := f.begin(OpTruncate, name); d.fire {
		return d.failure()
	}
	return f.inner.Truncate(name, size)
}

func (f *Faulty) SyncDir(path string) error {
	if d := f.begin(OpSyncDir, path); d.fire {
		return d.failure()
	}
	return f.inner.SyncDir(path)
}

type faultyFile struct {
	faulty *Faulty
	inner  fsx.File
}

func (ff *faultyFile) Name() string { return ff.inner.Name() }

func (ff *faultyFile) Seek(offset int64, whence int) (int64, error) {
	return ff.inner.Seek(offset, whence)
}

func (ff *faultyFile) Read(p []byte) (int, error) {
	d := ff.faulty.begin(OpRead, ff.inner.Name())
	if !d.fire {
		return ff.inner.Read(p)
	}
	if !d.short {
		return 0, d.failure()
	}
	n, err := ff.inner.Read(p[:len(p)/2])
	if err != nil {
		return n, err
	}
	return n, d.err
}

func (ff *faultyFile) ReadAt(p []byte, off int64) (int, error) {
	d := ff.faulty.begin(OpRead, ff.inner.Name())
	if !d.fire {
		return ff.inner.ReadAt(p, off)
	}
	if !d.short {
		return 0, d.failure()
	}
	n, err := ff.inner.ReadAt(p[:len(p)/2], off)
	if err != nil {
		return n, err
	}
	// io.ReaderAt must not return a short count without an error.
	if d.err == nil {
		return n, io.ErrUnexpectedEOF
	}
	return n, d.err
}

func (ff *faultyFile) Write(p []byte) (int, error) {
	d := ff.faulty.begin(OpWrite, ff.inner.Name())
	if !d.fire {
		return ff.inner.Write(p)
	}
	if !d.short {
		return 0, d.failure()
	}
	n, err := ff.inner.Write(p[:len(p)/2])
	if err != nil {
		return n, err
	}
	if d.err == nil {
		return n, io.ErrShortWrite
	}
	return n, d.err
}

func (ff *faultyFile) Sync() error {
	if d := ff.faulty.begin(OpSync, ff.inner.Name()); d.fire {
		return d.failure()
	}
	return ff.inner.Sync()
}

// Close always closes the underlying file, even when a fault fires, so a test
// that injects a close error does not leak descriptors.
func (ff *faultyFile) Close() error {
	d := ff.faulty.begin(OpClose, ff.inner.Name())
	err := ff.inner.Close()
	if d.fire {
		return d.failure()
	}
	return err
}

func (ff *faultyFile) Stat() (fs.FileInfo, error) {
	if d := ff.faulty.begin(OpStat, ff.inner.Name()); d.fire {
		return nil, d.failure()
	}
	return ff.inner.Stat()
}
