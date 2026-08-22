package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
)

// The Documents folder used to be fifteen files of counting-byte filler with a
// .pdf or .csv name stuck on the end. That was fine while the thumbnail
// generator ignored documents entirely, and became a fixture bug the moment it
// stopped: six "PDFs" that are not PDFs fail three times each, and the
// Documents folder fills up with the thumbnail-failure glyph -- which the
// Playwright suite asserts on, so the suite goes red for a reason that has
// nothing to do with the code under test.
//
// So the documents are real now. One corrupt file survives on purpose: the
// failure path is a thing the UI renders and it has to stay reachable.

// documentWriter picks the fixture writer for one seeded document.
//
// "bookmarks" has no extension at all, which is the case H0b's content sniffing
// exists for: mime.TypeByExtension answers nothing for it, so an
// extension-keyed queue can never reach it.
func documentWriter(name string, size int, blankFirstPage, corrupt bool) func(string) error {
	title := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	switch {
	case corrupt:
		return func(path string) error { return writeCorruptPDF(path, size) }
	case strings.HasSuffix(name, ".pdf"):
		return func(path string) error { return writeMinimalPDF(path, title, size, blankFirstPage) }
	case strings.HasSuffix(name, ".epub"):
		return func(path string) error { return writeMinimalEPUB(path, title, size) }
	default:
		// .txt, .csv, .md and the extension-less bookmark file.
		return func(path string) error { return writeTextDocument(path, title, size) }
	}
}

// writeMinimalPDF writes a valid single-page PDF of approximately size bytes.
//
// It is written by hand rather than with a library because the whole point of
// the fixture is to be the real format: a PDF assembled here is one poppler
// will parse, and if this ever stops being true the thumbnail tests say so.
func writeMinimalPDF(path, title string, size int, blankFirstPage bool) error {
	// The padding is a comment block before the xref table, so it moves only
	// startxref and no object offset. Changing it changes the digit count of
	// startxref, so converge rather than assuming one pass lands exactly.
	pad := 0
	var doc []byte
	for range 6 {
		doc = buildPDF(title, pad, blankFirstPage)
		if len(doc) == size {
			break
		}
		next := pad + size - len(doc)
		if next < 0 {
			// The document is already bigger than the requested size. That is
			// fine; the fixture's sizes are a budget shape, not a contract.
			break
		}
		pad = next
	}
	return os.WriteFile(path, doc, 0o644)
}

func buildPDF(title string, pad int, blankFirstPage bool) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	// A comment of high bytes on line two is the convention that tells tools
	// the file is binary rather than text.
	buf.Write([]byte{'%', 0xE2, 0xE3, 0xCF, 0xD3, '\n'})

	pages := []string{contentStream(title)}
	if blankFirstPage {
		// A cover sheet with nothing on it, so the "first page with content"
		// heuristic has something in the demo to actually work on.
		pages = []string{"", contentStream(title)}
	}

	// Object numbering: 1 catalog, 2 pages, 3 font, then two objects per page
	// (the page and its content stream).
	const firstPageObj = 4
	var offsets []int
	object := func(body string) {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}

	kids := make([]string, len(pages))
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", firstPageObj+i*2)
	}

	object("<< /Type /Catalog /Pages 2 0 R >>")
	object(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>",
		strings.Join(kids, " "), len(pages)))
	object("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	for i, content := range pages {
		object(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "+
			"/Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", firstPageObj+i*2+1))
		object(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	}

	if pad > 0 {
		buf.WriteByte('%')
		buf.Write(bytes.Repeat([]byte{'-'}, pad-2))
		buf.WriteByte('\n')
	}

	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(offsets)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(offsets)+1, xref)

	return buf.Bytes()
}

// contentStream draws a page with enough ink on it that the "first page with
// content" heuristic finds it, and enough words that a human looking at the
// demo can tell one document from another.
func contentStream(title string) string {
	var b strings.Builder
	b.WriteString("0.11 0.13 0.20 rg\n50 640 512 96 re f\n")
	b.WriteString("1 1 1 rg\nBT /F1 26 Tf 72 676 Td (" + pdfEscape(title) + ") Tj ET\n")
	b.WriteString("0.2 0.2 0.2 rg\n")
	for i := range 14 {
		fmt.Fprintf(&b, "BT /F1 12 Tf 72 %d Td (%s) Tj ET\n", 600-i*22,
			pdfEscape(fmt.Sprintf("Section %d -- seeded by scripts/seed-demo.", i+1)))
	}
	b.WriteString("0.11 0.13 0.20 rg\n72 120 200 140 re f\n")
	return b.String()
}

