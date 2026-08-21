package thumbnail

import (
	"bytes"
	"fmt"
	"image"
	"path"
	"strings"

	"github.com/disintegration/imaging"
	"github.com/tjmisko/coldstore/internal/epub"
)

// generateEPUB extracts a book's cover and saves it through the same saveThumb
// path as everything else, so the 400px / q80 contract comes free.
//
// The parsing lives in internal/epub because the preview API needs the same
// reader: two copies of the href resolution would be two chances to get the
// traversal check wrong.
func (g *Generator) generateEPUB(src, dst string) error {
	book, err := epub.Open(src)
	if err != nil {
		return err
	}
	defer book.Close()

	coverPath, err := book.CoverPath()
	if err != nil {
		return err
	}
	if strings.EqualFold(path.Ext(coverPath), ".svg") {
		// A clean thumb_error rather than a rasteriser dependency: an SVG
		// cover is nearly always a wrapper around a bitmap that is also in the
		// archive, and the five-branch lookup already prefers that bitmap
		// where the book declares it.
		return epub.ErrSVGCover
	}

	raw, err := book.Resource(coverPath, epub.MaxImageBytes)
	if err != nil {
		return fmt.Errorf("reading cover %q: %w", coverPath, err)
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("decoding cover %q: %w", coverPath, err)
	}
	return saveThumb(imaging.Fit(img, maxEdge, maxEdge, imaging.Lanczos), dst)
}
