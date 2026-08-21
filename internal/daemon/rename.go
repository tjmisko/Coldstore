package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/tjmisko/coldstore/internal/catalog"
)

var (
	ErrInvalidRenamePath = errors.New("invalid rename path")
	ErrRenameConflict    = errors.New("rename destination is occupied")
	ErrRenameSource      = errors.New("rename source does not exist")
	ErrRenameTransient   = errors.New("rename source contains a moving file")
	ErrRenamePair        = errors.New("rename would split a pair group")
	errRenameIncomplete  = errors.New("rename is incomplete and must be retried")
)

// CleanRenamePath accepts only the canonical slash-separated representation
// stored in files.relative_path. Refusing to silently clean user input keeps a
// payload's displayed path identical to the path the worker will mutate.
func CleanRenamePath(raw string) (string, error) {
	if raw == "" || raw == "." || strings.Contains(raw, "\\") ||
		strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%w: path must be a non-empty relative path", ErrInvalidRenamePath)
	}
	if path.IsAbs(raw) || path.Clean(raw) != raw {
		return "", fmt.Errorf("%w: %q is not a cleaned relative path", ErrInvalidRenamePath, raw)
	}
	for _, segment := range strings.Split(raw, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("%w: %q contains an unsafe segment", ErrInvalidRenamePath, raw)
		}
	}
	if strings.Split(raw, "/")[0] == ".stfolder" {
		return "", fmt.Errorf("%w: the Syncthing marker cannot be renamed", ErrInvalidRenamePath)
	}
	return raw, nil
}

type renameFSAction struct {
	root        string
	source      string
	destination string
	directory   bool
	resume      bool
}

type renameDerivedRemoval struct {
	root     string
	filename string
}

type renamePlan struct {
	folder       string
	from         string
	to           string
	directory    bool
	complete     bool
	files        []catalog.FileRecord
	fileRenames  []catalog.FileRename
	actions      []renameFSAction
	removeThumbs []renameDerivedRemoval
	warnings     []string
}

// PreflightRename validates under the same mutex every archive, restore and
// pair assignment uses. The job repeats all checks when it eventually runs;
// this pass exists to give the HTTP caller an immediate 4xx and glob warnings.
func (m *Mover) PreflightRename(ctx context.Context, folder, from, to string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	plan, err := m.planRename(folder, from, to, false)
	if err != nil {
		return nil, err
	}
	return sortedWarnings(plan.warnings), nil
}

// WithRenamePreflight keeps validation and durable enqueue in one mover-lock
// interval. Pair membership therefore cannot change between the group check
// and persistence of the payload the worker will later execute.
func (m *Mover) WithRenamePreflight(ctx context.Context, folder, from, to string,
	fn func([]string) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	plan, err := m.planRename(folder, from, to, false)
	if err != nil {
		return err
	}
	return fn(sortedWarnings(plan.warnings))
}

