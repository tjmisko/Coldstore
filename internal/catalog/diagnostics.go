package catalog

// DerivedFailureCounts summarises derived-data work that is either failed or
// still pending. MetadataBacklog counts eligible rows that remain below the
// three-attempt cap; exhausted rows remain visible through MetadataFailures.
type DerivedFailureCounts struct {
	ThumbnailFailures int64 `json:"thumbnail_failures"`
	DetectionBacklog  int64 `json:"detection_backlog"`
	MetadataFailures  int64 `json:"metadata_failures"`
	MetadataBacklog   int64 `json:"metadata_backlog"`
}

func (c *Catalog) DerivedFailureCounts() (DerivedFailureCounts, error) {
	var counts DerivedFailureCounts
	err := c.db.QueryRow(`
		SELECT COALESCE(SUM(CASE WHEN thumb_error <> '' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN detected_type = '' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN meta_error <> '' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN meta_scanned_at IS NULL AND meta_attempts < 3
		                         AND status IN ('synced', 'archived') THEN 1 ELSE 0 END), 0)
		FROM files`).Scan(&counts.ThumbnailFailures, &counts.DetectionBacklog,
		&counts.MetadataFailures, &counts.MetadataBacklog)
	return counts, err
}
