// Package index is the SQLite index of the archive core. It is derived state:
// every journal-derived row is a pure function of the journal, and the file may
// be deleted and rebuilt at any time. Operational columns (verification, bad
// flags, derived metadata, events) are never journaled and are lost on rebuild.
package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"

	"github.com/tjmisko/coldstore/internal/crashpoint"
	"github.com/tjmisko/coldstore/internal/journal"
)

// FormatVersion is PRAGMA user_version of index.db. There are no migrations.
const FormatVersion = 1

var (
	ErrFormatNewer = errors.New("index: written by a newer coldstore")
	ErrStale       = errors.New("index: older format or failed integrity check")
)

var (
	crashBeforeCommit = crashpoint.Register("index.apply.before_commit")
	crashCommitted    = crashpoint.Register("index.apply.committed")
)

const (
	driverWrite = "coldstore_index_rw"
	driverRead  = "coldstore_index_ro"

	// rebuildBatchRecords is the transaction size of Rebuild.
	rebuildBatchRecords = 1000
	cacheSizePages      = -8000 // 8 MiB
	maxReaderConns      = 2
)

func init() {
	sql.Register(driverWrite, &sqlite3.SQLiteDriver{ConnectHook: func(c *sqlite3.SQLiteConn) error {
		return configureConn(c, false)
	}})
	sql.Register(driverRead, &sqlite3.SQLiteDriver{ConnectHook: func(c *sqlite3.SQLiteConn) error {
		return configureConn(c, true)
	}})
}

func configureConn(c *sqlite3.SQLiteConn, readOnly bool) error {
	if err := c.RegisterFunc("ufold", strings.ToLower, true); err != nil {
		return err
	}
	if _, err := c.Exec("PRAGMA cache_size="+strconv.Itoa(cacheSizePages), nil); err != nil {
		return err
	}
	if !readOnly {
		return nil
	}
	_, err := c.Exec("PRAGMA query_only=1", nil)
	return err
}

// Index is the open index. One writer connection, at most two reader connections.
type Index struct {
	path  string
	write *sql.DB
	read  *sql.DB

	mu      sync.Mutex // guards applied
	applied uint64
}

const schema = `
CREATE TABLE meta (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	applied_seq INTEGER NOT NULL
);
INSERT INTO meta (id, applied_seq) VALUES (1, 0);
CREATE TABLE folders (
	name TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	declared_seq INTEGER NOT NULL
);
CREATE TABLE blobs (
	hash TEXT PRIMARY KEY,
	size INTEGER NOT NULL,
	added_seq INTEGER NOT NULL,
	verified_at INTEGER,
	bad INTEGER NOT NULL DEFAULT 0,
	bad_reason TEXT NOT NULL DEFAULT '',
	detected_type TEXT NOT NULL DEFAULT '',
	thumb_state TEXT NOT NULL DEFAULT '',
	thumb_error TEXT NOT NULL DEFAULT '',
	thumb_attempts INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE items (
	id TEXT PRIMARY KEY,
	current_hash TEXT NOT NULL,
	created_seq INTEGER NOT NULL,
	trashed_at INTEGER,
	description TEXT NOT NULL DEFAULT ''
);
CREATE INDEX items_current_hash ON items (current_hash);
CREATE TABLE versions (
	item_id TEXT NOT NULL,
	hash TEXT NOT NULL,
	superseded_seq INTEGER NOT NULL,
	PRIMARY KEY (item_id, superseded_seq)
);
CREATE TABLE placements (
	entry_id INTEGER PRIMARY KEY,
	folder TEXT NOT NULL,
	path TEXT NOT NULL,
	item_id TEXT NOT NULL,
	mtime INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	hot INTEGER NOT NULL DEFAULT 0,
	pinned INTEGER NOT NULL DEFAULT 0,
	UNIQUE (folder, path)
);
CREATE INDEX placements_folder_mtime ON placements (folder, mtime);
CREATE INDEX placements_item ON placements (item_id);
CREATE TABLE tags (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	fold TEXT NOT NULL UNIQUE
);
CREATE TABLE item_tags (
	item_id TEXT NOT NULL,
	tag_id INTEGER NOT NULL,
	PRIMARY KEY (item_id, tag_id)
);
CREATE INDEX item_tags_tag ON item_tags (tag_id);
CREATE TABLE events (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	type TEXT NOT NULL,
	folder TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	details TEXT NOT NULL DEFAULT ''
);
CREATE INDEX events_folder_type ON events (folder, type, id);
`

