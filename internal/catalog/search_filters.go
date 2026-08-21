package catalog

// normalizeSearchFilters validates the public FileQuery before SQL is built.
// IDs are an explicit reconciliation projection and intentionally override
// every filter, including malformed or over-cap tag filters.
func normalizeSearchFilters(q FileQuery) (FileQuery, error) {
	if len(q.IDs) > 0 {
		return q, nil
	}
	if len(q.Tags) > MaxTagFilters {
		return q, ErrTooManyTagFilters
	}
	tags, err := NormalizeTagFilters(q.Tags)
	if err != nil {
		return q, err
	}
	q.Tags = tags
	return q, nil
}
