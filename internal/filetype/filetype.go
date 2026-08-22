// Package filetype identifies a file from its leading bytes.
//
// It exists because the catalog's mime_type comes from mime.TypeByExtension,
// which returns nothing at all for a file with no extension -- and files with
// no extension are exactly the ones this is for: the browser bookmark exports
// and loose notes that sync alongside a camera roll. An extension-keyed
// thumbnail or preview queue cannot reach them.
//
// Everything here reads a bounded prefix. Nothing seeks, nothing decodes and
// nothing allocates in proportion to the file, so it is safe to run over a
// whole folder inside a scan.
package filetype

import (
	"bytes"
	"io"
	"os"
	"unicode/utf8"
)

// SniffLen is how many leading bytes a sniff looks at. Every signature below
// lives well inside it; the widest is the EPUB mimetype entry, which a
// conforming archive stores unencrypted at offset 30.
const SniffLen = 512

// Canonical type names. These are the values stored in files.detected_type and
// the ones every predicate elsewhere compares against, so they must stay
// stable once written.
const (
	TypeJPEG = "image/jpeg"
	TypePNG  = "image/png"
	TypeGIF  = "image/gif"
	TypeWebP = "image/webp"
	TypeHEIC = "image/heic"
	TypeHEIF = "image/heif"
	TypeAVIF = "image/avif"

	TypeMP4       = "video/mp4"
	TypeQuickTime = "video/quicktime"
	TypeMatroska  = "video/x-matroska"
	TypeWebM      = "video/webm"
	TypeAVI       = "video/x-msvideo"

	TypePDF  = "application/pdf"
	TypeEPUB = "application/epub+zip"
	TypeZIP  = "application/zip"

	TypeText = "text/plain"

	// TypeUnknown is what a completed sniff that recognised nothing records.
	//
	// It is deliberately NOT the empty string. Empty means "never sniffed",
	// and a file stored as empty is re-read on every single scan forever. A
	// file that is genuinely unidentifiable has to be able to say so once.
	TypeUnknown = "application/octet-stream"
)

// textCheckLen is git's heuristic window: a NUL inside the first 8000 bytes
// means binary. Anything past that is not worth reading to answer the
// question.
const textCheckLen = 8000

// SniffFile reads a bounded prefix of path and identifies it. A file it cannot
// open is TypeUnknown with the error; callers in a scan record the type and
// carry on rather than failing the file.
func SniffFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return TypeUnknown, err
	}
	defer f.Close()

	buf := make([]byte, textCheckLen)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return TypeUnknown, err
	}
	return Sniff(buf[:n]), nil
}

// Sniff identifies a leading byte prefix. A short or empty prefix is
// TypeUnknown rather than a guess.
func Sniff(head []byte) string {
	if len(head) == 0 {
		return TypeUnknown
	}
	if t := sniffMagic(head); t != "" {
		return t
	}
	if IsText(head) {
		return TypeText
	}
	return TypeUnknown
}

// sniffMagic returns the type implied by a magic number, or "" for no match.
func sniffMagic(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		return TypeJPEG
	case bytes.HasPrefix(head, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return TypePNG
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return TypeGIF
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return TypePDF
	}

	if t := sniffRIFF(head); t != "" {
		return t
	}
	if t := sniffISOBMFF(head); t != "" {
		return t
	}
	if t := sniffEBML(head); t != "" {
		return t
	}
	if t := sniffZIP(head); t != "" {
		return t
	}
	return ""
}

// sniffRIFF covers the RIFF container: "RIFF" then a 4-byte size then a form
// type. WebP and AVI both live here.
func sniffRIFF(head []byte) string {
	if len(head) < 12 || !bytes.HasPrefix(head, []byte("RIFF")) {
		return ""
	}
	switch string(head[8:12]) {
	case "WEBP":
		return TypeWebP
	case "AVI ":
		return TypeAVI
	}
	return ""
}

