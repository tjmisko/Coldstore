// Package archive is the service around the archive core: it owns the journal
// lock and opens store, journal, index and views in the order the design
// requires, runs the ingest pipeline, and offers the mutations the HTTP layer
// and the CLI need. The journal is the truth; everything else is derived.
package archive

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tjmisko/coldstore/internal/crashpoint"
	"github.com/tjmisko/coldstore/internal/fsx"
	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/ingest"
	"github.com/tjmisko/coldstore/internal/journal"
	"github.com/tjmisko/coldstore/internal/scrub"
	"github.com/tjmisko/coldstore/internal/store"
	"github.com/tjmisko/coldstore/internal/views"
)

var (
	crashArrivalOpened   = crashpoint.Register("ingest.arrival.opened")
	crashPutDone         = crashpoint.Register("ingest.put.done")
	crashJournalAppended = crashpoint.Register("ingest.journal.appended")
	crashIndexApplied    = crashpoint.Register("ingest.index.applied")
	crashViewsSynced     = crashpoint.Register("ingest.views.synced")
)

var (
	ErrCollectionUnknown = ingest.ErrCollectionUnknown
	ErrBlobBad           = errors.New("archive: blob failed verification")
	ErrImportRunning     = errors.New("archive: an import is already running")
	// ErrClosed is returned by every operation once Close has begun.
	ErrClosed = errors.New("archive: service is closed")
	// ErrBlobNotHeld means the journal does not name the blob.
	ErrBlobNotHeld = errors.New("archive: blob not held")
	// ErrEntryNotFound means an entry id names no placement.
	ErrEntryNotFound = errors.New("archive: no such entry")
	// ErrImportDir means the directory offered for import is not acceptable.
	ErrImportDir = errors.New("archive: unusable import directory")
	// ErrScrubRunning means a scrub pass is already in progress.
	ErrScrubRunning = errors.New("archive: a scrub is already running")
)

const (
	actorSystem    = "system"
	uploadDefault  = 2
	journalDirName = "journal"
	indexFileName  = "index.db"
	viewsDirName   = "views"
	thumbsDirName  = "thumbnails"
)

type Options struct {
	Root                 string   // cfg.Storage.ColdStoragePath
	Collections          []string // declared in coldstore.toml
	FS                   fsx.FS   // nil = fsx.OS()
	Logger               *slog.Logger
	UploadConcurrency    int           // default 2
	ScrubInterval        time.Duration // 0 = no schedule
	ScrubRateBytesPerSec int64
	// Render makes a thumbnail of src (extension ext, sniffed type detected) at
	// dst. nil disables thumbnails. It is how the service avoids importing the
	// ffmpeg-bearing generator directly and how tests avoid needing ffmpeg.
	Render func(ctx context.Context, src, ext, detected, dst string) error
	Now    func() time.Time
	Sleep  func(time.Duration)
}

