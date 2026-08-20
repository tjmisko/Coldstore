package api

import (
	"net/http"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/daemon"
)

type statusResponse struct {
	TotalFiles           int64          `json:"total_files"`
	SyncedFiles          int64          `json:"synced_files"`
	ArchivedFiles        int64          `json:"archived_files"`
	MissingFiles         int64          `json:"missing_files"`
	ColdStorageFreeBytes uint64         `json:"cold_storage_free_bytes"`
	Folders              []folderStatus `json:"folders"`
}

type folderStatus struct {
	Label string `json:"label"`
	// SyncedSizeBytes keeps its name for compatibility, but it is the folder's
	// ON-DEVICE FOOTPRINT: status IN ('synced','archiving','restoring'), the
	// same aggregate enforcement budgets against.
	SyncedSizeBytes int64 `json:"synced_size_bytes"`
	BudgetBytes     int64 `json:"budget_bytes"`
	// PinnedBytes is a SUBSET of SyncedSizeBytes, never an addend (D3).
	PinnedBytes    int64                `json:"pinned_bytes"`
	OverBudget     bool                 `json:"over_budget"`
	Wedged         bool                 `json:"wedged"`
	GraceMinutes   int                  `json:"grace_minutes"`
	Enabled        bool                 `json:"enabled"`
	PresentInTOML  bool                 `json:"present_in_toml"`
	LastEnforcedAt *int64               `json:"last_enforced_at"`
	SyncthingType  string               `json:"syncthing_type"`
	Counts         catalog.FolderCounts `json:"counts"`
}

// handleScan enqueues a scan job. It used to run a full filesystem walk inside
// the HTTP handler, with no guard against the daemon's own concurrent scan and
// no bound on how long the request took: on a 50,000-file camera roll on a cold
// pool that is minutes of a held connection, and two overlapping walks of the
// same tree with interleaving generation stamps.
//
// The path and the body are unchanged so the UI's refresh button keeps working;
// the 200 {"status":"ok"} became a 202 with a job id (design section 6.11).
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("manual scan triggered via API")
	s.enqueue(w, catalog.JobActionScan, "", nil)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	total, err := s.cat.FileCount("")
	if err != nil {
		s.statusError(w, "counting files", err)
		return
	}
	synced, err := s.cat.FileCount(catalog.StatusSynced)
	if err != nil {
		s.statusError(w, "counting synced files", err)
		return
	}
	archived, err := s.cat.FileCount(catalog.StatusArchived)
	if err != nil {
		s.statusError(w, "counting archived files", err)
		return
	}
	missing, err := s.cat.FileCount(catalog.StatusMissing)
	if err != nil {
		s.statusError(w, "counting missing files", err)
		return
	}

	// Every folder the catalog knows about, including the ones the user has
	// disabled and the ones that have disappeared from coldstore.toml. Both
	// are flagged rather than hidden, so the UI can explain why a folder has
	// stopped moving instead of losing it (design 3.4).
	configs, err := s.cat.ListFolderConfigs()
	if err != nil {
		s.statusError(w, "listing folder configs", err)
		return
	}

	folders := make([]folderStatus, 0, len(configs))
	for _, fc := range configs {
		fs, err := s.folderStatusFor(fc)
		if err != nil {
			s.statusError(w, "summarising folder "+fc.FolderLabel, err)
			return
		}
		folders = append(folders, fs)
	}

	coldFree, err := daemon.AvailableSpaceBytes(s.cfg.Storage.ColdStoragePath)
	if err != nil {
		s.statusError(w, "checking cold storage free space", err)
		return
	}

	writeJSON(w, http.StatusOK, statusResponse{
		TotalFiles:           total,
		SyncedFiles:          synced,
		ArchivedFiles:        archived,
		MissingFiles:         missing,
		ColdStorageFreeBytes: coldFree,
		Folders:              folders,
	})
}

// folderStatusFor builds one folder's live budget picture. It is shared by
// GET /status and by the pin endpoints, which return the same block so the UI
// can redraw the budget bar without a second round trip -- and so the two can
// never disagree about what "wedged" means.
func (s *Server) folderStatusFor(fc catalog.FolderConfigRecord) (folderStatus, error) {
	onDevice, pinned, err := s.cat.FolderSizes(fc.FolderLabel)
	if err != nil {
		return folderStatus{}, err
	}
	counts, err := s.cat.FolderCounts(fc.FolderLabel)
	if err != nil {
		return folderStatus{}, err
	}

	budget := catalog.FolderBudget{
		BudgetBytes: fc.BudgetBytes,
		TotalBytes:  onDevice,
		PinnedBytes: pinned,
	}

	return folderStatus{
		Label:           fc.FolderLabel,
		SyncedSizeBytes: onDevice,
		BudgetBytes:     fc.BudgetBytes,
		PinnedBytes:     pinned,
		OverBudget:      budget.OverBudget(),
		Wedged:          budget.Wedged(),
		GraceMinutes:    fc.GraceMinutes,
		Enabled:         fc.Enabled,
		PresentInTOML:   fc.PresentInTOML,
		LastEnforcedAt:  fc.LastEnforcedAt,
		// The Syncthing API client is out of scope; "" means unverified.
		SyncthingType: "",
		Counts:        counts,
	}, nil
}

// folderStatusForLabel is folderStatusFor with the config read included.
func (s *Server) folderStatusForLabel(label string) (folderStatus, error) {
	fc, err := s.cat.FolderConfigByLabel(label)
	if err != nil {
		return folderStatus{}, err
	}
	return s.folderStatusFor(*fc)
}

// statusError refuses to render a broken read as zeros. A locked database used
// to show up as an empty, healthy-looking dashboard.
func (s *Server) statusError(w http.ResponseWriter, what string, err error) {
	s.logger.Error("status: "+what, "error", err)
	writeError(w, http.StatusInternalServerError, "failed to read status: "+what)
}