func dsn(path string) string {
	u := &url.URL{Scheme: "file", Path: path}
	q := url.Values{}
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "NORMAL")
	q.Set("_busy_timeout", "5000")
	u.RawQuery = q.Encode()
	return u.String()
}

// Open opens path, creating an empty index when the file is absent or empty.
// ErrFormatNewer means refuse; ErrStale means the caller should Rebuild.
func Open(path string) (*Index, error) {
	info, statErr := os.Stat(path)
	existing := statErr == nil && info.Size() > 0
	ix, err := openConnections(path)
	if err != nil {
		if existing {
			return nil, fmt.Errorf("opening index %s: %w: %v", path, ErrStale, err)
		}
		return nil, fmt.Errorf("opening index %s: %w", path, err)
	}
	if !existing {
		if err := ix.createSchema(); err != nil {
			_ = ix.Close()
			return nil, fmt.Errorf("opening index %s: %w", path, err)
		}
		return ix, nil
	}
	if err := ix.checkExisting(); err != nil {
		_ = ix.Close()
		return nil, fmt.Errorf("opening index %s: %w", path, err)
	}
	return ix, nil
}

func openConnections(path string) (*Index, error) {
	write, err := sql.Open(driverWrite, dsn(path))
	if err != nil {
		return nil, err
	}
	write.SetMaxOpenConns(1)
	if err := write.Ping(); err != nil {
		_ = write.Close()
		return nil, err
	}
	read, err := sql.Open(driverRead, dsn(path))
	if err != nil {
		_ = write.Close()
		return nil, err
	}
	read.SetMaxOpenConns(maxReaderConns)
	return &Index{path: path, write: write, read: read}, nil
}

func (ix *Index) createSchema() error {
	tx, err := ix.write.Begin()
	if err != nil {
		return fmt.Errorf("creating schema: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("creating schema: %w", err)
	}
	if _, err := tx.Exec("PRAGMA user_version=" + strconv.Itoa(FormatVersion)); err != nil {
		return fmt.Errorf("setting user_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("creating schema: %w", err)
	}
	return nil
}

func (ix *Index) checkExisting() error {
	var version int
	if err := ix.write.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("%w: reading user_version: %v", ErrStale, err)
	}
	if version > FormatVersion {
		return fmt.Errorf("%w: user_version %d, supported %d", ErrFormatNewer, version, FormatVersion)
	}
	if version < FormatVersion {
		return fmt.Errorf("%w: user_version %d, want %d", ErrStale, version, FormatVersion)
	}
	var check string
	if err := ix.write.QueryRow("PRAGMA quick_check(1)").Scan(&check); err != nil {
		return fmt.Errorf("%w: quick_check: %v", ErrStale, err)
	}
	if check != "ok" {
		return fmt.Errorf("%w: quick_check: %s", ErrStale, check)
	}
	var applied int64
	if err := ix.write.QueryRow("SELECT applied_seq FROM meta WHERE id = 1").Scan(&applied); err != nil {
		return fmt.Errorf("%w: reading applied_seq: %v", ErrStale, err)
	}
	ix.applied = uint64(applied)
	return nil
}

// Close checkpoints the WAL and closes both connection pools.
func (ix *Index) Close() error {
	_, checkpointErr := ix.write.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	readErr := ix.read.Close()
	writeErr := ix.write.Close()
	return errors.Join(checkpointErr, readErr, writeErr)
}

// AppliedSeq is the seq of the last journal record the index has applied.
func (ix *Index) AppliedSeq() uint64 {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.applied
}

// Apply applies one record in one transaction and advances applied_seq.
func (ix *Index) Apply(rec journal.Record) error {
	return ix.ApplyAll([]journal.Record{rec})
}

// ApplyAll applies recs in one transaction. A record with Seq <= AppliedSeq()
// is skipped; a record that leaves a gap is an error and nothing is applied.
func (ix *Index) ApplyAll(recs []journal.Record) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	last := ix.applied
	var pending []journal.Record
	for _, rec := range recs {
		if rec.Seq <= last && len(pending) == 0 {
			continue
		}
		if rec.Seq != last+1 {
			return fmt.Errorf("applying record seq %d: expected seq %d", rec.Seq, last+1)
		}
		pending = append(pending, rec)
		last = rec.Seq
	}
	if len(pending) == 0 {
		return nil
	}

	tx, err := ix.write.Begin()
	if err != nil {
		return fmt.Errorf("applying records: %w", err)
	}
	defer tx.Rollback()
	for _, rec := range pending {
		if err := rec.Validate(); err != nil {
			return fmt.Errorf("applying record seq %d: %w", rec.Seq, err)
		}
		if err := applyRecord(tx, rec); err != nil {
			return fmt.Errorf("applying record seq %d (%s): %w", rec.Seq, rec.Type, err)
		}
	}
	if _, err := tx.Exec("UPDATE meta SET applied_seq = ? WHERE id = 1", int64(last)); err != nil {
		return fmt.Errorf("advancing applied_seq: %w", err)
	}
	crashBeforeCommit.Hit()
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing records: %w", err)
	}
	crashCommitted.Hit()
	ix.applied = last
	return nil
}

