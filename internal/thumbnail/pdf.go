package thumbnail

import (
	"context"
	"fmt"
	"image"
	"math"
	"strconv"
	"time"
)

// maxPDFPagesProbed is how many pages the "first page with content" search
// will render before giving up and keeping page 1.
//
// Three, on one shared deadline. The microserver is a 2011 HP N40L: two cores
// at 1.5 GHz, sharing those cores with the scanner and with Syncthing. A cover
// sheet followed by two more blank pages is a document whose real first page
// is not worth four rasterisations to find.
const maxPDFPagesProbed = 3

// inkCoverageThreshold is the fraction of sampled pixels that must be dark
// enough to count as marks before a page is considered to have content.
//
// Half a percent. A page holding one line of small type is comfortably above
// it; a scanner's speckle and a JPEG's ringing around the page edge are not.
const inkCoverageThreshold = 0.005

// inkLuminanceCeiling is how dark a pixel has to be to count as ink, as a
// fraction of full white.
//
// Coverage, not standard deviation. The two disagree on gradient covers --
// where deviation is high and the page is a smooth wash with no marks on it --
// and coverage is the one that behaves on scans, where the paper is an even
// off-white and the text is the only dark thing on it.
const inkLuminanceCeiling = 0.86

// inkBorderMarginFraction is how much of each edge the coverage test ignores.
//
// Three percent. A scan carries a dark rim from the platen lid and a
// photographed page carries shadow; counting either as content makes every
// blank page in the document look like it has writing on it.
const inkBorderMarginFraction = 0.03

// inkSampleTarget is roughly how many pixels the coverage test looks at. The
// stride is derived from it, so the cost of the test does not grow with the
// page size.
const inkSampleTarget = 10000

// inkCeiling is inkLuminanceCeiling scaled to image/color's 16-bit channels,
// so the per-pixel test is an integer comparison. It runs over tens of
// thousands of pixels on a 1.5 GHz core.
var inkCeiling = uint32(math.Round(inkLuminanceCeiling * 0xFFFF))

// generatePDF renders the first page of a PDF that actually has something on
// it, and saves it through the same saveThumb path as everything else, so the
// 400px / q80 contract comes free.
//
// It never fails a file over the content search: if pages 2 and 3 cannot be
// rendered or are blank too, page 1 is kept regardless. A document of blank
// pages is a legitimate document, and a blank thumbnail is a better answer
// than a failure glyph.
func (g *Generator) generatePDF(ctx context.Context, src, dst string) error {
	// ONE deadline across every page this tries, not one per page: three
	// pages of a pathological PDF must not be able to take three times the
	// budget a single one gets.
	cctx, cancel, timeout := g.pdf.deadline(ctx)
	defer cancel()

	var firstPage image.Image
	for page := 1; page <= maxPDFPagesProbed; page++ {
		img, err := g.renderPDFPage(cctx, src, page, timeout)
		if err != nil {
			if page == 1 {
				// Page 1 failing is the file failing: a PDF with no readable
				// first page is one poppler cannot open at all.
				return err
			}
			// Past the last page, or a page that would not render. Either way
			// the search is over and page 1 stands.
			break
		}
		if firstPage == nil {
			firstPage = img
		}
		if hasInk(img) {
			return saveThumb(img, dst)
		}
	}

	if firstPage == nil {
		return fmt.Errorf("%s produced no page for %s", g.pdf.name, src)
	}
	return saveThumb(firstPage, dst)
}

// renderPDFPage rasterises one page to stdout and decodes it.
//
// The flags are exact and load-bearing:
//
//	-jpeg        write JPEG rather than the default PDF passthrough
//	-singlefile  no page-number suffix, which is what makes stdout usable
//	-scale-to    rasterise at thumbnail size rather than at full page size and
//	             throwing the pixels away, which on a 1.5 GHz core is most of
//	             the cost of the operation
//	-f/-l N      one page
//	-            stdout. pdftocairo supports this; pdftoppm does NOT -- it
//	             creates a file called "-.jpg" instead.
func (g *Generator) renderPDFPage(cctx context.Context, src string, page int, timeout time.Duration) (image.Image, error) {
	cmd := g.pdf.command(cctx,
		"-jpeg",
		"-singlefile",
		"-scale-to", strconv.Itoa(maxEdge),
		"-f", strconv.Itoa(page),
		"-l", strconv.Itoa(page),
		src,
		"-",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Captured, not inherited: poppler is chatty on damaged files and the
	// warnings belong in the recorded thumb_error, not in the daemon's stderr.
	var stderr limitedBuffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s start: %w", g.pdf.name, err)
	}

	img, _, decodeErr := image.Decode(stdout)
	if waitErr := cmd.Wait(); waitErr != nil {
		return nil, g.pdf.fail(cctx, timeout, src, waitErr, stderr.String())
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("decoding %s output for page %d of %s: %w",
			g.pdf.name, page, src, decodeErr)
	}
	return img, nil
}

// hasInk reports whether enough of a rendered page is dark enough to be marks.
func hasInk(img image.Image) bool {
	return inkCoverage(img) >= inkCoverageThreshold
}

// inkCoverage is the fraction of sampled pixels dark enough to count as ink,
// ignoring a margin around every edge.
func inkCoverage(img image.Image) float64 {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return 0
	}

	marginX := int(float64(width) * inkBorderMarginFraction)
	marginY := int(float64(height) * inkBorderMarginFraction)
	minX, maxX := bounds.Min.X+marginX, bounds.Max.X-marginX
	minY, maxY := bounds.Min.Y+marginY, bounds.Max.Y-marginY
	if minX >= maxX || minY >= maxY {
		// Too small for a margin to make sense; use the whole thing rather
		// than reporting no coverage on a legitimate tiny page.
		minX, maxX, minY, maxY = bounds.Min.X, bounds.Max.X, bounds.Min.Y, bounds.Max.Y
	}

	stride := strideFor((maxX-minX)*(maxY-minY), inkSampleTarget)

	ceiling := inkCeiling

	sampled, inked := 0, 0
	for y := minY; y < maxY; y += stride {
		for x := minX; x < maxX; x += stride {
			r, g, b, _ := img.At(x, y).RGBA()
			// Rec. 601 luma, in integer arithmetic: this runs over tens of
			// thousands of pixels on a 1.5 GHz core.
			luma := (299*r + 587*g + 114*b) / 1000
			sampled++
			if luma < ceiling {
				inked++
			}
		}
	}

	if sampled == 0 {
		return 0
	}
	return float64(inked) / float64(sampled)
}

// strideFor picks a sampling step that keeps the number of samples near a
// target, so the cost of the coverage test does not grow with the page size.
func strideFor(pixels, target int) int {
	if pixels <= target || target <= 0 {
		return 1
	}
	stride := 1
	for (pixels/(stride*stride)) > target && stride < 64 {
		stride++
	}
	return stride
}