// isobmffBrands maps an ISO base media file format brand to its type. The
// brand sits at offset 8, immediately after the "ftyp" box header, and it is
// the only thing distinguishing a HEIC still from an MP4 video -- both are the
// same container.
var isobmffBrands = map[string]string{
	"heic": TypeHEIC, "heix": TypeHEIC, "heim": TypeHEIC, "heis": TypeHEIC,
	"hevc": TypeHEIF, "hevm": TypeHEIF, "hevs": TypeHEIF,
	"mif1": TypeHEIF, "msf1": TypeHEIF,
	"avif": TypeAVIF, "avis": TypeAVIF,
	"qt  ": TypeQuickTime,
	"isom": TypeMP4, "iso2": TypeMP4, "iso4": TypeMP4, "iso5": TypeMP4, "iso6": TypeMP4,
	"mp41": TypeMP4, "mp42": TypeMP4, "mp71": TypeMP4,
	"avc1": TypeMP4, "dash": TypeMP4, "M4V ": TypeMP4, "M4A ": TypeMP4, "M4P ": TypeMP4,
	"3gp4": TypeMP4, "3gp5": TypeMP4, "3g2a": TypeMP4,
}

func sniffISOBMFF(head []byte) string {
	if len(head) < 12 || !bytes.Equal(head[4:8], []byte("ftyp")) {
		return ""
	}
	if t, ok := isobmffBrands[string(head[8:12])]; ok {
		return t
	}
	// An unrecognised brand in a well-formed ftyp box is still some flavour of
	// MP4 far more often than it is anything else, and calling it a video is
	// what routes it to the ffmpeg arm that can actually decode it.
	return TypeMP4
}

// sniffEBML covers Matroska and WebM, which share a magic number and are told
// apart only by the DocType string a little further in.
func sniffEBML(head []byte) string {
	if !bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3}) {
		return ""
	}
	// The DocType element is inside the EBML header, comfortably within the
	// first few dozen bytes. Bound the search anyway: this must not become a
	// scan of whatever length of prefix a caller happened to pass.
	window := head
	if len(window) > SniffLen {
		window = window[:SniffLen]
	}
	if bytes.Contains(window, []byte("webm")) {
		return TypeWebM
	}
	return TypeMatroska
}

// epubMimetypeOffset is where a conforming EPUB stores the literal
// "application/epub+zip". OCF requires the first zip entry to be an
// uncompressed, unencrypted member named "mimetype", which puts its content at
// exactly this offset: 30 bytes of local file header plus the 8-byte name.
const epubMimetypeOffset = 38

func sniffZIP(head []byte) string {
	if !bytes.HasPrefix(head, []byte{'P', 'K', 0x03, 0x04}) {
		return ""
	}
	end := epubMimetypeOffset + len(TypeEPUB)
	if len(head) >= end &&
		bytes.Equal(head[30:38], []byte("mimetype")) &&
		string(head[epubMimetypeOffset:end]) == TypeEPUB {
		return TypeEPUB
	}
	return TypeZIP
}

// IsText reports whether a byte prefix looks like text, using git's heuristic:
// a NUL byte in the first 8000 bytes means binary, and so does a prefix that
// is not valid UTF-8.
//
// http.DetectContentType is not enough on its own here. It answers
// application/octet-stream for anything without a magic number, which is every
// plain text file that does not happen to start with a BOM -- so it cannot
// tell a bookmark export from a firmware blob.
func IsText(head []byte) bool {
	if len(head) == 0 {
		// An empty file is not binary, and rendering it as an empty preview is
		// more useful than refusing to.
		return true
	}
	window := head
	if len(window) > textCheckLen {
		window = window[:textCheckLen]
	}
	if bytes.IndexByte(window, 0) >= 0 {
		return false
	}
	return validUTF8Prefix(window)
}

// validUTF8Prefix is utf8.Valid, except that a rune truncated by the end of
// the window is not a failure. Without this every text file longer than the
// window whose 8000th byte lands mid-rune reads as binary.
func validUTF8Prefix(b []byte) bool {
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size <= 1 {
			// A partial rune at the very end is truncation, not corruption.
			return len(b) < utf8.UTFMax && !utf8.FullRune(b)
		}
		b = b[size:]
	}
	return true
}

// IsTextType reports whether a detected type should be previewed as text.
func IsTextType(detected string) bool { return detected == TypeText }
