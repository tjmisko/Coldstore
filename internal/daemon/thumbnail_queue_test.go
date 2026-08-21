package daemon

import (
	"slices"
	"testing"

	"github.com/tjmisko/coldstore/internal/thumbnail"
)

// The extension list the thumbnail queue selects on used to be hand-copied
// here from internal/thumbnail. Nothing tied the copies together, so a format
// added to the generator and not to the daemon was supported by one and
// invisible to the other -- with no failure anywhere to say so. These tests
// are the tie.

func TestThumbnailExtsShouldMatchTheGeneratorWhenEitherChanges(t *testing.T) {
	want := thumbnail.SupportedExtensions()
	if !slices.Equal(thumbnailExts, want) {
		t.Errorf("the queue's extension list has drifted from the generator's:\n queue = %v\n generator = %v",
			thumbnailExts, want)
	}
}

func TestThumbnailExtsShouldBeAcceptedByCanThumbnailWhenQueried(t *testing.T) {
	if len(thumbnailExts) == 0 {
		t.Fatal("the thumbnail queue would select nothing")
	}
	for _, ext := range thumbnailExts {
		if !thumbnail.CanThumbnail(ext) {
			t.Errorf("the queue selects %q but the generator cannot thumbnail it", ext)
		}
	}
}

func TestThumbnailExtsShouldCoverEveryFormatWhenTheGeneratorSupportsIt(t *testing.T) {
	for _, ext := range thumbnail.SupportedExtensions() {
		if !slices.Contains(thumbnailExts, ext) {
			t.Errorf("the generator supports %q but the queue never selects it", ext)
		}
	}
}
