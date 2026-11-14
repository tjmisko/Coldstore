package thumbnail

import (
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjmisko/coldstore/internal/filetype"
)

func TestRenderTo(t *testing.T) {
	t.Run("should render to the given path without a catalog when RenderTo is called", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src.jpg")
		img := image.NewRGBA(image.Rect(0, 0, 64, 48))
		for y := 0; y < 48; y++ {
			for x := 0; x < 64; x++ {
				img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
			}
		}
		f, err := os.Create(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := jpeg.Encode(f, img, nil); err != nil {
			t.Fatal(err)
		}
		f.Close()

		gen := NewGenerator(dir, nil, nil) // no catalog: RenderTo must never touch it
		out := filepath.Join(dir, "nested", "out.thumb.jpg")
		if err := gen.RenderTo(context.Background(), src, ".JPG", "", out); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(out)
		if err != nil || info.Size() == 0 {
			t.Fatalf("no thumbnail at %s: %v", out, err)
		}
	})

	t.Run("should render by the sniffed type when the extension is empty", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "noext")
		img := image.NewRGBA(image.Rect(0, 0, 8, 8))
		f, err := os.Create(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := jpeg.Encode(f, img, nil); err != nil {
			t.Fatal(err)
		}
		f.Close()
		gen := NewGenerator(dir, nil, nil)
		out := filepath.Join(dir, "out.thumb.jpg")
		if err := gen.RenderTo(context.Background(), src, "", filetype.TypeJPEG, out); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("should return an unsupported error when neither extension nor detected type is known", func(t *testing.T) {
		dir := t.TempDir()
		gen := NewGenerator(dir, nil, nil)
		err := gen.RenderTo(context.Background(), filepath.Join(dir, "x"), ".xyz", "text", filepath.Join(dir, "out.jpg"))
		if err == nil || !strings.Contains(err.Error(), "unsupported extension") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("should remove a partial output when rendering fails", func(t *testing.T) {
		dir := t.TempDir()
		gen := NewGenerator(dir, nil, nil)
		out := filepath.Join(dir, "out.thumb.jpg")
		if err := gen.RenderTo(context.Background(), filepath.Join(dir, "missing.jpg"), ".jpg", "", out); err == nil {
			t.Fatal("rendering a missing file succeeded")
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatalf("output left behind: %v", err)
		}
	})
}
