package images

import (
	libresize "github.com/nfnt/resize"
	"image"
)

func resize(img image.Image, width, height *uint, algo libresize.InterpolationFunction) image.Image {
	// Get original dimensions
	origBounds := img.Bounds()
	origWidth := uint(origBounds.Dx())
	origHeight := uint(origBounds.Dy())

	// Determine new dimensions
	var newWidth, newHeight uint
	if width != nil && height != nil {
		// Both dimensions specified
		newWidth = *width
		newHeight = *height
	} else if width != nil {
		// Only width specified, maintain aspect ratio
		newWidth = *width
		newHeight = uint(float64(*width) * float64(origHeight) / float64(origWidth))
	} else if height != nil {
		// Only height specified, maintain aspect ratio
		newHeight = *height
		newWidth = uint(float64(*height) * float64(origWidth) / float64(origHeight))
	} else {
		return img
	}

	return libresize.Resize(newWidth, newHeight, img, algo)
}
