package thumbnail

import (
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/filetype"
)

// epubEntry is one member of a test archive.
type epubEntry struct {
	name string
	body []byte
}

// buildEPUB writes a zip with the given entries plus the OCF mimetype member,
// and returns its path.
func buildEPUB(t *testing.T, entries ...epubEntry) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	w, err := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("application/epub+zip")); err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "book.epub")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// containerXML points at an OPF in OEBPS/, so every test here exercises the
// case that actually breaks implementations: hrefs resolve relative to the
// OPF's own directory, not to the zip root.
func containerXML(opfPath string) []byte {
	return []byte(`<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="` + opfPath + `" media-type="application/oebps-package+xml"/></rootfiles>
</container>`)
}

// coloredPNG makes a distinguishable cover, so a test can prove WHICH image
// was picked and not merely that some image was.
func coloredPNG(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 60, 90))
	for y := range 90 {
		for x := range 60 {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func coloredGIF(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewPaletted(image.Rect(0, 0, 60, 90), color.Palette{color.White, c})
	for y := range 90 {
		for x := range 60 {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func seedEPUB(t *testing.T, cat *catalog.Catalog, rel string) *catalog.FileRecord {
	t.Helper()
	if _, err := cat.UpsertFile(&catalog.FileRecord{
		FolderLabel:  "Documents",
		RelativePath: rel,
		Filename:     filepath.Base(rel),
		Extension:    ".epub",
		SizeBytes:    4096,
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

// thumbnailAverage reads back the generated thumbnail and reports its mean
// colour, which is how these tests assert on WHICH image was extracted.
func thumbnailAverage(t *testing.T, path string) color.RGBA {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("no thumbnail was written: %v", err)
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatalf("the thumbnail does not decode: %v", err)
	}
	b := img.Bounds()
	var rs, gs, bs, n uint64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			rs += uint64(r >> 8)
			gs += uint64(g >> 8)
			bs += uint64(bl >> 8)
			n++
		}
	}
	if n == 0 {
		t.Fatal("the thumbnail has no pixels")
	}
	return color.RGBA{uint8(rs / n), uint8(gs / n), uint8(bs / n), 255}
}

func nearlyEqual(a, b color.RGBA) bool {
	diff := func(x, y uint8) int {
		if x > y {
			return int(x - y)
		}
		return int(y - x)
	}
	// JPEG at q80 through a Lanczos resize does not preserve a colour exactly.
	return diff(a.R, b.R) < 24 && diff(a.G, b.G) < 24 && diff(a.B, b.B) < 24
}

// generateEPUBFor runs the generator over one built archive and returns the
// thumbnail path.
func generateEPUBFor(t *testing.T, src string) (*Generator, string) {
	t.Helper()
	gen, cat := setupTestThumb(t)
	file := seedEPUB(t, cat, "book.epub")
	if err := gen.Generate(file, src); err != nil {
		t.Fatalf("generating: %v", err)
	}
	return gen, gen.ThumbPath("Documents", "book.epub")
}

var (
	red  = color.RGBA{220, 40, 40, 255}
	blue = color.RGBA{40, 60, 220, 255}
)

func TestGenerateShouldExtractTheCoverWhenEPUB3DeclaresItInTheManifest(t *testing.T) {
	src := buildEPUB(t,
		epubEntry{"META-INF/container.xml", containerXML("OEBPS/content.opf")},
		epubEntry{"OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <manifest>
    <item id="c" href="images/front.png" media-type="image/png" properties="cover-image"/>
  </manifest>
  <spine/>
</package>`)},
		epubEntry{"OEBPS/images/front.png", coloredPNG(t, red)},
	)

	_, thumb := generateEPUBFor(t, src)

	if got := thumbnailAverage(t, thumb); !nearlyEqual(got, red) {
		t.Errorf("thumbnail averages %v, want the cover's %v", got, red)
	}
}

func TestGenerateShouldResolveTheCoverRelativeToTheOPFWhenItIsInASubdirectory(t *testing.T) {
	// The bug every naive implementation ships. "images/front.png" declared by
	// an OPF at OEBPS/content.opf is OEBPS/images/front.png, not
	// images/front.png. The decoy at the zip root is the wrong answer.
	src := buildEPUB(t,
		epubEntry{"META-INF/container.xml", containerXML("OEBPS/content.opf")},
		epubEntry{"OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package version="3.0">
  <manifest><item id="c" href="images/front.png" media-type="image/png" properties="cover-image"/></manifest>
  <spine/>
</package>`)},
		epubEntry{"OEBPS/images/front.png", coloredPNG(t, red)},
		epubEntry{"images/front.png", coloredPNG(t, blue)},
	)

	_, thumb := generateEPUBFor(t, src)

	got := thumbnailAverage(t, thumb)
	if nearlyEqual(got, blue) {
		t.Fatal("the href was resolved against the zip root instead of the OPF's directory")
	}
	if !nearlyEqual(got, red) {
		t.Errorf("thumbnail averages %v, want the cover's %v", got, red)
	}
}

func TestGenerateShouldExtractTheCoverWhenEPUB2NamesItByManifestID(t *testing.T) {
	src := buildEPUB(t,
		epubEntry{"META-INF/container.xml", containerXML("OEBPS/content.opf")},
		epubEntry{"OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package version="2.0">
  <metadata><meta name="cover" content="the-cover"/></metadata>
  <manifest>
    <item id="the-cover" href="art/jacket.png" media-type="image/png"/>
    <item id="ch1" href="ch1.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine><itemref idref="ch1"/></spine>
</package>`)},
		epubEntry{"OEBPS/art/jacket.png", coloredPNG(t, red)},
	)

	_, thumb := generateEPUBFor(t, src)

	if got := thumbnailAverage(t, thumb); !nearlyEqual(got, red) {
		t.Errorf("thumbnail averages %v, want the cover's %v", got, red)
	}
}

func TestGenerateShouldExtractTheCoverWhenTheMetaNamesAFileInsteadOfAnID(t *testing.T) {
	// Out of spec, and common enough in the wild to be worth handling.
	src := buildEPUB(t,
		epubEntry{"META-INF/container.xml", containerXML("OEBPS/content.opf")},
		epubEntry{"OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package version="2.0">
  <metadata><meta name="cover" content="art/jacket.png"/></metadata>
  <manifest><item id="ch1" href="ch1.xhtml" media-type="application/xhtml+xml"/></manifest>
  <spine/>
</package>`)},
		epubEntry{"OEBPS/art/jacket.png", coloredPNG(t, red)},
	)

	_, thumb := generateEPUBFor(t, src)

	if got := thumbnailAverage(t, thumb); !nearlyEqual(got, red) {
		t.Errorf("thumbnail averages %v, want the cover's %v", got, red)
	}
}

func TestGenerateShouldExtractTheCoverWhenTheManifestIDIsConventional(t *testing.T) {
	src := buildEPUB(t,
		epubEntry{"META-INF/container.xml", containerXML("OEBPS/content.opf")},
		epubEntry{"OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package version="2.0">
  <manifest><item id="cover-image" href="cover.png" media-type="image/png"/></manifest>
  <spine/>
</package>`)},
		epubEntry{"OEBPS/cover.png", coloredPNG(t, red)},
	)

	_, thumb := generateEPUBFor(t, src)

	if got := thumbnailAverage(t, thumb); !nearlyEqual(got, red) {
		t.Errorf("thumbnail averages %v, want the cover's %v", got, red)
	}
}

func TestGenerateShouldFollowTheGuideWhenTheCoverIsAPageWithAnImageOnIt(t *testing.T) {
	src := buildEPUB(t,
		epubEntry{"META-INF/container.xml", containerXML("OEBPS/content.opf")},
		epubEntry{"OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package version="2.0">
  <manifest>
    <item id="coverpage" href="pages/cover.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine/>
  <guide><reference type="cover" href="pages/cover.xhtml"/></guide>
</package>`)},
		epubEntry{"OEBPS/pages/cover.xhtml", []byte(
			`<html><body><div><img src="../art/front.png" alt="cover"/></div></body></html>`)},
		epubEntry{"OEBPS/art/front.png", coloredPNG(t, red)},
	)

	_, thumb := generateEPUBFor(t, src)

	if got := thumbnailAverage(t, thumb); !nearlyEqual(got, red) {
		t.Errorf("thumbnail averages %v, want the cover's %v", got, red)
	}
}

func TestGenerateShouldWalkTheSpineWhenNothingDeclaresACover(t *testing.T) {
	src := buildEPUB(t,
		epubEntry{"META-INF/container.xml", containerXML("OEBPS/content.opf")},
		epubEntry{"OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package version="2.0">
  <manifest>
    <item id="ch1" href="ch1.xhtml" media-type="application/xhtml+xml"/>
    <item id="ch2" href="ch2.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine><itemref idref="ch1"/><itemref idref="ch2"/></spine>
</package>`)},
		epubEntry{"OEBPS/ch1.xhtml", []byte(`<html><body><p>No pictures here.</p></body></html>`)},
		epubEntry{"OEBPS/ch2.xhtml", []byte(`<html><body><img src="plate.png"/></body></html>`)},
		epubEntry{"OEBPS/plate.png", coloredPNG(t, red)},
	)

	_, thumb := generateEPUBFor(t, src)

	if got := thumbnailAverage(t, thumb); !nearlyEqual(got, red) {
		t.Errorf("thumbnail averages %v, want the plate's %v", got, red)
	}
}

func TestGenerateShouldFindTheCoverWhenItIsWrappedInSVG(t *testing.T) {
	// EPUB 2's most common cover shape: an XHTML page holding an <svg> whose
	// <image xlink:href> is the real bitmap.
	src := buildEPUB(t,
		epubEntry{"META-INF/container.xml", containerXML("OEBPS/content.opf")},
		epubEntry{"OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package version="2.0">
  <manifest><item id="cv" href="cover.xhtml" media-type="application/xhtml+xml"/></manifest>
  <spine><itemref idref="cv"/></spine>
</package>`)},
		epubEntry{"OEBPS/cover.xhtml", []byte(
			`<html><body><svg xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 60 90">` +
				`<image width="60" height="90" xlink:href="front.png"/></svg></body></html>`)},
		epubEntry{"OEBPS/front.png", coloredPNG(t, red)},
	)

	_, thumb := generateEPUBFor(t, src)

	if got := thumbnailAverage(t, thumb); !nearlyEqual(got, red) {
		t.Errorf("thumbnail averages %v, want the cover's %v", got, red)
	}
}

func TestGenerateShouldDecodeTheCoverWhenItIsAGIF(t *testing.T) {
	// image/gif was not registered anywhere in the package before this, and
	// some older covers are GIF.
	src := buildEPUB(t,
		epubEntry{"META-INF/container.xml", containerXML("OEBPS/content.opf")},
		epubEntry{"OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package version="3.0">
  <manifest><item id="c" href="cover.gif" media-type="image/gif" properties="cover-image"/></manifest>
  <spine/>
</package>`)},
		epubEntry{"OEBPS/cover.gif", coloredGIF(t, red)},
	)

	_, thumb := generateEPUBFor(t, src)

	if got := thumbnailAverage(t, thumb); !nearlyEqual(got, red) {
		t.Errorf("thumbnail averages %v, want the cover's %v", got, red)
	}
}

func TestGenerateShouldDecodeTheCoverWhenItsHrefIsPercentEncoded(t *testing.T) {
	src := buildEPUB(t,
		epubEntry{"META-INF/container.xml", containerXML("OEBPS/content.opf")},
		epubEntry{"OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package version="3.0">
  <manifest><item id="c" href="My%20Cover.png" media-type="image/png" properties="cover-image"/></manifest>
  <spine/>
</package>`)},
		epubEntry{"OEBPS/My Cover.png", coloredPNG(t, red)},
	)

	_, thumb := generateEPUBFor(t, src)

	if got := thumbnailAverage(t, thumb); !nearlyEqual(got, red) {
		t.Errorf("thumbnail averages %v, want the cover's %v", got, red)
	}
}