// RenamePath executes a restart-resumable rename while excluding every move.
// Each filesystem action accepts source-absent/destination-present as an
// already-completed step, and the catalog change is one transaction.
func (m *Mover) RenamePath(ctx context.Context, folder, from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scannerMu != nil {
		m.scannerMu.Lock()
		defer m.scannerMu.Unlock()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	plan, err := m.planRename(folder, from, to, true)
	if err != nil {
		return err
	}
	if plan.complete {
		m.cat.LogEvent("folder_renamed", folder, plan.from, "renamed to "+plan.to)
		return nil
	}
	for _, action := range plan.actions {
		if err := performRenameActionWithHook(action, m.beforeRenameCommit); err != nil {
			return fmt.Errorf("%w: %w", errRenameIncomplete, err)
		}
	}
	for _, thumb := range plan.removeThumbs {
		if err := removeDerivedFile(thumb); err != nil {
			return fmt.Errorf("%w: %w", errRenameIncomplete, err)
		}
	}
	if plan.directory {
		if _, err := m.cat.RenameDirectoryRows(folder, plan.from, plan.to); err != nil {
			return fmt.Errorf("%w: %w", errRenameIncomplete, err)
		}
	} else if err := m.cat.RenameFileRows(folder, plan.fileRenames); err != nil {
		return fmt.Errorf("%w: %w", errRenameIncomplete, err)
	}
	m.cat.LogEvent("folder_renamed", folder, plan.from, "renamed to "+plan.to)
	return nil
}

func (m *Mover) planRename(folder, fromRaw, toRaw string, allowResume bool) (*renamePlan, error) {
	from, err := CleanRenamePath(fromRaw)
	if err != nil {
		return nil, err
	}
	to, err := CleanRenamePath(toRaw)
	if err != nil {
		return nil, err
	}
	if from == to {
		return nil, fmt.Errorf("%w: source and destination are identical", ErrInvalidRenamePath)
	}
	if strings.HasPrefix(to, from+"/") {
		return nil, fmt.Errorf("%w: destination %q is inside source %q", ErrInvalidRenamePath, to, from)
	}
	fc, err := m.cat.FolderConfigByLabel(folder)
	if err != nil {
		return nil, err
	}
	files, err := m.cat.FilesAtOrBelow(folder, from)
	if err != nil {
		return nil, err
	}
	destinationRows, err := m.cat.FilesAtOrBelow(folder, to)
	if err != nil {
		return nil, err
	}
	for ancestor := path.Dir(to); ancestor != "." && ancestor != ""; ancestor = path.Dir(ancestor) {
		if _, ancestorErr := m.cat.GetFile(folder, ancestor); ancestorErr == nil {
			return nil, fmt.Errorf("%w: catalog parent %q is a file", ErrRenameConflict, ancestor)
		} else if !errors.Is(ancestorErr, catalog.ErrNoSuchFile) {
			return nil, ancestorErr
		}
	}
	warnings, err := m.cat.AffectedPersistentPatterns(folder, from)
	if err != nil {
		return nil, err
	}
	plan := &renamePlan{folder: folder, from: from, to: to, files: files, warnings: warnings}

	deviceFrom, err := safeTreePath(fc.SyncthingPath, from)
	if err != nil {
		return nil, err
	}
	archiveRoot := m.restorer.ArchiveRoot(folder)
	archiveFrom, err := safeTreePath(archiveRoot, from)
	if err != nil {
		return nil, err
	}
	deviceInfo, deviceExists, err := lstatOptional(deviceFrom)
	if err != nil {
		return nil, err
	}
	archiveInfo, archiveExists, err := lstatOptional(archiveFrom)
	if err != nil {
		return nil, err
	}
	thumbRoot := filepath.Join(m.cfg.Storage.ColdStoragePath, "thumbnails", folder)
	thumbFrom, err := safeTreePath(thumbRoot, from)
	if err != nil {
		return nil, err
	}
	thumbInfo, thumbExists, err := lstatOptional(thumbFrom)
	if err != nil {
		return nil, err
	}

	hasDescendant := false
	hasExact := false
	for _, file := range files {
		hasExact = hasExact || file.RelativePath == from
		hasDescendant = hasDescendant || strings.HasPrefix(file.RelativePath, from+"/")
	}
	plan.directory = hasDescendant || (deviceExists && deviceInfo.IsDir()) ||
		(archiveExists && archiveInfo.IsDir()) || (thumbExists && thumbInfo.IsDir())
	if err := validateRenameDestination(to, plan.directory); err != nil {
		return nil, err
	}

	if len(files) == 0 && !deviceExists && !archiveExists && !thumbExists {
		if allowResume && len(destinationRows) > 0 {
			plan.complete = true
			return plan, nil
		}
		if allowResume {
			for _, root := range []string{fc.SyncthingPath, archiveRoot, thumbRoot} {
				destination, pathErr := safeTreePath(root, to)
				if pathErr != nil {
					return nil, pathErr
				}
				info, exists, statErr := lstatOptional(destination)
				if statErr != nil {
					return nil, statErr
				}
				if exists {
					if !info.IsDir() {
						return nil, fmt.Errorf("%w: %s", ErrRenameConflict, destination)
					}
					plan.complete = true
				}
			}
			if plan.complete {
				return plan, nil
			}
		}
		return nil, fmt.Errorf("%w: %q", ErrRenameSource, from)
	}
	if len(destinationRows) > 0 && (len(files) > 0 || !allowResume) {
		return nil, fmt.Errorf("%w: catalog path %q", ErrRenameConflict, to)
	}
	if plan.directory {
		if hasExact {
			return nil, fmt.Errorf("%w: %q is both a file row and a directory prefix", ErrRenameConflict, from)
		}
		return m.planDirectoryRename(plan, fc.SyncthingPath, archiveRoot, deviceExists, archiveExists,
			deviceInfo, archiveInfo, allowResume)
	}
	if len(files) != 1 || files[0].RelativePath != from {
		return nil, fmt.Errorf("%w: no catalogued file at %q", ErrRenameSource, from)
	}
	return m.planFileRename(plan, fc.SyncthingPath, archiveRoot, allowResume)
}

func (m *Mover) planDirectoryRename(plan *renamePlan, deviceRoot, archiveRoot string,
	deviceExists, archiveExists bool, deviceInfo, archiveInfo os.FileInfo, allowResume bool) (*renamePlan, error) {
	if deviceExists && !deviceInfo.IsDir() || archiveExists && !archiveInfo.IsDir() {
		return nil, fmt.Errorf("%w: directory source is a file in one tree", ErrRenameConflict)
	}
	thumbRoot := filepath.Join(m.cfg.Storage.ColdStoragePath, "thumbnails", plan.folder)
	thumbFrom, err := safeTreePath(thumbRoot, plan.from)
	if err != nil {
		return nil, err
	}
	thumbInfo, thumbExists, err := lstatOptional(thumbFrom)
	if err != nil {
		return nil, err
	}
	if thumbExists && !thumbInfo.IsDir() {
		return nil, fmt.Errorf("%w: thumbnail directory source is a file", ErrRenameConflict)
	}
	if !allowResume && !deviceExists && !archiveExists && !thumbExists {
		return nil, fmt.Errorf("%w: directory %q is absent from every tree", ErrRenameSource, plan.from)
	}
	if err := rejectTransientFiles(plan.files); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(plan.files))
	for _, file := range plan.files {
		ids = append(ids, file.ID)
	}
	expanded, err := m.cat.ExpandPairGroups(ids)
	if err != nil {
		return nil, err
	}
	groupFiles, err := m.cat.FilesByIDs(expanded)
	if err != nil {
		return nil, err
	}
	for _, file := range groupFiles {
		if file.FolderLabel != plan.folder ||
			(file.RelativePath != plan.from && !strings.HasPrefix(file.RelativePath, plan.from+"/")) {
			return nil, fmt.Errorf("%w: %s is outside %s", ErrRenamePair, file.RelativePath, plan.from)
		}
	}
	for _, root := range []string{deviceRoot, archiveRoot, thumbRoot} {
		source, err := safeTreePath(root, plan.from)
		if err != nil {
			return nil, err
		}
		destination, err := safeTreePath(root, plan.to)
		if err != nil {
			return nil, err
		}
		plan.actions = append(plan.actions, renameFSAction{
			root: root, source: source, destination: destination, directory: true, resume: true,
		})
	}
	if err := preflightActions(plan.actions, allowResume); err != nil {
		return nil, err
	}
	return plan, nil
}

