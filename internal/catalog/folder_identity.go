package catalog

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
)

// FolderIdentity is a catalogued label and its current Syncthing identity.
// The label is permanent while the Syncthing ID and path may be refreshed
// under that same label. Policy and present_in_toml are deliberately absent:
// identity checks must include disabled and removed folders too.
type FolderIdentity struct {
	FolderLabel   string
	SyncthingID   string
	SyncthingPath string
}

// ListFolderIdentities returns every catalogued label/Syncthing association.
// The reconciler calls this before its first write so a TOML label rename
// cannot silently strand the files recorded under the original label.
func (c *Catalog) ListFolderIdentities() ([]FolderIdentity, error) {
	return queryFolderIdentities(c.db)
}

// ReadFolderIdentities opens an existing catalog strictly read-only and
// returns its label/Syncthing associations without running migrations. A
// missing catalog has no associations and is not created. This is the catalog
// access used by `coldstore --check`.
func ReadFolderIdentities(dbPath string) ([]FolderIdentity, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
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
	defer db.Close()

	identities, err := queryFolderIdentities(db)
	if err != nil {
		return nil, fmt.Errorf("reading folder identities: %w", err)
	}
	return identities, nil
}

func queryFolderIdentities(db *sql.DB) ([]FolderIdentity, error) {
	rows, err := db.Query(`SELECT folder_label, syncthing_id, syncthing_path FROM folder_configs
		ORDER BY folder_label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var identities []FolderIdentity
	for rows.Next() {
		var identity FolderIdentity
		if err := rows.Scan(&identity.FolderLabel, &identity.SyncthingID, &identity.SyncthingPath); err != nil {
			return nil, err
		}
		identities = append(identities, identity)
	}
	return identities, rows.Err()
}

// readOnlyDSN requests SQLite's immutable read-only mode as well as query_only.
// Immutable is load-bearing: mode=ro alone can create WAL/SHM sidecars when the
// database header says WAL. Callers first reject a non-empty WAL so immutable
// cannot hide committed data. url.URL keeps path characters such as '?' from
// becoming DSN parameters.
func readOnlyDSN(dbPath string) string {
	u := &url.URL{Scheme: "file", Path: dbPath}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_query_only", "1")
	q.Set("immutable", "1")
	u.RawQuery = q.Encode()
	return u.String()
}