func (o Options) withDefaults() Options {
	if o.FS == nil {
		o.FS = fsx.OS()
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.UploadConcurrency <= 0 {
		o.UploadConcurrency = uploadDefault
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	return o
}

// core is what steps 1 to 4 of Open produce.
type core struct {
	lock     *journal.Lock
	st       *store.Disk
	j        *journal.Log
	ix       *index.Index
	rebuilt  bool
	replayed int

	rebuildReason string
}

func (c *core) close() error {
	var errs []error
	if c.ix != nil {
		errs = append(errs, c.ix.Close())
	}
	if c.j != nil {
		errs = append(errs, c.j.Close())
	}
	if c.lock != nil {
		errs = append(errs, c.lock.Release())
	}
	return errors.Join(errs...)
}

func journalDir(root string) string { return filepath.Join(root, journalDirName) }
func indexPath(root string) string  { return filepath.Join(root, indexFileName) }

// openCore runs steps 1 to 4 of the Open order. Each step returns early on
// error and releases what the earlier steps took.
func openCore(ctx context.Context, opts Options, forceRebuild bool) (*core, error) {
	dir := journalDir(opts.Root)
	diskVersion, err := journal.DiskFormatVersion(opts.FS, dir)
	if err != nil {
		return nil, fmt.Errorf("opening archive: %w", err)
	}
	if diskVersion > journal.FormatVersion {
		return nil, fmt.Errorf("opening archive: %w: journal format %d, this binary reads %d",
			journal.ErrFormatNewer, diskVersion, journal.FormatVersion)
	}

	lock, err := journal.AcquireLock(dir)
	if err != nil {
		return nil, fmt.Errorf("opening archive: %w", err)
	}
	c := &core{lock: lock}
	fail := func(err error) (*core, error) {
		return nil, errors.Join(err, c.close())
	}

	if c.st, err = store.Open(opts.FS, opts.Root); err != nil {
		return fail(fmt.Errorf("opening archive: %w", err))
	}
	jopts := journal.Options{Now: opts.Now, Logger: opts.Logger}
	if c.j, err = journal.Open(ctx, opts.FS, dir, jopts); err != nil {
		return fail(fmt.Errorf("opening archive: %w", err))
	}
	if err := c.openIndex(ctx, opts, forceRebuild); err != nil {
		return fail(err)
	}
	c.recordOpenEvents(opts)
	return c, nil
}

func (c *core) openIndex(ctx context.Context, opts Options, forceRebuild bool) error {
	path := indexPath(opts.Root)
	rebuildReason := ""
	if forceRebuild {
		rebuildReason = "requested"
	}
	if rebuildReason == "" {
		info, statErr := os.Stat(path)
		existing := statErr == nil && info.Size() > 0
		ix, err := index.Open(path)
		switch {
		case err == nil:
			c.ix = ix
		case errors.Is(err, index.ErrFormatNewer):
			return fmt.Errorf("opening archive: %w", err)
		case errors.Is(err, index.ErrStale) || existing:
			rebuildReason = err.Error()
		default:
			return fmt.Errorf("opening archive: %w", err)
		}
	}
	if c.ix != nil && c.ix.AppliedSeq() > c.j.LastSeq() {
		rebuildReason = fmt.Sprintf("index applied seq %d is ahead of the journal's %d", c.ix.AppliedSeq(), c.j.LastSeq())
		if err := c.ix.Close(); err != nil {
			return fmt.Errorf("opening archive: closing index: %w", err)
		}
		c.ix = nil
	}
	if rebuildReason != "" {
		ix, err := index.Rebuild(ctx, c.j, path)
		if err != nil {
			return fmt.Errorf("opening archive: %w", err)
		}
		c.ix, c.rebuilt = ix, true
		c.rebuildReason = rebuildReason
		return nil
	}
	err := c.j.Replay(ctx, c.ix.AppliedSeq(), func(rec journal.Record) error {
		c.replayed++
		return c.ix.Apply(rec)
	})
	if err != nil {
		return fmt.Errorf("opening archive: replaying the journal into the index: %w", err)
	}
	return nil
}

// recordOpenEvents writes the events Open owes after the index is open.
func (c *core) recordOpenEvents(opts Options) {
	now := opts.Now()
	if torn := c.j.Torn(); torn != nil {
		details := fmt.Sprintf("segment %s truncated at offset %d: %d bytes and %d intact records dropped",
			torn.Segment, torn.Offset, torn.DroppedBytes, torn.DroppedRecs)
		if err := c.ix.EventRecord(now, "journal_torn", "", "", details); err != nil {
			opts.Logger.Warn("archive: recording journal_torn", "err", err)
		}
	}
	if c.rebuilt {
		if err := c.ix.EventRecord(now, "index_rebuilt", "", "", c.rebuildReason); err != nil {
			opts.Logger.Warn("archive: recording index_rebuilt", "err", err)
		}
	}
}

// Service is the open archive core.
type Service struct {
	opts        Options
	core        *core
	views       *views.Tree
	collections map[string]bool

	// writeMu serialises Resolve, Append, ApplyAll and views.Sync.
	writeMu   sync.Mutex
	uploadSem chan struct{}

	lifeMu sync.Mutex
	closed bool
	active sync.WaitGroup // every operation and background goroutine
	ctx    context.Context
	cancel context.CancelFunc

	startOnce sync.Once
	wake      chan struct{}

	deriveAttempts map[store.Hash]int

	scrubMu   sync.Mutex
	scrubbing bool
	scrubDone int
	scrubAll  int
	scrubLast *scrub.Report

	importMu    sync.Mutex
	importSeq   int64
	importBusy  bool
	imports     map[int64]*ImportStatus
	importOrder []int64
}

// Open order (each step returns early on error and nothing later runs):
//  1. journal.DiskFormatVersion > journal.FormatVersion -> ErrFormatNewer, nothing touched
//  2. journal.AcquireLock
//  3. store.Open, journal.Open (torn tail truncated, journal_torn event after step 4)
//  4. index.Open; ErrFormatNewer -> refuse; ErrStale or any open failure of an
//     existing file -> index.Rebuild; then Replay(ix.AppliedSeq(), ix.Apply)
//  5. append folder_declared for each Options.Collections name the index lacks
//  6. views.Regenerate when the index was rebuilt or replay applied any record
func Open(ctx context.Context, opts Options) (*Service, error) {
	opts = opts.withDefaults()
	c, err := openCore(ctx, opts, false)
	if err != nil {
		return nil, err
	}
	s := &Service{
		opts:           opts,
		core:           c,
		collections:    map[string]bool{},
		uploadSem:      make(chan struct{}, opts.UploadConcurrency),
		wake:           make(chan struct{}, 1),
		deriveAttempts: map[store.Hash]int{},
		imports:        map[int64]*ImportStatus{},
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	for _, name := range opts.Collections {
		s.collections[name] = true
	}
	s.views = views.New(opts.FS, filepath.Join(opts.Root, viewsDirName), c.st.Path)

	if err := s.declareCollections(ctx); err != nil {
		s.cancel()
		return nil, errors.Join(err, c.close())
	}
	// Regenerate on every open, not only after a replay. A crash after the
	// index transaction commits but before views.Sync finishes leaves the
	// index current with the journal, so replay applies nothing, yet views/
	// is missing that placement or still holds a staging link.
	if _, err := s.views.Regenerate(ctx, c.ix); err != nil {
		s.cancel()
		return nil, errors.Join(fmt.Errorf("opening archive: regenerating views: %w", err), c.close())
	}
	return s, nil
}

func (s *Service) declareCollections(ctx context.Context) error {
	var recs []journal.Record
	for _, name := range s.opts.Collections {
		_, declared, err := s.core.ix.FolderByName(name)
		if err != nil {
			return fmt.Errorf("opening archive: %w", err)
		}
		if declared {
			continue
		}
		recs = append(recs, journal.Record{
			By: actorSystem, Type: journal.TypeFolderDeclared, Folder: name, Kind: journal.FolderKindCollection,
		})
	}
	if len(recs) == 0 {
		return nil
	}
	if _, err := s.appendAndApply(ctx, recs); err != nil {
		return fmt.Errorf("opening archive: declaring collections: %w", err)
	}
	return nil
}

func (s *Service) Index() *index.Index { return s.core.ix }
func (s *Service) Store() *store.Disk  { return s.core.st }

// CollectionDeclared reports whether name is in Options.Collections.
func (s *Service) CollectionDeclared(name string) bool { return s.collections[name] }

// Root is the archive root the service was opened on.
func (s *Service) Root() string { return s.opts.Root }

func (s *Service) now() time.Time { return s.opts.Now().UTC() }

// enter registers one running operation; the caller must call leave. It fails
// once Close has begun.
func (s *Service) enter() error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.active.Add(1)
	return nil
}

func (s *Service) leave() { s.active.Done() }

// Start runs the derive worker and, when ScrubInterval > 0, the scrub
// schedule. It returns immediately; cancelling ctx or calling Close stops them.
func (s *Service) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		workerCtx, cancel := context.WithCancel(s.ctx)
		stop := context.AfterFunc(ctx, cancel)
		s.startDeriveWorker(workerCtx)
		if s.opts.ScrubInterval > 0 {
			s.startScrubSchedule(workerCtx)
		}
		// Release the AfterFunc and context once the workers are gone.
		if err := s.enter(); err != nil {
			stop()
			cancel()
			return
		}
		go func() {
			defer s.leave()
			<-workerCtx.Done()
			stop()
			cancel()
		}()
	})
}

