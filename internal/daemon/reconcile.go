package daemon

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

// ReconcileFolders folds the folders declared in coldstore.toml into
// folder_configs (design section 3.2).
//
// The split of authority is the whole point (D1): coldstore.toml DECLARES
// folders -- label, path, syncthing id -- and the database OWNS policy --
// budget, grace, enabled, persistent globs. So the budget and the grace period
// in TOML are SEED VALUES, written only when a folder is first sighted. On
// every later start they are ignored, because by then they may have been
// edited in the UI and re-applying TOML would silently undo that edit.
//
// A folder that has disappeared from TOML keeps its row and its tuning; it is
// only flagged present_in_toml = 0 so the daemon stops scanning and enforcing
// it while its files stay browsable (section 3.4).
//
// It runs before RecoverStartupState and before any concurrent actor exists,
// and a failure is fatal to the process: a daemon that cannot establish its
// policy table must not start enforcing against whatever happens to be in the
// database.
func ReconcileFolders(cfg *config.Config, cat *catalog.Catalog, logger *slog.Logger) error {
	identities, err := cat.ListFolderIdentities()
	if err != nil {
		return fmt.Errorf("listing folder identities before reconciliation: %w", err)
	}
	if err := ValidateFolderIdentityChanges(cfg, identities); err != nil {
		return err
	}

	declared := make([]string, 0, len(cfg.Folders))

	for _, f := range cfg.Folders {
		// Already defaulted to 30 by config.setDefaults.
		seedGrace := cfg.Daemon.GracePeriodMinutes

		inserted, err := cat.ReconcileFolderFromTOML(f.Label, f.SyncthingID, f.Path,
			f.BudgetBytes, seedGrace)
		if err != nil {
			return fmt.Errorf("reconciling folder %q: %w", f.Label, err)
		}

		declared = append(declared, f.Label)

		if !inserted {
			logger.Debug("folder already known, policy left to the database",
				"folder", f.Label, "path", f.Path)
			continue
		}

		// First sighting of this folder: its TOML persistent globs seed the
		// table the scanner and the offloader read from now on.
		for _, pattern := range f.Persistent {
			if err := cat.AddPersistentPath(f.Label, pattern); err != nil {
				return fmt.Errorf("seeding persistent path %q for folder %q: %w",
					pattern, f.Label, err)
			}
		}

		if err := cat.LogEvent("folder_seeded", f.Label, "",
			fmt.Sprintf("budget %d bytes, grace %d min", f.BudgetBytes, seedGrace)); err != nil {
			return fmt.Errorf("logging folder_seeded for %q: %w", f.Label, err)
		}

		logger.Info("seeded folder policy from coldstore.toml",
			"folder", f.Label, "path", f.Path,
			"budget_bytes", f.BudgetBytes, "grace_minutes", seedGrace)
	}

	if err := cat.MarkFoldersAbsentFromTOML(declared); err != nil {
		return fmt.Errorf("marking folders absent from coldstore.toml: %w", err)
	}

	all, err := cat.ListFolderConfigs()
	if err != nil {
		return fmt.Errorf("listing folder configs after reconciliation: %w", err)
	}
	for _, fc := range all {
		if fc.PresentInTOML {
			continue
		}
		logger.Warn("folder is no longer declared in coldstore.toml; "+
			"it will not be scanned or enforced, but its files stay browsable",
			"folder", fc.FolderLabel, "path", fc.SyncthingPath)
	}

	return nil
}

// ValidateFolderIdentityChanges rejects a TOML declaration that assigns an
// already-catalogued path to a different label. Labels key file rows, archive
// paths, policy and persistent globs, so changing one is not a cosmetic rename:
// it creates a second folder while stranding everything under the original.
//
// Both sides are cleaned before comparison so a trailing slash or a path such
// as /srv/sync/old/../Photos cannot bypass the invariant. A path move under the
// same label remains valid, as does removing a folder from TOML; removed
// catalog rows still reserve their identity in case they are later restored.
func ValidateFolderIdentityChanges(cfg *config.Config, identities []catalog.FolderIdentity) error {
	// Validate the entire declaration set before reconciliation writes its
	// first row. Config.Load already enforces these basics in production, but
	// keeping the boundary self-contained prevents partial writes from direct
	// callers and tests.
	for _, f := range cfg.Folders {
		if f.Label == "" || f.Path == "" {
			return fmt.Errorf("folder declaration missing label or path (label %q, path %q)",
				f.Label, f.Path)
		}
		if f.BudgetBytes <= 0 {
			return fmt.Errorf("folder %q has a non-positive budget (%d bytes)",
				f.Label, f.BudgetBytes)
		}
	}

	for _, f := range cfg.Folders {
		declaredPath := filepath.Clean(f.Path)
		for _, identity := range identities {
			if identity.FolderLabel == f.Label || filepath.Clean(identity.SyncthingPath) != declaredPath {
				continue
			}
			return fmt.Errorf("folder %q declares path %q, but that path is already catalogued under label %q; "+
				"folder labels are permanent identifiers. Restore label %q in coldstore.toml, or use a different path; "+
				"display_name is the cosmetic alternative when this version exposes it",
				f.Label, declaredPath, identity.FolderLabel, identity.FolderLabel)
		}
	}
	return nil
}