func TestGenerateShouldFailCleanlyWhenTheCoverIsSVG(t *testing.T) {
	// A clean thumb_error, not a rasteriser dependency.
	gen, cat := setupTestThumb(t)
	src := buildEPUB(t,
		epubEntry{"META-INF/container.xml", containerXML("OEBPS/content.opf")},
		epubEntry{"OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package version="3.0">
  <manifest><item id="c" href="cover.svg" media-type="image/svg+xml" properties="cover-image"/></manifest>
  <spine/>
</package>`)},
		epubEntry{"OEBPS/cover.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`)},
	)

	file := seedEPUB(t, cat, "book.epub")
	err := gen.Generate(file, src)

	if err == nil {
		t.Fatal("an SVG cover should be a clean failure, not a thumbnail")
	}
	if !strings.Contains(err.Error(), "SVG") {
		t.Errorf("error = %v, want it to name SVG", err)
	}
}

func TestGenerateShouldFailWhenTheArchiveDeclaresNoCover(t *testing.T) {
	gen, cat := setupTestThumb(t)
	src := buildEPUB(t,
		epubEntry{"META-INF/container.xml", containerXML("OEBPS/content.opf")},
		epubEntry{"OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package version="2.0">
  <manifest><item id="ch1" href="ch1.xhtml" media-type="application/xhtml+xml"/></manifest>
  <spine><itemref idref="ch1"/></spine>
</package>`)},
		epubEntry{"OEBPS/ch1.xhtml", []byte(`<html><body><p>Text only.</p></body></html>`)},
	)

	file := seedEPUB(t, cat, "book.epub")
	if err := gen.Generate(file, src); err == nil {
		t.Fatal("a book with no images should not produce a thumbnail")
	}

	after, err := cat.GetFile("Documents", "book.epub")
	if err != nil {
		t.Fatal(err)
	}
	if after.ThumbnailPath != "" {
		t.Error("a failed extraction must not record a thumbnail path")
	}
}

func TestGenerateShouldFailWhenTheArchiveHasNoContainer(t *testing.T) {
	gen, cat := setupTestThumb(t)
	src := buildEPUB(t, epubEntry{"random.txt", []byte("not a book")})

	file := seedEPUB(t, cat, "book.epub")
	if err := gen.Generate(file, src); err == nil {
		t.Fatal("an archive with no container.xml is not an epub")
	}
}

func TestGenerateShouldFailWhenTheFileIsNotAZip(t *testing.T) {
	gen, cat := setupTestThumb(t)
	src := filepath.Join(t.TempDir(), "book.epub")
	if err := os.WriteFile(src, []byte("this is not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}

	file := seedEPUB(t, cat, "book.epub")
	if err := gen.Generate(file, src); err == nil {
		t.Fatal("a corrupt archive should fail, not hang or panic")
	}
}

func TestGenerateShouldReachAnEPUBWhenItHasNoExtensionButWasSniffed(t *testing.T) {
	gen, cat := setupTestThumb(t)
	src := buildEPUB(t,
		epubEntry{"META-INF/container.xml", containerXML("OEBPS/content.opf")},
		epubEntry{"OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package version="3.0">
  <manifest><item id="c" href="cover.png" media-type="image/png" properties="cover-image"/></manifest>
  <spine/>
</package>`)},
		epubEntry{"OEBPS/cover.png", coloredPNG(t, red)},
	)

	if _, err := cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "Documents", RelativePath: "unnamed", Filename: "unnamed",
		SizeBytes: 4096, FileMtime: time.Now().UTC(), Status: catalog.StatusSynced,
	}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	rec, err := cat.GetFile("Documents", "unnamed")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.SetDetectedType(rec.ID, filetype.TypeEPUB); err != nil {
		t.Fatal(err)
	}
	rec, err = cat.GetFile("Documents", "unnamed")
	if err != nil {
		t.Fatal(err)
	}

	if err := gen.Generate(rec, src); err != nil {
		t.Fatalf("generating: %v", err)
	}
	if _, err := os.Stat(gen.ThumbPath("Documents", "unnamed")); err != nil {
		t.Errorf("no thumbnail was written: %v", err)
	}
}

