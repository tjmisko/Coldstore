package catalog

// DerivedFailureCounts summarises derived-data work that is either failed or
// still pending. Metadata extraction does not exist in schema 6, so it is not
// represented here and callers must report that limitation explicitly.
type DerivedFailureCounts struct {
	ThumbnailFailures int64 `json:"thumbnail_failures"`
	DetectionBacklog  int64 `json:"detection_backlog"`
}

func (c *Catalog) DerivedFailureCounts() (DerivedFailureCounts, error) {
	var counts DerivedFailureCounts
	err := c.db.QueryRow(`
		SELECT COALESCE(SUM(CASE WHEN thumb_error <> '' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN detected_type = '' THEN 1 ELSE 0 END), 0)
		FROM files`).Scan(&counts.ThumbnailFailures, &counts.DetectionBacklog)
	return counts, err
}