// Close stops workers, waits for running operations, closes the index and the
// journal and releases the lock. It is safe to call twice.
func (s *Service) Close() error {
	s.lifeMu.Lock()
	if s.closed {
		s.lifeMu.Unlock()
		return nil
	}
	s.closed = true
	s.lifeMu.Unlock()
	s.cancel()
	s.active.Wait()
	return s.core.close()
}

func (s *Service) event(eventType, folder, path, details string) {
	if err := s.core.ix.EventRecord(s.now(), eventType, folder, path, details); err != nil {
		s.opts.Logger.Warn("archive: recording event", "type", eventType, "folder", folder, "path", path, "err", err)
	}
}

// catchUpIndexLocked brings the index level with the journal when an earlier
// ApplyAll failed after its Append succeeded. Every write section calls it
// before reading state, so no record is ever built on a stale index. The
// caller holds writeMu. A failed catch-up refuses the write.
func (s *Service) catchUpIndexLocked(ctx context.Context) error {
	applied := s.core.ix.AppliedSeq()
	if applied >= s.core.j.LastSeq() {
		return nil
	}
	if err := s.core.j.Replay(ctx, applied, s.core.ix.Apply); err != nil {
		return fmt.Errorf("the index is behind the journal and catching up failed, write refused: %w", err)
	}
	if s.core.ix.AppliedSeq() < s.core.j.LastSeq() {
		return fmt.Errorf("the index is behind the journal (applied %d, journal %d), write refused", s.core.ix.AppliedSeq(), s.core.j.LastSeq())
	}
	// The views tree may lack the caught-up placements; Regenerate heals it.
	if _, err := s.views.Regenerate(ctx, s.core.ix); err != nil {
		s.opts.Logger.Warn("archive: regenerating views after an index catch-up", "err", err)
		s.event("views_sync_failed", "", "", err.Error())
	}
	return nil
}