func TestResolveArchivePathShouldRefuseWhenTheHrefEscapesTheRoot(t *testing.T) {
	// A zip entry name is attacker-controlled in the sense that the archive
	// arrived over Syncthing, and "../../etc/passwd" inside a zip is a real
	// thing real archives contain.
	escapes := []struct{ base, href string }{
		{"OEBPS", "../../../etc/passwd"},
		{"", "../outside.png"},
		{"OEBPS", "a/../../../outside.png"},
		{"OEBPS", "/etc/passwd"},
		{"OEBPS", "https://example.invalid/cover.png"},
		{"OEBPS", ""},
		{"OEBPS", "#only-a-fragment"},
	}
	for _, tt := range escapes {
		if got, err := resolveArchivePath(tt.base, tt.href); err == nil {
			t.Errorf("resolveArchivePath(%q, %q) = %q, want an error", tt.base, tt.href, got)
		}
	}
}

func TestResolveArchivePathShouldResolveRelativeToTheBaseWhenTheHrefIsOrdinary(t *testing.T) {
	tests := []struct{ base, href, want string }{
		{"OEBPS", "cover.png", "OEBPS/cover.png"},
		{"OEBPS", "images/cover.png", "OEBPS/images/cover.png"},
		{"OEBPS/pages", "../art/cover.png", "OEBPS/art/cover.png"},
		{".", "content.opf", "content.opf"},
		{"", "content.opf", "content.opf"},
		{"OEBPS", "cover.png#page1", "OEBPS/cover.png"},
		{"OEBPS", "My%20Cover.png", "OEBPS/My Cover.png"},
	}
	for _, tt := range tests {
		got, err := resolveArchivePath(tt.base, tt.href)
		if err != nil {
			t.Errorf("resolveArchivePath(%q, %q): %v", tt.base, tt.href, err)
			continue
		}
		if got != tt.want {
			t.Errorf("resolveArchivePath(%q, %q) = %q, want %q", tt.base, tt.href, got, tt.want)
		}
	}
}

