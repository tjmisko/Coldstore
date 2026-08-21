package metadata

import (
	"math"
	"testing"
)

func TestParseISO6709Shapes(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		lat      float64
		lon      float64
		altitude *float64
	}{
		{"decimal with altitude", "+37.7749-122.4194+52.300/", 37.7749, -122.4194, float64Pointer(52.3)},
		{"decimal without altitude", "+37.7749-122.4194/", 37.7749, -122.4194, nil},
		{"degrees decimal minutes", "+3746.494-12225.164+10/", 37 + 46.494/60, -(122 + 25.164/60), float64Pointer(10)},
		{"degrees minutes seconds", "+372410.339-1220038.701+4.35CRSWGS_84/", 37 + 24.0/60 + 10.339/3600, -(122 + 38.701/3600), float64Pointer(4.35)},
		{"zero padded decimal", "+41.1971+019.1686+089.373/", 41.1971, 19.1686, float64Pointer(89.373)},
		{"missing trailing slash", "+37.7749-122.4194", 37.7749, -122.4194, nil},
		{"southern and below sea level", "-33.9249+018.4241-12.5/", -33.9249, 18.4241, float64Pointer(-12.5)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lat, lon, altitude, err := ParseISO6709(test.value)
			if err != nil {
				t.Fatal(err)
			}
			assertClose(t, "latitude", lat, test.lat)
			assertClose(t, "longitude", lon, test.lon)
			if test.altitude == nil {
				if altitude != nil {
					t.Fatalf("altitude = %v, want nil", *altitude)
				}
			} else if altitude == nil {
				t.Fatal("altitude = nil, want a value")
			} else {
				assertClose(t, "altitude", *altitude, *test.altitude)
			}
		})
	}
}

func TestParseISO6709RejectsAmbiguousOrOutOfRangeValues(t *testing.T) {
	for _, value := range []string{
		"37.7749-122.4194/",
		"+91.0000-122.0000/",
		"+3760.000-12200.000/",
		"+372460.000-1220038.000/",
	} {
		if _, _, _, err := ParseISO6709(value); err == nil {
			t.Errorf("ParseISO6709(%q) succeeded, want an error", value)
		}
	}
}

func assertClose(t *testing.T, field string, got, want float64) {
	t.Helper()
	assertCloseWithin(t, field, got, want, 1e-9)
}

func assertCloseWithin(t *testing.T, field string, got, want, tolerance float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Fatalf("%s = %.12f, want %.12f", field, got, want)
	}
}
