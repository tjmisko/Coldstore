package thumbnail

import (
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func setupTestThumb(t *testing.T) (*Generator, *catalog.Catalog) {
	t.Helper()

	coldDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	gen := NewGenerator(coldDir, cat, logger)
	return gen, cat
}

func createTestJPEG(t *testing.T, path string, width, height int) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0755)
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	jpeg.Encode(f, img, &jpeg.Options{Quality: 90})
}

func createTestPNG(t *testing.T, path string, width, height int) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0755)
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 200, 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	png.Encode(f, img)
}

func TestCanThumbnail(t *testing.T) {
	tests := []struct {
		ext  string
		want bool
	}{
		{".jpg", true},
		{".jpeg", true},
		{".png", true},
		{".webp", true},
		{".heic", true},
		{".mp4", true},
		{".mov", true},
		{".pdf", false},
		{".txt", false},
		{".doc", false},
	}

	for _, tt := range tests {
		t.Run(tt.ext, func(t *testing.T) {
			if got := CanThumbnail(tt.ext); got != tt.want {
				t.Errorf("CanThumbnail(%q) = %v, want %v", tt.ext, got, tt.want)
			}
		})
	}
}

func TestSupportedExtensionsShouldAgreeWithCanThumbnailWhenEitherChanges(t *testing.T) {
	supported := SupportedExtensions()
	if len(supported) == 0 {
		t.Fatal("SupportedExtensions() returned nothing")
	}

	inList := make(map[string]bool, len(supported))
	for _, ext := range supported {
		if inList[ext] {
			t.Errorf("SupportedExtensions() lists %q twice", ext)
		}
		inList[ext] = true
		if !CanThumbnail(ext) {
			t.Errorf("SupportedExtensions() lists %q but CanThumbnail(%q) is false", ext, ext)
		}
	}

	// The other direction: nothing the generator handles may be missing from
	// the queue's list. Reaching into the category maps rather than the
	// exported list is the point -- a new category that forgot to register in
	// extCategories would still be caught by the Generate dispatch test below.
	for _, set := range extCategories {
		for ext := range set {
			if !inList[ext] {
				t.Errorf("CanThumbnail accepts %q but SupportedExtensions() omits it", ext)
			}
		}
	}
}

func TestSupportedExtensionsShouldBeSortedWhenReturned(t *testing.T) {
	supported := SupportedExtensions()
	if !sort.StringsAreSorted(supported) {
		t.Errorf("SupportedExtensions() is not sorted: %v", supported)
	}
}

func TestGenerateShouldRejectAnExtensionWhenItIsNotSupported(t *testing.T) {
	gen, _ := setupTestThumb(t)

	record := &catalog.FileRecord{
		FolderLabel:  "Docs",
		RelativePath: "notes.xyz",
		Extension:    ".xyz",
	}
	err := gen.Generate(record, filepath.Join(t.TempDir(), "notes.xyz"))
	if err == nil {
		t.Fatal("Generate accepted an unsupported extension")
	}
	if !strings.Contains(err.Error(), "unsupported extension") {
		t.Errorf("want an unsupported-extension error, got %v", err)
	}
}

func TestGenerateShouldDispatchEveryExtensionWhenItIsSupported(t *testing.T) {
	// Guards the third consumer of extCategories: Generate's switch. A
	// category registered in extCategories but missing an arm in the switch
	// would make CanThumbnail promise a format Generate rejects outright.
	gen, _ := setupTestThumb(t)

	for _, ext := range SupportedExtensions() {
		t.Run(ext, func(t *testing.T) {
			record := &catalog.FileRecord{
				FolderLabel:  "Docs",
				RelativePath: "sample" + ext,
				Extension:    ext,
			}
			// The source does not exist, so every arm fails -- but it must
			// fail on the file, not on the dispatch.
			err := gen.Generate(record, filepath.Join(t.TempDir(), "sample"+ext))
			if err != nil && strings.Contains(err.Error(), "unsupported extension") {
				t.Errorf("Generate has no arm for %q", ext)
			}
		})
	}
}

