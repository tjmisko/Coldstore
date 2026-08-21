package daemon

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	metadatapkg "github.com/tjmisko/coldstore/internal/metadata"
)

type metadataCandidateProber interface {
	ProbeStillCandidate(context.Context, string, string) (metadatapkg.Extracted, error)
}

type pairCandidateIndex map[string][]int64

func (d *Daemon) buildPairCandidateIndex() (pairCandidateIndex, error) {
	if _, ok := d.metadataExtractor.(metadataCandidateProber); !ok {
		return nil, nil
	}
	refs, err := d.cat.PairCandidateRefs()
	if err != nil {
		return nil, err
	}
	index := make(pairCandidateIndex)
	types := make(map[string]struct{ still, video bool })
	for _, ref := range refs {
		key := pairClusterIdentity(ref.FolderLabel, ref.RelativePath, ref.Filename)
		index[key] = append(index[key], ref.ID)
		kind := types[key]
		kind.still = kind.still || isPairStill(ref.Extension)
		kind.video = kind.video || isPairVideo(ref.Extension)
		types[key] = kind
	}
	for key, kind := range types {
		if !kind.still || !kind.video {
			delete(index, key)
		}
	}
	return index, nil
}

func pairClusterIdentity(folder, relativePath, filename string) string {
	dir := path.Dir(relativePath)
	if dir == "." {
		dir = ""
	}
	return folder + "\x00" + dir + "\x00" + catalog.CanonicalPairStem(filename)
}

// recordMetadataAndPair makes a newly discovered CID and every pairing
// consequence visible under one mover-lock acquisition. An archive therefore
// sees either the old unpaired state or the completed candidate comparison,
// never a MOV with a freshly written CID while its matching still is omitted.
// Extractors without the narrow ExifTool probe still use the same metadata
// write path and simply skip candidate resolution.
func (d *Daemon) recordMetadataAndPair(ctx context.Context, file catalog.FileRecord,
	value metadatapkg.Extracted, sourcePathFn func(*catalog.FileRecord) string,
	pairIndex pairCandidateIndex) error {

	clusterKey := pairClusterIdentity(file.FolderLabel, file.RelativePath, file.Filename)
	err := d.mover.WithPairLock(ctx, func(ctx context.Context) error {
		if err := d.cat.SetExtractedMetadata(file.ID, value, time.Now().Unix()); err != nil {
			return err
		}
		prober, ok := d.metadataExtractor.(metadataCandidateProber)
		if !ok || (!isPairStill(file.Extension) && !isPairVideo(file.Extension)) {
			return nil
		}
		candidateIDs := pairIndex[clusterKey]
		if len(candidateIDs) == 0 {
			return nil
		}
		anchor, err := d.cat.GetFileByID(file.ID)
		if err != nil {
			return err
		}
		return d.pairCandidateCluster(ctx, *anchor, candidateIDs, sourcePathFn, prober)
	})
	if err == nil && len(pairIndex[clusterKey]) > 0 {
		d.pairAttempted.Store(clusterKey, struct{}{})
	}
	return err
}

// backfillPairCandidates reaches catalogs whose ordinary metadata extraction
// completed before pair grouping existed. A definitive result is remembered
// for this daemon process so an empty still probe is not repeated every tick;
// transient failures are deleted from the set and retry on the next pass.
func (d *Daemon) backfillPairCandidates(ctx context.Context, index pairCandidateIndex,
	sourcePathFn func(*catalog.FileRecord) string) {
	prober, ok := d.metadataExtractor.(metadataCandidateProber)
	if !ok || len(index) == 0 {
		return
	}
	keys := make([]string, 0, len(index))
	for key := range index {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}
		if _, loaded := d.pairAttempted.LoadOrStore(key, struct{}{}); loaded {
			continue
		}
		ids := index[key]
		err := d.mover.WithPairLock(ctx, func(ctx context.Context) error {
			files, err := d.cat.FilesByIDs(ids)
			if err != nil {
				return err
			}
			for _, file := range files {
				if isPairVideo(file.Extension) {
					return d.pairCandidateCluster(ctx, file, ids, sourcePathFn, prober)
				}
			}
			return nil
		})
		if err != nil {
			d.pairAttempted.Delete(key)
			if ctx.Err() == nil {
				d.logger.Warn("Live Photo backfill failed", "cluster", key, "error", err)
			}
		}
	}
}

