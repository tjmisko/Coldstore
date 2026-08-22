//go:build !linux

package daemon

import "fmt"

// Rename safety depends on Linux's descriptor-relative RENAME_NOREPLACE. Other
// platforms refuse the operation instead of falling back to check-then-rename.
func performBoundRename(renameFSAction, func(renameFSAction) error) error {
	return fmt.Errorf("%w: safe path rename requires Linux renameat2", ErrRenameConflict)
}

func removeBoundFile(renameDerivedRemoval) error {
	return fmt.Errorf("%w: safe derived-file removal requires Linux", ErrRenameConflict)
}