// appendAndApply appends recs as one batch, then applies them to the index.
// The caller holds writeMu (or is Open, which is single-threaded). It returns
// the records with the Seq and Batch the journal assigned.
func (s *Service) appendAndApply(ctx context.Context, recs []journal.Record) ([]journal.Record, error) {
	for i := range recs {
		if recs[i].At.IsZero() {
			recs[i].At = s.now()
		}
	}
	lastSeq, err := s.core.j.Append(ctx, recs...)
	if err != nil {
		return nil, fmt.Errorf("appending to the journal: %w", err)
	}
	firstSeq := lastSeq - uint64(len(recs)) + 1
	for i := range recs {
		recs[i].Seq, recs[i].Batch = firstSeq+uint64(i), lastSeq
	}
	crashJournalAppended.Hit()
	if err := s.core.ix.ApplyAll(recs); err != nil {
		return nil, fmt.Errorf("applying journal records to the index (the journal has them; the next Open heals the index): %w", err)
	}
	crashIndexApplied.Hit()
	return recs, nil
}

// Ingest is design 4.5's Pipeline.Ingest. store.Put runs outside the write
// mutex (bounded by UploadConcurrency for by != "import"); Resolve, Append,
// ApplyAll and views.Sync run under it. On failure the source is untouched and
// an ingest_failed event is recorded.
func (s *Service) Ingest(ctx context.Context, by string, a ingest.Arrival) (ingest.Result, error) {
	if err := s.enter(); err != nil {
		return ingest.Result{}, err
	}
	defer s.leave()
	res, err := s.ingest(ctx, by, a)
	if err != nil {
		s.recordIngestFailure(a, err)
		return ingest.Result{}, err
	}
	s.wakeDerive()
	return res, nil
}

