package thumbnail

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"io"
	"net/url"
	"path"
	"strings"

	"github.com/disintegration/imaging"
)

// EPUB cover extraction, by hand over archive/zip and encoding/xml.
//
// No new dependency, because there is nothing to depend on: an EPUB is a zip
// with an XML manifest in it, and the standard library reads both. The work is
// not the parsing, it is knowing WHERE the cover is -- there are five places
// it can be declared, across two versions of the format, and real books use
// all of them.

const (
	// maxOPFBytes caps the package document. A manifest is kilobytes; anything
	// claiming to be megabytes of it is not a book.
	maxOPFBytes = 2 << 20

	// maxCoverBytes caps the cover image. Checked against the archive's
	// DECLARED size first and enforced again while reading, because a
	// declared size is a claim by the file, not a fact.
	maxCoverBytes = 24 << 20

	// maxSpineDocsWalked bounds the fallback search. If the first ten
	// documents of a book have no image in them, the eleventh is not a cover.
	maxSpineDocsWalked = 10

	// maxSpineDocBytes caps one chapter read during that walk.
	maxSpineDocBytes = 2 << 20
)

// errSVGCover is returned for a cover that is SVG. Rasterising SVG would mean
// pulling in a renderer for the one format where the cover is usually a
// wrapper around a bitmap that is also in the archive; a clean thumb_error is
// the better trade.
var errSVGCover = errors.New("cover is SVG, which needs a rasteriser")

// generateEPUB extracts a book's cover and saves it through the same saveThumb
// path as everything else.
func (g *Generator) generateEPUB(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("opening epub: %w", err)
	}
	defer zr.Close()

	entries := indexZip(&zr.Reader)

	opfPath, err := findOPFPath(entries)
	if err != nil {
		return err
	}

	pkg, err := readOPF(entries, opfPath)
	if err != nil {
		return err
	}

	// Every href in the package document is relative to the package document's
	// OWN directory, not to the zip root. Resolving against the root is the
	// bug every naive implementation ships, and it is invisible in the
	// minority of books that keep the OPF at the top level.
	base := path.Dir(opfPath)

	coverPath, err := findCoverPath(entries, pkg, base)
	if err != nil {
		return err
	}

	img, err := readImageEntry(entries, coverPath)
	if err != nil {
		return err
	}
	return saveThumb(img, dst)
}

// zipIndex is a case-preserving lookup of the archive's entries. EPUB paths
// are case-sensitive, so this is an exact map; it exists to avoid a linear
// scan per lookup on a book with a few hundred entries.
type zipIndex map[string]*zip.File

func indexZip(r *zip.Reader) zipIndex {
	index := make(zipIndex, len(r.File))
	for _, f := range r.File {
		index[f.Name] = f
	}
	return index
}

// order returns the archive entries in their stored order, which is what the
// spine fallback walks when it has nothing better to go on.
func (z zipIndex) order(r *zip.Reader) []*zip.File { return r.File }

func findOPFPath(entries zipIndex) (string, error) {
	f, ok := entries["META-INF/container.xml"]
	if !ok {
		return "", errors.New("epub has no META-INF/container.xml")
	}
	raw, err := readEntry(f, maxOPFBytes)
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
		resolved, err := resolveArchivePath("", rf.FullPath)
		if err != nil {
			return "", err
		}
		if _, ok := entries[resolved]; ok {
			return resolved, nil
		}
	}
	return "", errors.New("epub declares no readable package document")
}

type opfPackage struct {
	Metadata struct {
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
			IDRef string `xml:"idref,attr"`
		} `xml:"itemref"`
	} `xml:"spine"`
	Guide struct {
		References []struct {
			Type string `xml:"type,attr"`
			Href string `xml:"href,attr"`
		} `xml:"reference"`
	} `xml:"guide"`
}

type opfItem struct {
	ID         string `xml:"id,attr"`
	Href       string `xml:"href,attr"`
	MediaType  string `xml:"media-type,attr"`
	Properties string `xml:"properties,attr"`
}