func (m *Mover) planFileRename(plan *renamePlan, deviceRoot, archiveRoot string,
	allowResume bool) (*renamePlan, error) {
	source := plan.files[0]
	expanded, err := m.cat.ExpandPairGroups([]int64{source.ID})
	if err != nil {
		return nil, err
	}
	files, err := m.cat.FilesByIDs(expanded)
	if err != nil {
		return nil, err
	}
	if err := rejectTransientFiles(files); err != nil {
		return nil, err
	}
	for _, file := range files {
		if file.Status == catalog.StatusMissing {
			return nil, fmt.Errorf("%w: pair member %s is missing", ErrRenamePair, file.RelativePath)
		}
		if path.Dir(file.RelativePath) != path.Dir(source.RelativePath) {
			return nil, fmt.Errorf("%w: pair member %s is in another directory", ErrRenamePair, file.RelativePath)
		}
	}

	renames, err := groupFileRenames(source, plan.to, files)
	if err != nil {
		return nil, err
	}
	plan.files = files
	plan.fileRenames = renames
	for _, rename := range renames {
		occupants, err := m.cat.FilesAtOrBelow(plan.folder, rename.To)
		if err != nil {
			return nil, err
		}
		for _, occupant := range occupants {
			if occupant.ID != rename.ID {
				return nil, fmt.Errorf("%w: catalog path %s", ErrRenameConflict, rename.To)
			}
		}
	}
	thumbRoot := filepath.Join(m.cfg.Storage.ColdStoragePath, "thumbnails")
	seenDestinations := make(map[string]struct{})
	for i, rename := range renames {
		file := files[i]
		expectedPhysical := false
		for rootIndex, root := range []string{deviceRoot, archiveRoot} {
			src, err := safeTreePath(root, rename.From)
			if err != nil {
				return nil, err
			}
			dst, err := safeTreePath(root, rename.To)
			if err != nil {
				return nil, err
			}
			if _, duplicate := seenDestinations[dst]; duplicate {
				return nil, fmt.Errorf("%w: two pair members target %q", ErrRenamePair, rename.To)
			}
			seenDestinations[dst] = struct{}{}
			_, exists, statErr := lstatOptional(src)
			if statErr != nil {
				return nil, statErr
			}
			_, destinationExists, statErr := lstatOptional(dst)
			if statErr != nil {
				return nil, statErr
			}
			resume := rootIndex == 0 && file.Status == catalog.StatusSynced ||
				rootIndex == 1 && file.Status == catalog.StatusArchived
			if resume && (exists || allowResume && destinationExists) {
				expectedPhysical = true
			}
			plan.actions = append(plan.actions, renameFSAction{
				root: root, source: src, destination: dst, resume: resume,
			})
		}
		if !expectedPhysical {
			if len(files) > 1 {
				return nil, fmt.Errorf("%w: pair member %s is absent from its catalogued tree",
					ErrRenamePair, file.RelativePath)
			}
			return nil, fmt.Errorf("%w: file %s is absent from its catalogued tree",
				ErrRenameSource, file.RelativePath)
		}

		oldThumbRel := file.ThumbnailPath
		if oldThumbRel == "" {
			oldThumbRel = path.Join(plan.folder, rename.From) + ".thumb.jpg"
		}
		oldThumb, err := safeTreePath(thumbRoot, oldThumbRel)
		if err != nil {
			return nil, err
		}
		newThumbRel := path.Join(plan.folder, rename.To) + ".thumb.jpg"
		newThumb, err := safeTreePath(thumbRoot, newThumbRel)
		if err != nil {
			return nil, err
		}
		if strings.ToLower(path.Ext(rename.From)) != strings.ToLower(path.Ext(rename.To)) {
			if _, exists, err := lstatOptional(newThumb); err != nil {
				return nil, err
			} else if exists {
				return nil, fmt.Errorf("%w: thumbnail %s", ErrRenameConflict, newThumbRel)
			}
			plan.removeThumbs = append(plan.removeThumbs, renameDerivedRemoval{
				root: thumbRoot, filename: oldThumb,
			})
		} else {
			plan.actions = append(plan.actions, renameFSAction{
				root: thumbRoot, source: oldThumb, destination: newThumb, resume: file.ThumbnailPath != "",
			})
		}
	}
	if err := preflightActions(plan.actions, allowResume); err != nil {
		return nil, err
	}
	return plan, nil
}

