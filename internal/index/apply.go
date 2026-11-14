package index

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/journal"
)

// applyRecord applies one record inside tx. A record that references an
// unknown item, folder, blob or placement is an error, never a silent skip.
func applyRecord(tx *sql.Tx, rec journal.Record) error {
	switch rec.Type {
	case journal.TypeFolderDeclared:
		return applyFolderDeclared(tx, rec)
	case journal.TypeBlobAdded:
		return applyBlobAdded(tx, rec)
	case journal.TypeItemCreated:
		return applyItemCreated(tx, rec)
	case journal.TypeItemVersioned:
		return applyItemVersioned(tx, rec)
	case journal.TypePlacementSet:
		return applyPlacementSet(tx, rec)
	case journal.TypePlacementCleared:
		return applyPlacementCleared(tx, rec)
	case journal.TypePlacementPinned:
		return applyPlacementPinned(tx, rec)
	case journal.TypeItemTagged:
		return applyItemTagged(tx, rec)
	case journal.TypeItemDescribed:
		return applyItemDescribed(tx, rec)
	case journal.TypeItemTrashed:
		return applyItemTrashed(tx, rec)
	case journal.TypeItemUntrashed:
		return applyItemUntrashed(tx, rec)
	}
	return fmt.Errorf("unknown record type %q", rec.Type)
}

func exists(tx *sql.Tx, query string, args ...any) (bool, error) {
	var one int
	err := tx.QueryRow(query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func requireItem(tx *sql.Tx, item journal.ItemID) error {
	ok, err := exists(tx, "SELECT 1 FROM items WHERE id = ?", string(item))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("unknown item %q", item)
	}
	return nil
}

func requireBlob(tx *sql.Tx, hash string) error {
	ok, err := exists(tx, "SELECT 1 FROM blobs WHERE hash = ?", hash)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("unknown blob %s", hash)
	}
	return nil
}

func applyFolderDeclared(tx *sql.Tx, rec journal.Record) error {
	_, err := tx.Exec(`INSERT INTO folders (name, kind, declared_seq) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET kind = excluded.kind`,
		rec.Folder, rec.Kind, int64(rec.Seq))
	return err
}

func applyBlobAdded(tx *sql.Tx, rec journal.Record) error {
	_, err := tx.Exec("INSERT OR IGNORE INTO blobs (hash, size, added_seq) VALUES (?, ?, ?)",
		rec.Hash, rec.Size, int64(rec.Seq))
	return err
}

func applyItemCreated(tx *sql.Tx, rec journal.Record) error {
	if err := requireBlob(tx, rec.Hash); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT INTO items (id, current_hash, created_seq) VALUES (?, ?, ?)",
		string(rec.Item), rec.Hash, int64(rec.Seq))
	return err
}

func applyItemVersioned(tx *sql.Tx, rec journal.Record) error {
	if err := requireBlob(tx, rec.Hash); err != nil {
		return err
	}
	var current string
	err := tx.QueryRow("SELECT current_hash FROM items WHERE id = ?", string(rec.Item)).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("unknown item %q", rec.Item)
	}
	if err != nil {
		return err
	}
	if current == rec.Hash {
		return nil
	}
	if _, err := tx.Exec("INSERT INTO versions (item_id, hash, superseded_seq) VALUES (?, ?, ?)",
		string(rec.Item), current, int64(rec.Seq)); err != nil {
		return err
	}
	_, err = tx.Exec("UPDATE items SET current_hash = ? WHERE id = ?", rec.Hash, string(rec.Item))
	return err
}

func applyPlacementSet(tx *sql.Tx, rec journal.Record) error {
	ok, err := exists(tx, "SELECT 1 FROM folders WHERE name = ?", rec.Folder)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("unknown folder %q", rec.Folder)
	}
	if err := requireItem(tx, rec.Item); err != nil {
		return err
	}
	mtime := rec.At
	if rec.Mtime != nil {
		mtime = *rec.Mtime
	}
	_, err = tx.Exec(`INSERT INTO placements (entry_id, folder, path, item_id, mtime, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(folder, path) DO UPDATE SET item_id = excluded.item_id, mtime = excluded.mtime`,
		int64(rec.Seq), rec.Folder, rec.Path, string(rec.Item), toNanos(mtime), toNanos(rec.At))
	return err
}

func applyPlacementCleared(tx *sql.Tx, rec journal.Record) error {
	result, err := tx.Exec("DELETE FROM placements WHERE folder = ? AND path = ?", rec.Folder, rec.Path)
	if err != nil {
		return err
	}
	return requireAffected(result, "placement", rec.Folder, rec.Path)
}

func applyPlacementPinned(tx *sql.Tx, rec journal.Record) error {
	pinned := 0
	if *rec.Pinned {
		pinned = 1
	}
	result, err := tx.Exec("UPDATE placements SET pinned = ? WHERE folder = ? AND path = ?",
		pinned, rec.Folder, rec.Path)
	if err != nil {
		return err
	}
	return requireAffected(result, "placement", rec.Folder, rec.Path)
}

func requireAffected(result sql.Result, what, folder, path string) error {
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("unknown %s %s/%s", what, folder, path)
	}
	return nil
}

func applyItemTagged(tx *sql.Tx, rec journal.Record) error {
	if err := requireItem(tx, rec.Item); err != nil {
		return err
	}
	for _, raw := range rec.Add {
		if err := addTag(tx, rec.Item, raw); err != nil {
			return err
		}
	}
	for _, raw := range rec.Remove {
		if err := removeTag(tx, rec.Item, raw); err != nil {
			return err
		}
	}
	return nil
}

func addTag(tx *sql.Tx, item journal.ItemID, raw string) error {
	display, fold, err := catalog.NormalizeTagName(raw)
	if err != nil {
		return fmt.Errorf("adding tag %q: %w", raw, err)
	}
	if _, err := tx.Exec("INSERT OR IGNORE INTO tags (name, fold) VALUES (?, ?)", display, fold); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT OR IGNORE INTO item_tags (item_id, tag_id)
		SELECT ?, id FROM tags WHERE fold = ?`, string(item), fold)
	return err
}

func removeTag(tx *sql.Tx, item journal.ItemID, raw string) error {
	_, fold, err := catalog.NormalizeTagName(raw)
	if err != nil {
		return fmt.Errorf("removing tag %q: %w", raw, err)
	}
	if _, err := tx.Exec(`DELETE FROM item_tags WHERE item_id = ?
		AND tag_id IN (SELECT id FROM tags WHERE fold = ?)`, string(item), fold); err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM tags WHERE fold = ?
		AND NOT EXISTS (SELECT 1 FROM item_tags WHERE tag_id = tags.id)`, fold)
	return err
}

func applyItemDescribed(tx *sql.Tx, rec journal.Record) error {
	result, err := tx.Exec("UPDATE items SET description = ? WHERE id = ?", *rec.Text, string(rec.Item))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("unknown item %q", rec.Item)
	}
	return nil
}

func applyItemTrashed(tx *sql.Tx, rec journal.Record) error {
	result, err := tx.Exec("UPDATE items SET trashed_at = ? WHERE id = ?", toNanos(rec.At), string(rec.Item))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("unknown item %q", rec.Item)
	}
	return nil
}

func applyItemUntrashed(tx *sql.Tx, rec journal.Record) error {
	result, err := tx.Exec("UPDATE items SET trashed_at = NULL WHERE id = ?", string(rec.Item))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("unknown item %q", rec.Item)
	}
	return nil
}
