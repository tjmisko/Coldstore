package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// ErrCatalogNotFound lets an offline diagnostic distinguish an absent archive
// catalog from an unreadable or corrupt one without matching error strings.
var ErrCatalogNotFound = errors.New("catalog does not exist")

// ErrUncheckpointedWAL is returned rather than opening an immutable snapshot
// that would ignore committed pages still present only in catalog.db-wal. The
// diagnostic never checkpoints or creates shared-memory sidecars; the owner
// must stop the writer cleanly (or use SQLite's own backup tooling) first.
var ErrUncheckpointedWAL = errors.New("catalog has an uncheckpointed WAL")

func ensureNoUncheckpointedWAL(dbPath string) error {
	info, err := os.Stat(dbPath + "-wal")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stating database WAL: %w", err)
	}
	// A WAL header is 32 bytes. Anything beyond it contains at least one frame
	// whose committed data might not yet be present in the main database.
	if info.Size() > 32 {
		return fmt.Errorf("%w: stop the daemon cleanly before running offline status", ErrUncheckpointedWAL)
	}
	return nil
}

// OpenReadOnly opens an existing catalog in SQLite read-only and query-only
// modes. It never creates a directory or database, runs migrations, changes
// journal mode, writes the schema marker, or performs reconciliation. The
// returned Catalog still has write methods because it is the normal query
// surface, but SQLite refuses every attempted mutation at the connection.
func OpenReadOnly(dbPath string) (*Catalog, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrCatalogNotFound, dbPath)
		}
		return nil, fmt.Errorf("stating database: %w", err)
	}
	if err := ensureNoUncheckpointedWAL(dbPath); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite3", readOnlyDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("opening database read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening database read-only: %w", err)
	}
	return &Catalog{db: db}, nil
}