// Rebuild deletes path (and its -wal/-shm), replays the whole journal into a
// fresh index in batches of 1000 and returns it open.
func Rebuild(ctx context.Context, j journal.Journal, path string) (*Index, error) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("rebuilding index: removing %s: %w", path+suffix, err)
		}
	}
	ix, err := Open(path)
	if err != nil {
		return nil, fmt.Errorf("rebuilding index: %w", err)
	}
	batch := make([]journal.Record, 0, rebuildBatchRecords)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := ix.ApplyAll(batch)
		batch = batch[:0]
		return err
	}
	err = j.Replay(ctx, 0, func(rec journal.Record) error {
		batch = append(batch, rec)
		if len(batch) < rebuildBatchRecords {
			return nil
		}
		return flush()
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		_ = ix.Close()
		return nil, fmt.Errorf("rebuilding index: %w", err)
	}
	return ix, nil
}

// Dump writes a canonical, sorted, line-oriented text of every journal-derived
// table. Operational columns and the events table are excluded. Two indexes
// are equal iff their dumps are equal.
func (ix *Index) Dump(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "applied_seq\t%d\n", ix.AppliedSeq()); err != nil {
		return err
	}
	sections := []struct {
		label string
		query string
		cols  int
	}{
		{"folder", "SELECT name, kind, declared_seq FROM folders ORDER BY name", 3},
		{"blob", "SELECT hash, size, added_seq FROM blobs ORDER BY hash", 3},
		{"item", "SELECT id, current_hash, created_seq, COALESCE(trashed_at, -1), description FROM items ORDER BY id", 5},
		{"version", "SELECT item_id, superseded_seq, hash FROM versions ORDER BY item_id, superseded_seq", 3},
		{"placement", "SELECT entry_id, folder, path, item_id, mtime, created_at, hot, pinned FROM placements ORDER BY folder, path", 8},
		{"tag", "SELECT name, fold FROM tags ORDER BY fold", 2},
		{"item_tag", "SELECT it.item_id, t.fold FROM item_tags it JOIN tags t ON t.id = it.tag_id ORDER BY it.item_id, t.fold", 2},
	}
	for _, section := range sections {
		if err := ix.dumpSection(w, section.label, section.query, section.cols); err != nil {
			return fmt.Errorf("dumping %s: %w", section.label, err)
		}
	}
	return nil
}

func (ix *Index) dumpSection(w io.Writer, label, query string, cols int) error {
	rows, err := ix.read.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	values := make([]any, cols)
	targets := make([]any, cols)
	for i := range values {
		targets[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(targets...); err != nil {
			return err
		}
		fields := make([]string, 0, cols+1)
		fields = append(fields, label)
		for _, v := range values {
			switch typed := v.(type) {
			case string:
				fields = append(fields, strconv.Quote(typed))
			case []byte:
				fields = append(fields, strconv.Quote(string(typed)))
			default:
				fields = append(fields, fmt.Sprint(typed))
			}
		}
		if _, err := io.WriteString(w, strings.Join(fields, "\t")+"\n"); err != nil {
			return err
		}
	}
	return rows.Err()
}

func toNanos(t time.Time) int64 { return t.UnixNano() }

func fromNanos(n int64) time.Time { return time.Unix(0, n).UTC() }
