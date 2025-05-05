package images

import (
	"io"
	"os"
	"testing"
)

func TestResizeWebp(t *testing.T) {
	f, err := os.Open("testdata/image.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rsz := &pngResizer{}

	width := uint(800)
	height := uint(250)
	img, err := rsz.Resize(f, ResizeOptions{
		Width:  &width,
		Height: &height,
		Fit:    FitModePad,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(img)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile("testdata/image_result.png", data, 0600)
}
