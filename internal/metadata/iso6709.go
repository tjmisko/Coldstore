package metadata

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

type isoShape struct {
	re         *regexp.Regexp
	components int
}

// The order is load-bearing: a DMS-shaped value can otherwise be consumed as
// a less precise representation with the wrong field boundaries.
var iso6709Shapes = []isoShape{
	{regexp.MustCompile(`^([+-])(\d{2}(?:\.\d+)?)([+-])(\d{3}(?:\.\d+)?)([+-]\d+(?:\.\d+)?)?(?:/?CRS[^/]*)?/?$`), 1},
	{regexp.MustCompile(`^([+-])(\d{2})(\d{2}(?:\.\d+)?)([+-])(\d{3})(\d{2}(?:\.\d+)?)([+-]\d+(?:\.\d+)?)?(?:/?CRS[^/]*)?/?$`), 2},
	{regexp.MustCompile(`^([+-])(\d{2})(\d{2})(\d{2}(?:\.\d+)?)([+-])(\d{3})(\d{2})(\d{2}(?:\.\d+)?)([+-]\d+(?:\.\d+)?)?(?:/?CRS[^/]*)?/?$`), 3},
}

// ParseISO6709 accepts decimal-degree, degree/minute, and
// degree/minute/second forms used by Apple and other QuickTime writers.
func ParseISO6709(value string) (lat, lon float64, altitude *float64, err error) {
	value = strings.TrimSpace(value)
	for _, shape := range iso6709Shapes {
		match := shape.re.FindStringSubmatch(value)
		if match == nil {
			continue
		}
		switch shape.components {
		case 1:
			lat, err = signedFloat(match[1], match[2])
			if err == nil {
				lon, err = signedFloat(match[3], match[4])
			}
			altitude, err = parseAltitude(match[5], err)
		case 2:
			lat, err = signedDegreesMinutes(match[1], match[2], match[3])
			if err == nil {
				lon, err = signedDegreesMinutes(match[4], match[5], match[6])
			}
			altitude, err = parseAltitude(match[7], err)
		case 3:
			lat, err = signedDegreesMinutesSeconds(match[1], match[2], match[3], match[4])
			if err == nil {
				lon, err = signedDegreesMinutesSeconds(match[5], match[6], match[7], match[8])
			}
			altitude, err = parseAltitude(match[9], err)
		}
		if err != nil {
			return 0, 0, nil, fmt.Errorf("metadata: parse ISO 6709 %q: %w", value, err)
		}
		if math.Abs(lat) > 90 || math.Abs(lon) > 180 {
			return 0, 0, nil, fmt.Errorf("metadata: ISO 6709 coordinate out of range: %q", value)
		}
		return lat, lon, altitude, nil
	}
	return 0, 0, nil, fmt.Errorf("metadata: invalid ISO 6709 coordinate %q", value)
}

func signedFloat(sign, value string) (float64, error) {
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, err
	}
	if sign == "-" {
		parsed = -parsed
	}
	return parsed, nil
}

func signedDegreesMinutes(sign, degrees, minutes string) (float64, error) {
	deg, err := strconv.ParseFloat(degrees, 64)
	if err != nil {
		return 0, err
	}
	min, err := strconv.ParseFloat(minutes, 64)
	if err != nil {
		return 0, err
	}
	if min >= 60 {
		return 0, fmt.Errorf("minutes out of range")
	}
	return signedFloat(sign, strconv.FormatFloat(deg+min/60, 'g', -1, 64))
}

func signedDegreesMinutesSeconds(sign, degrees, minutes, seconds string) (float64, error) {
	deg, err := strconv.ParseFloat(degrees, 64)
	if err != nil {
		return 0, err
	}
	min, err := strconv.ParseFloat(minutes, 64)
	if err != nil {
		return 0, err
	}
	sec, err := strconv.ParseFloat(seconds, 64)
	if err != nil {
		return 0, err
	}
	if min >= 60 || sec >= 60 {
		return 0, fmt.Errorf("minutes or seconds out of range")
	}
	return signedFloat(sign, strconv.FormatFloat(deg+min/60+sec/3600, 'g', -1, 64))
}

func parseAltitude(value string, prior error) (*float64, error) {
	if prior != nil {
		return nil, prior
	}
	if value == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
