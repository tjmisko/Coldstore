// Package epub reads EPUB archives: enough of the format to find a book's
// cover and to walk its reading order, with no dependency outside the standard
// library.
//
// There is nothing to depend on. An EPUB is a zip with an XML manifest in it,
// and archive/zip and encoding/xml read both. The work is not the parsing, it
// is knowing WHERE things are: a cover can be declared in five different
// places across two versions of the format, and real books use all of them.
//
// One parser, two consumers: the thumbnail generator extracts the cover, and
// the preview API serves the spine and its resources. Two copies of the href
// resolution below would be two chances to get the traversal check wrong.
package epub

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
)

const (
	// MaxOPFBytes caps the package document. A manifest is kilobytes; anything
	// claiming to be megabytes of it is not a book.
	MaxOPFBytes = 2 << 20

	// MaxImageBytes caps a cover or an illustration. Checked against the
	// archive's DECLARED size first and enforced again while reading, because
	// a declared size is a claim by the file, not a fact.
	MaxImageBytes = 24 << 20

	// MaxDocumentBytes caps one chapter.
	MaxDocumentBytes = 2 << 20

	// MaxResourceBytes caps anything else served out of the archive.
	MaxResourceBytes = 24 << 20

	// maxSpineDocsWalked bounds the cover fallback search. If the first ten
	// documents of a book have no image in them, the eleventh is not a cover.
	maxSpineDocsWalked = 10

	// MaxSpineEntries caps how much reading order is reported. A book with
	// more sections than this is one the preview lists the beginning of.
	MaxSpineEntries = 2000
)

// ErrSVGCover is returned for a cover that is SVG. Rasterising SVG would mean
// pulling in a renderer for the one format where the cover is usually a
// wrapper around a bitmap that is also in the archive; a clean error is the
// better trade.
var ErrSVGCover = errors.New("cover is SVG, which needs a rasteriser")

// ErrNoCover is returned when nothing in the archive declares or contains a
// usable cover image.
var ErrNoCover = errors.New("epub declares no cover image")

// ErrNoSuchEntry is returned for a resource that is not in the archive.
var ErrNoSuchEntry = errors.New("no such entry in the archive")

// Book is an opened EPUB. Close it.
type Book struct {
	rc      *zip.ReadCloser
	entries map[string]*zip.File

	// opfPath is the package document's entry name, and opfDir is its
	// directory. Every href in the package document resolves relative to
	// opfDir, NOT to the zip root -- that is the bug every naive
	// implementation ships, and it is invisible in the minority of books that
	// keep the OPF at the top level.
	opfPath string
	opfDir  string

	pkg *opfPackage
}

// Open reads an EPUB's structure. It does not read any content.
func Open(path string) (*Book, error) {
	rc, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("opening epub: %w", err)
	}

	b := &Book{rc: rc, entries: make(map[string]*zip.File, len(rc.File))}
	for _, f := range rc.File {
		b.entries[f.Name] = f
	}

	opfPath, err := b.findOPFPath()
	if err != nil {
		rc.Close()
		return nil, err
	}
	b.opfPath = opfPath
	b.opfDir = path2Dir(opfPath)

	pkg, err := b.readOPF()
	if err != nil {
		rc.Close()
		return nil, err
	}
	b.pkg = pkg
	return b, nil
}

func (b *Book) Close() error { return b.rc.Close() }

// Title is the book's dc:title, or "" if it does not declare one.
func (b *Book) Title() string { return strings.TrimSpace(b.pkg.Metadata.Title) }

func path2Dir(p string) string {
	dir := path.Dir(p)
	if dir == "." {
		return ""
	}
	return dir
}

