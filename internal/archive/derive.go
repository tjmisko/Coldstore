package archive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tjmisko/coldstore/internal/filetype"
	"github.com/tjmisko/coldstore/internal/scrub"
	"github.com/tjmisko/coldstore/internal/store"
	"github.com/tjmisko/coldstore/internal/thumbnail"
)

const (
	derivePageSize     = 64
	deriveMaxAttempts  = 3
	scrubCheckInterval = time.Hour
	thumbSuffix        = ".thumb.jpg"
	thumbTempMiddle    = ".tmp"
)

func (s *Service) wakeDerive() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) startDeriveWorker(ctx context.Context) {
	if err := s.enter(); err != nil {
		return
	}
	go func() {
		defer s.leave()
		for {
			for ctx.Err() == nil && s.deriveOnce(ctx) > 0 {
			}
			select {
			case <-s.wake:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// deriveOnce derives one page of blobs and returns how many it recorded.
func (s *Service) deriveOnce(ctx context.Context) int {
	hashes, err := s.core.ix.BlobsToDerive(derivePageSize)
	if err != nil {
		s.opts.Logger.Warn("archive: listing blobs to derive", "err", err)
		return 0
	}
	recorded := 0
	for _, h := range hashes {
		if ctx.Err() != nil {
			break
		}
		if s.deriveBlob(ctx, h) {
			recorded++
		}
	}
	return recorded
}

// deriveBlob sniffs one blob and renders its thumbnail. It reports whether the
// outcome was recorded.
func (s *Service) deriveBlob(ctx context.Context, h store.Hash) bool {
	blobPath := s.core.st.Path(h)
	detected, err := filetype.SniffFile(blobPath)
	if err != nil {
		return s.deriveFailed(h, "", fmt.Errorf("sniffing: %w", err))
	}
	ext := s.thumbnailExtension(h)
	if s.opts.Render == nil || (ext == "" && !thumbnail.CanThumbnailType(detected)) {
		// Nothing can be rendered: a final state without an error, so the UI shows no glyph.
		return s.deriveRecord(h, detected, "failed", "")
	}

	dst := filepath.Join(s.opts.Root, thumbsDirName, filepath.FromSlash(h.RelPath())+thumbSuffix)
	tmp := strings.TrimSuffix(dst, thumbSuffix) + thumbTempMiddle + thumbSuffix
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return s.deriveFailed(h, detected, fmt.Errorf("creating thumbnail directory: %w", err))
	}
	if err := s.opts.Render(ctx, blobPath, ext, detected, tmp); err != nil {
		_ = os.Remove(tmp)
		if ctx.Err() != nil {
			return false
		}
		return s.deriveFailed(h, detected, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return s.deriveFailed(h, detected, fmt.Errorf("installing thumbnail: %w", err))
	}
	delete(s.deriveAttempts, h)
	return s.deriveRecord(h, detected, "done", "")
}

// thumbnailExtension is the first extension, over every placement of the blob,
// the generator can render; "" when none is.
func (s *Service) thumbnailExtension(h store.Hash) string {
	folders, err := s.core.ix.Folders()
	if err != nil {
		return ""
	}
	for _, folder := range folders {
		placements, err := s.core.ix.PlacementsByCurrentHash(folder.Name, h)
		if err != nil {
			continue
		}
		for _, p := range placements {
			ext := strings.ToLower(filepath.Ext(p.Path))
			if ext != "" && thumbnail.CanThumbnail(ext) {
				return ext
			}
		}
	}
	return ""
}

func (s *Service) deriveRecord(h store.Hash, detected, state, thumbErr string) bool {
	if err := s.core.ix.BlobDerivedSet(h, detected, state, thumbErr); err != nil {
		s.opts.Logger.Warn("archive: recording derived state", "hash", h.String(), "err", err)
		return false
	}
	return true
}

// deriveFailed records one failed attempt; the third becomes the final state.
func (s *Service) deriveFailed(h store.Hash, detected string, cause error) bool {
	s.deriveAttempts[h]++
	state := ""
	if s.deriveAttempts[h] >= deriveMaxAttempts {
		state = "failed"
		delete(s.deriveAttempts, h)
	}
	return s.deriveRecord(h, detected, state, cause.Error())
}

type ScrubStatus struct {
	Running bool          `json:"running"`
	Checked int           `json:"checked"`
	Total   int           `json:"total"`
	Last    *scrub.Report `json:"last"`
}

// ScrubStart runs one scrub pass in the background. rateBytesPerSec 0 is
// unthrottled. It fails with ErrScrubRunning when a pass is in progress.
func (s *Service) ScrubStart(rateBytesPerSec int64) error {
	if err := s.enter(); err != nil {
		return err
	}
	s.scrubMu.Lock()
	if s.scrubbing {
		s.scrubMu.Unlock()
		s.leave()
		return ErrScrubRunning
	}
	stats, err := s.core.ix.ArchiveStats()
	if err != nil {
		s.scrubMu.Unlock()
		s.leave()
		return err
	}
	s.scrubbing, s.scrubDone, s.scrubAll = true, 0, int(stats.BlobCount)
	s.scrubMu.Unlock()

	go func() {
		defer s.leave()
		report, err := scrub.Run(s.ctx, s.core.st, s.core.ix, scrub.Options{
			RateBytesPerSec: rateBytesPerSec,
			Now:             s.opts.Now,
			Sleep:           s.opts.Sleep,
			Progress: func(checked int, _ int64) {
				s.scrubMu.Lock()
				s.scrubDone = checked
				s.scrubMu.Unlock()
			},
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			s.opts.Logger.Warn("archive: scrub stopped", "err", err)
		}
		s.scrubMu.Lock()
		s.scrubbing = false
		s.scrubLast = &report
		s.scrubMu.Unlock()
	}()
	return nil
}

func (s *Service) ScrubStatus() ScrubStatus {
	s.scrubMu.Lock()
	defer s.scrubMu.Unlock()
	return ScrubStatus{Running: s.scrubbing, Checked: s.scrubDone, Total: s.scrubAll, Last: s.scrubLast}
}

// lastScrubTime is when a pass last finished: this process's last report, else
// the oldest verified_at (a pass stamps every blob), else the zero time when a
// blob has never been verified.
func (s *Service) lastScrubTime() (time.Time, bool) {
	s.scrubMu.Lock()
	last := s.scrubLast
	s.scrubMu.Unlock()
	if last != nil {
		return last.FinishedAt, true
	}
	hashes, err := s.core.ix.BlobsByVerifiedAge(1)
	if err != nil || len(hashes) == 0 {
		return time.Time{}, false
	}
	info, ok, err := s.core.ix.Blob(hashes[0])
	if err != nil || !ok || info.VerifiedAt == nil {
		return time.Time{}, err == nil && ok
	}
	return *info.VerifiedAt, true
}

// scrubIfDue starts a pass when the last one is older than ScrubInterval.
func (s *Service) scrubIfDue() {
	last, any := s.lastScrubTime()
	if !any || s.now().Sub(last) < s.opts.ScrubInterval {
		return
	}
	if err := s.ScrubStart(s.opts.ScrubRateBytesPerSec); err != nil && !errors.Is(err, ErrScrubRunning) && !errors.Is(err, ErrClosed) {
		s.opts.Logger.Warn("archive: starting a scheduled scrub", "err", err)
	}
}

func (s *Service) startScrubSchedule(ctx context.Context) {
	if err := s.enter(); err != nil {
		return
	}
	go func() {
		defer s.leave()
		ticker := time.NewTicker(scrubCheckInterval)
		defer ticker.Stop()
		for {
			s.scrubIfDue()
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
}