func TestGenerateShouldRefuseWhenTheCoverDeclaresAnAbsurdSize(t *testing.T) {
	// The declared size is a claim by the file. Rejecting an honest 400 MB
	// entry costs nothing; the read is capped again anyway because a
	// dishonest one exists.
	gen, cat := setupTestThumb(t)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	w.Write([]byte("application/epub+zip"))
	w, _ = zw.Create("META-INF/container.xml")
	w.Write(containerXML("OEBPS/content.opf"))
	w, _ = zw.Create("OEBPS/content.opf")
	w.Write([]byte(`<?xml version="1.0"?>
<package version="3.0">
  <manifest><item id="c" href="cover.png" media-type="image/png" properties="cover-image"/></manifest>
  <spine/>
</package>`))
	// A highly compressible entry far larger than the cap: a zip bomb in
	// miniature.
	w, _ = zw.Create("OEBPS/cover.png")
	w.Write(bytes.Repeat([]byte{0}, maxCoverBytes+1024))
	zw.Close()

	src := filepath.Join(t.TempDir(), "bomb.epub")
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	file := seedEPUB(t, cat, "book.epub")
	err := gen.Generate(file, src)
	if err == nil {
		t.Fatal("an oversized cover must be refused, not decoded")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error = %v, want it to name the cap", err)
	}
}

