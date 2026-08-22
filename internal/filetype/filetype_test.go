package filetype

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSniffShouldIdentifyTheTypeWhenTheMagicNumberIsKnown(t *testing.T) {
	tests := []struct {
		name string
		head []byte
		want string
	}{
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}, TypeJPEG},
		{"png", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0}, TypePNG},
		{"gif87a", []byte("GIF87a\x00\x00"), TypeGIF},
		{"gif89a", []byte("GIF89a\x00\x00"), TypeGIF},
		{"pdf", []byte("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n"), TypePDF},
		{"webp", []byte("RIFF\x24\x00\x00\x00WEBPVP8 "), TypeWebP},
		{"avi", []byte("RIFF\x24\x00\x00\x00AVI LIST"), TypeAVI},
		{"heic", ftyp("heic"), TypeHEIC},
		{"heif", ftyp("mif1"), TypeHEIF},
		{"avif", ftyp("avif"), TypeAVIF},
		{"quicktime", ftyp("qt  "), TypeQuickTime},
		{"mp4", ftyp("isom"), TypeMP4},
		{"unknown ftyp brand falls back to mp4", ftyp("zzzz"), TypeMP4},
		{"matroska", append([]byte{0x1A, 0x45, 0xDF, 0xA3}, []byte("\x42\x82\x88matroska")...), TypeMatroska},
		{"webm", append([]byte{0x1A, 0x45, 0xDF, 0xA3}, []byte("\x42\x82\x84webm")...), TypeWebM},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Sniff(tt.head); got != tt.want {
				t.Errorf("Sniff(%s) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// ftyp builds the first 12 bytes of an ISO base media file with the given brand.
func ftyp(brand string) []byte {
	head := make([]byte, 0, 16)
	head = append(head, 0x00, 0x00, 0x00, 0x20)
	head = append(head, []byte("ftyp")...)
	head = append(head, []byte(brand)...)
	head = append(head, 0x00, 0x00, 0x02, 0x00)
	return head
}

func TestSniffShouldReturnEPUBWhenTheZipCarriesTheOCFMimetypeEntry(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// OCF requires the mimetype member first and STORED, which is what puts
	// its content at the fixed offset the sniff reads.
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(TypeEPUB)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	if got := Sniff(buf.Bytes()); got != TypeEPUB {
		t.Errorf("Sniff(epub) = %q, want %q", got, TypeEPUB)
	}
}

func TestSniffShouldReturnZipWhenTheArchiveIsNotAnEPUB(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("readme.txt")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("hello"))
	zw.Close()

	if got := Sniff(buf.Bytes()); got != TypeZIP {
		t.Errorf("Sniff(zip) = %q, want %q", got, TypeZIP)
	}
}

func TestSniffShouldReturnTextWhenTheFileHasNoMagicNumberAndNoNulByte(t *testing.T) {
	// The case the whole package exists for: a bookmark export with no
	// extension and no magic number.
	head := []byte("https://example.invalid/a\nhttps://example.invalid/b\n")
	if got := Sniff(head); got != TypeText {
		t.Errorf("Sniff(bookmarks) = %q, want %q", got, TypeText)
	}
}

func TestSniffShouldReturnUnknownWhenTheFileIsBinaryWithNoSignature(t *testing.T) {
	head := []byte{0x01, 0x02, 0x00, 0x03, 0x04, 0x05}
	if got := Sniff(head); got != TypeUnknown {
		t.Errorf("Sniff(binary) = %q, want %q", got, TypeUnknown)
	}
}

func TestSniffShouldReturnUnknownWhenThePrefixIsEmpty(t *testing.T) {
	if got := Sniff(nil); got != TypeUnknown {
		t.Errorf("Sniff(empty) = %q, want %q", got, TypeUnknown)
	}
}

func TestIsTextShouldReturnFalseWhenAnEarlyNulByteIsPresent(t *testing.T) {
	if IsText([]byte("plain text\x00more")) {
		t.Error("a NUL byte in the prefix must read as binary")
	}
}

func TestIsTextShouldReturnFalseWhenTheBytesAreNotValidUTF8(t *testing.T) {
	if IsText([]byte{'a', 'b', 0xC3, 0x28, 'c'}) {
		t.Error("invalid UTF-8 must read as binary")
	}
}

func TestIsTextShouldReturnTrueWhenARuneIsTruncatedAtTheWindowEdge(t *testing.T) {
	// 8000 bytes of ASCII then the first byte of a multi-byte rune. Without
	// tolerating the truncation every long text file whose window happens to
	// end mid-rune would be called binary.
	head := append(bytes.Repeat([]byte("a"), textCheckLen-1), 0xE2)
	if !IsText(head) {
		t.Error("a rune truncated by the window edge must not read as binary")
	}
}

func TestIsTextShouldReturnTrueWhenTheFileIsEmpty(t *testing.T) {
	if !IsText(nil) {
		t.Error("an empty file must not read as binary")
	}
}

func TestIsTextShouldReturnTrueWhenTheTextIsMultibyteUTF8(t *testing.T) {
	if !IsText([]byte("notes: café, naïve, 日本語\n")) {
		t.Error("valid multibyte UTF-8 must read as text")
	}
}

func TestSniffFileShouldIdentifyTheTypeWhenTheFileIsOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.pdf")
	if err := os.WriteFile(path, []byte("%PDF-1.4\ntrailer\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := SniffFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != TypePDF {
		t.Errorf("SniffFile = %q, want %q", got, TypePDF)
	}
}

func TestSniffFileShouldReturnUnknownWhenTheFileCannotBeOpened(t *testing.T) {
	got, err := SniffFile(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Error("want an error for a missing file")
	}
	if got != TypeUnknown {
		t.Errorf("SniffFile = %q, want %q", got, TypeUnknown)
	}
}

func TestSniffFileShouldNotReadTheWholeFileWhenItIsLarge(t *testing.T) {
	// The sniff runs inside a folder scan, so it must be bounded regardless of
	// how big the file is. A 4 MB file identified from its first bytes is the
	// observable form of that.
	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")
	body := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("x"), 4<<20)...)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := SniffFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != TypePDF {
		t.Errorf("SniffFile = %q, want %q", got, TypePDF)
	}
}

func TestUnknownShouldNotBeTheEmptyStringWhenASniffCompletes(t *testing.T) {
	// Empty means "never sniffed" in the catalog. If an unidentifiable file
	// recorded empty it would be re-read on every scan for the life of the
	// installation.
	if TypeUnknown == "" {
		t.Fatal("TypeUnknown must be distinguishable from an unsniffed row")
	}
	if strings.TrimSpace(TypeUnknown) == "" {
		t.Fatal("TypeUnknown must not be blank")
	}
}
