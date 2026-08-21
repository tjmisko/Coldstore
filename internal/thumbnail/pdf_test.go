package thumbnail

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/filetype"
)

// fakePDFRenderer writes a shell script standing in for pdftocairo, so these
// tests run on a machine -- and in a CI job -- with no poppler installed.
func fakePDFRenderer(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-pdftocairo")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// jpegPage renders a page-shaped JPEG to a file, so a fake renderer can cat it
// to stdout the way pdftocairo writes to "-".
func jpegPage(t *testing.T, fill func(x, y int) color.RGBA) string {
	t.Helper()
	const w, h = 300, 400
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, fill(x, y))
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "page.jpg")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func blankPage(t *testing.T) string {
	return jpegPage(t, func(int, int) color.RGBA { return color.RGBA{255, 255, 255, 255} })
}

func inkedPage(t *testing.T) string {
	return jpegPage(t, func(x, y int) color.RGBA {
		// A band of black across the middle: unmistakably content, and well
		// inside the border margin the coverage test ignores.
		if y > 180 && y < 220 {
			return color.RGBA{0, 0, 0, 255}
		}
		return color.RGBA{255, 255, 255, 255}
	})
}

func seedPDF(t *testing.T, cat *catalog.Catalog, rel string) *catalog.FileRecord {
	t.Helper()
	if _, err := cat.UpsertFile(&catalog.FileRecord{
		FolderLabel:  "Documents",
		RelativePath: rel,
		Filename:     filepath.Base(rel),
		Extension:    ".pdf",
		SizeBytes:    2048,
		FileMtime:    time.Now().UTC(),
		Status:       catalog.StatusSynced,
	}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	f, err := cat.GetFile("Documents", rel)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestGenerateShouldProduceAThumbnailWhenThePDFRendererSucceeds(t *testing.T) {
	gen, cat := setupTestThumb(t)
	gen.SetPDFRenderer(fakePDFRenderer(t, "cat "+inkedPage(t)), 5*time.Second)

	file := seedPDF(t, cat, "report.pdf")

	if err := gen.Generate(file, filepath.Join(t.TempDir(), "report.pdf")); err != nil {
		t.Fatalf("generating: %v", err)
	}

	if _, err := os.Stat(gen.ThumbPath("Documents", "report.pdf")); err != nil {
		t.Errorf("no thumbnail was written: %v", err)
	}
	after, err := cat.GetFile("Documents", "report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if after.ThumbnailPath == "" {
		t.Error("thumbnail_path was not recorded")
	}
}

func TestGenerateShouldRenderPageOneToStdoutWhenThePDFIsThumbnailed(t *testing.T) {
	// pdftoppm cannot write to stdout -- given "-" it creates a file called
	// "-.jpg". The flags are the whole reason this uses pdftocairo, so assert
	// on them.
	gen, cat := setupTestThumb(t)
	argsFile := filepath.Join(t.TempDir(), "args")
	gen.SetPDFRenderer(
		fakePDFRenderer(t, `echo "$@" > `+argsFile+"\ncat "+inkedPage(t)),
		5*time.Second,
	)

	file := seedPDF(t, cat, "flags.pdf")
	if err := gen.Generate(file, filepath.Join(t.TempDir(), "flags.pdf")); err != nil {
		t.Fatalf("generating: %v", err)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := string(raw)
	for _, want := range []string{"-jpeg", "-singlefile", "-scale-to 400", "-f 1", "-l 1"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q are missing %q", strings.TrimSpace(args), want)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(args), " -") {
		t.Errorf("args %q do not end in the stdout marker", strings.TrimSpace(args))
	}
}

func TestGenerateShouldFailFastWhenThePDFRendererExceedsItsTimeout(t *testing.T) {
	gen, cat := setupTestThumb(t)
	gen.SetPDFRenderer(fakePDFRenderer(t, "sleep 30"), 150*time.Millisecond)

	file := seedPDF(t, cat, "hung.pdf")

	start := time.Now()
	err := gen.Generate(file, filepath.Join(t.TempDir(), "hung.pdf"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a hung renderer should produce an error, not block forever")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want it to name the timeout", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("generation took %s; the per-invocation timeout did not fire", elapsed)
	}
}

func TestGenerateShouldShareOneDeadlineWhenSeveralPagesAreProbed(t *testing.T) {
	// Three pages of a pathological PDF must not be able to take three times
	// the budget a single one gets.
	gen, cat := setupTestThumb(t)
	gen.SetPDFRenderer(fakePDFRenderer(t, "sleep 30"), 300*time.Millisecond)

	file := seedPDF(t, cat, "slow.pdf")

	start := time.Now()
	_ = gen.Generate(file, filepath.Join(t.TempDir(), "slow.pdf"))
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("generation took %s; the deadline is being restarted per page", elapsed)
	}
}

func TestGenerateShouldNotRecordAThumbnailPathWhenThePDFRendererFails(t *testing.T) {
	gen, cat := setupTestThumb(t)
	gen.SetPDFRenderer(fakePDFRenderer(t, "echo 'Syntax Error: Couldn'\\''t find trailer' >&2; exit 1"), time.Second)

	file := seedPDF(t, cat, "corrupt.pdf")

	err := gen.Generate(file, filepath.Join(t.TempDir(), "corrupt.pdf"))
	if err == nil {
		t.Fatal("a failing renderer should produce an error")
	}
	if !strings.Contains(err.Error(), "trailer") {
		t.Errorf("error = %v, want it to carry the renderer's diagnostics", err)
	}

	after, err := cat.GetFile("Documents", "corrupt.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if after.ThumbnailPath != "" {
		t.Errorf("thumbnail_path = %q, want empty after a failed generation", after.ThumbnailPath)
	}
	if _, err := os.Stat(gen.ThumbPath("Documents", "corrupt.pdf")); !os.IsNotExist(err) {
		t.Error("a failed generation must not leave a partial thumbnail behind")
	}
}

func TestGenerateShouldSkipToPageTwoWhenPageOneIsBlank(t *testing.T) {
	// H3: a cover sheet is a real thing documents have, and a thumbnail of a
	// blank page is a thumbnail of nothing.
	gen, cat := setupTestThumb(t)
	blank, inked := blankPage(t), inkedPage(t)
	pageLog := filepath.Join(t.TempDir(), "pages")
	gen.SetPDFRenderer(fakePDFRenderer(t, `
page=""
while [ $# -gt 0 ]; do
  if [ "$1" = "-f" ]; then page="$2"; fi
  shift
done
echo "$page" >> `+pageLog+`
if [ "$page" = "1" ]; then cat `+blank+`; else cat `+inked+`; fi
`), 5*time.Second)

	file := seedPDF(t, cat, "cover.pdf")
	if err := gen.Generate(file, filepath.Join(t.TempDir(), "cover.pdf")); err != nil {
		t.Fatalf("generating: %v", err)
	}

	raw, err := os.ReadFile(pageLog)
	if err != nil {
		t.Fatal(err)
	}
	pages := strings.Fields(string(raw))
	if len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Errorf("rendered pages %v, want [1 2]", pages)
	}
}

func TestGenerateShouldStopAtPageOneWhenItAlreadyHasContent(t *testing.T) {
	// The common case, and the one that must not cost three rasterisations on
	// a 1.5 GHz core.
	gen, cat := setupTestThumb(t)
	pageLog := filepath.Join(t.TempDir(), "pages")
	gen.SetPDFRenderer(fakePDFRenderer(t, `echo x >> `+pageLog+"\ncat "+inkedPage(t)), 5*time.Second)

	file := seedPDF(t, cat, "content.pdf")
	if err := gen.Generate(file, filepath.Join(t.TempDir(), "content.pdf")); err != nil {
		t.Fatalf("generating: %v", err)
	}

	raw, err := os.ReadFile(pageLog)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Fields(string(raw))); n != 1 {
		t.Errorf("rendered %d pages, want 1", n)
	}
}

func TestGenerateShouldStopProbingAfterThreePagesWhenEveryPageIsBlank(t *testing.T) {
	gen, cat := setupTestThumb(t)
	pageLog := filepath.Join(t.TempDir(), "pages")
	gen.SetPDFRenderer(fakePDFRenderer(t, `echo x >> `+pageLog+"\ncat "+blankPage(t)), 5*time.Second)

	file := seedPDF(t, cat, "empty.pdf")
	if err := gen.Generate(file, filepath.Join(t.TempDir(), "empty.pdf")); err != nil {
		t.Fatalf("a document of blank pages must still get a thumbnail: %v", err)
	}

	raw, err := os.ReadFile(pageLog)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Fields(string(raw))); n != maxPDFPagesProbed {
		t.Errorf("rendered %d pages, want the %d-page cap", n, maxPDFPagesProbed)
	}
	if _, err := os.Stat(gen.ThumbPath("Documents", "empty.pdf")); err != nil {
		t.Errorf("page 1 was not kept as the fallback: %v", err)
	}
}

