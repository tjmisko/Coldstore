package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tjmisko/coldstore/internal/filetype"
)

// The Documents fixture used to be counting-byte filler with a .pdf name on
// it. Nothing caught that, because nothing ever tried to open one. These tests
// are what stops it coming back: a fixture that is not the format it claims is
// a test failure now, not a surprise three commits later when the thumbnail
// generator learns the format.

func TestWriteMinimalPDFShouldProduceAFileThatSniffsAsPDFWhenWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.pdf")
	if err := writeMinimalPDF(path, "Quarterly Report", 15_000, false); err != nil {
		t.Fatal(err)
	}

	got, err := filetype.SniffFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != filetype.TypePDF {
		t.Errorf("detected type = %q, want %q", got, filetype.TypePDF)
	}
}

func TestWriteMinimalPDFShouldHitTheRequestedSizeWhenPadded(t *testing.T) {
	// The Documents folder's total has to stay above its 100 kB budget for the
	// demo to show the over-budget state at all, so the fixture sizes are part
	// of the fixture.
	for _, size := range []int{6_700, 15_000, 25_000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "doc.pdf")
			if err := writeMinimalPDF(path, "Doc", size, false); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() != int64(size) {
				t.Errorf("size = %d, want %d", info.Size(), size)
			}
		})
	}
}

func TestWriteMinimalPDFShouldWriteACrossReferenceTableThatPointsAtEveryObject(t *testing.T) {
	// The xref offsets are the part of a hand-rolled PDF that silently rots:
	// add a byte anywhere above the table and every offset below it is wrong,
	// and the file still looks fine until a renderer opens it.
	path := filepath.Join(t.TempDir(), "doc.pdf")
	if err := writeMinimalPDF(path, "Doc", 9_800, false); err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// "\nxref\n", not "xref\n": the latter also matches the "startxref" near
	// the end of every PDF, which is how this test first failed.
	start := bytes.Index(doc, []byte("\nxref\n"))
	if start < 0 {
		t.Fatal("no xref table")
	}
	lines := strings.Split(string(doc[start+1:]), "\n")
	if len(lines) < 3 {
		t.Fatal("xref table is too short")
	}
	// lines[0] "xref", lines[1] "0 N", lines[2] the free entry, then the
	// in-use entries in object order.
	var count int
	if _, err := fmt.Sscanf(lines[1], "0 %d", &count); err != nil {
		t.Fatalf("parsing the subsection header %q: %v", lines[1], err)
	}
	if count < 2 {
		t.Fatalf("xref declares %d objects", count)
	}

	for i := 1; i < count; i++ {
		entry := lines[2+i]
		off, err := strconv.Atoi(strings.TrimSpace(entry[:10]))
		if err != nil {
			t.Fatalf("object %d: parsing offset %q: %v", i, entry, err)
		}
		want := fmt.Sprintf("%d 0 obj", i)
		if off < 0 || off+len(want) > len(doc) {
			t.Fatalf("object %d: offset %d is outside the file", i, off)
		}
		if got := string(doc[off : off+len(want)]); got != want {
			t.Errorf("object %d: offset %d points at %q, want %q", i, off, got, want)
		}
	}
}

func TestWriteMinimalPDFShouldWriteTwoPagesWhenTheFirstIsBlank(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.pdf")
	if err := writeMinimalPDF(path, "Doc", 18_200, true); err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(doc, []byte("/Count 2")) {
		t.Error("a blank-first-page fixture must declare two pages")
	}
}

func TestPDFEscapeShouldEscapeTheCharactersThatTerminateALiteralString(t *testing.T) {
	// A title with a bracket in it produces a file poppler rejects, which is
	// precisely the class of bug the fixture exists to stop shipping.
	if got, want := pdfEscape("Report (draft) \\ final"), `Report \(draft\) \\ final`; got != want {
		t.Errorf("pdfEscape = %q, want %q", got, want)
	}
}

func TestWriteMinimalEPUBShouldProduceAFileThatSniffsAsEPUBWhenWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "book.epub")
	if err := writeMinimalEPUB(path, "Pocket Guide", 6_700); err != nil {
		t.Fatal(err)
	}

	got, err := filetype.SniffFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != filetype.TypeEPUB {
		t.Errorf("detected type = %q, want %q", got, filetype.TypeEPUB)
	}
}

