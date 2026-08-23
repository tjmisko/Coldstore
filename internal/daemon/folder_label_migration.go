package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

// FolderLabelMigrationResult combines the catalog rewrite with the data roots
// moved on disk. The operation is intentionally offline and is invoked only by
// the explicit command-line maintenance mode.
type FolderLabelMigrationResult struct {
	Catalog    catalog.FolderLabelRenameResult
	MovedRoots []string
}

// MigrateFolderLabel changes a folder's durable Coldstore identity while
// retaining the Syncthing identity declared by the replacement configuration.
// The replacement config must contain newLabel and must no longer contain
// oldLabel. Archive and thumbnail directories move before the catalog
// transaction; any database failure rolls those moves back.
func MigrateFolderLabel(cfg *config.Config, cat *catalog.Catalog, oldLabel, newLabel string,
	budgetBytes int64) (FolderLabelMigrationResult, error) {
	var result FolderLabelMigrationResult
	if err := validateMigrationLabel(oldLabel); err != nil {
		return result, fmt.Errorf("invalid source label: %w", err)
	}
	if err := validateMigrationLabel(newLabel); err != nil {
		return result, fmt.Errorf("invalid destination label: %w", err)
	}

	var target *config.FolderConfig
	for i := range cfg.Folders {
		folder := &cfg.Folders[i]
		if folder.Label == oldLabel {
			return result, fmt.Errorf("replacement config still declares source label %q", oldLabel)
		}
		if folder.Label == newLabel {
			target = folder
		}
	}
	if target == nil {
		return result, fmt.Errorf("replacement config does not declare destination label %q", newLabel)
	}
	if budgetBytes <= 0 {
		return result, fmt.Errorf("migration budget must be positive, got %d", budgetBytes)
	}

	if err := cat.ValidateFolderLabelRename(oldLabel, newLabel, target.SyncthingID); err != nil {
		return result, err
	}
	identities, err := cat.ListFolderIdentities()
	if err != nil {
		return result, fmt.Errorf("checking destination path ownership: %w", err)
	}
	targetPath := filepath.Clean(target.Path)
	for _, identity := range identities {
		if identity.FolderLabel == oldLabel {
			continue
		}
		if filepath.Clean(identity.SyncthingPath) == targetPath {
			return result, fmt.Errorf("destination path %q is already catalogued under folder %q",
				targetPath, identity.FolderLabel)
		}
	}

	moves, err := moveFolderLabelRoots(cfg.Storage.ColdStoragePath, oldLabel, newLabel)
	if err != nil {
		return result, err
	}
	rollback := func(cause error) error {
		for i := len(moves) - 1; i >= 0; i-- {
			move := moves[i]
			if err := os.Rename(move.destination, move.source); err != nil {
				return fmt.Errorf("%v; additionally failed to roll back %q to %q: %w",
					cause, move.destination, move.source, err)
			}
		}
		return cause
	}

	result.Catalog, err = cat.RenameFolderLabel(oldLabel, newLabel, target.SyncthingID,
		target.Path, budgetBytes)
	if err != nil {
		return result, rollback(err)
	}
	for _, move := range moves {
		result.MovedRoots = append(result.MovedRoots, move.kind)
	}
	return result, nil
}

func validateMigrationLabel(label string) error {
	if strings.TrimSpace(label) == "" {
		return fmt.Errorf("label is empty")
	}
	if label == "." || label == ".." || filepath.Base(label) != label ||
		strings.ContainsAny(label, `/\`) {
		return fmt.Errorf("label %q is not a single safe path component", label)
	}
	return nil
}

type folderRootMove struct {
	kind        string
	source      string
	destination string
}

func moveFolderLabelRoots(coldRoot, oldLabel, newLabel string) ([]folderRootMove, error) {
	var moves []folderRootMove
	for _, kind := range []string{"archive", "trash", "thumbnails"} {
		parent := filepath.Join(coldRoot, kind)
		source := filepath.Join(parent, oldLabel)
		destination := filepath.Join(parent, newLabel)

		sourceInfo, sourceErr := os.Lstat(source)
		if sourceErr != nil && !os.IsNotExist(sourceErr) {
			return nil, rollbackFolderRootMoves(moves,
				fmt.Errorf("stating source %s root %q: %w", kind, source, sourceErr))
		}
		if os.IsNotExist(sourceErr) {
			if _, destinationErr := os.Lstat(destination); destinationErr == nil {
				return nil, rollbackFolderRootMoves(moves,
					fmt.Errorf("destination %s root %q exists while source %q does not; "+
						"refusing an ambiguous partial migration", kind, destination, source))
			} else if !os.IsNotExist(destinationErr) {
				return nil, rollbackFolderRootMoves(moves,
					fmt.Errorf("stating destination %s root %q: %w", kind, destination, destinationErr))
			}
			continue
		}
		if !sourceInfo.IsDir() || sourceInfo.Mode()&os.ModeSymlink != 0 {
			return nil, rollbackFolderRootMoves(moves,
				fmt.Errorf("source %s root %q is not a real directory", kind, source))
		}
		if _, destinationErr := os.Lstat(destination); destinationErr == nil {
			return nil, rollbackFolderRootMoves(moves,
				fmt.Errorf("destination %s root %q already exists", kind, destination))
		} else if !os.IsNotExist(destinationErr) {
			return nil, rollbackFolderRootMoves(moves,
				fmt.Errorf("stating destination %s root %q: %w", kind, destination, destinationErr))
		}
		if err := os.Rename(source, destination); err != nil {
			return nil, rollbackFolderRootMoves(moves,
				fmt.Errorf("moving %s root from %q to %q: %w", kind, source, destination, err))
		}
		moves = append(moves, folderRootMove{kind: kind, source: source, destination: destination})
	}
	return moves, nil
}

func rollbackFolderRootMoves(moves []folderRootMove, cause error) error {
	for i := len(moves) - 1; i >= 0; i-- {
		move := moves[i]
		if err := os.Rename(move.destination, move.source); err != nil {
			return fmt.Errorf("%v; additionally failed to roll back %q to %q: %w",
				cause, move.destination, move.source, err)
		}
	}
	return cause
}
