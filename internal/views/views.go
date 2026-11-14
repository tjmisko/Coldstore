// Package views maintains views/, a browsable hardlink mirror of the live,
// untrashed placements of the index. It is derived state: Regenerate rebuilds
// it from the index alone and never touches blobs/.
package views

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tjmisko/coldstore/internal/crashpoint"
	"github.com/tjmisko/coldstore/internal/fsx"
	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/journal"
	"github.com/tjmisko/coldstore/internal/store"
)

var (
	crashLinkStaged  = crashpoint.Register("views.link.staged")
	crashLinkRenamed = crashpoint.Register("views.link.renamed")
)

// TempSuffix names the staging link; Regenerate removes any left behind.
const TempSuffix = ".coldstore-view-tmp"

// ErrBadPath is returned for a folder or path that could escape views/.
var ErrBadPath = errors.New("views: path is not a safe relative path")

type Report struct {
	Linked, Removed int
}

type Tree struct {
	fsys     fsx.FS
	root     string
	blobPath func(store.Hash) string
}

func New(fsys fsx.FS, viewsRoot string, blobPath func(store.Hash) string) *Tree {
	return &Tree{fsys: fsys, root: viewsRoot, blobPath: blobPath}
}

// segments validates a folder and a path and returns every segment below views/.
func segments(folder, path string) ([]string, error) {
	if folder == "" || strings.Contains(folder, "/") {
		return nil, fmt.Errorf("%w: folder %q", ErrBadPath, folder)
	}
	parts := append([]string{folder}, strings.Split(path, "/")...)
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, 0) {
			return nil, fmt.Errorf("%w: %q in %q", ErrBadPath, part, folder+"/"+path)
		}
	}
	return parts, nil
}

func (t *Tree) viewPath(parts []string) string {
	return filepath.Join(append([]string{t.root}, parts...)...)
}

// dirSet collects directories whose entries changed, to fsync once per Sync.
type dirSet map[string]struct{}

// Sync makes views/ agree with the index for the placements touched by recs.
func (t *Tree) Sync(ix *index.Index, recs []journal.Record) error {
	targets, err := t.touched(ix, recs)
	if err != nil {
		return err
	}
	dirs := dirSet{}
	for _, target := range targets {
		if err := t.syncOne(ix, target.folder, target.path, dirs); err != nil {
			return err
		}
	}
	return t.syncDirs(dirs)
}

type target struct{ folder, path string }

func (t *Tree) touched(ix *index.Index, recs []journal.Record) ([]target, error) {
	seen := map[target]bool{}
	var out []target
	add := func(folder, path string) {
		key := target{folder, path}
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, key)
	}
	for _, rec := range recs {
		switch rec.Type {
		case journal.TypePlacementSet, journal.TypePlacementCleared:
			add(rec.Folder, rec.Path)
		case journal.TypeItemTrashed, journal.TypeItemUntrashed, journal.TypeItemVersioned:
			placements, err := ix.PlacementsByItem(rec.Item)
			if err != nil {
				return nil, fmt.Errorf("syncing views: %w", err)
			}
			for _, p := range placements {
				add(p.Folder, p.Path)
			}
		}
	}
	return out, nil
}

func (t *Tree) syncOne(ix *index.Index, folder, path string, dirs dirSet) error {
	parts, err := segments(folder, path)
	if err != nil {
		return err
	}
	hash, live, err := ix.LiveLinkAt(folder, path)
	if err != nil {
		return fmt.Errorf("syncing views: %w", err)
	}
	if !live {
		return t.removeLink(parts, dirs)
	}
	_, err = t.ensureLink(parts, hash, dirs)
	return err
}

func (t *Tree) syncDirs(dirs dirSet) error {
	paths := make([]string, 0, len(dirs))
	for dir := range dirs {
		paths = append(paths, dir)
	}
	sort.Strings(paths)
	for _, dir := range paths {
		if err := t.fsys.SyncDir(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("syncing views: fsync %s: %w", dir, err)
		}
	}
	return nil
}

