package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/epub"
	"github.com/tjmisko/coldstore/internal/filetype"
)

// EPUB preview: the spine, and the resources it names, served out of the
// archive by the reader that already had to exist for cover thumbnails.
//
// The alternative was shipping epub.js and handing the browser the whole book
// as an arrayBuffer to unzip. That is a real 20 MB download and a real unzip on
// a phone, for a feature whose job is "look before downloading". This is 0 KB
// of bundle and one parser, already tested.
//
// The untrusted component here is a ZIP ENTRY NAME taken straight off the URL.
// It is validated against the archive root explicitly, inside epub.Resource --
// not by relying on the mux to have cleaned the path, which cleans a URL and
// knows nothing about what is inside a zip.

// epubSpineResponse is what the reader needs to render a book: the sections in
// reading order, each addressable through the resource route.
type epubSpineResponse struct {
	Title string `json:"title"`
	// Sections are archive entry names, ready to append to the resource route.
	Sections []epub.Section `json:"sections"`
}

func (s *Server) openEPUB(w http.ResponseWriter, r *http.Request) (*epub.Book, bool) {
	file, filePath, ok := s.resolveFileBytes(w, r)
	if !ok {
		return nil, false
	}

	// The catalog's own classification decides whether this file is a book.
	// Opening whatever the caller named and seeing if it happens to unzip
	// would make this route a general-purpose archive extractor over every
	// file in every synced folder.
	if !s.isEPUB(file, filePath) {
		writeError(w, http.StatusUnsupportedMediaType, "this file is not an epub")
		return nil, false
	}

	book, err := epub.Open(filePath)
	if err != nil {
		s.logger.Warn("opening epub for preview", "id", file.ID, "error", err)
		writeError(w, http.StatusUnprocessableEntity, "this epub could not be read")
		return nil, false
	}
	return book, true
}

func (s *Server) handleEPUBSpine(w http.ResponseWriter, r *http.Request) {
	book, ok := s.openEPUB(w, r)
	if !ok {
		return
	}
	defer book.Close()

	sections := book.Spine()
	if sections == nil {
		sections = []epub.Section{}
	}
	writeJSON(w, http.StatusOK, epubSpineResponse{Title: book.Title(), Sections: sections})
}

func (s *Server) handleEPUBResource(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("resource")
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing resource path")
		return
	}

	// The type is decided from the NAME before the archive is even opened, and
	// an extension a book has no business containing is refused outright. A
	// preview route that will serve an arbitrary type out of a file synced
	// from a phone is a file-hosting hole.
	mediaType := epub.MediaTypeOf(name)
	if mediaType == "" {
		writeError(w, http.StatusUnsupportedMediaType,
			fmt.Sprintf("no preview for %q", name))
		return
	}

	book, ok := s.openEPUB(w, r)
	if !ok {
		return
	}
	defer book.Close()

	// epub.Resource re-validates the entry name against the archive root. This
	// is the only place in the API where a path component from the URL reaches
	// a filesystem-shaped lookup, so it does not get to skip that.
	body, err := book.Resource(name, epub.MaxResourceBytes)
	if err != nil {
		if errors.Is(err, epub.ErrNoSuchEntry) {
			writeError(w, http.StatusNotFound, "no such resource in this epub")
			return
		}
		s.logger.Warn("reading epub resource", "resource", name, "error", err)
		writeError(w, http.StatusUnprocessableEntity, "this resource could not be read")
		return
	}

	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The chapter renders inside a sandboxed iframe with no scripts and an
	// opaque origin, and this is the matching server-side statement: the
	// document may load images, styles and fonts from this origin and reach
	// nothing else, and only this origin may frame it.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
			"font-src 'self'; frame-ancestors 'self'; form-action 'none'; base-uri 'none'")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		s.logger.Debug("writing epub resource", "resource", name, "error", err)
	}
}

// isEPUB decides whether this file is a book, using the same order of
// authority as the preview route: the sniffed type, then the extension, then a
// bounded sniff for a row the scanner has not reached -- which is every
// archived file catalogued before migration 6.
func (s *Server) isEPUB(file *catalog.FileRecord, path string) bool {
	if file.DetectedType == filetype.TypeEPUB {
		return true
	}
	if strings.EqualFold(file.Extension, ".epub") {
		return true
	}
	return s.previewType(file, path) == filetype.TypeEPUB
}
