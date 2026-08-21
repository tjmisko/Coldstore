package epub

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name string
	body []byte
}

func build(t *testing.T, entries ...entry) string {
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

func container(opfPath string) []byte {
	return []byte(`<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="` + opfPath + `" media-type="application/oebps-package+xml"/></rootfiles>
</container>`)
}

const sampleOPF = `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>The Pocket Guide</dc:title>
  </metadata>
  <manifest>
    <item id="c" href="cover.png" media-type="image/png" properties="cover-image"/>
    <item id="ch1" href="text/ch1.xhtml" media-type="application/xhtml+xml"/>
    <item id="ch2" href="text/ch2.xhtml" media-type="application/xhtml+xml"/>
    <item id="style" href="css/book.css" media-type="text/css"/>
  </manifest>
  <spine>
    <itemref idref="ch1"/>
    <itemref idref="ch2"/>
  </spine>
  <guide><reference type="text" href="text/ch1.xhtml" title="Chapter One"/></guide>
</package>`

func sampleBook(t *testing.T) *Book {
	t.Helper()
	src := build(t,
		entry{"META-INF/container.xml", container("OEBPS/content.opf")},
		entry{"OEBPS/content.opf", []byte(sampleOPF)},
		entry{"OEBPS/cover.png", []byte("not really a png, this package does not decode")},
		entry{"OEBPS/text/ch1.xhtml", []byte(`<html><body><p>One</p></body></html>`)},
		entry{"OEBPS/text/ch2.xhtml", []byte(`<html><body><p>Two</p></body></html>`)},
		entry{"OEBPS/css/book.css", []byte(`body { color: #eee; }`)},
	)
	book, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	return book
}

func TestOpenShouldReadTheTitleWhenTheBookDeclaresOne(t *testing.T) {
	if got := sampleBook(t).Title(); got != "The Pocket Guide" {
		t.Errorf("Title = %q, want The Pocket Guide", got)
	}
}

func TestSpineShouldReturnTheReadingOrderWhenTheBookHasOne(t *testing.T) {
	sections := sampleBook(t).Spine()

	if len(sections) != 2 {
		t.Fatalf("got %d sections, want 2", len(sections))
	}
	// Resolved against the OPF's directory, not the zip root.
	if sections[0].Path != "OEBPS/text/ch1.xhtml" {
		t.Errorf("first section = %q, want OEBPS/text/ch1.xhtml", sections[0].Path)
	}
	if sections[1].Path != "OEBPS/text/ch2.xhtml" {
		t.Errorf("second section = %q", sections[1].Path)
	}
}

func TestSpineShouldCarryTheGuideLabelWhenOneIsDeclared(t *testing.T) {
	sections := sampleBook(t).Spine()

	if sections[0].Label != "Chapter One" {
		t.Errorf("first label = %q, want Chapter One", sections[0].Label)
	}
}

func TestSpineShouldSkipASectionWhenItIsNotInTheArchive(t *testing.T) {
	src := build(t,
		entry{"META-INF/container.xml", container("OEBPS/content.opf")},
		entry{"OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package version="3.0">
  <manifest>
    <item id="ch1" href="text/ch1.xhtml" media-type="application/xhtml+xml"/>
    <item id="ghost" href="text/missing.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine><itemref idref="ch1"/><itemref idref="ghost"/></spine>
</package>`)},
		entry{"OEBPS/text/ch1.xhtml", []byte(`<html><body><p>One</p></body></html>`)},
	)
	book, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()

	// A section the preview would 404 on must not be offered in the first
	// place.
	if got := book.Spine(); len(got) != 1 {
		t.Errorf("got %d sections, want 1: %+v", len(got), got)
	}
}

func TestResourceShouldReturnTheBytesWhenTheEntryExists(t *testing.T) {
	raw, err := sampleBook(t).Resource("OEBPS/css/book.css", MaxResourceBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "color") {
		t.Errorf("body = %q", string(raw))
	}
}

func TestResourceShouldFailWhenTheEntryDoesNotExist(t *testing.T) {
	_, err := sampleBook(t).Resource("OEBPS/nope.css", MaxResourceBytes)
	if !errors.Is(err, ErrNoSuchEntry) {
		t.Errorf("error = %v, want ErrNoSuchEntry", err)
	}
}

func TestResourceShouldRefuseWhenTheNameEscapesTheArchiveRoot(t *testing.T) {
	// A preview endpoint takes this name straight off the URL, so Resource
	// validates it again rather than trusting its caller.
	book := sampleBook(t)
	for _, name := range []string{
		"../../../etc/passwd",
		"OEBPS/../../etc/passwd",
		"/etc/passwd",
		`..\..\windows\win.ini`,
	} {
		if _, err := book.Resource(name, MaxResourceBytes); err == nil {
			t.Errorf("Resource(%q) succeeded; it must refuse", name)
		}
	}
}

func TestResourceShouldRefuseWhenTheArchiveItselfContainsAnEscapingEntry(t *testing.T) {
	// The non-vacuous version of the traversal test: the entry really is in
	// the archive under an escaping name, so the lookup would find it. Only
	// the explicit check stands between that name and a caller who asked for
	// it by URL. Zip files with "../" entries are a real thing.
	src := build(t,
		entry{"META-INF/container.xml", container("OEBPS/content.opf")},
		entry{"OEBPS/content.opf", []byte(sampleOPF)},
		entry{"../escape.css", []byte("ESCAPED")},
	)
	book, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()

	body, err := book.Resource("../escape.css", MaxResourceBytes)
	if err == nil {
		t.Fatalf("read an escaping entry: %q", string(body))
	}
	if strings.Contains(err.Error(), "no such entry") {
		t.Fatal("the entry was missing, so the guard was not what refused it; the fixture is wrong")
	}
}

func TestResourceShouldRefuseWhenTheEntryDeclaresMoreThanTheCap(t *testing.T) {
	// The declared size is a claim by the file. Rejecting an honest oversized
	// entry costs nothing; the read is capped again anyway because a dishonest
	// one exists.
	src := build(t,
		entry{"META-INF/container.xml", container("OEBPS/content.opf")},
		entry{"OEBPS/content.opf", []byte(sampleOPF)},
		entry{"OEBPS/big.png", bytes.Repeat([]byte{0}, 4096)},
	)
	book, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()

	if _, err := book.Resource("OEBPS/big.png", 1024); err == nil {
		t.Error("an entry over the cap must be refused, not truncated")
	}
}

func TestOpenShouldFailWhenTheArchiveHasNoContainer(t *testing.T) {
	src := build(t, entry{"random.txt", []byte("not a book")})

	if _, err := Open(src); err == nil {
		t.Error("an archive with no container.xml is not an epub")
	}
}

func TestOpenShouldFailWhenTheFileIsNotAZip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "book.epub")
	if err := os.WriteFile(src, []byte("this is not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(src); err == nil {
		t.Error("a corrupt archive should fail, not hang or panic")
	}
}

func TestResolvePathShouldRefuseWhenTheHrefEscapesTheRoot(t *testing.T) {
	// An entry name is attacker-controlled in two senses: the archive arrived
	// over Syncthing, and the preview endpoint takes one straight off the URL.
	// "../../etc/passwd" inside a zip is a real thing real archives contain.
	escapes := []struct{ base, href string }{
		{"OEBPS", "../../../etc/passwd"},
		{"", "../outside.png"},
		{"OEBPS", "a/../../../outside.png"},
		{"OEBPS", "/etc/passwd"},
		{"OEBPS", "https://example.invalid/cover.png"},
		{"OEBPS", ""},
		{"OEBPS", "#only-a-fragment"},
		{"", ".."},
		{"", "."},
		// A backslash is a separator on the platform many of these archives
		// were built on, and Go's path package does not treat it as one.
		{"", `..\..\windows\win.ini`},
		{"", "cover\x00.png"},
	}
	for _, tt := range escapes {
		if got, err := ResolvePath(tt.base, tt.href); err == nil {
			t.Errorf("ResolvePath(%q, %q) = %q, want an error", tt.base, tt.href, got)
		}
	}
}

func TestResolvePathShouldResolveRelativeToTheBaseWhenTheHrefIsOrdinary(t *testing.T) {
	tests := []struct{ base, href, want string }{
		{"OEBPS", "cover.png", "OEBPS/cover.png"},
		{"OEBPS", "images/cover.png", "OEBPS/images/cover.png"},
		{"OEBPS/pages", "../art/cover.png", "OEBPS/art/cover.png"},
		{"", "content.opf", "content.opf"},
		{"OEBPS", "cover.png#page1", "OEBPS/cover.png"},
		{"OEBPS", "cover.png?v=2", "OEBPS/cover.png"},
		{"OEBPS", "My%20Cover.png", "OEBPS/My Cover.png"},
	}
	for _, tt := range tests {
		got, err := ResolvePath(tt.base, tt.href)
		if err != nil {
			t.Errorf("ResolvePath(%q, %q): %v", tt.base, tt.href, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ResolvePath(%q, %q) = %q, want %q", tt.base, tt.href, got, tt.want)
		}
	}
}

func TestMediaTypeOfShouldNameTheTypeWhenTheExtensionIsOneABookUses(t *testing.T) {
	tests := map[string]string{
		"a/b.xhtml": "text/html; charset=utf-8",
		"a/b.html":  "text/html; charset=utf-8",
		"a/b.css":   "text/css; charset=utf-8",
		"a/b.png":   "image/png",
		"a/b.JPG":   "image/jpeg",
		"a/b.woff2": "font/woff2",
		"a/b.svg":   "image/svg+xml",
	}
	for name, want := range tests {
		if got := MediaTypeOf(name); got != want {
			t.Errorf("MediaTypeOf(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestMediaTypeOfShouldReturnNothingWhenTheExtensionIsNotOneABookUses(t *testing.T) {
	// A preview endpoint that will serve an arbitrary type out of a file
	// synced from a phone is a file-hosting hole. An unknown extension has to
	// come back empty so the handler can refuse it.
	for _, name := range []string{"payload.exe", "script.js", "run.sh", "noextension", "a.wasm"} {
		if got := MediaTypeOf(name); got != "" {
			t.Errorf("MediaTypeOf(%q) = %q, want empty", name, got)
		}
	}
}

func TestMediaTypeOfShouldServeMarkupAsHTMLWhenItIsAChapter(t *testing.T) {
	// text/html, not application/xhtml+xml: a strict XHTML parse shows an
	// error page for one malformed entity in a book every reader displays
	// fine. The iframe is sandboxed to an opaque origin with no scripts.
	if got := MediaTypeOf("OEBPS/ch1.xhtml"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("MediaTypeOf(chapter) = %q, want text/html", got)
	}
}