// removeLink removes the file for parts and any directory it leaves empty.
func (t *Tree) removeLink(parts []string, dirs dirSet) error {
	path := t.viewPath(parts)
	if err := t.fsys.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("removing view link %s: %w", path, err)
	}
	dirs[filepath.Dir(path)] = struct{}{}
	t.pruneEmptyParents(filepath.Dir(path), dirs)
	return nil
}

// pruneEmptyParents removes dir and its ancestors below the views root while
// they are empty. A failed Remove (not empty) ends the climb quietly.
func (t *Tree) pruneEmptyParents(dir string, dirs dirSet) {
	for dir != t.root && strings.HasPrefix(dir, t.root+string(filepath.Separator)) {
		if err := t.fsys.Remove(dir); err != nil {
			return
		}
		parent := filepath.Dir(dir)
		dirs[parent] = struct{}{}
		delete(dirs, dir)
		dir = parent
	}
}

// ensureLink makes views/<parts> a hardlink to the blob of hash. It reports
// whether it had to create or replace the link.
func (t *Tree) ensureLink(parts []string, hash store.Hash, dirs dirSet) (bool, error) {
	final := t.viewPath(parts)
	blob := t.blobPath(hash)

	// A blob that is gone (scrub will report it) cannot be linked; that must not
	// stop the rest of the tree from being built.
	if _, err := t.fsys.Stat(blob); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if same, err := t.sameInode(final, blob); err != nil {
		return false, err
	} else if same {
		return false, nil
	}
	if err := t.makeParents(parts[:len(parts)-1], dirs); err != nil {
		return false, err
	}
	info, err := t.fsys.Lstat(final)
	if err == nil && info.IsDir() {
		return false, fmt.Errorf("linking view %s: a directory is in the way", final)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("linking view %s: %w", final, err)
	}

	temp := final + TempSuffix
	if err := t.fsys.Remove(temp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("linking view %s: clearing stale temp: %w", final, err)
	}
	if err := t.fsys.Link(blob, temp); err != nil {
		return false, fmt.Errorf("linking view %s: %w", final, err)
	}
	crashLinkStaged.Hit()
	if err := t.fsys.Rename(temp, final); err != nil {
		_ = t.fsys.Remove(temp)
		return false, fmt.Errorf("linking view %s: renaming into place: %w", final, err)
	}
	crashLinkRenamed.Hit()
	dirs[filepath.Dir(final)] = struct{}{}
	return true, nil
}

// sameInode reports whether path is a regular file sharing blob's inode. A
// missing path, or a symlink at path, is simply "not the same".
func (t *Tree) sameInode(path, blob string) (bool, error) {
	linkInfo, err := t.fsys.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking view %s: %w", path, err)
	}
	if !linkInfo.Mode().IsRegular() {
		return false, nil
	}
	blobInfo, err := t.fsys.Stat(blob)
	if err != nil {
		return false, fmt.Errorf("checking view %s: stat blob: %w", path, err)
	}
	return os.SameFile(linkInfo, blobInfo), nil
}

// makeParents creates each missing directory one level at a time, refusing to
// pass through a symlink or a non-directory.
func (t *Tree) makeParents(parts []string, dirs dirSet) error {
	dir := t.root
	if err := t.fsys.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating views root: %w", err)
	}
	for _, part := range parts {
		parent := dir
		dir = filepath.Join(dir, part)
		info, err := t.fsys.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			if err := t.fsys.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("creating view directory %s: %w", dir, err)
			}
			dirs[parent] = struct{}{}
			continue
		}
		if err != nil {
			return fmt.Errorf("checking view directory %s: %w", dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("creating view directory %s: not a directory (symlink or file in the way)", dir)
		}
	}
	return nil
}