func TestGenerateShouldKeepPageOneWhenThereIsNoSecondPage(t *testing.T) {
	// A one-page blank document. Page 2 does not exist, so the probe fails --
	// and that must not fail the file.
	gen, cat := setupTestThumb(t)
	blank := blankPage(t)
	gen.SetPDFRenderer(fakePDFRenderer(t, `
page=""
while [ $# -gt 0 ]; do
  if [ "$1" = "-f" ]; then page="$2"; fi
  shift
done
if [ "$page" = "1" ]; then cat `+blank+`; else echo "Error: Wrong page range" >&2; exit 1; fi
`), 5*time.Second)

	file := seedPDF(t, cat, "onepage.pdf")
	if err := gen.Generate(file, filepath.Join(t.TempDir(), "onepage.pdf")); err != nil {
		t.Fatalf("a one-page blank document must still get a thumbnail: %v", err)
	}
	if _, err := os.Stat(gen.ThumbPath("Documents", "onepage.pdf")); err != nil {
		t.Errorf("no thumbnail was written: %v", err)
	}
}

func TestGenerateShouldReachAPDFWhenItHasNoExtensionButWasSniffed(t *testing.T) {
	gen, cat := setupTestThumb(t)
	gen.SetPDFRenderer(fakePDFRenderer(t, "cat "+inkedPage(t)), 5*time.Second)

	if _, err := cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "Documents", RelativePath: "scan", Filename: "scan",
		SizeBytes: 2048, FileMtime: time.Now().UTC(), Status: catalog.StatusSynced,
	}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	rec, err := cat.GetFile("Documents", "scan")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.SetDetectedType(rec.ID, filetype.TypePDF); err != nil {
		t.Fatal(err)
	}
	rec, err = cat.GetFile("Documents", "scan")
	if err != nil {
		t.Fatal(err)
	}

	if err := gen.Generate(rec, filepath.Join(t.TempDir(), "scan")); err != nil {
		t.Fatalf("generating: %v", err)
	}
	if _, err := os.Stat(gen.ThumbPath("Documents", "scan")); err != nil {
		t.Errorf("no thumbnail was written: %v", err)
	}
}

