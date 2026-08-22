package catalog

import (
	"fmt"
	"strings"
)

// FileEdits carries owner-authored catalog fields. Pointers distinguish an
// omitted field from an explicit empty value.
type FileEdits struct {
	Description   *string
	CapturedLocal *string
	GPS           *GPSCoordinates
}

type GPSCoordinates struct {
	Lat float64
	Lon float64
}

// UpdateFile applies owner-authored metadata without touching the source
// bytes. Manual capture and GPS values are marked as such so the background
// extractor cannot overwrite them.
func (c *Catalog) UpdateFile(id int64, edits FileEdits) (*FileRecord, error) {
	tx, err := c.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	set := make([]string, 0, 8)
	args := make([]any, 0, 8)
	if edits.Description != nil {
		set = append(set, "description = ?")
		args = append(args, *edits.Description)
	}
	if edits.CapturedLocal != nil {
		set = append(set, "captured_local = ?", "captured_offset_minutes = NULL")
		args = append(args, *edits.CapturedLocal)
		if *edits.CapturedLocal == "" {
			// Clearing the manual value means "look again", not "we looked and
			// found nothing". Reset only the extraction queue state; a manual GPS
			// value remains protected by gps_source below.
			set = append(set,
				"captured_source = ''",
				"meta_scanned_at = NULL",
				"meta_attempts = 0",
				"meta_last_attempt = NULL",
				"meta_error = ''",
			)
		} else {
			set = append(set, "captured_source = 'manual'")
		}
	}
	if edits.GPS != nil {
		set = append(set,
			"gps_lat = ?",
			"gps_lon = ?",
			"gps_altitude_m = NULL",
			"gps_accuracy_m = NULL",
			"gps_source = 'manual'",
		)
		args = append(args, edits.GPS.Lat, edits.GPS.Lon)
	}

	if len(set) > 0 {
		args = append(args, id)
		result, err := tx.Exec("UPDATE files SET "+strings.Join(set, ", ")+" WHERE id = ?", args...)
		if err != nil {
			return nil, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if changed == 0 {
			return nil, ErrNoSuchFile
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing file metadata update: %w", err)
	}
	// Read through the public lookup after commit so its tag hydration remains
	// part of the PATCH response as that projection evolves.
	return c.GetFileByID(id)
}