func TestGenerateShouldProduceAThumbnailNoLargerThanTheContractWhenTheCoverIsHuge(t *testing.T) {
	// The 400px / q80 contract comes from saveThumb and must apply to covers
	// exactly as it does to photographs.
	img := image.NewRGBA(image.Rect(0, 0, 1600, 2400))
	for y := range 2400 {
		for x := range 1600 {
			img.Set(x, y, red)
		}
	}
	var cover bytes.Buffer
	if err := jpeg.Encode(&cover, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}

	src := buildEPUB(t,
		epubEntry{"META-INF/container.xml", containerXML("OEBPS/content.opf")},
		epubEntry{"OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package version="3.0">
  <manifest><item id="c" href="cover.jpg" media-type="image/jpeg" properties="cover-image"/></manifest>
  <spine/>
</package>`)},
		epubEntry{"OEBPS/cover.jpg", cover.Bytes()},
	)

	_, thumb := generateEPUBFor(t, src)

	f, err := os.Open(thumb)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	decoded, _, err := image.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if b := decoded.Bounds(); b.Dx() > maxEdge || b.Dy() > maxEdge {
		t.Errorf("thumbnail is %dx%d, want neither edge over %d", b.Dx(), b.Dy(), maxEdge)
	}
}

func TestGenerateShouldExtractTheCoverWhenTheDemoFixtureIsUsed(t *testing.T) {
	// scripts/seed-demo builds a real EPUB, and the browser tier browses it.
	// If the two ever disagree about the format, this is where it shows.
	src := buildEPUB(t,
		epubEntry{"META-INF/container.xml", containerXML("OEBPS/content.opf")},
		epubEntry{"OEBPS/content.opf", []byte(`<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="bookid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Pocket Guide</dc:title>
  </metadata>
  <manifest>
    <item id="cover" href="cover.png" media-type="image/png" properties="cover-image"/>
    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
    <item id="ch1" href="chapter1.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine><itemref idref="ch1"/></spine>
</package>`)},
		epubEntry{"OEBPS/cover.png", coloredPNG(t, red)},
		epubEntry{"OEBPS/nav.xhtml", []byte(`<html><body><nav/></body></html>`)},
		epubEntry{"OEBPS/chapter1.xhtml", []byte(`<html><body><h1>Pocket Guide</h1></body></html>`)},
	)

	_, thumb := generateEPUBFor(t, src)

	if got := thumbnailAverage(t, thumb); !nearlyEqual(got, red) {
		t.Errorf("thumbnail averages %v, want the cover's %v", got, red)
	}
}