func groupFileRenames(source catalog.FileRecord, explicitTo string,
	files []catalog.FileRecord) ([]catalog.FileRename, error) {
	renames := make([]catalog.FileRename, 0, len(files))
	targetStem := visiblePairStem(path.Base(explicitTo))
	targetExt := path.Ext(explicitTo)
	canonical := catalog.CanonicalPairStem(source.Filename)
	targets := make(map[string]struct{}, len(files))
	for _, file := range files {
		to := explicitTo
		if file.ID != source.ID {
			suffix, ok := pairFilenameSuffix(file.Filename, canonical)
			if !ok {
				return nil, fmt.Errorf("%w: cannot safely derive a name for %s", ErrRenamePair, file.RelativePath)
			}
			// Appended sidecars name the still extension (photo.heic.xmp).
			// When that still is the explicitly renamed member and changes
			// extension, keep the sidecar name aligned with its new coordinate.
			withoutXMP := strings.TrimSuffix(file.Filename, path.Ext(file.Filename))
			if isPairStill(path.Ext(source.Filename)) &&
				strings.EqualFold(path.Ext(file.Filename), ".xmp") &&
				isPairStill(path.Ext(withoutXMP)) {
				suffix = targetExt + path.Ext(file.Filename)
			}
			to = path.Join(path.Dir(explicitTo), targetStem+suffix)
		}
		if _, duplicate := targets[to]; duplicate {
			return nil, fmt.Errorf("%w: pair members would both become %s", ErrRenamePair, to)
		}
		targets[to] = struct{}{}
		renames = append(renames, catalog.FileRename{ID: file.ID, From: file.RelativePath, To: to})
	}
	return renames, nil
}