func (d *Daemon) pairCandidateCluster(ctx context.Context, anchor catalog.FileRecord,
	candidateIDs []int64,
	sourcePathFn func(*catalog.FileRecord) string, prober metadataCandidateProber) error {

	candidates, err := d.cat.FilesByIDs(candidateIDs)
	if err != nil {
		return err
	}
	stable := candidates[:0]
	for _, candidate := range candidates {
		if candidate.Status == catalog.StatusSynced || candidate.Status == catalog.StatusArchived {
			stable = append(stable, candidate)
		}
	}
	candidates = stable

	videos := make([]catalog.FileRecord, 0)
	stills := make([]catalog.FileRecord, 0)
	sidecars := make([]catalog.FileRecord, 0)
	for _, candidate := range candidates {
		switch {
		case isPairVideo(candidate.Extension):
			videos = append(videos, candidate)
		case isPairStill(candidate.Extension):
			stills = append(stills, candidate)
		case isPairSidecar(candidate.Extension):
			sidecars = append(sidecars, candidate)
		}
	}
	if len(videos) == 0 || len(stills) == 0 {
		return nil
	}

	assignments := make([]catalog.PairAssignment, 0, len(candidates))
	stillKeys := make(map[int64]string)
	for i := range stills {
		still := &stills[i]
		if !hasCloseVideo(*still, videos) {
			continue
		}
		if still.PairSource == "content_id" && strings.HasPrefix(still.PairKey, "cid:") {
			stillKeys[still.ID] = still.PairKey
			continue
		}
		value, probeErr := prober.ProbeStillCandidate(ctx, sourcePathFn(still), still.Extension)
		if probeErr != nil {
			if errors.Is(probeErr, exec.ErrNotFound) {
				return d.assignDegradedPair(anchor, candidates, videos, stills)
			}
			if ctx.Err() == nil {
				if recordErr := d.cat.RecordPairingFailure(still.ID, probeErr.Error(), time.Now().Unix()); recordErr != nil {
					return fmt.Errorf("recording pairing failure: %w", recordErr)
				}
			}
			return probeErr
		}
		contentID := strings.ToLower(strings.TrimSpace(value.ContentID))
		if contentID == "" {
			continue // a successful empty probe is authoritative absence
		}
		key := "cid:" + contentID
		stillKeys[still.ID] = key
		assignments = append(assignments, catalog.PairAssignment{
			ID: still.ID, Key: key, Source: "content_id",
		})
	}
	// A sidecar has no CID to compare. It inherits a content-id authority only
	// when exactly one CID is confirmed by both a video and a still in this
	// candidate cluster; multiple confirmed CIDs make basename attachment
	// ambiguous, so the sidecar stays unpaired rather than joining the wrong one.
	videoKeys := make(map[string]struct{})
	for _, video := range videos {
		if video.PairSource == "content_id" && strings.HasPrefix(video.PairKey, "cid:") {
			videoKeys[video.PairKey] = struct{}{}
		}
	}
	confirmed := make(map[string]struct{})
	for _, still := range stills {
		key := stillKeys[still.ID]
		if _, ok := videoKeys[key]; ok && key != "" {
			confirmed[key] = struct{}{}
		}
	}
	if len(confirmed) == 1 {
		var key string
		for key = range confirmed {
		}
		for _, sidecar := range sidecars {
			assignments = append(assignments, catalog.PairAssignment{
				ID: sidecar.ID, Key: key, Source: "content_id",
			})
		}
	}
	// Stills and authority-inheriting sidecars become visible together. The
	// mover lock prevents movement during the transaction; the transaction
	// prevents other catalog readers from observing a half-assigned cluster.
	return d.cat.SetPairAssignments(assignments)
}

func (d *Daemon) assignDegradedPair(anchor catalog.FileRecord, candidates, videos,
	stills []catalog.FileRecord) error {
	eligible := make(map[int64]struct{})
	for _, still := range stills {
		if hasCloseVideo(still, videos) {
			eligible[still.ID] = struct{}{}
			for _, video := range videos {
				if captureTimesClose(still, video) {
					eligible[video.ID] = struct{}{}
				}
			}
		}
	}
	if len(eligible) < 2 {
		return nil
	}
	dir := path.Dir(anchor.RelativePath)
	if dir == "." {
		dir = ""
	}
	key := "stem:" + path.Join(dir, catalog.CanonicalPairStem(anchor.Filename))
	ids := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		if _, ok := eligible[candidate.ID]; ok || isPairSidecar(candidate.Extension) {
			ids = append(ids, candidate.ID)
		}
	}
	if err := d.cat.SetDegradedPair(ids, key); err != nil {
		return err
	}
	d.cat.LogEventDeduped("pairing_degraded", anchor.FolderLabel, anchor.RelativePath,
		"ExifTool is unavailable; Live Photo candidates are grouped by basename", 60)
	d.logger.Warn("ExifTool unavailable; using degraded Live Photo pairing",
		"folder", anchor.FolderLabel, "path", anchor.RelativePath)
	return nil
}

func hasCloseVideo(still catalog.FileRecord, videos []catalog.FileRecord) bool {
	for _, video := range videos {
		if captureTimesClose(still, video) {
			return true
		}
	}
	return false
}

func captureTimesClose(a, b catalog.FileRecord) bool {
	at, aok := parseCapturedLocal(a.CapturedLocal)
	bt, bok := parseCapturedLocal(b.CapturedLocal)
	if !aok || !bok {
		return true
	}
	delta := at.Sub(bt)
	if delta < 0 {
		delta = -delta
	}
	return delta <= time.Minute
}

func parseCapturedLocal(value string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02 15:04:05.999", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func isPairStill(ext string) bool {
	switch strings.ToLower(ext) {
	case ".heic", ".heif", ".jpg", ".jpeg":
		return true
	default:
		return false
	}
}

func isPairVideo(ext string) bool {
	switch strings.ToLower(ext) {
	case ".mov", ".mp4":
		return true
	default:
		return false
	}
}

func isPairSidecar(ext string) bool {
	switch strings.ToLower(ext) {
	case ".aae", ".xmp":
		return true
	default:
		return false
	}
}
