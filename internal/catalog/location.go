package catalog

import (
	"errors"
	"math"
	"strings"
)

const (
	earthRadiusKM   = 6371.0088
	maxNearRadiusKM = 500
)

var ErrInvalidNear = errors.New("invalid near filter")

// NearFilter is a WGS84 centre and radius in kilometres. A pointer on
// FileQuery distinguishes no location filter from the real coordinate 0,0.
type NearFilter struct {
	Lat      float64
	Lon      float64
	RadiusKM float64
}

func (n NearFilter) valid() bool {
	return finite(n.Lat) && finite(n.Lon) && finite(n.RadiusKM) &&
		n.Lat >= -90 && n.Lat <= 90 && n.Lon >= -180 && n.Lon <= 180 &&
		n.RadiusKM > 0 && n.RadiusKM <= maxNearRadiusKM
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

type locationBounds struct {
	minLat, maxLat  float64
	minLon, maxLon  float64
	allLongitudes   bool
	crossesDateLine bool
}

// bounds returns a spherical bounding box that is a superset of the circle.
// Longitude is unrestricted when the circle reaches a pole; otherwise a
// wrapped interval represents a dateline crossing.
func (n NearFilter) bounds() locationBounds {
	angular := n.RadiusKM / earthRadiusKM
	latDelta := angular * 180 / math.Pi
	bounds := locationBounds{
		minLat: math.Max(-90, n.Lat-latDelta),
		maxLat: math.Min(90, n.Lat+latDelta),
	}
	if bounds.minLat <= -90 || bounds.maxLat >= 90 {
		bounds.allLongitudes = true
		return bounds
	}
	ratio := math.Sin(angular) / math.Cos(n.Lat*math.Pi/180)
	lonDelta := math.Asin(math.Min(1, ratio)) * 180 / math.Pi
	bounds.minLon = wrapLongitude(n.Lon - lonDelta)
	bounds.maxLon = wrapLongitude(n.Lon + lonDelta)
	bounds.crossesDateLine = bounds.minLon > bounds.maxLon
	return bounds
}

func wrapLongitude(value float64) float64 {
	wrapped := math.Mod(value+180, 360)
	if wrapped < 0 {
		wrapped += 360
	}
	return wrapped - 180
}

// appendBounds adds the index-backed SQL prefilter. The exact circle is
// applied in Go after the query; this predicate must never exclude a point
// that the haversine pass would accept.
func (n NearFilter) appendBounds(clause *strings.Builder, args *[]any) {
	bounds := n.bounds()
	clause.WriteString(" AND gps_lat IS NOT NULL AND gps_lon IS NOT NULL")
	clause.WriteString(" AND gps_lat BETWEEN ? AND ?")
	*args = append(*args, bounds.minLat, bounds.maxLat)
	if bounds.allLongitudes {
		return
	}
	if bounds.crossesDateLine {
		clause.WriteString(" AND (gps_lon >= ? OR gps_lon <= ?)")
	} else {
		clause.WriteString(" AND gps_lon BETWEEN ? AND ?")
	}
	*args = append(*args, bounds.minLon, bounds.maxLon)
}

func (n NearFilter) contains(lat, lon float64) bool {
	lat1 := n.Lat * math.Pi / 180
	lat2 := lat * math.Pi / 180
	dLat := lat2 - lat1
	dLon := (lon - n.Lon) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	a = math.Max(0, math.Min(1, a))
	distance := 2 * earthRadiusKM * math.Asin(math.Sqrt(a))
	return distance <= n.RadiusKM+1e-9
}

func filterFilesNear(files []FileRecord, near NearFilter) []FileRecord {
	result := make([]FileRecord, 0, len(files))
	for _, file := range files {
		if file.GPSLat != nil && file.GPSLon != nil && near.contains(*file.GPSLat, *file.GPSLon) {
			result = append(result, file)
		}
	}
	return result
}

// listFileIDsNear computes both the exact total and the capped id page from
// the same bounding-box rows. It deliberately does not LIMIT in SQL: the box
// is a superset, so limiting before the exact pass can under-fill a page and
// make total disagree with GET /files.
func (c *Catalog) listFileIDsNear(where string, args []any, near NearFilter, capacity int) ([]int64, int64, error) {
	rows, err := c.db.Query(`SELECT id, gps_lat, gps_lon FROM files WHERE 1=1`+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	ids := make([]int64, 0, min(capacity, 256))
	var total int64
	for rows.Next() {
		var id int64
		var lat, lon float64
		if err := rows.Scan(&id, &lat, &lon); err != nil {
			return nil, 0, err
		}
		if near.contains(lat, lon) {
			total++
			if len(ids) < capacity {
				ids = append(ids, id)
			}
		}
	}
	return ids, total, rows.Err()
}
