package images

import (
	"bytes"
	"github.com/chai2010/webp"
	"golang.org/x/image/bmp"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
)

const (
	FitModeScaleDown = "scale-down"
	FitModeContain   = "contain"
	FitModeCover     = "cover"
	FitModeCrop      = "crop"
	FitModePad       = "pad"
)

type ResizeOptions struct {
	Width             *uint
	Height            *uint
	PreserveAnimation bool
	// Fit is one of FitModeScaleDown, FitModeContain, FitModeCover, FitModeCrop, FitModePad
	Fit              string
	WidthGravity     float32
	HeightGravity    float32
	Quality          int
	DevicePixelRatio float32
}

type Resizer interface {
	Resize(imgReader io.Reader, opts ResizeOptions) (io.Reader, error)
}

func DefaultResizeOptions() ResizeOptions {
	return ResizeOptions{
		Width:             nil,
		Height:            nil,
		PreserveAnimation: false,
		Fit:               FitModeScaleDown,
		Quality:           80,
		DevicePixelRatio:  1.0,
	}
}

func ResizerFromMimeType(mimeType string) Resizer {
	if mimeType == "image/webp" {
		return &webpResizer{}
	}
	if mimeType == "image/png" {
		return &pngResizer{}
	}
	if mimeType == "image/jpeg" || mimeType == "image/jpg" {
		return &jpgResizer{}
	}
	if mimeType == "image/bmp" {
		return &bmpResizer{}
	}
	if mimeType == "image/gif" {
		return &gifResizer{}
	}
	return &noOpResizer{}
}

type noOpResizer struct{}

func (rsz *noOpResizer) Resize(imgReader io.Reader, _ ResizeOptions) (io.Reader, error) {
	return imgReader, nil
}

type webpResizer struct{}

func (rsz *webpResizer) Resize(imgReader io.Reader, opts ResizeOptions) (io.Reader, error) {
	// Read the entire image content
	imgData, err := io.ReadAll(imgReader)
	if err != nil {
		return nil, err
	}

	// Decode the WebP image
	img, err := webp.Decode(bytes.NewReader(imgData))
	if err != nil {
		return nil, err
	}

	// resize img
	resizedImg := resize(img, opts)

	// Encode the resized image as WebP
	var buf bytes.Buffer
	options := &webp.Options{
		Lossless: true,
		Quality:  float32(opts.Quality),
	}
	if err := webp.Encode(&buf, resizedImg, options); err != nil {
		return nil, err
	}

	return bytes.NewReader(buf.Bytes()), nil
}

type pngResizer struct{}

func (rsz *pngResizer) Resize(imgReader io.Reader, opts ResizeOptions) (io.Reader, error) {
	imgData, err := io.ReadAll(imgReader)
	if err != nil {
		return nil, err
	}

	// Decode into image.Image
	img, err := png.Decode(bytes.NewReader(imgData))
	if err != nil {
		return nil, err
	}

	// Resize
	resizedImg := resize(img, opts)

	// Encode as png
	var buf bytes.Buffer
	if err := png.Encode(&buf, resizedImg); err != nil {
		return nil, err
	}
	return bytes.NewReader(buf.Bytes()), nil
}

type jpgResizer struct{}

func (rsz *jpgResizer) Resize(imgReader io.Reader, opts ResizeOptions) (io.Reader, error) {
	imgData, err := io.ReadAll(imgReader)
	if err != nil {
		return nil, err
	}

	img, err := jpeg.Decode(bytes.NewReader(imgData))
	if err != nil {
		return nil, err
	}

	resizedImg := resize(img, opts)

	var buf bytes.Buffer
	options := &jpeg.Options{
		Quality: opts.Quality,
	}
	if err := jpeg.Encode(&buf, resizedImg, options); err != nil {
		return nil, err
	}
	return bytes.NewReader(buf.Bytes()), nil
}

type bmpResizer struct{}

func (rsz *bmpResizer) Resize(imgReader io.Reader, opts ResizeOptions) (io.Reader, error) {
	imgData, err := io.ReadAll(imgReader)
	if err != nil {
		return nil, err
	}

	img, err := bmp.Decode(bytes.NewReader(imgData))
	if err != nil {
		return nil, err
	}

	resizedImg := resize(img, opts)

	var buf bytes.Buffer
	if err := bmp.Encode(&buf, resizedImg); err != nil {
		return nil, err
	}
	return bytes.NewReader(buf.Bytes()), nil
}

type gifResizer struct{}

func (rsz *gifResizer) Resize(imgReader io.Reader, opts ResizeOptions) (io.Reader, error) {
	var buf bytes.Buffer

	if opts.PreserveAnimation {
		animatedImg, err := gif.DecodeAll(imgReader)
		if err != nil {
			return nil, err
		}

		for idx, frame := range animatedImg.Image {
			// Convert Paletted frame to RGBA
			bounds := frame.Bounds()
			rgbaImg := image.NewRGBA(bounds)
			for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
				for x := bounds.Min.X; x < bounds.Max.X; x++ {
					rgbaImg.Set(x, y, frame.At(x, y))
				}
			}

			// resize the frame
			resizedFrame := resize(rgbaImg, opts)

			// Convert back to Paletted (using original palette)
			newBounds := resizedFrame.Bounds()
			palettedImg := image.NewPaletted(newBounds, frame.Palette)
			for y := newBounds.Min.Y; y < newBounds.Max.Y; y++ {
				for x := newBounds.Min.X; x < newBounds.Max.X; x++ {
					palettedImg.Set(x, y, resizedFrame.At(x, y))
				}
			}
			animatedImg.Image[idx] = palettedImg

			animatedImg.Config.Width = palettedImg.Bounds().Dx()
			animatedImg.Config.Height = palettedImg.Bounds().Dy()
		}

		if err = gif.EncodeAll(&buf, animatedImg); err != nil {
			return nil, err
		}
	}

	if !opts.PreserveAnimation {
		firstFrame, err := gif.Decode(imgReader)
		if err != nil {
			return nil, err
		}
		resizedFrame := resize(firstFrame, opts)
		if err = gif.Encode(&buf, resizedFrame, &gif.Options{
			NumColors: 256,
		}); err != nil {
			return nil, err
		}
	}
	return bytes.NewReader(buf.Bytes()), nil
}
