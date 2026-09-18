package main

import (
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"

	"golang.org/x/image/tiff"
)

// Encoding a finished panorama into the format the user asks for at download
// time. The stitch always produces a lossless master, so changing format or
// JPEG quality afterwards costs a re-encode rather than a whole re-stitch, and
// never compounds lossy compression.

// encodeTo returns a file holding the master encoded as the requested format.
//
// A request for the master's own format is served by the master itself: the
// TIFF Hugin wrote is already what a TIFF download should be, and re-writing
// it would only lose its LZW compression, which this package's TIFF writer
// does not implement.
//
// Encoded files are cached per format and quality, so re-downloading or
// switching back to a format already produced costs nothing.
func encodeTo(master, dir, key string, quality int) (string, error) {
	format, ok := formats[key]
	if !ok {
		return "", fmt.Errorf("unknown format %q", key)
	}
	if !format.Convertible() {
		return "", fmt.Errorf("%s output has to be chosen before stitching", format.Label)
	}
	if sameFormat(master, format) {
		return master, nil
	}

	out := filepath.Join(dir, cacheName(key, quality, format.Ext))
	if info, err := os.Stat(out); err == nil && info.Size() > 0 {
		return out, nil
	}

	img, err := decodeImage(master)
	if err != nil {
		return "", err
	}

	tmp := out + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if err := encodeImage(f, img, key, quality); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	// Rename last, so a reader never sees a half-written file.
	if err := os.Rename(tmp, out); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return out, nil
}

func cacheName(key string, quality int, ext string) string {
	if key == "jpg" {
		return fmt.Sprintf("download-%s-q%d%s", key, quality, ext)
	}
	return "download-" + key + ext
}

// sameFormat reports whether the master file is already in the wanted format.
func sameFormat(master string, format Format) bool {
	ext := filepath.Ext(master)
	for _, name := range format.Basenames {
		if filepath.Ext(name) == ext {
			return true
		}
	}
	return false
}

func decodeImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("could not read the stitched panorama: %w", err)
	}
	return img, nil
}

func encodeImage(f *os.File, img image.Image, key string, quality int) error {
	switch key {
	case "jpg":
		// JPEG has no alpha, so the transparent margin left by the crop has
		// to be flattened. Black matches what the blender writes there.
		return jpeg.Encode(f, flatten(img), &jpeg.Options{Quality: quality})
	case "png":
		encoder := png.Encoder{CompressionLevel: png.DefaultCompression}
		return encoder.Encode(f, img)
	case "tif":
		// Deflate, because this package's TIFF writer tags LZW output without
		// actually compressing it, producing a file it cannot read back.
		return tiff.Encode(f, img, &tiff.Options{Compression: tiff.Deflate, Predictor: true})
	default:
		return fmt.Errorf("cannot encode %q", key)
	}
}

// flatten composites an image onto black and drops the alpha channel.
func flatten(img image.Image) image.Image {
	if opaque, ok := img.(interface{ Opaque() bool }); ok && opaque.Opaque() {
		return img
	}
	bounds := img.Bounds()
	out := image.NewRGBA(bounds)
	draw.Draw(out, bounds, image.Black, image.Point{}, draw.Src)
	draw.Draw(out, bounds, img, bounds.Min, draw.Over)
	return out
}
