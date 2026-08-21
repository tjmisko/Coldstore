package catalog

import (
	"strings"

	"github.com/tjmisko/coldstore/internal/metadata"
)

const (
	defaultMetadataLimit = 200
	maxMetadataAttempts  = 3
)

// FilesNeedingMetadata returns synced and archived files which have not been
// successfully inspected, are supported by the extractor, and have attempts
// left. A non-positive limit uses the same bounded default as the derived-data
// pass; there is deliberately no unbounded form.
func (c *Catalog) FilesNeedingMetadata(extensions []string, limit int) ([]FileRecord, error) {
	if len(extensions) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = defaultMetadataLimit
	}

	args := make([]any, 0, len(extensions)+2)
	for _, ext := range extensions {
		args = append(args, ext)
	}
	args = append(args, maxMetadataAttempts, limit)

	return c.queryFiles(`SELECT `+fileColumns+`
		FROM files
		WHERE meta_scanned_at IS NULL
		AND extension IN (`+placeholders(len(extensions))+`)
		AND status IN ('synced', 'archived')
		AND meta_attempts < ?
		ORDER BY file_mtime DESC
		LIMIT ?`, args...)
}

// SetExtractedMetadata atomically records a successful extraction. Empty
// capture/location values are a successful "looked and found nothing", not a
// failure. Owner-entered values win even if they are written concurrently:
// the source checks happen in the UPDATE itself, not in a preceding read.
func (c *Catalog) SetExtractedMetadata(id int64, value metadata.Extracted, scannedAt int64) error {
	contentID := strings.ToLower(strings.TrimSpace(value.ContentID))
	pairKey := ""
	if contentID != "" {
		pairKey = "cid:" + contentID
	}
	_, err := c.db.Exec(`
		UPDATE files SET
			captured_local = CASE WHEN captured_source = 'manual'
				THEN captured_local ELSE ? END,
			captured_offset_minutes = CASE WHEN captured_source = 'manual'
				THEN captured_offset_minutes ELSE ? END,
			captured_source = CASE WHEN captured_source = 'manual'
				THEN captured_source ELSE ? END,
			gps_lat = CASE WHEN gps_source = 'manual' THEN gps_lat ELSE ? END,
			gps_lon = CASE WHEN gps_source = 'manual' THEN gps_lon ELSE ? END,
			gps_altitude_m = CASE WHEN gps_source = 'manual' THEN gps_altitude_m ELSE ? END,
			gps_accuracy_m = CASE WHEN gps_source = 'manual' THEN gps_accuracy_m ELSE ? END,
			gps_source = CASE WHEN gps_source = 'manual' THEN gps_source ELSE ? END,
			-- A video ContentIdentifier is already authoritative. Persist it so
			-- the later pairing pass does not have to reopen a successfully
			-- scanned MOV. It may replace a basename guess, but never an
			-- existing content-id or a future/manual authority.
			pair_key = CASE
				WHEN ? <> '' AND (pair_source = 'basename' OR
					(pair_source = '' AND pair_key = '')) THEN ?
				ELSE pair_key END,
			pair_source = CASE
				WHEN ? <> '' AND (pair_source = 'basename' OR
					(pair_source = '' AND pair_key = '')) THEN 'content_id'
				ELSE pair_source END,
			meta_scanned_at = ?,
			meta_error = ''
		WHERE id = ?`,
		value.CapturedLocal, value.CapturedOffset, value.CapturedSource,
		value.Lat, value.Lon, value.AltitudeM, value.AccuracyM, value.GPSSource,
		pairKey, pairKey, pairKey,
		scannedAt, id)
	return err
}

// RecordMetadataFailure increments the extraction-specific attempt counter and
// records the last failure. It does not touch thumbnail state or claim the file
// was successfully inspected.
func (c *Catalog) RecordMetadataFailure(id int64, errMsg string, attemptedAt int64) error {
	_, err := c.db.Exec(`
		UPDATE files SET meta_attempts = meta_attempts + 1,
			meta_last_attempt = ?, meta_error = ?
		WHERE id = ?`, attemptedAt, errMsg, id)
	return err
}

// ResetMetadataFailures clears only failed-attempt bookkeeping so an owner
// rescan can retry repaired files. Successful rows stay scanned. Pass an empty
// label to reset every folder.
func (c *Catalog) ResetMetadataFailures(folder string) (int64, error) {
	query := `UPDATE files SET meta_attempts = 0, meta_last_attempt = NULL, meta_error = ''
		WHERE meta_attempts > 0`
	var args []any
	if folder != "" {
		query += ` AND folder_label = ?`
		args = append(args, folder)
	}
	res, err := c.db.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
