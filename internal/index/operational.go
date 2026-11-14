package index

import (
	"fmt"
	"time"

	"github.com/tjmisko/coldstore/internal/store"
)

// Operational state is never journaled: it is lost and regenerated on rebuild.

// MaxThumbAttempts bounds how often BlobsToDerive offers a blob.
const MaxThumbAttempts = 3

type Event struct {
	ID                 int64
	At                 time.Time
	Type, Folder, Path string
	Details            string
}

func (ix *Index) BlobVerifiedSet(h store.Hash, at time.Time) error {
	if _, err := ix.write.Exec("UPDATE blobs SET verified_at = ? WHERE hash = ?", toNanos(at), h.String()); err != nil {
		return fmt.Errorf("stamping blob %s verified: %w", h, err)
	}
	return nil
}

func (ix *Index) BlobBadSet(h store.Hash, bad bool, reason string) error {
	flag := 0
	if bad {
		flag = 1
	} else {
		reason = ""
	}
	if _, err := ix.write.Exec("UPDATE blobs SET bad = ?, bad_reason = ? WHERE hash = ?", flag, reason, h.String()); err != nil {
		return fmt.Errorf("marking blob %s bad=%v: %w", h, bad, err)
	}
	return nil
}

// BlobDerivedSet records one derive attempt. An empty thumbState records the
// attempt without a final state, so BlobsToDerive offers the blob again until
// MaxThumbAttempts.
func (ix *Index) BlobDerivedSet(h store.Hash, detectedType, thumbState, thumbError string) error {
	_, err := ix.write.Exec(`UPDATE blobs SET detected_type = ?, thumb_state = ?, thumb_error = ?,
		thumb_attempts = thumb_attempts + 1 WHERE hash = ?`, detectedType, thumbState, thumbError, h.String())
	if err != nil {
		return fmt.Errorf("recording derived state of blob %s: %w", h, err)
	}
	return nil
}

func (ix *Index) hashesQuery(query string, args ...any) ([]store.Hash, error) {
	rows, err := ix.read.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Hash
	for rows.Next() {
		var hex string
		if err := rows.Scan(&hex); err != nil {
			return nil, err
		}
		h, err := store.ParseHash(hex)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// BlobsToDerive lists blobs with no derived state yet and fewer than
// MaxThumbAttempts attempts, oldest first.
func (ix *Index) BlobsToDerive(limit int) ([]store.Hash, error) {
	out, err := ix.hashesQuery(`SELECT hash FROM blobs
		WHERE thumb_state = '' AND thumb_attempts < ? AND bad = 0
		ORDER BY added_seq, hash LIMIT ?`, MaxThumbAttempts, clampLimit(limit, MaxIDs))
	if err != nil {
		return nil, fmt.Errorf("listing blobs to derive: %w", err)
	}
	return out, nil
}

// BlobsByVerifiedAge lists never-verified blobs first, then the least recently verified.
func (ix *Index) BlobsByVerifiedAge(limit int) ([]store.Hash, error) {
	out, err := ix.hashesQuery(`SELECT hash FROM blobs
		ORDER BY verified_at IS NOT NULL, verified_at, hash LIMIT ?`, clampLimit(limit, MaxIDs))
	if err != nil {
		return nil, fmt.Errorf("listing blobs by verified age: %w", err)
	}
	return out, nil
}

func (ix *Index) EventRecord(at time.Time, eventType, folder, path, details string) error {
	_, err := ix.write.Exec("INSERT INTO events (at, type, folder, path, details) VALUES (?, ?, ?, ?, ?)",
		toNanos(at), eventType, folder, path, details)
	if err != nil {
		return fmt.Errorf("recording %s event: %w", eventType, err)
	}
	return nil
}

// Events lists events newest first. An empty folder or eventType matches any.
func (ix *Index) Events(folder, eventType string, limit int) ([]Event, error) {
	rows, err := ix.read.Query(`SELECT id, at, type, folder, path, details FROM events
		WHERE (? = '' OR folder = ?) AND (? = '' OR type = ?) ORDER BY id DESC LIMIT ?`,
		folder, folder, eventType, eventType, clampLimit(limit, MaxPageLimit))
	if err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Type, &e.Folder, &e.Path, &e.Details); err != nil {
			return nil, fmt.Errorf("listing events: %w", err)
		}
		e.At = fromNanos(at)
		out = append(out, e)
	}
	return out, rows.Err()
}