func (s *Service) recordIngestFailure(a ingest.Arrival, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.event("ingest_failed", a.Folder, a.Path, err.Error())
}

func (s *Service) ingest(ctx context.Context, by string, a ingest.Arrival) (ingest.Result, error) {
	path, err := ingest.CleanPath(a.Path)
	if err != nil {
		return ingest.Result{}, err
	}
	a.Path = path
	if !s.collections[a.Folder] {
		return ingest.Result{}, fmt.Errorf("ingesting %s/%s: %w", a.Folder, a.Path, ErrCollectionUnknown)
	}
	h, size, err := s.putArrival(ctx, by, a)
	if err != nil {
		return ingest.Result{}, err
	}
	return s.commit(ctx, by, a, h, size)
}

func (s *Service) putArrival(ctx context.Context, by string, a ingest.Arrival) (store.Hash, int64, error) {
	if by != "import" {
		select {
		case s.uploadSem <- struct{}{}:
			defer func() { <-s.uploadSem }()
		case <-ctx.Done():
			return store.Hash{}, 0, ctx.Err()
		}
	}
	rc, err := a.Open()
	if err != nil {
		return store.Hash{}, 0, fmt.Errorf("opening %s/%s: %w", a.Folder, a.Path, err)
	}
	defer rc.Close()
	crashArrivalOpened.Hit()
	h, size, err := s.core.st.Put(ctx, rc, a.Want)
	if err != nil {
		return store.Hash{}, 0, fmt.Errorf("storing %s/%s: %w", a.Folder, a.Path, err)
	}
	crashPutDone.Hit()
	return h, size, nil
}

// commit is the write section: Resolve, Append, ApplyAll, views.Sync.
func (s *Service) commit(ctx context.Context, by string, a ingest.Arrival, h store.Hash, size int64) (ingest.Result, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if err := s.catchUpIndexLocked(ctx); err != nil {
		return ingest.Result{}, err
	}
	recs, res, err := ingest.Resolve(s.core.ix, a, h, size, by, func() ingest.ItemID { return ingest.NewItemID(s.now()) },
		func(string, string) bool { return false })
	if err != nil {
		return ingest.Result{}, err
	}
	if len(recs) == 0 {
		return res, nil
	}
	recs, err = s.appendAndApply(ctx, recs)
	if err != nil {
		return ingest.Result{}, fmt.Errorf("placing %s/%s: %w", a.Folder, a.Path, err)
	}
	if res.EntryID == 0 {
		placed, found, err := s.core.ix.PlacementAt(a.Folder, res.Path)
		if err != nil || !found {
			return ingest.Result{}, fmt.Errorf("placing %s/%s: placement missing after apply (found=%v): %v", a.Folder, res.Path, found, err)
		}
		res.EntryID = placed.EntryID
	}
	s.syncViews(recs)
	return res, nil
}

// syncViews links the touched placements. A failure is logged and recorded:
// the journal is the truth and Regenerate heals the tree.
func (s *Service) syncViews(recs []journal.Record) {
	if err := s.views.Sync(s.core.ix, recs); err != nil {
		s.opts.Logger.Warn("archive: syncing views after a committed append", "err", err)
		s.event("views_sync_failed", "", "", err.Error())
		return
	}
	crashViewsSynced.Hit()
}

