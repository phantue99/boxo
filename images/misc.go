package images

import (
	"github.com/disintegration/gift"
	"image"
	"image/color"
)

func resize(img image.Image, opts ResizeOptions) image.Image {
	// Get original dimensions
	origBounds := img.Bounds()
	// Get new dimensions
	width, height := getDimensions(uint(origBounds.Dx()), uint(origBounds.Dy()), opts)

	var resized image.Image
	if opts.Fit == FitModeCover || opts.Fit == FitModeCrop {
		resized = resizeCrop(img, int(width), int(height))
	}
	if opts.Fit == FitModePad {
		resized = resizePad(img, int(width), int(height), origBounds)
	}
	if opts.Fit == FitModeScaleDown || opts.Fit == FitModeContain {
		resized = resizeDefault(img, int(width), int(height))
	}

	if opts.DevicePixelRatio != 1 {
		return resizeDefault(resized, int(float32(width)*opts.DevicePixelRatio), int(float32(height)*opts.DevicePixelRatio))
	}
	return resized
}

func resizeDefault(img image.Image, width, height int) image.Image {
	filters := gift.New(
		gift.Resize(width, height, gift.LanczosResampling),
	)
	return applyImgFilters(img, filters, width, height)
}

func resizeCrop(img image.Image, width, height int) image.Image {
	filters := gift.New(
		gift.ResizeToFill(width, height, gift.LanczosResampling, gift.CenterAnchor),
	)
	return applyImgFilters(img, filters, width, height)
}

func resizePad(img image.Image, width, height int, origBounds image.Rectangle) image.Image {
	var (
		canvas                                    = getFullWhiteRGBAImg(width, height)
		widthRatio, heightRatio, actualScaleRatio float64
		newWidth, newHeight                       uint
	)

	// Calculate scaling factor to preserve aspect ratio
	widthRatio = float64(width) / float64(origBounds.Dx())
	heightRatio = float64(height) / float64(origBounds.Dy())
	if widthRatio < heightRatio {
		actualScaleRatio = widthRatio
	} else {
		actualScaleRatio = heightRatio
	}

	// Create the resized image to fit inside the white canvas
	newWidth = uint(float64(origBounds.Dx()) * actualScaleRatio)
	newHeight = uint(float64(origBounds.Dy()) * actualScaleRatio)
	filters := gift.New(
		gift.Resize(int(newWidth), int(newHeight), gift.LanczosResampling),
	)
	imageInsidePadding := applyImgFilters(img, filters, width, height)

	// Calculate position to center the resize image
	offsetX := (width - int(newWidth)) / 2
	offsetY := (height - int(newHeight)) / 2

	// Copy the resized image onto the white canvas
	for y := 0; y < int(newHeight); y++ {
		for x := 0; x < int(newWidth); x++ {
			canvas.Set(x+offsetX, y+offsetY, imageInsidePadding.At(x, y))
		}
	}

	return canvas
}

func getDimensions(originalWidth uint, originalHeight uint, opts ResizeOptions) (uint, uint) {
	var (
		width       = originalWidth
		height      = originalHeight
		aspectRatio = float64(originalWidth) / float64(originalHeight)
	)
	// No dimension specified, return the original dimensions
	if opts.Width == nil && opts.Height == nil {
		return originalWidth, originalHeight
	}

	// Scale Down mode
	if opts.Fit == FitModeScaleDown {
		// Implementation note: the new dimensions should meet these requirements
		// 1. Preserve aspect ratio
		// 2. Shrink to fully fit
		// 3. Do not enlarge
		if opts.Width != nil && *opts.Width < width {
			width = *opts.Width
			height = uint(float64(width) / aspectRatio)
		}
		if opts.Height != nil && *opts.Height < height {
			height = *opts.Height
			width = uint(float64(height) * aspectRatio)
		}
		return width, height
	}

	// Contain mode
	if opts.Fit == FitModeContain {
		// Implementation note: the new dimensions should meet these requirements
		// 1. Preserve aspect ratio
		// 2. Shrink or enlarge to be as large as possible
		// 3. Fit within the space of given width or height

		if opts.Width != nil && opts.Height == nil {
			return *opts.Width, uint(float64(*opts.Width) / aspectRatio)
		}
		if opts.Height != nil && opts.Width == nil {
			return uint(float64(*opts.Height) * aspectRatio), *opts.Height
		}
		if opts.Width != nil {
			width = *opts.Width
			height = uint(float64(width) / aspectRatio)
		}
		// If the new height exceeds the user-given height, choose user-given height
		if opts.Height != nil && *opts.Height < height {
			height = *opts.Height
			width = uint(float64(height) * aspectRatio)
		}
		return width, height
	}

	// Cover mode
	if opts.Fit == FitModeCover {
		// Implementation note: fill entire area specified by the width and height
		// if both are specified, return them
		// if only one is specified, calculate the other one using aspect ratio
		if opts.Width != nil && opts.Height != nil {
			return *opts.Width, *opts.Height
		}
		if opts.Width != nil {
			return *opts.Width, uint(float64(*opts.Width) / aspectRatio)
		}
		if opts.Height != nil {
			return uint(float64(*opts.Height) * aspectRatio), *opts.Height
		}
	}

	// Crop mode
	if opts.Fit == FitModeCrop {
		// Implementation note:
		// 1. Image will be shrunk and cropped to fit within the area specified by width and height
		// 2. The image won’t be enlarged
		if opts.Width != nil && opts.Height == nil {
			return *opts.Width, uint(float64(*opts.Width) / aspectRatio)
		}
		if opts.Height != nil && opts.Width == nil {
			return uint(float64(*opts.Height) * aspectRatio), *opts.Height
		}
		if opts.Width != nil && *opts.Width < width {
			width = *opts.Width
		}
		if opts.Height != nil && *opts.Height < height {
			height = *opts.Height
		}
	}

	// Pad mode
	if opts.Fit == FitModePad {
		// Implementation note:
		// 1. Image will be resized (shrunk or enlarged) to be as large as possible within the given width or height
		// 2. Preserve the aspect ratio
		// 3. The extra area will be filled with a background color (white by default)
		if opts.Width != nil && opts.Height == nil {
			return *opts.Width, uint(float64(*opts.Width) / aspectRatio)
		}
		if opts.Height != nil && opts.Width == nil {
			return uint(float64(*opts.Height) * aspectRatio), *opts.Height
		}
		return *opts.Width, *opts.Height
	}

	return width, height
}

func getFullWhiteRGBAImg(width, height int) *image.RGBA {
	result := image.NewRGBA(image.Rect(0, 0, width, height))
	white := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			result.Set(x, y, white)
		}
	}
	return result
}

func applyImgFilters(baseImg image.Image, filters *gift.GIFT, width, height int) image.Image {
	pane := image.NewRGBA(image.Rect(0, 0, width, height))
	filters.Draw(pane, baseImg)
	return pane
}
