//go:build e2e

package e2e

import (
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// genFiles creates n files with spread mtimes over 30 days in the given directory.
func genFiles(t *testing.T, dir string, n int, ext string, sizeBytes int) []string {
	t.Helper()
	os.MkdirAll(dir, 0755)

	now := time.Now()
	spread := 30 * 24 * time.Hour
	paths := make([]string, n)

	for i := 0; i < n; i++ {
		name := "file" + padInt(i, 4) + ext
		p := filepath.Join(dir, name)
		data := make([]byte, sizeBytes)
		if err := os.WriteFile(p, data, 0644); err != nil {
			t.Fatal(err)
		}
		// Spread mtimes: oldest first
		age := time.Duration(float64(spread) * float64(n-1-i) / float64(n))
		mtime := now.Add(-age)
		os.Chtimes(p, mtime, mtime)
		paths[i] = p
	}
	return paths
}

// genImageFile creates a valid 4x4 image file at the given path.
func genImageFile(t *testing.T, path string, ext string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0755)

	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 255})
		}
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	switch ext {
	case ".jpg", ".jpeg":
		if err := jpeg.Encode(f, img, nil); err != nil {
			t.Fatal(err)
		}
	case ".png":
		if err := png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported image extension: %s", ext)
	}
}

func padInt(n, width int) string {
	s := ""
	for i := 0; i < width; i++ {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
