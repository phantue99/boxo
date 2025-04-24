package images

import (
	"io"
	"os"
	"testing"
)

func TestResizeWebp(t *testing.T) {
	f, err := os.Open("testdata/cat.webp")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rsz := &webpResizer{}

	width := uint(100)
	height := uint(100)
	img, err := rsz.Resize(f, &width, &height, false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(img)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile("testdata/cat_result.webp", data, 0600)
	if err != nil {
		t.Fatal(err)
	}
}

func TestResizePng(t *testing.T) {
	f, err := os.Open("testdata/image.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rsz := &pngResizer{}

	width := uint(1000)
	height := uint(100)
	img, err := rsz.Resize(f, &width, &height, false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(img)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile("testdata/image_result.png", data, 0600)
	if err != nil {
		t.Fatal(err)
	}
}

func TestResizeJpg(t *testing.T) {
	f, err := os.Open("testdata/cat.jpg")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rsz := &jpgResizer{}

	width := uint(300)
	height := uint(100)
	img, err := rsz.Resize(f, &width, &height, false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(img)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile("testdata/cat_result.jpg", data, 0600)
	if err != nil {
		t.Fatal(err)
	}
}

func TestResizeGif(t *testing.T) {
	f, err := os.Open("testdata/shocked-surprised.gif")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	rsz := &gifResizer{}
	width := uint(150)
	height := uint(100)
	img, err := rsz.Resize(f, &width, &height, true)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(img)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile("testdata/shocked-surprised_animated.gif", data, 0600)
	if err != nil {
		t.Fatal(err)
	}
}