func TestWriteMinimalEPUBShouldStoreTheMimetypeEntryFirstWhenWritten(t *testing.T) {
	// OCF requires it, and it is what makes the archive identifiable from its
	// leading bytes instead of only by reading the central directory.
	path := filepath.Join(t.TempDir(), "book.epub")
	if err := writeMinimalEPUB(path, "Pocket Guide", 6_700); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	if len(zr.File) == 0 {
		t.Fatal("empty archive")
	}
	first := zr.File[0]
	if first.Name != "mimetype" {
		t.Errorf("first entry = %q, want mimetype", first.Name)
	}
	if first.Method != zip.Store {
		t.Errorf("mimetype is compressed; OCF requires it stored")
	}
	rc, err := first.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "application/epub+zip" {
		t.Errorf("mimetype = %q", string(body))
	}
}

func TestWriteMinimalEPUBShouldPutTheOPFInASubdirectoryWhenWritten(t *testing.T) {
	// Deliberate: hrefs inside the OPF resolve relative to the OPF's own
	// directory, not the zip root. A cover extractor that resolves against the
	// root finds nothing, and a fixture with the OPF at the root would never
	// catch it.
	path := filepath.Join(t.TempDir(), "book.epub")
	if err := writeMinimalEPUB(path, "Pocket Guide", 6_700); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	names := make(map[string]bool, len(zr.File))
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, want := range []string{"META-INF/container.xml", "OEBPS/content.opf", "OEBPS/cover.png"} {
		if !names[want] {
			t.Errorf("missing entry %q", want)
		}
	}
	if names["content.opf"] {
		t.Error("the OPF must not be at the zip root: that is the case the extractor gets wrong")
	}
}

func TestWriteMinimalEPUBShouldCarryACoverThatDecodesWhenExtracted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "book.epub")
	if err := writeMinimalEPUB(path, "Pocket Guide", 6_700); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if f.Name != "OEBPS/cover.png" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		img, err := png.Decode(rc)
		if err != nil {
			t.Fatalf("the cover does not decode: %v", err)
		}
		if img.Bounds().Dx() == 0 || img.Bounds().Dy() == 0 {
			t.Error("the cover has no pixels")
		}
		return
	}
	t.Fatal("no cover entry")
}

func TestWriteMinimalEPUBShouldHitTheRequestedSizeWhenPadded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "book.epub")
	if err := writeMinimalEPUB(path, "Pocket Guide", 6_700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 6_700 {
		t.Errorf("size = %d, want 6700", info.Size())
	}
}

func TestWriteCorruptPDFShouldAnnounceItselfAsAPDFWhenWritten(t *testing.T) {
	// It has to reach the thumbnail queue to fail in it. A file that does not
	// sniff as a PDF is simply skipped, and the failure glyph the UI renders
	// stays unreachable in the demo.
	path := filepath.Join(t.TempDir(), "broken.pdf")
	if err := writeCorruptPDF(path, 22_000); err != nil {
		t.Fatal(err)
	}

	got, err := filetype.SniffFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != filetype.TypePDF {
		t.Errorf("detected type = %q, want %q", got, filetype.TypePDF)
	}

	doc, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(doc, []byte("%%EOF")) {
		t.Error("the corrupt fixture must not be a renderable PDF")
	}
	if len(doc) != 22_000 {
		t.Errorf("size = %d, want 22000", len(doc))
	}
}

func TestWriteTextDocumentShouldSniffAsTextWhenTheFileHasNoExtension(t *testing.T) {
	// The bookmarks fixture: the extension-less case H0b's content sniffing
	// exists for.
	path := filepath.Join(t.TempDir(), "bookmarks")
	if err := writeTextDocument(path, "bookmarks", 5_600); err != nil {
		t.Fatal(err)
	}

	got, err := filetype.SniffFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != filetype.TypeText {
		t.Errorf("detected type = %q, want %q", got, filetype.TypeText)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 5_600 {
		t.Errorf("size = %d, want 5600", info.Size())
	}
}

func TestDocumentWriterShouldPickTheFormatWhenTheNameSaysSo(t *testing.T) {
	tests := []struct {
		name    string
		corrupt bool
		want    string
	}{
		{name: "Report.pdf", want: filetype.TypePDF},
		{name: "Guide.epub", want: filetype.TypeEPUB},
		{name: "Notes.txt", want: filetype.TypeText},
		{name: "Budget.csv", want: filetype.TypeText},
		{name: "bookmarks", want: filetype.TypeText},
		{name: "Broken.pdf", corrupt: true, want: filetype.TypePDF},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), filepath.Base(tt.name))
			write := documentWriter(tt.name, 8_000, false, tt.corrupt)
			if err := write(path); err != nil {
				t.Fatal(err)
			}
			got, err := filetype.SniffFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("detected type = %q, want %q", got, tt.want)
			}
		})
	}
}