// pdfEscape escapes the three characters that terminate or nest a PDF literal
// string. Without it a document title containing a bracket produces a file
// poppler rejects, which is exactly the class of bug this fixture exists to
// stop shipping.
func pdfEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`)
	return r.Replace(s)
}

// writeMinimalEPUB writes a valid EPUB of size bytes, with a real PNG cover
// declared the EPUB 3 way.
//
// The filler entry's length is converged on rather than computed: a zip's
// final size includes a central directory whose size is not known until the
// archive is closed, so guessing it once lands short.
func writeMinimalEPUB(path, title string, size int) error {
	filler := 0
	var doc []byte
	for range 6 {
		built, err := buildEPUB(title, filler)
		if err != nil {
			return err
		}
		doc = built
		if len(doc) == size {
			break
		}
		next := filler + size - len(doc)
		if next < 0 {
			break
		}
		filler = next
	}
	return os.WriteFile(path, doc, 0o644)
}

func buildEPUB(title string, filler int) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// OCF requires "mimetype" to be the first entry and STORED. It is what
	// makes the archive identifiable from its first bytes rather than only by
	// reading the central directory.
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	if err != nil {
		return nil, err
	}
	if _, err := w.Write([]byte("application/epub+zip")); err != nil {
		return nil, err
	}

	cover, err := coverPNG(title)
	if err != nil {
		return nil, err
	}

	entries := []struct {
		name string
		body []byte
	}{
		{"META-INF/container.xml", []byte(epubContainerXML)},
		// The OPF is deliberately NOT at the zip root: every href inside it
		// resolves relative to this directory, and a cover extractor that
		// resolves against the root instead finds nothing. That is the bug
		// worth having a fixture for.
		{"OEBPS/content.opf", []byte(epubOPF(title))},
		{"OEBPS/cover.png", cover},
		{"OEBPS/nav.xhtml", []byte(epubNav(title))},
		{"OEBPS/chapter1.xhtml", []byte(epubChapter(title))},
	}
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(e.body); err != nil {
			return nil, err
		}
	}

	// Pad with a STORED filler entry, so the folder's byte totals -- and the
	// over-budget state the demo depends on -- do not move now the fixture is
	// a real file. Stored, not deflated, so its compressed length is its
	// uncompressed length and the convergence above is a fixed point.
	if filler > 0 {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: "OEBPS/notes.txt", Method: zip.Store})
		if err != nil {
			return nil, err
		}
		body := bytes.Repeat([]byte("seeded by scripts/seed-demo.\n"), filler/29+1)
		if _, err := w.Write(body[:filler]); err != nil {
			return nil, err
		}
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

const epubContainerXML = `<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>
`

func epubOPF(title string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="bookid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:identifier id="bookid">urn:uuid:coldstore-demo-` + xmlEscape(title) + `</dc:identifier>
    <dc:title>` + xmlEscape(title) + `</dc:title>
    <dc:language>en</dc:language>
  </metadata>
  <manifest>
    <item id="cover" href="cover.png" media-type="image/png" properties="cover-image"/>
    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
    <item id="ch1" href="chapter1.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine>
    <itemref idref="ch1"/>
  </spine>
</package>
`
}

func epubNav(title string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops">
<head><title>` + xmlEscape(title) + `</title></head>
<body><nav epub:type="toc"><ol><li><a href="chapter1.xhtml">Chapter 1</a></li></ol></nav></body>
</html>
`
}

func epubChapter(title string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml">
<head><title>` + xmlEscape(title) + `</title></head>
<body><h1>` + xmlEscape(title) + `</h1>
<p>Seeded by scripts/seed-demo so the document preview has something to render.</p>
</body>
</html>
`
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	// xml.EscapeText would do, but it also escapes newlines and tabs into
	// entities, which makes the fixture unreadable when a human opens it.
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	b.WriteString(r.Replace(s))
	return b.String()
}

// coverPNG draws a small cover so the EPUB extractor has a real image to find
// and decode rather than a placeholder that only looks like one.
func coverPNG(title string) ([]byte, error) {
	const w, h = 200, 300
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// A vertical gradient, plus a band whose position depends on the title, so
	// two different covers are visibly different in the demo.
	seed := 0
	for _, c := range title {
		seed += int(c)
	}
	band := 60 + seed%120
	for y := range h {
		for x := range w {
			shade := uint8(40 + y*120/h)
			c := color.RGBA{R: shade / 2, G: shade / 2, B: shade, A: 255}
			if y >= band && y < band+40 {
				c = color.RGBA{R: 220, G: 200, B: 120, A: 255}
			}
			img.Set(x, y, c)
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeCorruptPDF writes a file that announces itself as a PDF and is not one.
//
// Deliberate. The thumbnail-failure glyph, the thumb_error text and the
// "retry thumbnails" action are all things the UI renders, and they are
// unreachable in the demo unless something in it actually fails.
func writeCorruptPDF(path string, size int) error {
	body := make([]byte, 0, size)
	body = append(body, []byte("%PDF-1.4\n")...)
	for len(body) < size {
		body = append(body, byte(len(body)%251))
	}
	return os.WriteFile(path, body[:size], 0o644)
}

// writeTextDocument writes readable text rather than counting bytes, so the
// plaintext preview has something worth looking at.
func writeTextDocument(path, title string, size int) error {
	var b bytes.Buffer
	b.WriteString(title + "\n")
	b.WriteString(strings.Repeat("=", len(title)) + "\n\n")
	for i := 1; b.Len() < size; i++ {
		fmt.Fprintf(&b, "%3d. Seeded by scripts/seed-demo so the preview has real text.\n", i)
	}
	return os.WriteFile(path, b.Bytes()[:size], 0o644)
}
