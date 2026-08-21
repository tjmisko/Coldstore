package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrFolderUnavailable means a folder's on-disk root is not in a state any
// mover may write to or read from.
var ErrFolderUnavailable = errors.New("folder is not currently available")

// stfolderMarker is the file Syncthing plants in every folder it manages.
const stfolderMarker = ".stfolder"

// FolderReady reports whether a folder may be moved into or out of.
//
// A syncthing_path that exists as a directory is NOT proof the folder is
// available. The dangerous case is an unmounted mountpoint: the directory
// exists and is empty, os.Stat succeeds, a restore's MkdirAll materialises the
// whole tree on the ROOT filesystem, the move deletes each cold-storage
// source, and when the drive is finally mounted every one of those files is
// shadowed and invisible while the archive copies are gone.
//
// Syncthing creates .stfolder inside every folder precisely so an unmounted or
// wiped volume is distinguishable from an empty one, which is what this uses
// it for. It is a directory in current Syncthing and was a file in older
// versions; either is accepted. Two stats, on every entry point that touches
// the folder's filesystem (design section 3.6).
func FolderReady(syncthingPath string) error {
	if syncthingPath == "" {
		return fmt.Errorf("%w: no path is configured", ErrFolderUnavailable)
	}
	info, err := os.Stat(syncthingPath)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrFolderUnavailable, syncthingPath)
	}
	if _, err := os.Stat(filepath.Join(syncthingPath, stfolderMarker)); err != nil {
		return fmt.Errorf("%w: %s has no %s marker (unmounted, or not a syncthing folder)",
			ErrFolderUnavailable, syncthingPath, stfolderMarker)
	}
	return nil
}
