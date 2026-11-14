// Package journal is the append-only, checksummed log of every decision a
// person or a device made. The index is derived from it and can be rebuilt by
// replaying it; the journal is the one piece of state that cannot be rebuilt.
package journal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// FormatVersion is the on-disk format this binary writes and understands.
const FormatVersion = 1

type ItemID string

const (
	TypeFolderDeclared   = "folder_declared"   // Folder, Kind
	TypeBlobAdded        = "blob_added"        // Hash, Size
	TypeItemCreated      = "item_created"      // Item, Hash
	TypeItemVersioned    = "item_versioned"    // Item, Hash (new current; the old one is kept as a version)
	TypePlacementSet     = "placement_set"     // Folder, Path, Item, Mtime, Asked (optional)
	TypePlacementCleared = "placement_cleared" // Folder, Path
	TypePlacementPinned  = "placement_pinned"  // Folder, Path, Pinned
	TypeItemTagged       = "item_tagged"       // Item, Add, Remove
	TypeItemDescribed    = "item_described"    // Item, Text
	TypeItemTrashed      = "item_trashed"      // Item
	TypeItemUntrashed    = "item_untrashed"    // Item
)

const (
	FolderKindSync       = "sync"
	FolderKindCollection = "collection"
)

// Record is one journal entry.
type Record struct {
	Seq   uint64    `json:"seq"` // assigned by Append
	At    time.Time `json:"at"`  // UTC, RFC 3339; assigned by Append when zero
	By    string    `json:"by"`  // actor; required
	Type  string    `json:"t"`
	Batch uint64    `json:"batch"` // seq of the last record of this Append call; assigned by Append

	Folder string     `json:"folder,omitempty"`
	Kind   string     `json:"kind,omitempty"`
	Path   string     `json:"path,omitempty"`
	Asked  string     `json:"asked,omitempty"`
	Hash   string     `json:"hash,omitempty"` // 64 hex
	Size   int64      `json:"size,omitempty"`
	Item   ItemID     `json:"item,omitempty"`
	Mtime  *time.Time `json:"mtime,omitempty"`
	Add    []string   `json:"add,omitempty"`
	Remove []string   `json:"remove,omitempty"`
	Pinned *bool      `json:"pinned,omitempty"`
	Text   *string    `json:"text,omitempty"`
}

// ErrUnknownType is wrapped by Validate for a record type this binary does not
// know: it is how an older binary refuses records written by a newer one.
var ErrUnknownType = errors.New("journal: unknown record type")

// Validate checks the fields Type requires. An unknown Type is an error.
func (r Record) Validate() error {
	if r.By == "" {
		return errors.New("journal: record has no actor (by)")
	}
	switch r.Type {
	case TypeFolderDeclared:
		if r.Folder == "" {
			return missing(r, "folder")
		}
		if r.Kind != FolderKindSync && r.Kind != FolderKindCollection {
			return fmt.Errorf("journal: %s has kind %q, want %q or %q", r.Type, r.Kind, FolderKindSync, FolderKindCollection)
		}
	case TypeBlobAdded:
		if !validHash(r.Hash) {
			return fmt.Errorf("journal: %s hash %q is not 64 lowercase hex characters", r.Type, r.Hash)
		}
		if r.Size < 0 {
			return fmt.Errorf("journal: %s has negative size %d", r.Type, r.Size)
		}
	case TypeItemCreated, TypeItemVersioned:
		if r.Item == "" {
			return missing(r, "item")
		}
		if !validHash(r.Hash) {
			return fmt.Errorf("journal: %s hash %q is not 64 lowercase hex characters", r.Type, r.Hash)
		}
	case TypePlacementSet:
		if r.Folder == "" {
			return missing(r, "folder")
		}
		if r.Path == "" {
			return missing(r, "path")
		}
		if r.Item == "" {
			return missing(r, "item")
		}
	case TypePlacementCleared:
		if r.Folder == "" {
			return missing(r, "folder")
		}
		if r.Path == "" {
			return missing(r, "path")
		}
	case TypePlacementPinned:
		if r.Folder == "" {
			return missing(r, "folder")
		}
		if r.Path == "" {
			return missing(r, "path")
		}
		if r.Pinned == nil {
			return missing(r, "pinned")
		}
	case TypeItemTagged:
		if r.Item == "" {
			return missing(r, "item")
		}
		if len(r.Add) == 0 && len(r.Remove) == 0 {
			return missing(r, "add or remove")
		}
	case TypeItemDescribed:
		if r.Item == "" {
			return missing(r, "item")
		}
		if r.Text == nil {
			return missing(r, "text")
		}
	case TypeItemTrashed, TypeItemUntrashed:
		if r.Item == "" {
			return missing(r, "item")
		}
	default:
		return fmt.Errorf("%w %q", ErrUnknownType, r.Type)
	}
	return nil
}

func missing(r Record, field string) error {
	return fmt.Errorf("journal: %s requires %s", r.Type, field)
}

func validHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

type Journal interface {
	// Append assigns the next sequence number, writes and fsyncs. It returns
	// only after the record is durable. All records of one call are one atomic
	// batch.
	Append(ctx context.Context, recs ...Record) (lastSeq uint64, err error)

	// Replay calls fn for every intact record after fromSeq, in order. Damage
	// anywhere is a *DamagedError.
	Replay(ctx context.Context, fromSeq uint64, fn func(Record) error) error
}

type Options struct {
	SegmentBytes int64            // default 16 << 20
	Now          func() time.Time // default time.Now
	Logger       *slog.Logger
}

// Torn describes a damaged tail that Open dropped.
type Torn struct {
	Segment      string
	Offset       int64 // where the intact prefix ends
	DroppedBytes int64
	DroppedRecs  int // intact records dropped because their batch was incomplete
}

var (
	ErrLocked      = errors.New("journal: locked by another process")
	ErrFormatNewer = errors.New("journal: written by a newer coldstore")
	errClosed      = errors.New("journal: closed")
)

// DamagedError reports damage anywhere but the tail of the last segment.
type DamagedError struct {
	Segment string
	Line    int
	Reason  string // "checksum" | "json" | "seq_gap" | "unknown_type" | "line_too_long"
}

func (e *DamagedError) Error() string {
	return fmt.Sprintf("journal: segment %s line %d is damaged: %s", e.Segment, e.Line, e.Reason)
}
