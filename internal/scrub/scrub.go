// Package scrub cross-checks the blob store and the index both ways: a blob the
// index names that is missing or fails verification is marked bad, and a blob
// on disk that nothing names is reported as an orphan. Scrub never deletes.
package scrub

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/store"
)

const (
	FindingBad    = "bad"
	FindingOrphan = "orphan"

	ReasonMissing = "missing"

	pageBlobs = 1000

	// MaxErroredBlobs ends a pass with an error once this many named blobs have
	// failed with ordinary errors: that many is a systemic fault (EMFILE storm,
	// an unreadable shard directory), not a few unlucky blobs, and the pass must
	// not be recorded as completed. It also keeps the page query under the
	// index's id cap.
	MaxErroredBlobs = 10000

	// OrphanGrace is how old a blob file must be before it is reported as an
	// orphan. Put links a blob before the journal record that names it is
	// appended, so a younger unnamed blob is an ingest in flight.
	OrphanGrace = 10 * time.Minute
)

type Finding struct {
	Hash   store.Hash
	Kind   string // "bad" (named by the index, missing or failing Verify) | "orphan" (on disk, no record names it)
	Reason string // "missing" | "hash_mismatch" | "truncated" | "unreadable" | ""
}

type Report struct {
	Checked      int
	CheckedBytes int64
	Findings     []Finding
	Stray        []string // files under blobs/ that are not hash-named
	// Unverified lists blobs that could not be verified this pass because of an
	// ordinary error (EMFILE, EACCES, ...). They are not flagged bad and not
	// stamped, so the next pass retries them.
	Unverified            []Unverified
	StartedAt, FinishedAt time.Time
}

// Unverified is one blob scrub could not reach a verdict on.
type Unverified struct {
	Hash  store.Hash
	Error string
}

type Options struct {
	RateBytesPerSec int64 // 0 = unthrottled
	Now             func() time.Time
	Sleep           func(time.Duration) // injectable for tests
	Progress        func(checked int, bytes int64)
}

type runner struct {
	st     *store.Disk
	ix     *index.Index
	opts   Options
	report Report
	// errored holds the named blobs that failed to verify this pass. They stay
	// unstamped, so without this set they would head every page forever.
	errored map[store.Hash]bool
}

// Run cross-checks store and index both ways, marks bad blobs in the index,
// clears the bad flag of a blob that verifies again, stamps verified_at and
// records one event per finding. Cancellation returns the partial report and
// ctx.Err().
func Run(ctx context.Context, st *store.Disk, ix *index.Index, opts Options) (Report, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}
	r := &runner{st: st, ix: ix, opts: opts, errored: map[store.Hash]bool{}}
	r.report.StartedAt = opts.Now()
	err := r.run(ctx)
	r.report.FinishedAt = opts.Now()
	if err != nil {
		return r.report, err
	}
	return r.report, nil
}

func (r *runner) run(ctx context.Context) error {
	if err := r.checkNamedBlobs(ctx); err != nil {
		return err
	}
	if err := r.checkOrphans(ctx); err != nil {
		return err
	}
	return r.recordStray(ctx)
}

// checkNamedBlobs verifies every blob the index names, never-verified first,
// then least recently verified. Each blob is stamped as it is checked, so a
// page that starts with a blob stamped during this run means the pass is done,
// and a restart resumes where the stamps left off.
func (r *runner) checkNamedBlobs(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Errored blobs stay unstamped and so stay at the front of the order:
		// ask for enough extra rows that a full page of unattempted blobs follows.
		page, err := r.ix.BlobsByVerifiedAge(pageBlobs + len(r.errored))
		if err != nil {
			return fmt.Errorf("scrubbing: %w", err)
		}
		page = r.withoutErrored(page)
		if len(page) == 0 {
			return nil
		}
		if len(page) > pageBlobs {
			page = page[:pageBlobs]
		}
		first, ok, err := r.ix.Blob(page[0])
		if err != nil {
			return fmt.Errorf("scrubbing: %w", err)
		}
		if ok && first.VerifiedAt != nil && !first.VerifiedAt.Before(r.report.StartedAt) {
			return nil
		}
		for _, h := range page {
			if err := r.checkNamed(ctx, h); err != nil {
				return err
			}
			if len(r.errored) >= MaxErroredBlobs {
				return fmt.Errorf("scrubbing: %d blobs failed with ordinary errors, last: %s",
					len(r.errored), r.report.Unverified[len(r.report.Unverified)-1].Error)
			}
		}
	}
}

func (r *runner) withoutErrored(page []store.Hash) []store.Hash {
	if len(r.errored) == 0 {
		return page
	}
	kept := make([]store.Hash, 0, len(page))
	for _, h := range page {
		if !r.errored[h] {
			kept = append(kept, h)
		}
	}
	return kept
}

