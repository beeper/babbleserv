package util

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
)

var (
	ErrThumbnailTooLarge    = errors.New("the requested content is too large to thumbnail")
	ErrThumbnailUnsupported = errors.New("the requested content cannot be thumbnailled")
)

// GenerateThumbnail decodes a bounded source and returns a static PNG. The
// config check happens before full decode to bound decompression-bomb pixels.
func GenerateThumbnail(
	input io.Reader,
	maxEncodedBytes, maxSourcePixels int64,
	width, height int,
	method string,
) ([]byte, error) {
	encodedSource, err := io.ReadAll(io.LimitReader(input, maxEncodedBytes))
	if err != nil {
		return nil, err
	}
	if int64(len(encodedSource)) == maxEncodedBytes {
		var extra [1]byte
		if _, err = io.ReadFull(input, extra[:]); err == nil {
			return nil, ErrThumbnailTooLarge
		} else if err != io.EOF {
			return nil, err
		}
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(encodedSource))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return nil, ErrThumbnailUnsupported
	}
	if int64(config.Width) > maxSourcePixels/int64(config.Height) {
		return nil, ErrThumbnailTooLarge
	}
	source, _, err := image.Decode(bytes.NewReader(encodedSource))
	if err != nil {
		return nil, ErrThumbnailUnsupported
	}
	output := resizeThumbnail(source, width, height, method)
	var encoded bytes.Buffer
	if err = png.Encode(&encoded, output); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func resizeThumbnail(source image.Image, width, height int, method string) image.Image {
	bounds := source.Bounds()
	sourceWidth, sourceHeight := bounds.Dx(), bounds.Dy()
	scaleX := float64(width) / float64(sourceWidth)
	scaleY := float64(height) / float64(sourceHeight)
	scale := min(scaleX, scaleY, 1)
	if method == "crop" {
		scale = max(scaleX, scaleY)
		if scale > 1 {
			// Preserve the requested aspect ratio without enlarging the source.
			width = max(1, int(float64(width)/scale))
			height = max(1, int(float64(height)/scale))
			scale = 1
		}
	}

	scaledWidth := max(1, int(float64(sourceWidth)*scale+0.5))
	scaledHeight := max(1, int(float64(sourceHeight)*scale+0.5))
	outputWidth, outputHeight := scaledWidth, scaledHeight
	if method == "crop" {
		outputWidth, outputHeight = width, height
	}
	output := image.NewRGBA(image.Rect(0, 0, outputWidth, outputHeight))
	offsetX, offsetY := (scaledWidth-outputWidth)/2, (scaledHeight-outputHeight)/2
	for y := range outputHeight {
		for x := range outputWidth {
			sourceX := bounds.Min.X + min(sourceWidth-1, int(float64(x+offsetX)/scale))
			sourceY := bounds.Min.Y + min(sourceHeight-1, int(float64(y+offsetY)/scale))
			output.Set(x, y, color.RGBAModel.Convert(source.At(sourceX, sourceY)))
		}
	}
	return output
}