// Regenerate makes the whole tree agree with the index.
func (t *Tree) Regenerate(ctx context.Context, ix *index.Index) (Report, error) {
	var report Report
	if err := t.fsys.MkdirAll(t.root, 0o755); err != nil {
		return report, fmt.Errorf("regenerating views: %w", err)
	}
	dirs := dirSet{}
	err := ix.LiveLinks(func(folder, path string, h store.Hash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		parts, err := segments(folder, path)
		if err != nil {
			return err
		}
		linked, err := t.ensureLink(parts, h, dirs)
		if linked {
			report.Linked++
		}
		return err
	})
	if err != nil {
		return report, fmt.Errorf("regenerating views: %w", err)
	}
	removed, err := t.prune(ctx, ix, t.root, nil, dirs)
	report.Removed = removed
	if err != nil {
		return report, fmt.Errorf("regenerating views: %w", err)
	}
	if err := t.syncDirs(dirs); err != nil {
		return report, err
	}
	return report, nil
}

// prune removes files the index does not name and directories left empty.
// rel holds the path segments of dir below the views root.
func (t *Tree) prune(ctx context.Context, ix *index.Index, dir string, rel []string, dirs dirSet) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	entries, err := t.fsys.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("listing %s: %w", dir, err)
	}
	removed := 0
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		childRel := append(append([]string(nil), rel...), entry.Name())
		if entry.IsDir() {
			n, err := t.prune(ctx, ix, path, childRel, dirs)
			removed += n
			if err != nil {
				return removed, err
			}
			if len(childRel) > 0 {
				t.removeIfEmpty(path, dir, dirs)
			}
			continue
		}
		keep, err := t.indexNames(ix, entry, childRel)
		if err != nil {
			return removed, err
		}
		if keep {
			continue
		}
		if err := t.fsys.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, fmt.Errorf("removing %s: %w", path, err)
		}
		dirs[dir] = struct{}{}
		removed++
	}
	return removed, nil
}

func (t *Tree) removeIfEmpty(path, parent string, dirs dirSet) {
	if err := t.fsys.Remove(path); err != nil {
		return // not empty
	}
	dirs[parent] = struct{}{}
	delete(dirs, path)
}

// indexNames reports whether a regular file at rel is a live link of the index.
// Symlinks and devices are never named.
func (t *Tree) indexNames(ix *index.Index, entry fs.DirEntry, rel []string) (bool, error) {
	if !entry.Type().IsRegular() || len(rel) < 2 {
		return false, nil
	}
	_, ok, err := ix.LiveLinkAt(rel[0], strings.Join(rel[1:], "/"))
	return ok, err
}

// Manifest writes sorted "<folder>/<path>\t<hash>" lines. The hash is the one
// whose blob shares the link's inode; a link to any other inode prints "-".
func (t *Tree) Manifest(ctx context.Context, ix *index.Index, w io.Writer) error {
	var lines []string
	err := t.collect(ctx, ix, t.root, "", &lines)
	if err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}
	sort.Strings(lines)
	for _, line := range lines {
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return fmt.Errorf("writing manifest: %w", err)
		}
	}
	return nil
}

func (t *Tree) collect(ctx context.Context, ix *index.Index, dir, rel string, lines *[]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := t.fsys.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) && rel == "" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing %s: %w", dir, err)
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		childRel := entry.Name()
		if rel != "" {
			childRel = rel + "/" + entry.Name()
		}
		if entry.IsDir() {
			if err := t.collect(ctx, ix, path, childRel, lines); err != nil {
				return err
			}
			continue
		}
		hash, err := t.hashOfLink(ix, path, childRel)
		if err != nil {
			return err
		}
		*lines = append(*lines, childRel+"\t"+hash)
	}
	return nil
}

func (t *Tree) hashOfLink(ix *index.Index, path, rel string) (string, error) {
	folder, itemPath, found := strings.Cut(rel, "/")
	if !found {
		return "-", nil
	}
	hash, ok, err := ix.LiveLinkAt(folder, itemPath)
	if err != nil {
		return "", err
	}
	if !ok {
		return "-", nil
	}
	same, err := t.sameInode(path, t.blobPath(hash))
	if err != nil {
		return "", err
	}
	if !same {
		return "-", nil
	}
	return hash.String(), nil
}
