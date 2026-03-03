package api

import (
	"net/http"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/daemon"
)

type statusResponse struct {
	TotalFiles    int64          `json:"total_files"`
	SyncedFiles   int64          `json:"synced_files"`
	ArchivedFiles int64          `json:"archived_files"`
	MissingFiles  int64          `json:"missing_files"`
	Folders       []folderStatus `json:"folders"`
}

type folderStatus struct {
	Label      string `json:"label"`
	SyncedSize int64  `json:"synced_size_bytes"`
	Budget     int64  `json:"budget_bytes"`
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("manual scan triggered via API")
	if err := s.scanner.ScanAll(); err != nil {
		s.logger.Error("manual scan failed", "error", err)
		writeError(w, http.StatusInternalServerError, "scan failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	total, _ := s.cat.FileCount("")
	synced, _ := s.cat.FileCount(catalog.StatusSynced)
	archived, _ := s.cat.FileCount(catalog.StatusArchived)
	missing, _ := s.cat.FileCount(catalog.StatusMissing)

	var folders []folderStatus
	for _, f := range s.cfg.Folders {
		size, _ := s.cat.FolderSyncedSize(f.Label)
		folders = append(folders, folderStatus{
			Label:      f.Label,
			SyncedSize: size,
			Budget:     f.BudgetBytes,
		})
	}

	// Add cold storage disk info if available
	coldFree, _ := daemon.AvailableSpaceBytes(s.cfg.Storage.ColdStoragePath)

	resp := struct {
		statusResponse
		ColdStorageFreeBytes uint64 `json:"cold_storage_free_bytes"`
	}{
		statusResponse: statusResponse{
			TotalFiles:    total,
			SyncedFiles:   synced,
			ArchivedFiles: archived,
			MissingFiles:  missing,
			Folders:       folders,
		},
		ColdStorageFreeBytes: coldFree,
	}

	writeJSON(w, http.StatusOK, resp)
}