// Place is a Placement for a blob already held, re-verified first.
func (s *Service) Place(ctx context.Context, by, collection, path string, mtime time.Time, h store.Hash) (ingest.Result, error) {
	if err := s.enter(); err != nil {
		return ingest.Result{}, err
	}
	defer s.leave()
	a := ingest.Arrival{Folder: collection, Path: path, Mtime: mtime, Want: &h}
	res, err := s.place(ctx, by, a, h)
	if err != nil {
		s.recordIngestFailure(a, err)
		return ingest.Result{}, err
	}
	s.wakeDerive()
	return res, nil
}

func (s *Service) place(ctx context.Context, by string, a ingest.Arrival, h store.Hash) (ingest.Result, error) {
	path, err := ingest.CleanPath(a.Path)
	if err != nil {
		return ingest.Result{}, err
	}
	a.Path = path
	if !s.collections[a.Folder] {
		return ingest.Result{}, fmt.Errorf("placing %s/%s: %w", a.Folder, a.Path, ErrCollectionUnknown)
	}
	info, held, err := s.core.ix.Blob(h)
	if err != nil {
		return ingest.Result{}, err
	}
	if !held {
		return ingest.Result{}, fmt.Errorf("placing %s/%s: %w", a.Folder, a.Path, ErrBlobNotHeld)
	}
	if err := s.verifyBlob(ctx, h, info); err != nil {
		return ingest.Result{}, fmt.Errorf("placing %s/%s: %w", a.Folder, a.Path, err)
	}
	return s.commit(ctx, by, a, h, info.Size)
}

// verifyBlob re-hashes a held blob. A damaged or missing blob is marked bad and
// reported as ErrBlobBad; a blob that verifies again has its bad flag cleared.
func (s *Service) verifyBlob(ctx context.Context, h store.Hash, info index.BlobInfo) error {
	err := s.core.st.VerifySize(ctx, h, info.Size)
	if err == nil {
		now := s.now()
		if info.Bad {
			if clearErr := s.core.ix.BlobBadSet(h, false, ""); clearErr != nil {
				s.opts.Logger.Warn("archive: clearing bad flag", "hash", h.String(), "err", clearErr)
			}
		}
		if stampErr := s.core.ix.BlobVerifiedSet(h, now); stampErr != nil {
			s.opts.Logger.Warn("archive: stamping blob verified", "hash", h.String(), "err", stampErr)
		}
		return nil
	}
	reason := ""
	var corrupt *store.CorruptError
	switch {
	case errors.As(err, &corrupt):
		reason = corrupt.Reason
	case errors.Is(err, store.ErrNotFound):
		reason = "missing"
	default:
		return fmt.Errorf("verifying blob %s: %w", h, err)
	}
	if markErr := s.core.ix.BlobBadSet(h, true, reason); markErr != nil {
		s.opts.Logger.Warn("archive: marking blob bad", "hash", h.String(), "err", markErr)
	}
	s.event("blob_bad", "", "", fmt.Sprintf("%s: %s", h, reason))
	return fmt.Errorf("blob %s: %s: %w", h, reason, ErrBlobBad)
}

// OpenVerified returns the blob for a full-body download. ErrBlobBad when the
// index marks it bad. The caller verifies the bytes as it streams them.
func (s *Service) OpenVerified(h store.Hash) (io.ReadSeekCloser, int64, error) {
	if err := s.enter(); err != nil {
		return nil, 0, err
	}
	defer s.leave()
	info, held, err := s.core.ix.Blob(h)
	if err != nil {
		return nil, 0, err
	}
	if !held {
		return nil, 0, fmt.Errorf("opening blob %s: %w", h, ErrBlobNotHeld)
	}
	if info.Bad {
		return nil, 0, fmt.Errorf("opening blob %s: %w", h, ErrBlobBad)
	}
	rc, err := s.core.st.Open(h)
	if err != nil {
		return nil, 0, err
	}
	return rc, info.Size, nil
}