func (b *Book) findOPFPath() (string, error) {
	f, ok := b.entries["META-INF/container.xml"]
	if !ok {
		return "", errors.New("epub has no META-INF/container.xml")
	}
	raw, err := readEntry(f, MaxOPFBytes)
	if err != nil {
		return "", fmt.Errorf("reading container.xml: %w", err)
	}

	var container struct {
		Rootfiles []struct {
			FullPath string `xml:"full-path,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	if err := xml.Unmarshal(raw, &container); err != nil {
		return "", fmt.Errorf("parsing container.xml: %w", err)
	}
	for _, rf := range container.Rootfiles {
		if rf.FullPath == "" {
			continue
		}
		resolved, err := ResolvePath("", rf.FullPath)
		if err != nil {
			return "", err
		}
		if _, ok := b.entries[resolved]; ok {
			return resolved, nil
		}
	}
	return "", errors.New("epub declares no readable package document")
}

type opfPackage struct {
	Metadata struct {
		Title string `xml:"title"`
		Metas []struct {
			Name    string `xml:"name,attr"`
			Content string `xml:"content,attr"`
		} `xml:"meta"`
	} `xml:"metadata"`
	Manifest struct {
		Items []opfItem `xml:"item"`
	} `xml:"manifest"`
	Spine struct {
		ItemRefs []struct {
			IDRef  string `xml:"idref,attr"`
			Linear string `xml:"linear,attr"`
		} `xml:"itemref"`
	} `xml:"spine"`
	Guide struct {
		References []struct {
			Type  string `xml:"type,attr"`
			Href  string `xml:"href,attr"`
			Title string `xml:"title,attr"`
		} `xml:"reference"`
	} `xml:"guide"`
}

type opfItem struct {
	ID         string `xml:"id,attr"`
	Href       string `xml:"href,attr"`
	MediaType  string `xml:"media-type,attr"`
	Properties string `xml:"properties,attr"`
}

func (b *Book) readOPF() (*opfPackage, error) {
	f, ok := b.entries[b.opfPath]
	if !ok {
		return nil, fmt.Errorf("package document %q is not in the archive", b.opfPath)
	}
	raw, err := readEntry(f, MaxOPFBytes)
	if err != nil {
		return nil, fmt.Errorf("reading package document: %w", err)
	}

	var pkg opfPackage
	// The struct tags carry no namespace, which makes the decoder match on
	// local name alone. EPUB 2 and EPUB 3 use different OPF namespaces and
	// plenty of real books declare neither correctly.
	if err := xml.Unmarshal(raw, &pkg); err != nil {
		return nil, fmt.Errorf("parsing package document: %w", err)
	}
	return &pkg, nil
}

// coverIDs are the manifest ids books use when they declare a cover by
// convention rather than by markup.
var coverIDs = map[string]bool{
	"cover": true, "cover-image": true, "coverimage": true, "cover_image": true,
	"ci": true, "my-cover-image": true,
}

// CoverPath returns the archive entry name of the book's cover image.
//
// The five branches run in descending order of how much the book actually
// TOLD us, versus how much is being guessed.
func (b *Book) CoverPath() (string, error) {
	byID := make(map[string]opfItem, len(b.pkg.Manifest.Items))
	for _, item := range b.pkg.Manifest.Items {
		byID[item.ID] = item
	}

	// 1. EPUB 3: the manifest item says so itself.
	for _, item := range b.pkg.Manifest.Items {
		for _, prop := range strings.Fields(item.Properties) {
			if prop == "cover-image" {
				return ResolvePath(b.opfDir, item.Href)
			}
		}
	}

	// 2. EPUB 2: <meta name="cover" content="ID"> naming a manifest id.
	for _, meta := range b.pkg.Metadata.Metas {
		if !strings.EqualFold(meta.Name, "cover") || meta.Content == "" {
			continue
		}
		if item, ok := byID[meta.Content]; ok {
			return ResolvePath(b.opfDir, item.Href)
		}
		// 3. The same meta, but naming a FILE rather than an id. Out of spec
		// and common enough to be worth handling.
		if resolved, err := ResolvePath(b.opfDir, meta.Content); err == nil {
			if _, ok := b.entries[resolved]; ok {
				return resolved, nil
			}
		}
	}

	// 4. A manifest id that is conventionally the cover.
	for _, item := range b.pkg.Manifest.Items {
		if coverIDs[strings.ToLower(item.ID)] && isImageMediaType(item.MediaType) {
			return ResolvePath(b.opfDir, item.Href)
		}
	}

	// 5. <guide type="cover"> pointing at a page; the cover is the first image
	// on that page.
	for _, ref := range b.pkg.Guide.References {
		if !strings.EqualFold(ref.Type, "cover") || ref.Href == "" {
			continue
		}
		target, err := ResolvePath(b.opfDir, ref.Href)
		if err != nil {
			continue
		}
		f, ok := b.entries[target]
		if !ok {
			continue
		}
		if isImageMediaType(MediaTypeOf(target)) {
			return target, nil
		}
		if src, ok := firstImageIn(f); ok {
			if resolved, err := ResolvePath(path2Dir(target), src); err == nil {
				if _, ok := b.entries[resolved]; ok {
					return resolved, nil
				}
			}
		}
	}

	// Fallback: walk the reading order for the first document with an image in
	// it. A book whose cover is declared nowhere still opens on one.
	if found, ok := b.walkSpineForImage(byID); ok {
		return found, nil
	}

	return "", ErrNoCover
}

func (b *Book) walkSpineForImage(byID map[string]opfItem) (string, bool) {
	walked := 0
	for _, ref := range b.pkg.Spine.ItemRefs {
		if walked >= maxSpineDocsWalked {
			return "", false
		}
		item, ok := byID[ref.IDRef]
		if !ok || item.Href == "" {
			continue
		}
		docPath, err := ResolvePath(b.opfDir, item.Href)
		if err != nil {
			continue
		}
		f, ok := b.entries[docPath]
		if !ok {
			continue
		}
		walked++

		src, ok := firstImageIn(f)
		if !ok {
			continue
		}
		resolved, err := ResolvePath(path2Dir(docPath), src)
		if err != nil {
			continue
		}
		if _, ok := b.entries[resolved]; ok {
			return resolved, true
		}
	}
	return "", false
}

// Section is one entry of the reading order.
type Section struct {
	// Path is the archive entry name, ready to hand back to Resource.
	Path string `json:"path"`
	// Label is a human-readable name if the guide supplies one, else "".
	Label string `json:"label"`
}

// Spine returns the reading order, capped.
//
// Non-linear items are included: they are the front matter and the cover page,
// which is exactly what someone opening a preview wants to see first.
func (b *Book) Spine() []Section {
	byID := make(map[string]opfItem, len(b.pkg.Manifest.Items))
	for _, item := range b.pkg.Manifest.Items {
		byID[item.ID] = item
	}

	labels := make(map[string]string)
	for _, ref := range b.pkg.Guide.References {
		if ref.Href == "" || ref.Title == "" {
			continue
		}
		if resolved, err := ResolvePath(b.opfDir, ref.Href); err == nil {
			labels[resolved] = ref.Title
		}
	}

	sections := make([]Section, 0, len(b.pkg.Spine.ItemRefs))
	for _, ref := range b.pkg.Spine.ItemRefs {
		if len(sections) >= MaxSpineEntries {
			break
		}
		item, ok := byID[ref.IDRef]
		if !ok || item.Href == "" {
			continue
		}
		resolved, err := ResolvePath(b.opfDir, item.Href)
		if err != nil {
			continue
		}
		if _, ok := b.entries[resolved]; !ok {
			continue
		}
		sections = append(sections, Section{Path: resolved, Label: labels[resolved]})
	}
	return sections
}

// Resource reads one archive entry by its exact entry name, capped at limit.
//
// name is expected to have come from CoverPath or Spine, but it is validated
// again regardless: a preview endpoint takes it straight off the URL, and an
// entry name that arrived inside a file synced from a phone is not a trusted
// string.
func (b *Book) Resource(name string, limit int64) ([]byte, error) {
	resolved, err := ResolvePath("", name)
	if err != nil {
		return nil, err
	}
	f, ok := b.entries[resolved]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoSuchEntry, resolved)
	}
	// The DECLARED size, checked before reading a byte. It is a claim by the
	// file rather than a fact, which is why readEntry enforces the cap again
	// while reading -- but an honest 400 MB entry can be rejected for free.
	if f.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("entry %q declares %d bytes, over the %d-byte cap",
			resolved, f.UncompressedSize64, limit)
	}
	return readEntry(f, limit)
}

// firstImageIn returns the src of the first <img> or <image> in an XHTML
// document.
//
// A token-stream walk rather than a full unmarshal: chapter markup is XHTML in
// principle and whatever the authoring tool emitted in practice, and a decoder
// that stops at the first malformed entity finds nothing in a book that a
// reader displays fine.
func firstImageIn(f *zip.File) (string, bool) {
	raw, err := readEntry(f, MaxDocumentBytes)
	if err != nil {
		return "", false
	}

	dec := xml.NewDecoder(strings.NewReader(string(raw)))
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity

	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch strings.ToLower(start.Name.Local) {
		case "img":
			if src := attr(start, "src"); src != "" {
				return src, true
			}
		case "image":
			// SVG's <image xlink:href="...">, which is how a great many
			// EPUB 2 covers are wrapped.
			if href := attr(start, "href"); href != "" {
				return href, true
			}
		}
	}
}

func attr(start xml.StartElement, name string) string {
	for _, a := range start.Attr {
		if strings.EqualFold(a.Name.Local, name) {
			return a.Value
		}
	}
	return ""
}

// ResolvePath turns an href from the package document into an archive entry
// name, relative to base.
//
// It validates explicitly rather than trusting path.Clean to have removed
// everything dangerous. An entry name is attacker-controlled in two senses:
// the archive arrived over Syncthing, and the preview endpoint takes one
// straight off the URL. "../../etc/passwd" inside a zip is a real thing real
// archives contain.
func ResolvePath(base, href string) (string, error) {
	if href == "" {
		return "", errors.New("empty href")
	}

	// Fragments address a place inside a document, never a different file.
	if i := strings.IndexByte(href, '#'); i >= 0 {
		href = href[:i]
	}
	if i := strings.IndexByte(href, '?'); i >= 0 {
		href = href[:i]
	}
	if href == "" {
		return "", errors.New("href is only a fragment")
	}

	// Hrefs are URLs, so they may be percent-encoded. A cover called
	// "My Book.png" is stored as "My%20Book.png" by conforming producers.
	if unescaped, err := url.PathUnescape(href); err == nil {
		href = unescaped
	}

	// A backslash is a path separator on the platform a lot of these archives
	// were built on, and Go's path package does not treat it as one -- so
	// "..\\..\\etc" would survive every check below untouched.
	if strings.ContainsRune(href, '\\') {
		return "", fmt.Errorf("href %q contains a backslash", href)
	}
	if strings.ContainsRune(href, 0) {
		return "", errors.New("href contains a NUL byte")
	}
	if strings.HasPrefix(href, "/") {
		return "", fmt.Errorf("absolute href %q", href)
	}
	if strings.Contains(href, "://") {
		return "", fmt.Errorf("external href %q", href)
	}

	cleaned := path.Clean(path.Join(base, href))

	// After cleaning, a path that still starts with ".." escaped the root.
	// Checking the CLEANED form is the point: "a/../../b" only reveals itself
	// once joined and cleaned.
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("href %q escapes the archive root", href)
	}
	if strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("href %q resolves outside the archive", href)
	}
	if cleaned == "." || cleaned == "" {
		return "", fmt.Errorf("href %q resolves to nothing", href)
	}
	return cleaned, nil
}

func isImageMediaType(mediaType string) bool {
	return strings.HasPrefix(strings.ToLower(mediaType), "image/")
}

// MediaTypeOf is the Content-Type an archive entry is served as.
//
// An explicit table, not mime.TypeByExtension: the set of things that may come
// out of a book is small and closed, and anything outside it is refused rather
// than guessed at. A preview endpoint that will serve an arbitrary type out of
// a file synced from a phone is a file-hosting hole.
func MediaTypeOf(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".xhtml", ".html", ".htm":
		// text/html, not application/xhtml+xml: a browser parsing XHTML
		// strictly shows a parse error for one malformed entity in a book that
		// every reader displays fine. The iframe is sandboxed to an opaque
		// origin with no scripts, so the lenient parser costs nothing.
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".ttf":
		return "font/ttf"
	case ".otf":
		return "font/otf"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".xml", ".ncx", ".opf":
		return "application/xml; charset=utf-8"
	}
	return ""
}

// readEntry reads at most limit bytes of an archive entry, and reports an
// entry that would exceed it rather than silently truncating -- a half-read
// cover decodes into a half-drawn thumbnail.
func readEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	// limit+1 so the read can tell "exactly at the cap" from "over it".
	raw, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("entry %q exceeds the %d-byte cap", f.Name, limit)
	}
	return raw, nil
}
