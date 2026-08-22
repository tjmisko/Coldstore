package metadata

import "testing"

func TestSupportedExtensionsReturnsADefensiveCopy(t *testing.T) {
	first := SupportedExtensions()
	if len(first) == 0 {
		t.Fatal("metadata extractor has no supported extensions")
	}
	want := first[0]
	first[0] = ".mutated"
	if got := SupportedExtensions()[0]; got != want {
		t.Errorf("mutating returned extensions changed extractor allowlist to %q", got)
	}
}