func (r *runner) checkNamed(ctx context.Context, h store.Hash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, ok, err := r.ix.Blob(h)
	if err != nil {
		return fmt.Errorf("scrubbing %s: %w", h, err)
	}
	if !ok {
		return nil
	}
	if info.VerifiedAt != nil && !info.VerifiedAt.Before(r.report.StartedAt) {
		return nil // a short page reaches past the unstamped blobs into ones this pass already did
	}
	began := r.opts.Now()
	verifyErr := r.st.VerifySize(ctx, h, info.Size)
	if verifyErr != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	reason, bad := badReason(verifyErr)
	if verifyErr != nil && !bad {
		// Could not verify right now: say nothing about the blob. It stays
		// unflagged and unstamped and the next pass retries it.
		r.errored[h] = true
		r.noteUnverified(h, verifyErr)
		return nil
	}

	if err := r.ix.BlobVerifiedSet(h, r.opts.Now()); err != nil {
		return fmt.Errorf("scrubbing %s: %w", h, err)
	}
	if err := r.settle(h, info, bad, reason); err != nil {
		return err
	}
	if !bad || reason != ReasonMissing {
		r.charge(info.Size, began)
	}
	r.report.Checked++
	if !bad || reason != ReasonMissing {
		r.report.CheckedBytes += info.Size
	}
	if r.opts.Progress != nil {
		r.opts.Progress(r.report.Checked, r.report.CheckedBytes)
	}
	return nil
}

func (r *runner) noteUnverified(h store.Hash, err error) {
	r.report.Unverified = append(r.report.Unverified, Unverified{Hash: h, Error: err.Error()})
}

// badReason classifies a Verify error: a missing blob or a *CorruptError is
// "bad"; nil is good; anything else (EMFILE, EACCES, ...) is no verdict.
func badReason(err error) (reason string, bad bool) {
	if err == nil {
		return "", false
	}
	if errors.Is(err, store.ErrNotFound) {
		return ReasonMissing, true
	}
	var corrupt *store.CorruptError
	if errors.As(err, &corrupt) {
		return corrupt.Reason, true
	}
	return "", false
}

// settle updates the bad flag and records the finding for a named blob.
func (r *runner) settle(h store.Hash, info index.BlobInfo, bad bool, reason string) error {
	if !bad {
		if !info.Bad {
			return nil
		}
		if err := r.ix.BlobBadSet(h, false, ""); err != nil {
			return fmt.Errorf("scrubbing %s: %w", h, err)
		}
		return nil
	}
	if err := r.ix.BlobBadSet(h, true, reason); err != nil {
		return fmt.Errorf("scrubbing %s: %w", h, err)
	}
	r.report.Findings = append(r.report.Findings, Finding{Hash: h, Kind: FindingBad, Reason: reason})
	if err := r.ix.EventRecord(r.opts.Now(), "blob_bad", "", "", fmt.Sprintf("blob %s: %s", h, reason)); err != nil {
		return fmt.Errorf("scrubbing %s: %w", h, err)
	}
	return nil
}

// charge sleeps long enough that the blob read cost at least size/rate, less
// the time the read itself took. Time is never banked.
func (r *runner) charge(size int64, began time.Time) {
	if r.opts.RateBytesPerSec <= 0 || size <= 0 {
		return
	}
	owed := time.Duration(float64(size) / float64(r.opts.RateBytesPerSec) * float64(time.Second))
	owed -= r.opts.Now().Sub(began)
	if owed > 0 {
		r.opts.Sleep(owed)
	}
}

// checkOrphans walks the store and reports every blob the index does not name.
// An orphan is verified (so a damaged one is described) but never removed.
func (r *runner) checkOrphans(ctx context.Context) error {
	return r.st.Walk(ctx, func(h store.Hash, size int64) error {
		_, named, err := r.ix.Blob(h)
		if err != nil {
			return fmt.Errorf("scrubbing orphans: %w", err)
		}
		if named {
			return nil
		}
		if r.inGrace(h) {
			return nil
		}
		return r.checkOrphan(ctx, h, size)
	})
}

// inGrace reports whether the blob file is younger than OrphanGrace, or cannot
// be stat'ed: a blob Put linked a moment ago may not be named by the index yet.
func (r *runner) inGrace(h store.Hash) bool {
	modified, err := r.st.ModTime(h)
	if err != nil {
		return true // gone or unreadable right now: do not accuse it
	}
	return r.opts.Now().Sub(modified) < OrphanGrace
}

func (r *runner) checkOrphan(ctx context.Context, h store.Hash, size int64) error {
	began := r.opts.Now()
	verifyErr := r.st.Verify(ctx, h)
	if verifyErr != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	reason, bad := badReason(verifyErr)
	if verifyErr != nil && !bad {
		r.noteUnverified(h, verifyErr) // still an orphan; its bytes are unjudged
	}
	r.charge(size, began)
	r.report.Findings = append(r.report.Findings, Finding{Hash: h, Kind: FindingOrphan, Reason: reason})
	details := fmt.Sprintf("blob %s is on disk and no journal record names it", h)
	if bad {
		details += " (" + reason + ")"
	}
	if err := r.ix.EventRecord(r.opts.Now(), "blob_orphan", "", "", details); err != nil {
		return fmt.Errorf("scrubbing orphan %s: %w", h, err)
	}
	r.report.Checked++
	r.report.CheckedBytes += size
	if r.opts.Progress != nil {
		r.opts.Progress(r.report.Checked, r.report.CheckedBytes)
	}
	return nil
}

func (r *runner) recordStray(ctx context.Context) error {
	return r.st.WalkStray(ctx, func(path string) error {
		r.report.Stray = append(r.report.Stray, path)
		if err := r.ix.EventRecord(r.opts.Now(), "blob_stray", "", path, "file under blobs/ is not a hash-named blob"); err != nil {
			return fmt.Errorf("scrubbing stray %s: %w", path, err)
		}
		return nil
	})
}
