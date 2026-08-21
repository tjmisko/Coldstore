package api

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func parseNear(raw string) (*catalog.NearFilter, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) != 3 {
		return nil, fmt.Errorf("near must be lat,lon,km")
	}
	values := make([]float64, 3)
	for i, part := range parts {
		value, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("near must contain finite numbers")
		}
		values[i] = value
	}
	if values[0] < -90 || values[0] > 90 {
		return nil, fmt.Errorf("near latitude must be between -90 and 90")
	}
	if values[1] < -180 || values[1] > 180 {
		return nil, fmt.Errorf("near longitude must be between -180 and 180")
	}
	if values[2] <= 0 || values[2] > 500 {
		return nil, fmt.Errorf("near radius must be greater than 0 and at most 500 km")
	}
	return &catalog.NearFilter{Lat: values[0], Lon: values[1], RadiusKM: values[2]}, nil
}

func validCoordinates(lat, lon float64) bool {
	return !math.IsNaN(lat) && !math.IsInf(lat, 0) && !math.IsNaN(lon) && !math.IsInf(lon, 0) &&
		lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}
