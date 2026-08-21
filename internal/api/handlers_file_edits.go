package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/tjmisko/coldstore/internal/catalog"
)

const maxDescriptionCharacters = 2000

var manualCapturePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?:\.\d{3})?$`)
var captureDayPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

type updateFileRequest struct {
	Description   *string  `json:"description"`
	CapturedLocal *string  `json:"captured_local"`
	GPSLat        *float64 `json:"gps_lat"`
	GPSLon        *float64 `json:"gps_lon"`
}

func (s *Server) handleUpdateFile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid file id")
		return
	}

	var req updateFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Description != nil && utf8.RuneCountInString(*req.Description) > maxDescriptionCharacters {
		writeError(w, http.StatusBadRequest, "description must be at most 2000 characters")
		return
	}
	if req.CapturedLocal != nil && *req.CapturedLocal != "" {
		if !validManualCapture(*req.CapturedLocal) {
			writeError(w, http.StatusBadRequest, "captured_local must be a real date in YYYY-MM-DD HH:MM:SS or YYYY-MM-DD HH:MM:SS.sss form")
			return
		}
	}
	if (req.GPSLat == nil) != (req.GPSLon == nil) {
		writeError(w, http.StatusBadRequest, "gps_lat and gps_lon must be provided together")
		return
	}
	if req.GPSLat != nil && (*req.GPSLat < -90 || *req.GPSLat > 90 || *req.GPSLon < -180 || *req.GPSLon > 180) {
		writeError(w, http.StatusBadRequest, "GPS coordinates are out of range")
		return
	}

	edits := catalog.FileEdits{Description: req.Description, CapturedLocal: req.CapturedLocal}
	if req.GPSLat != nil {
		edits.GPS = &catalog.GPSCoordinates{Lat: *req.GPSLat, Lon: *req.GPSLon}
	}
	file, err := s.cat.UpdateFile(id, edits)
	if errors.Is(err, catalog.ErrNoSuchFile) {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	if err != nil {
		s.logger.Error("updating file metadata", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update file")
		return
	}
	writeJSON(w, http.StatusOK, file)
}

func validManualCapture(value string) bool {
	if !manualCapturePattern.MatchString(value) {
		return false
	}
	_, err := time.Parse("2006-01-02 15:04:05.999", value)
	return err == nil
}

func validCaptureDay(value string) bool {
	if !captureDayPattern.MatchString(value) {
		return false
	}
	_, err := time.Parse("2006-01-02", value)
	return err == nil
}
