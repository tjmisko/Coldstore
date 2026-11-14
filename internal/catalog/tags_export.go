package catalog

// NormalizeTagName applies the tag vocabulary rules to one tag name and returns
// its display form and its case-folded form. It exists so the archive index
// shares the legacy catalog's rules without a second copy of them.
func NormalizeTagName(raw string) (display, fold string, err error) {
	normalized, err := normalizeTagName(raw)
	if err != nil {
		return "", "", err
	}
	return normalized.name, normalized.fold, nil
}