func readOPF(entries zipIndex, opfPath string) (*opfPackage, error) {
	f, ok := entries[opfPath]
	if !ok {
		return nil, fmt.Errorf("package document %q is not in the archive", opfPath)
	}
	raw, err := readEntry(f, maxOPFBytes)
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

// findCoverPath is the five-branch lookup, in descending order of how much the
// book actually TOLD us versus how much we are guessing.
func findCoverPath(entries zipIndex, pkg *opfPackage, base string) (string, error) {
	byID := make(map[string]opfItem, len(pkg.Manifest.Items))
	for _, item := range pkg.Manifest.Items {
		byID[item.ID] = item
	}

	// 1. EPUB 3: the manifest item says so itself.
	for _, item := range pkg.Manifest.Items {
		for _, prop := range strings.Fields(item.Properties) {
			if prop == "cover-image" {
				return resolveArchivePath(base, item.Href)
			}
		}
	}

	// 2. EPUB 2: <meta name="cover" content="ID"> naming a manifest id.
	for _, meta := range pkg.Metadata.Metas {
		if !strings.EqualFold(meta.Name, "cover") || meta.Content == "" {
			continue
		}
		if item, ok := byID[meta.Content]; ok {
			return resolveArchivePath(base, item.Href)
		}
		// 3. The same meta, but naming a FILE rather than an id. Out of spec
		// and common enough to be worth handling.
		if resolved, err := resolveArchivePath(base, meta.Content); err == nil {
			if _, ok := entries[resolved]; ok {
				return resolved, nil
			}
		}
	}

	// 4. A manifest id that is conventionally the cover.
	for _, item := range pkg.Manifest.Items {
		if coverIDs[strings.ToLower(item.ID)] && isImageMediaType(item.MediaType) {
			return resolveArchivePath(base, item.Href)
		}
	}

	// 5. <guide type="cover"> pointing at a page; the cover is the first image
	// on that page.
	for _, ref := range pkg.Guide.References {
		if !strings.EqualFold(ref.Type, "cover") || ref.Href == "" {
			continue
		}
		target, err := resolveArchivePath(base, ref.Href)
		if err != nil {
			continue
		}
		if f, ok := entries[target]; ok {
			if isImageMediaType(guessMediaType(target)) {
				return target, nil
			}
			if src, ok := firstImageIn(f); ok {
				if resolved, err := resolveArchivePath(path.Dir(target), src); err == nil {
					if _, ok := entries[resolved]; ok {
						return resolved, nil
					}
				}
			}
		}
	}

	// Fallback: walk the reading order for the first document with an image in
	// it. A book whose cover is declared nowhere still opens on it.
	if found, ok := walkSpineForImage(entries, pkg, byID, base); ok {
		return found, nil
	}

	return "", errors.New("epub declares no cover image")
}

func walkSpineForImage(entries zipIndex, pkg *opfPackage, byID map[string]opfItem, base string) (string, bool) {
	walked := 0
	for _, ref := range pkg.Spine.ItemRefs {
		if walked >= maxSpineDocsWalked {
			return "", false
		}
		item, ok := byID[ref.IDRef]
		if !ok || item.Href == "" {
			continue
		}
		docPath, err := resolveArchivePath(base, item.Href)
		if err != nil {
			continue
		}
		f, ok := entries[docPath]
		if !ok {
			continue
		}
		walked++

		src, ok := firstImageIn(f)
		if !ok {
			continue
		}
		resolved, err := resolveArchivePath(path.Dir(docPath), src)
		if err != nil {
			continue
		}
		if _, ok := entries[resolved]; ok {
			return resolved, true
		}
	}
	return "", false
}

// firstImageIn returns the src of the first <img> or <image> in an XHTML
// document.
//
// A token-stream walk rather than a full unmarshal: chapter markup is XHTML in
// principle and whatever the authoring tool emitted in practice, and a decoder
// that stops at the first malformed entity finds nothing in a book that a
// reader displays fine.
func firstImageIn(f *zip.File) (string, bool) {
	raw, err := readEntry(f, maxSpineDocBytes)
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

// resolveArchivePath turns an href from the package document into an entry
// name, relative to base.
//
// It validates explicitly rather than trusting path.Clean to have removed
// everything dangerous: an entry name is attacker-controlled in the sense that
// it comes out of a file that arrived over Syncthing, and "../../etc/passwd"
// inside a zip is a real thing that real archives contain.
func resolveArchivePath(base, href string) (string, error) {
	if href == "" {
		return "", errors.New("empty href")
	}

	// Fragments address a place inside a document, never a different file.
	if i := strings.IndexByte(href, '#'); i >= 0 {
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

	if strings.HasPrefix(href, "/") {
		return "", fmt.Errorf("absolute href %q", href)
	}
	if strings.Contains(href, "://") {
		return "", fmt.Errorf("external href %q", href)
	}

	joined := path.Join(base, href)
	cleaned := path.Clean(joined)

	// After cleaning, a path that still starts with ".." escaped the root.
	// Checking the cleaned form is the point: "a/../../b" only reveals itself
	// once joined and cleaned.
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("href %q escapes the archive root", href)
	}
	if strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("href %q resolves outside the archive", href)
	}
	return cleaned, nil
}

func isImageMediaType(mediaType string) bool {
	return strings.HasPrefix(strings.ToLower(mediaType), "image/")
}

func guessMediaType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
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
	}
	return ""
}

// readImageEntry decodes one archive entry as an image.
func readImageEntry(entries zipIndex, name string) (image.Image, error) {
	if strings.EqualFold(path.Ext(name), ".svg") {
		return nil, errSVGCover
	}

	f, ok := entries[name]
	if !ok {
		return nil, fmt.Errorf("cover %q is not in the archive", name)
	}
	// The DECLARED size, checked before reading a byte. It is a claim by the
	// file rather than a fact, which is why readEntry enforces the cap again
	// while reading -- but an honest 400 MB entry can be rejected for free.
	if f.UncompressedSize64 > maxCoverBytes {
		return nil, fmt.Errorf("cover %q declares %d bytes, over the %d-byte cap",
			name, f.UncompressedSize64, maxCoverBytes)
	}

	raw, err := readEntry(f, maxCoverBytes)
	if err != nil {
		return nil, fmt.Errorf("reading cover %q: %w", name, err)
	}

	img, _, err := image.Decode(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("decoding cover %q: %w", name, err)
	}
	return imaging.Fit(img, maxEdge, maxEdge, imaging.Lanczos), nil
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