type RebuildReport struct {
	Records     uint64        `json:"records"`
	Entries     int64         `json:"entries"`
	ViewsLinked int           `json:"views_linked"`
	Torn        *journal.Torn `json:"torn"`
}

// RebuildIndex takes the lock, deletes and rebuilds index.db from the journal,
// and regenerates views/. It returns journal.ErrLocked when the daemon holds
// the lock. It never opens the legacy catalog.
func RebuildIndex(ctx context.Context, opts Options) (RebuildReport, error) {
	opts = opts.withDefaults()
	c, err := openCore(ctx, opts, true)
	if err != nil {
		return RebuildReport{}, err
	}
	report, err := rebuildReport(ctx, opts, c)
	return report, errors.Join(err, c.close())
}

func rebuildReport(ctx context.Context, opts Options, c *core) (RebuildReport, error) {
	tree := views.New(opts.FS, filepath.Join(opts.Root, viewsDirName), c.st.Path)
	regenerated, err := tree.Regenerate(ctx, c.ix)
	if err != nil {
		return RebuildReport{}, fmt.Errorf("regenerating views: %w", err)
	}
	folders, err := c.ix.Folders()
	if err != nil {
		return RebuildReport{}, err
	}
	var entries int64
	for _, f := range folders {
		stats, err := c.ix.FolderStats(f.Name)
		if err != nil {
			return RebuildReport{}, err
		}
		entries += stats.Live + stats.Trashed
	}
	return RebuildReport{Records: c.j.LastSeq(), Entries: entries, ViewsLinked: regenerated.Linked, Torn: c.j.Torn()}, nil
}

// ScrubOffline takes the lock and runs one full scrub pass. It returns
// journal.ErrLocked when the daemon holds the lock.
func ScrubOffline(ctx context.Context, opts Options) (scrub.Report, error) {
	opts = opts.withDefaults()
	c, err := openCore(ctx, opts, false)
	if err != nil {
		return scrub.Report{}, err
	}
	report, err := scrub.Run(ctx, c.st, c.ix, scrub.Options{
		RateBytesPerSec: opts.ScrubRateBytesPerSec, Now: opts.Now, Sleep: opts.Sleep,
	})
	return report, errors.Join(err, c.close())
}

type FormatReport struct {
	JournalDisk   int  `json:"journal_disk"`
	JournalBinary int  `json:"journal_binary"`
	IndexDisk     int  `json:"index_disk"`
	IndexBinary   int  `json:"index_binary"`
	Newer         bool `json:"newer"`
}

// FormatVersions reports the on-disk and supported format versions without
// taking the lock or modifying anything. A missing file reports 0.
func FormatVersions(root string) (FormatReport, error) {
	journalDisk, err := journal.DiskFormatVersion(fsx.OS(), journalDir(root))
	if err != nil {
		return FormatReport{}, err
	}
	indexDisk, err := indexFormatOnDisk(indexPath(root))
	if err != nil {
		return FormatReport{}, err
	}
	report := FormatReport{
		JournalDisk: journalDisk, JournalBinary: journal.FormatVersion,
		IndexDisk: indexDisk, IndexBinary: index.FormatVersion,
	}
	report.Newer = journalDisk > report.JournalBinary || indexDisk > report.IndexBinary
	return report, nil
}

var sqliteMagic = []byte("SQLite format 3\x00")

// indexFormatOnDisk reads PRAGMA user_version (a big-endian int at offset 60 of
// the SQLite header) straight from the file, so nothing is opened or modified.
func indexFormatOnDisk(path string) (int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading index format: %w", err)
	}
	defer f.Close()
	header := make([]byte, 64)
	n, err := io.ReadFull(f, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("reading index format: %w", err)
	}
	if n < len(header) || !bytes.HasPrefix(header, sqliteMagic) {
		return 0, nil
	}
	return int(binary.BigEndian.Uint32(header[60:64])), nil
}
