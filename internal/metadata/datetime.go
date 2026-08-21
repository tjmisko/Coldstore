package metadata

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

type captureCandidate struct {
	local  string
	offset *int
	source string
}

// chooseCapture encodes the authoritative precedence from the tagging design.
func chooseCapture(original, quicktime, digitized, mvhd captureCandidate) captureCandidate {
	for _, candidate := range []captureCandidate{original, quicktime, digitized, mvhd} {
		if candidate.local != "" {
			return candidate
		}
	}
	return captureCandidate{}
}

func parseEXIFCapture(value, subsecond, offset, source string, gpsUTC time.Time) (captureCandidate, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return captureCandidate{}, nil
	}
	wall, err := time.ParseInLocation("2006:01:02 15:04:05", value, time.UTC)
	if err != nil {
		return captureCandidate{}, fmt.Errorf("metadata: parse %s %q: %w", source, value, err)
	}
	fraction, err := normalizeSubsecond(subsecond)
	if err != nil {
		return captureCandidate{}, err
	}
	offsetMinutes, err := parseOffset(offset)
	if err != nil {
		return captureCandidate{}, err
	}
	if offsetMinutes == nil && !gpsUTC.IsZero() {
		offsetMinutes = deriveGPSOffset(wall, gpsUTC)
	}
	return captureCandidate{
		local:  formatWall(wall, fraction),
		offset: offsetMinutes,
		source: source,
	}, nil
}

func parseQuickTimeCapture(value string) (captureCandidate, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return captureCandidate{}, nil
	}
	var parsed time.Time
	var err error
	for _, layout := range []string{
		"2006-01-02T15:04:05-0700",
		time.RFC3339Nano,
	} {
		parsed, err = time.Parse(layout, value)
		if err == nil {
			_, seconds := parsed.Zone()
			offset := seconds / 60
			fraction, fractionErr := fractionFromTimestamp(value)
			if fractionErr != nil {
				return captureCandidate{}, fractionErr
			}
			return captureCandidate{
				local:  formatWall(parsed, fraction),
				offset: &offset,
				source: "quicktime",
			}, nil
		}
	}
	return captureCandidate{}, fmt.Errorf("metadata: parse QuickTime creation date %q: %w", value, err)
}

func parseMVHDCapture(value string, knownOffset *int) (captureCandidate, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return captureCandidate{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return captureCandidate{}, fmt.Errorf("metadata: parse mvhd creation_time %q: %w", value, err)
	}
	offset := 0
	if knownOffset != nil {
		offset = *knownOffset
		parsed = parsed.Add(time.Duration(offset) * time.Minute)
	}
	fraction, err := fractionFromTimestamp(value)
	if err != nil {
		return captureCandidate{}, err
	}
	return captureCandidate{
		local:  formatWall(parsed, fraction),
		offset: &offset,
		source: "quicktime_mvhd",
	}, nil
}

func parseOffset(value string) (*int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	for _, layout := range []string{"-07:00", "-0700"} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			_, seconds := parsed.Zone()
			minutes := seconds / 60
			return &minutes, nil
		}
	}
	return nil, fmt.Errorf("metadata: parse UTC offset %q", value)
}

func deriveGPSOffset(localWall, gpsUTC time.Time) *int {
	if localWall.IsZero() || gpsUTC.IsZero() {
		return nil
	}
	difference := localWall.Sub(gpsUTC.UTC())
	// A correct GPS date can differ by one calendar day from the local wall
	// clock near midnight. Fold the delta into the legal civil-zone range.
	for difference > 14*time.Hour {
		difference -= 24 * time.Hour
	}
	for difference < -14*time.Hour {
		difference += 24 * time.Hour
	}
	minutes := int(math.Round(difference.Minutes()/15) * 15)
	if minutes < -14*60 || minutes > 14*60 {
		return nil
	}
	return &minutes
}

func normalizeSubsecond(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "0.") {
		value = strings.TrimPrefix(value, "0.")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("metadata: invalid subsecond %q", value)
		}
	}
	return value, nil
}

func fractionFromTimestamp(value string) (string, error) {
	t := strings.IndexByte(value, 'T')
	if t < 0 {
		return "", nil
	}
	dot := strings.IndexByte(value[t:], '.')
	if dot < 0 {
		return "", nil
	}
	start := t + dot + 1
	end := start
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
	}
	return normalizeSubsecond(value[start:end])
}

func formatWall(value time.Time, subsecond string) string {
	formatted := value.Format("2006-01-02 15:04:05")
	if subsecond != "" {
		return formatted + "." + subsecond
	}
	return formatted
}

func subsecondFromAny(value any) (string, error) {
	switch value := value.(type) {
	case nil:
		return "", nil
	case string:
		return normalizeSubsecond(value)
	case fmt.Stringer:
		return normalizeSubsecond(value.String())
	case float64:
		return normalizeSubsecond(strconv.FormatFloat(value, 'f', -1, 64))
	default:
		return "", fmt.Errorf("metadata: unsupported subsecond value %T", value)
	}
}
