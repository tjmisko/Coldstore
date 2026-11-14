// Package ingest is the pipeline vocabulary of the archive core: what arrives
// (Arrival, Source), what became of it (Outcome, Result), the pure decision
// table that turns an arrival into journal records (Resolve), and the
// importdir Source. It performs no durable writes of its own.
package ingest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tjmisko/coldstore/internal/journal"
	"github.com/tjmisko/coldstore/internal/store"
)

type ItemID = journal.ItemID

// Arrival is one file offered to the archive.
type Arrival struct {
	Folder, Path string
	Mtime        time.Time
	Open         func() (io.ReadCloser, error)
	Want         *store.Hash
}

// Source produces arrivals until it is exhausted or ctx is done; it closes the
// channel when it finishes.
type Source interface {
	Arrivals(ctx context.Context) <-chan Arrival
}

type Outcome string

const (
	OutcomeCreated      Outcome = "created"       // new Item
	OutcomeUnchanged    Outcome = "unchanged"     // path already means these bytes
	OutcomeCopied       Outcome = "copied"        // same Item, second Placement
	OutcomeRenamed      Outcome = "renamed"       // M2 only in practice
	OutcomeVersioned    Outcome = "versioned"     // M2 only in practice (hot edit in place)
	OutcomePlacedBeside Outcome = "placed_beside" // new Item at <stem>.sha256-<12hex><ext>
)

type Result struct {
	Item    ItemID
	EntryID int64
	Hash    store.Hash
	Size    int64
	Path    string // the path actually placed
	Outcome Outcome
}

var (
	// ErrBadPath is wrapped by CleanPath for every rejected path.
	ErrBadPath = errors.New("ingest: invalid path")
	// ErrCollectionUnknown means the folder is not a declared collection.
	ErrCollectionUnknown = errors.New("ingest: no such collection")
	// ErrChangedWhileReading means a source file changed size or mtime while it was read.
	ErrChangedWhileReading = errors.New("ingest: file changed while reading")
)

const (
	maxSegmentBytes = 255
	maxPathBytes    = 4096
	besideHexLen    = 12
)

// CleanPath validates a slash-separated relative path: valid UTF-8, non-empty,
// no NUL, no empty/"."/".." segment, no leading or trailing "/", every segment
// <= 255 bytes, whole path <= 4096 bytes. Backslashes are ordinary characters.
func CleanPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: empty", ErrBadPath)
	}
	if !utf8.ValidString(p) {
		return "", fmt.Errorf("%w: not valid UTF-8", ErrBadPath)
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: contains NUL", ErrBadPath)
	}
	if len(p) > maxPathBytes {
		return "", fmt.Errorf("%w: %d bytes exceeds %d", ErrBadPath, len(p), maxPathBytes)
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("%w: segment %q in %q", ErrBadPath, segment, p)
		}
		if len(segment) > maxSegmentBytes {
			return "", fmt.Errorf("%w: segment of %d bytes exceeds %d", ErrBadPath, len(segment), maxSegmentBytes)
		}
	}
	return p, nil
}

// BesidePath is the deterministic sibling name for a second Item at one path:
// <dir>/<stem>.sha256-<first 12 hex of h><ext>, trimmed so the final segment
// stays within 255 bytes.
func BesidePath(path string, h store.Hash) string {
	dir, base := "", path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		dir, base = path[:i+1], path[i+1:]
	}
	ext := ""
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		ext = base[i:]
	}
	stem := strings.TrimSuffix(base, ext)
	marker := ".sha256-" + h.String()[:besideHexLen]

	for len(stem)+len(marker)+len(ext) > maxSegmentBytes && stem != "" {
		_, width := utf8.DecodeLastRuneInString(stem)
		stem = stem[:len(stem)-width]
	}
	if len(stem)+len(marker)+len(ext) > maxSegmentBytes {
		ext = ""
	}
	return dir + stem + marker + ext
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewItemID returns a 26-character Crockford base32 id: 48 bits of Unix
// milliseconds then 80 bits from crypto/rand.
func NewItemID(now time.Time) ItemID {
	var raw [16]byte
	ms := uint64(now.UnixMilli())
	for i := 0; i < 6; i++ {
		raw[i] = byte(ms >> (8 * (5 - i)))
	}
	if _, err := rand.Read(raw[6:]); err != nil {
		panic(fmt.Sprintf("ingest: crypto/rand failed: %v", err))
	}
	var out [26]byte
	for i := range out {
		// 130 bits: two leading zero bits, then the 128 id bits.
		var value byte
		for bit := 0; bit < 5; bit++ {
			position := i*5 + bit - 2
			value <<= 1
			if position < 0 {
				continue
			}
			value |= (raw[position/8] >> (7 - position%8)) & 1
		}
		out[i] = crockford[value]
	}
	return ItemID(out[:])
}
