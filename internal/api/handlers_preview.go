package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/filetype"
)

// The download route sets Content-Disposition: attachment unconditionally, so
// pointing an <iframe>, an <embed> or pdf.js at it makes the browser save the
// file instead of showing it. Preview needs its own route, and it needs to say
// what the bytes are rather than letting ServeFile guess from the extension --
// .epub is not in Go's default MIME table at all, and the files this is most
// for have no extension to guess from.

// maxTextPreviewBytes is the hard cap on an inline text preview.
//
// Without it a 2 GB log streams the box to death to render a preview nobody
// reads past the first screen -- on a machine with 3.8 GB of RAM, two 1.5 GHz
// cores and a SQLite handle every other request is already queued behind.
// Truncation is reported rather than hidden, so the UI can say the preview is
// partial instead of implying the file ends there.
const maxTextPreviewBytes = 512 * 1024

// previewTypes maps a detected type to the Content-Type the preview route
// states explicitly. Anything not in here is served as a download instead:
// guessing wrong on a preview means handing a browser bytes it will render as
// something the file is not.
var previewTypes = map[string]string{
	filetype.TypeJPEG:      "image/jpeg",
	filetype.TypePNG:       "image/png",
	filetype.TypeGIF:       "image/gif",
	filetype.TypeWebP:      "image/webp",
	filetype.TypeHEIC:      "image/heic",
	filetype.TypeHEIF:      "image/heif",
	filetype.TypeAVIF:      "image/avif",
	filetype.TypeMP4:       "video/mp4",
	filetype.TypeQuickTime: "video/quicktime",
	filetype.TypeWebM:      "video/webm",
	filetype.TypeMatroska:  "video/x-matroska",
	filetype.TypeAVI:       "video/x-msvideo",
	filetype.TypePDF:       "application/pdf",
	filetype.TypeEPUB:      "application/epub+zip",
	filetype.TypeText:      "text/plain; charset=utf-8",
}

// extensionTypes is the fallback for a row the scanner has not sniffed yet --
// an archived file catalogued before migration 6, most often. The extension is
// a weaker signal than the bytes but it is better than refusing to preview.
var extensionTypes = map[string]string{
	".jpg": filetype.TypeJPEG, ".jpeg": filetype.TypeJPEG,
	".png": filetype.TypePNG, ".gif": filetype.TypeGIF, ".webp": filetype.TypeWebP,
	".heic": filetype.TypeHEIC, ".heif": filetype.TypeHEIF, ".avif": filetype.TypeAVIF,
	".mp4": filetype.TypeMP4, ".mov": filetype.TypeQuickTime, ".webm": filetype.TypeWebM,
	".mkv": filetype.TypeMatroska, ".avi": filetype.TypeAVI,
	".pdf": filetype.TypePDF, ".epub": filetype.TypeEPUB,
	".txt": filetype.TypeText, ".md": filetype.TypeText, ".csv": filetype.TypeText,
	".log": filetype.TypeText, ".json": filetype.TypeText, ".xml": filetype.TypeText,
	".yaml": filetype.TypeText, ".yml": filetype.TypeText, ".ini": filetype.TypeText,
	".conf": filetype.TypeText, ".toml": filetype.TypeText,
}

// previewType decides what a file is for preview purposes.
//
// The sniffed type wins where it exists, because it read the bytes. The
// extension is the fallback, and a last-resort sniff of the file itself covers
// an archived row whose detected_type predates the scanner ever seeing it.
func (s *Server) previewType(file *catalog.FileRecord, path string) string {
	if t := file.DetectedType; t != "" && t != filetype.TypeUnknown {
		return t
	}
	if t, ok := extensionTypes[strings.ToLower(file.Extension)]; ok {
		return t
	}
	// A bounded read, not a full one. This is the only place the preview route
	// touches the file before deciding, and it reads the same prefix the
	// scanner would.
	detected, err := filetype.SniffFile(path)
	if err != nil {
		return filetype.TypeUnknown
	}
	return detected
}

// handlePreviewFile serves a file inline, with an explicit Content-Type and
// nosniff, so a browser renders it rather than saving it.
//
// It never renders anything itself. The daemon's job here is ServeFile with
// range requests, which is cheap; rasterising a PDF in a request handler would
// put that work on a 1.5 GHz core inside the one SQLite connection every other
// request is serialised behind.
func (s *Server) handlePreviewFile(w http.ResponseWriter, r *http.Request) {
	file, filePath, ok := s.resolveFileBytes(w, r)
	if !ok {
		return
	}

	contentType := s.previewType(file, filePath)
	mediaType, known := previewTypes[contentType]
	if !known {
		writeError(w, http.StatusUnsupportedMediaType,
			fmt.Sprintf("no inline preview for %s", contentType))
		return
	}

	// nosniff on every arm. The whole point of stating the type is that the
	// browser must not second-guess it: content sniffing is how a text file
	// full of markup becomes a rendered HTML document on the daemon's origin.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Same-origin embedding only. The reader runs on this origin; nothing else
	// has any business framing a preview of the owner's files.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'self'")
	w.Header().Set("Content-Type", mediaType)
	// Inline, and with a filename, so a viewer that does offer "save" offers
	// the right name.
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`inline; filename="%s"`, sanitizeFilename(file.Filename)))

	if contentType == filetype.TypeText {
		s.serveTextPreview(w, filePath)
		return
	}

	http.ServeFile(w, r, filePath)
}

// serveTextPreview writes at most maxTextPreviewBytes of a text file and says
// in a header whether it stopped early.
//
// A header rather than a wrapper object, so the body stays the file's own
// bytes and the client can render it in a <pre> without parsing anything.
func (s *Server) serveTextPreview(w http.ResponseWriter, path string) {
	f, err := os.Open(path)
	if err != nil {
		s.logger.Warn("opening file for text preview", "path", path, "error", err)
		writeError(w, http.StatusConflict, "file is not readable")
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		s.logger.Warn("stating file for text preview", "path", path, "error", err)
		writeError(w, http.StatusConflict, "file is not readable")
		return
	}

	truncated := info.Size() > maxTextPreviewBytes
	length := info.Size()
	if truncated {
		length = maxTextPreviewBytes
	}

	w.Header().Set("X-Coldstore-Truncated", fmt.Sprintf("%t", truncated))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", length))
	// No Accept-Ranges: a capped body is not the file, and letting a client
	// range-request into it would let it read past the cap one window at a
	// time -- which is the whole thing the cap exists to stop.
	w.Header().Set("Accept-Ranges", "none")
	w.WriteHeader(http.StatusOK)

	if _, err := io.CopyN(w, f, length); err != nil && !errors.Is(err, io.EOF) {
		s.logger.Warn("streaming text preview", "path", path, "error", err)
	}
}

// sanitizeFilename strips the characters that would let a filename break out
// of the quoted-string it is interpolated into. A Syncthing folder holds
// whatever names the owner's devices produced, including ones with quotes and
// newlines in them, and a header value with a CR in it is a response-splitting
// bug rather than a cosmetic one.
func sanitizeFilename(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, filepath.Base(name))
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "download"
	}
	return cleaned
}