func visiblePairStem(filename string) string {
	stem := strings.TrimSuffix(filename, path.Ext(filename))
	if strings.EqualFold(path.Ext(filename), ".xmp") {
		if inner := path.Ext(stem); isPairStill(inner) {
			stem = strings.TrimSuffix(stem, inner)
		}
	}
	return stem
}

func pairFilenameSuffix(filename, canonical string) (string, bool) {
	nameRunes := []rune(filename)
	canonicalRunes := []rune(canonical)
	if len(nameRunes) >= len(canonicalRunes) &&
		strings.EqualFold(string(nameRunes[:len(canonicalRunes)]), canonical) {
		return string(nameRunes[len(canonicalRunes):]), true
	}
	// IMG_E variants deliberately share a canonical stem with the original,
	// but renaming both stills to one visible stem would collide. Refuse this
	// ambiguous case instead of inventing a lossy naming convention.
	return "", false
}

func rejectTransientFiles(files []catalog.FileRecord) error {
	for _, file := range files {
		if file.Status == catalog.StatusArchiving || file.Status == catalog.StatusRestoring {
			return fmt.Errorf("%w: %s is %s", ErrRenameTransient, file.RelativePath, file.Status)
		}
	}
	return nil
}

func preflightActions(actions []renameFSAction, allowResume bool) error {
	for _, action := range actions {
		sourceInfo, sourceExists, err := lstatOptional(action.source)
		if err != nil {
			return err
		}
		destinationInfo, destinationExists, err := lstatOptional(action.destination)
		if err != nil {
			return err
		}
		if sourceExists && sourceInfo.IsDir() != action.directory {
			return fmt.Errorf("%w: source has unexpected type: %s", ErrRenameConflict, action.source)
		}
		if destinationExists && destinationInfo.IsDir() != action.directory {
			return fmt.Errorf("%w: destination has unexpected type: %s", ErrRenameConflict, action.destination)
		}
		if destinationExists && (sourceExists || !allowResume || !action.resume) {
			return fmt.Errorf("%w: %s", ErrRenameConflict, action.destination)
		}
	}
	return nil
}

func performRenameAction(action renameFSAction) error {
	return performRenameActionWithHook(action, nil)
}

func performRenameActionWithHook(action renameFSAction, beforeCommit func(renameFSAction) error) error {
	return performBoundRename(action, beforeCommit)
}

func removeDerivedFile(removal renameDerivedRemoval) error {
	return removeBoundFile(removal)
}

func validateRenameDestination(relative string, directory bool) error {
	segments := strings.Split(relative, "/")
	for i, segment := range segments {
		isFinal := i == len(segments)-1
		if (!isFinal || directory) && strings.HasPrefix(segment, ".") {
			return fmt.Errorf("%w: destination enters a scanner-ignored hidden directory", ErrInvalidRenamePath)
		}
		if skipScanEntry(segment) {
			return fmt.Errorf("%w: destination uses scanner-ignored name %q", ErrInvalidRenamePath, segment)
		}
	}
	return nil
}

func safeTreePath(root, relative string) (string, error) {
	joined := filepath.Join(root, filepath.FromSlash(relative))
	rel, err := filepath.Rel(root, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes %q", ErrInvalidRenamePath, relative, root)
	}
	// The configured root is trusted and may itself be a symlink, but no path
	// component beneath it may redirect the operation outside that root.
	// Leave the final component alone: renaming a symlink moves the link rather
	// than its target, while a symlink in any parent would be traversed.
	parent := filepath.Dir(rel)
	if parent != "." {
		current := root
		for _, component := range strings.Split(parent, string(filepath.Separator)) {
			current = filepath.Join(current, component)
			info, exists, statErr := lstatOptional(current)
			if statErr != nil {
				return "", statErr
			}
			if !exists {
				break
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("%w: %q traverses a symlink", ErrInvalidRenamePath, relative)
			}
			if !info.IsDir() {
				return "", fmt.Errorf("%w: parent is not a directory: %s", ErrRenameConflict, current)
			}
		}
	}
	return joined, nil
}

func lstatOptional(filename string) (os.FileInfo, bool, error) {
	info, err := os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return info, true, nil
}

func sortedWarnings(warnings []string) []string {
	sort.Strings(warnings)
	return warnings
}