func TestGenerateJPEGThumbnail(t *testing.T) {
	gen, cat := setupTestThumb(t)

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "photo.jpg")
	createTestJPEG(t, srcPath, 1920, 1080)

	record := &catalog.FileRecord{
		FolderLabel:  "Photos",
		RelativePath: "2024/photo.jpg",
		Filename:     "photo.jpg",
		Extension:    ".jpg",
		SizeBytes:    1000,
		FileMtime:    time.Now().UTC(),
		Status:       catalog.StatusSynced,
	}
	cat.UpsertFile(record, time.Now().UnixNano())
	file, _ := cat.GetFile("Photos", "2024/photo.jpg")

	if err := gen.Generate(file, srcPath); err != nil {
		t.Fatal(err)
	}

	// Thumbnail should exist
	thumbPath := gen.ThumbPath("Photos", "2024/photo.jpg")
	info, err := os.Stat(thumbPath)
	if err != nil {
		t.Fatal("thumbnail should exist:", err)
	}
	if info.Size() == 0 {
		t.Error("thumbnail should not be empty")
	}

	// Open and verify dimensions
	f, _ := os.Open(thumbPath)
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	bounds := img.Bounds()
	maxDim := bounds.Dx()
	if bounds.Dy() > maxDim {
		maxDim = bounds.Dy()
	}
	if maxDim > maxEdge {
		t.Errorf("thumbnail max dimension = %d, want <= %d", maxDim, maxEdge)
	}

	// Catalog should be updated
	file, _ = cat.GetFile("Photos", "2024/photo.jpg")
	if file.ThumbnailPath == "" {
		t.Error("thumbnail_path should be set in catalog")
	}
}

func TestGeneratePNGThumbnail(t *testing.T) {
	gen, cat := setupTestThumb(t)

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "diagram.png")
	createTestPNG(t, srcPath, 800, 600)

	record := &catalog.FileRecord{
		FolderLabel:  "Docs",
		RelativePath: "diagram.png",
		Filename:     "diagram.png",
		Extension:    ".png",
		SizeBytes:    5000,
		FileMtime:    time.Now().UTC(),
		Status:       catalog.StatusSynced,
	}
	cat.UpsertFile(record, time.Now().UnixNano())
	file, _ := cat.GetFile("Docs", "diagram.png")

	if err := gen.Generate(file, srcPath); err != nil {
		t.Fatal(err)
	}

	thumbPath := gen.ThumbPath("Docs", "diagram.png")
	if _, err := os.Stat(thumbPath); err != nil {
		t.Fatal("thumbnail should exist:", err)
	}
}

func TestProcessQueue(t *testing.T) {
	gen, cat := setupTestThumb(t)

	srcDir := t.TempDir()
	files := []struct {
		rel  string
		name string
	}{
		{"a.jpg", "a.jpg"},
		{"sub/b.jpg", "b.jpg"},
	}

	var records []catalog.FileRecord
	for _, f := range files {
		srcPath := filepath.Join(srcDir, f.rel)
		createTestJPEG(t, srcPath, 640, 480)

		record := &catalog.FileRecord{
			FolderLabel:  "Test",
			RelativePath: f.rel,
			Filename:     f.name,
			Extension:    ".jpg",
			SizeBytes:    1000,
			FileMtime:    time.Now().UTC(),
			Status:       catalog.StatusSynced,
		}
		cat.UpsertFile(record, time.Now().UnixNano())
		got, _ := cat.GetFile("Test", f.rel)
		records = append(records, *got)
	}

	gen.ProcessQueue(records, func(f *catalog.FileRecord) string {
		return filepath.Join(srcDir, f.RelativePath)
	}, 2)

	// Both thumbnails should exist
	for _, f := range files {
		thumbPath := gen.ThumbPath("Test", f.rel)
		if _, err := os.Stat(thumbPath); err != nil {
			t.Errorf("thumbnail for %s should exist: %v", f.rel, err)
		}
	}
}