// minimalPDF is a valid one-page PDF with a black band on it. Kept here rather
// than shared with scripts/seed-demo because a test fixture that depends on
// another program's fixture stops testing the thing it names.
const minimalPDF = "%PDF-1.4\n" +
	"1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n" +
	"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n" +
	"3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>\nendobj\n" +
	"4 0 obj\n<< /Length 44 >>\nstream\n0 0 0 rg\n100 300 400 200 re f\n\nendstream\nendobj\n" +
	"trailer\n<< /Size 5 /Root 1 0 R >>\n%%EOF\n"

// TestGenerateShouldProduceAThumbnailWhenRealPopplerRendersThePDF is the only
// test here that runs the actual binary. Everything else uses a fake so the
// unit suite does not need poppler; this one checks that the flags the fake is
// asserted against are flags the real pdftocairo accepts -- which is the part
// a fake can never tell you.
func TestGenerateShouldProduceAThumbnailWhenRealPopplerRendersThePDF(t *testing.T) {
	if _, err := exec.LookPath("pdftocairo"); err != nil {
		t.Skip("poppler-utils is not installed")
	}

	gen, cat := setupTestThumb(t)
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "real.pdf")
	if err := os.WriteFile(src, []byte(minimalPDF), 0o644); err != nil {
		t.Fatal(err)
	}

	file := seedPDF(t, cat, "real.pdf")
	if err := gen.Generate(file, src); err != nil {
		t.Fatalf("generating with the real renderer: %v", err)
	}

	out := gen.ThumbPath("Documents", "real.pdf")
	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("no thumbnail was written: %v", err)
	}
	defer f.Close()

	img, format, err := image.Decode(f)
	if err != nil {
		t.Fatalf("the thumbnail does not decode: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("format = %q, want jpeg", format)
	}
	if b := img.Bounds(); b.Dx() > maxEdge || b.Dy() > maxEdge {
		t.Errorf("thumbnail is %dx%d, want neither edge over %d", b.Dx(), b.Dy(), maxEdge)
	}
	if !hasInk(img) {
		t.Error("the rendered page has no ink; the black band did not survive the pipeline")
	}
}

