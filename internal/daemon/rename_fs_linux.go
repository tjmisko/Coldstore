//go:build linux

package daemon

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// performBoundRename commits one rename relative to an already-open trusted
// tree root. Every parent is opened with O_NOFOLLOW and the final operation is
// renameat2(RENAME_NOREPLACE), so neither a substituted parent symlink nor a
// destination created after preflight can redirect or overwrite the move.
func performBoundRename(action renameFSAction, beforeCommit func(renameFSAction) error) error {
	rootFD, err := unix.Open(action.root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening rename tree root %s: %w", action.root, err)
	}
	defer unix.Close(rootFD)

	sourceRel, err := relativeWithinRoot(action.root, action.source)
	if err != nil {
		return err
	}
	destinationRel, err := relativeWithinRoot(action.root, action.destination)
	if err != nil {
		return err
	}

	sourceParent, sourceParentExists, err := openParentAt(rootFD, sourceRel, false)
	if err != nil {
		return err
	}
	if sourceParentExists {
		defer unix.Close(sourceParent)
	}
	sourceExists := false
	var sourceStat unix.Stat_t
	if sourceParentExists {
		sourceStat, sourceExists, err = lstatAt(sourceParent, path.Base(sourceRel))
		if err != nil {
			return err
		}
	}

	destinationParent, destinationParentExists, err := openParentAt(rootFD, destinationRel, false)
	if err != nil {
		return err
	}
	if destinationParentExists {
		defer unix.Close(destinationParent)
	}
	destinationExists := false
	var destinationStat unix.Stat_t
	if destinationParentExists {
		destinationStat, destinationExists, err = lstatAt(destinationParent, path.Base(destinationRel))
		if err != nil {
			return err
		}
	}

	if sourceExists && statIsDir(sourceStat) != action.directory {
		return fmt.Errorf("%w: source has unexpected type: %s", ErrRenameConflict, action.source)
	}
	if destinationExists && statIsDir(destinationStat) != action.directory {
		return fmt.Errorf("%w: destination has unexpected type: %s", ErrRenameConflict, action.destination)
	}
	if destinationExists {
		if sourceExists || !action.resume {
			return fmt.Errorf("%w: %s", ErrRenameConflict, action.destination)
		}
		return nil
	}
	if !sourceExists {
		return nil
	}

	if !destinationParentExists {
		destinationParent, destinationParentExists, err = openParentAt(rootFD, destinationRel, true)
		if err != nil {
			return err
		}
		if !destinationParentExists {
			return fmt.Errorf("creating rename destination parent: %s", action.destination)
		}
		defer unix.Close(destinationParent)
	}
	// Recheck after creating/opening the parent. This is still only an early
	// conflict message; RENAME_NOREPLACE below is the indivisible guard.
	if destinationStat, destinationExists, err = lstatAt(destinationParent, path.Base(destinationRel)); err != nil {
		return err
	} else if destinationExists {
		if statIsDir(destinationStat) != action.directory {
			return fmt.Errorf("%w: destination has unexpected type: %s", ErrRenameConflict, action.destination)
		}
		return fmt.Errorf("%w: %s", ErrRenameConflict, action.destination)
	}

	if beforeCommit != nil {
		if err := beforeCommit(action); err != nil {
			return err
		}
	}
	if err := unix.Renameat2(sourceParent, path.Base(sourceRel), destinationParent,
		path.Base(destinationRel), unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("%w: %s", ErrRenameConflict, action.destination)
		}
		return fmt.Errorf("renaming %s to %s: %w", action.source, action.destination, err)
	}
	return nil
}

// removeBoundFile unlinks a derived file through no-follow parent descriptors.
// A final symlink is itself removed; it is never followed.
func removeBoundFile(removal renameDerivedRemoval) error {
	rootFD, err := unix.Open(removal.root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening derived-data root %s: %w", removal.root, err)
	}
	defer unix.Close(rootFD)

	relative, err := relativeWithinRoot(removal.root, removal.filename)
	if err != nil {
		return err
	}
	parentFD, exists, err := openParentAt(rootFD, relative, false)
	if err != nil || !exists {
		return err
	}
	defer unix.Close(parentFD)
	info, exists, err := lstatAt(parentFD, path.Base(relative))
	if err != nil || !exists {
		return err
	}
	if statIsDir(info) {
		return fmt.Errorf("%w: obsolete thumbnail is a directory: %s", ErrRenameConflict, removal.filename)
	}
	if err := unix.Unlinkat(parentFD, path.Base(relative), 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("removing obsolete thumbnail %s: %w", removal.filename, err)
	}
	return nil
}

func relativeWithinRoot(root, filename string) (string, error) {
	relative, err := filepath.Rel(root, filename)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes %q", ErrInvalidRenamePath, filename, root)
	}
	return filepath.ToSlash(relative), nil
}

// openParentAt resolves every directory below rootFD without following a
// symlink. With create=true, missing destination parents are created one level
// at a time, then opened with the same no-follow rule before traversal resumes.
func openParentAt(rootFD int, relative string, create bool) (fd int, exists bool, retErr error) {
	fd, err := unix.Dup(rootFD)
	if err != nil {
		return -1, false, err
	}
	parent := path.Dir(relative)
	if parent == "." {
		return fd, true, nil
	}
	for _, component := range strings.Split(parent, "/") {
		next, openErr := unix.Openat(fd, component,
			unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) && create {
			mkdirErr := unix.Mkdirat(fd, component, 0o755)
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				unix.Close(fd)
				return -1, false, fmt.Errorf("creating rename destination parent %s: %w", component, mkdirErr)
			}
			next, openErr = unix.Openat(fd, component,
				unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if errors.Is(openErr, unix.ENOENT) {
			unix.Close(fd)
			return -1, false, nil
		}
		if errors.Is(openErr, unix.ELOOP) {
			unix.Close(fd)
			return -1, false, fmt.Errorf("%w: %q traverses a symlink", ErrInvalidRenamePath, relative)
		}
		if err := unix.Close(fd); err != nil {
			if openErr == nil {
				unix.Close(next)
			}
			return -1, false, err
		}
		if openErr != nil {
			return -1, false, fmt.Errorf("opening rename parent %q: %w", component, openErr)
		}
		fd = next
	}
	return fd, true, nil
}

func lstatAt(parentFD int, name string) (unix.Stat_t, bool, error) {
	var info unix.Stat_t
	err := unix.Fstatat(parentFD, name, &info, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return info, false, nil
	}
	if err != nil {
		return info, false, err
	}
	return info, true, nil
}

func statIsDir(info unix.Stat_t) bool { return info.Mode&unix.S_IFMT == unix.S_IFDIR }