func TestInkCoverageShouldReportNothingWhenThePageIsBlank(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 300, 400))
	for y := range 400 {
		for x := range 300 {
			img.Set(x, y, color.RGBA{255, 255, 255, 255})
		}
	}

	if got := inkCoverage(img); got != 0 {
		t.Errorf("coverage = %f, want 0", got)
	}
	if hasInk(img) {
		t.Error("a blank page must not read as having content")
	}
}

func TestInkCoverageShouldIgnoreTheBorderWhenAScanHasADarkRim(t *testing.T) {
	// A scan carries a dark rim from the platen lid. Counting it as content
	// makes every blank page in the document look like it has writing on it.
	img := image.NewRGBA(image.Rect(0, 0, 300, 400))
	for y := range 400 {
		for x := range 300 {
			c := color.RGBA{255, 255, 255, 255}
			if x < 6 || x >= 294 || y < 8 || y >= 392 {
				c = color.RGBA{20, 20, 20, 255}
			}
			img.Set(x, y, c)
		}
	}

	if hasInk(img) {
		t.Errorf("a rim-only page read as content (coverage %f)", inkCoverage(img))
	}
}

func TestInkCoverageShouldReportContentWhenThePageHasASingleLineOfText(t *testing.T) {
	// The threshold has to be low enough for one line of small type on a full
	// page, or the heuristic skips past real content.
	img := image.NewRGBA(image.Rect(0, 0, 300, 400))
	for y := range 400 {
		for x := range 300 {
			img.Set(x, y, color.RGBA{255, 255, 255, 255})
		}
	}
	// ~200x8 px of marks in a 300x400 page: 1.3% of the area, which is what
	// one line of body text looks like.
	for y := 100; y < 108; y++ {
		for x := 50; x < 250; x++ {
			img.Set(x, y, color.RGBA{0, 0, 0, 255})
		}
	}

	if !hasInk(img) {
		t.Errorf("one line of text read as blank (coverage %f)", inkCoverage(img))
	}
}

func TestInkCoverageShouldReportContentWhenThePageIsADarkCover(t *testing.T) {
	// A dark cover is content, not a blank page. Coverage says so; a
	// deviation test would not.
	img := image.NewRGBA(image.Rect(0, 0, 300, 400))
	for y := range 400 {
		for x := range 300 {
			img.Set(x, y, color.RGBA{18, 22, 40, 255})
		}
	}

	if !hasInk(img) {
		t.Errorf("a dark cover read as blank (coverage %f)", inkCoverage(img))
	}
}

func TestInkCoverageShouldNotReportContentWhenThePageIsAnEvenOffWhite(t *testing.T) {
	// Scanned paper. Even, slightly off-white, and carrying no marks.
	img := image.NewRGBA(image.Rect(0, 0, 300, 400))
	for y := range 400 {
		for x := range 300 {
			img.Set(x, y, color.RGBA{246, 244, 238, 255})
		}
	}

	if hasInk(img) {
		t.Errorf("blank scanned paper read as content (coverage %f)", inkCoverage(img))
	}
}

func TestStrideForShouldKeepTheSampleCountBoundedWhenThePageIsLarge(t *testing.T) {
	// The coverage test must not get more expensive as pages get bigger.
	for _, pixels := range []int{1_000, 100_000, 4_000_000, 40_000_000} {
		stride := strideFor(pixels, inkSampleTarget)
		if stride < 1 {
			t.Fatalf("stride = %d for %d pixels", stride, pixels)
		}
		if samples := pixels / (stride * stride); samples > inkSampleTarget*2 {
			t.Errorf("%d pixels at stride %d gives %d samples, want near %d",
				pixels, stride, samples, inkSampleTarget)
		}
	}
}

func TestLimitedBufferShouldCapWhatItKeepsWhenTheToolIsChatty(t *testing.T) {
	// The captured text ends up in files.thumb_error, which the UI renders. A
	// renderer that prints a warning per malformed object in a 200 MB PDF must
	// not put megabytes of it into the catalog.
	var b limitedBuffer
	n, err := b.Write(bytes.Repeat([]byte("x"), maxCapturedStderr*3))
	if err != nil {
		t.Fatal(err)
	}
	if n != maxCapturedStderr*3 {
		t.Errorf("Write reported %d, want %d: a short write kills the tool with SIGPIPE",
			n, maxCapturedStderr*3)
	}
	if len(b.String()) > maxCapturedStderr+len(" [truncated]") {
		t.Errorf("kept %d bytes, want at most %d", len(b.String()), maxCapturedStderr)
	}
	if !strings.HasSuffix(b.String(), "[truncated]") {
		t.Error("a capped capture must say it was capped")
	}
}
