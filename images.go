package main

import (
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	// Decoders for the formats Hugin can emit as a stitch result. EXR is
	// absent on purpose: HDR jobs are previewed from their LDR companion.
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// makePreview converts a stitched image into an 8-bit PNG the browser can
// display. It returns "" when the image could not be decoded, which leaves the
// job's download intact and only costs the on-page preview.
func makePreview(imagePath, dir string) string {
	src, err := os.Open(imagePath)
	if err != nil {
		return ""
	}
	defer src.Close()

	img, _, err := image.Decode(src)
	if err != nil {
		return ""
	}

	// Flatten onto black so transparent crop margins do not render as an
	// alpha channel. An opaque image also lets the PNG encoder drop the
	// alpha channel entirely, which keeps the preview smaller.
	bounds := img.Bounds()
	flat := image.NewRGBA(bounds)
	draw.Draw(flat, bounds, image.Black, image.Point{}, draw.Src)
	draw.Draw(flat, bounds, img, bounds.Min, draw.Over)

	previewPath := filepath.Join(dir, "preview.png")
	out, err := os.Create(previewPath)
	if err != nil {
		return ""
	}
	defer out.Close()

	encoder := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := encoder.Encode(out, flat); err != nil {
		os.Remove(previewPath)
		return ""
	}
	return previewPath
}

// rawConverter is an external RAW decoder the pipeline can shell out to.
//
// Go has no pure-Go demosaicer, so RAW support works the same way the rest of
// the pipeline does: by driving a command-line tool. The binary itself stays
// free of cgo, so it still cross-compiles to every platform; a machine with
// none of these tools installed simply cannot accept RAW uploads.
type rawConverter struct {
	name string
	// args builds the command line.
	args func(src, dst string) []string
	// stdout is true when the decoded image arrives on the tool's standard
	// output rather than being written to a file.
	stdout bool
	// candidates lists the paths the tool may have written to. Decoders
	// disagree on whether they replace the source extension or append to it,
	// so look in every plausible place rather than assuming one convention.
	candidates func(src, dst string) []string
}

// rawConverters are probed in order: libraw's dcraw_emu first because it
// tracks new camera models, then dcraw, then darktable's batch tool.
var rawConverters = []rawConverter{
	{
		name: "dcraw_emu",
		args: func(src, dst string) []string { return []string{"-T", "-w", src} },
		candidates: func(src, dst string) []string {
			stem := strings.TrimSuffix(src, filepath.Ext(src))
			return []string{src + ".tiff", src + ".tif", stem + ".tiff", stem + ".tif"}
		},
	},
	{
		name:   "dcraw",
		args:   func(src, dst string) []string { return []string{"-w", "-T", "-c", src} },
		stdout: true,
	},
	{
		name:       "darktable-cli",
		args:       func(src, dst string) []string { return []string{src, dst} },
		candidates: func(src, dst string) []string { return []string{dst} },
	},
}

// detectRawConverter returns the first RAW decoder available on PATH.
func detectRawConverter() (rawConverter, bool) {
	for _, conv := range rawConverters {
		if have(conv.name) {
			return conv, true
		}
	}
	return rawConverter{}, false
}

// ErrNoRawConverter is reported when a RAW file is uploaded to a machine with
// no decoder installed.
var ErrNoRawConverter = errors.New(
	"RAW files need a decoder on PATH: install libraw-bin (dcraw_emu), dcraw, or darktable")

// prepareSource passes normal images straight through and demosaics RAW files
// into a TIFF that Hugin can read. It returns the path to use as a stitch input.
func prepareSource(path, dir, converterName string) (string, error) {
	if !rawExts[strings.ToLower(filepath.Ext(path))] {
		return path, nil
	}
	conv, ok := rawConverterByName(converterName)
	if !ok {
		return "", ErrNoRawConverter
	}

	base := filepath.Base(path)
	dst := filepath.Join(dir, strings.TrimSuffix(base, filepath.Ext(base))+"_raw.tif")
	cmd := exec.Command(conv.name, conv.args(path, dst)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")

	if conv.stdout {
		out, err := os.Create(dst)
		if err != nil {
			return "", err
		}
		var stderr strings.Builder
		cmd.Stdout = out
		cmd.Stderr = &stderr
		runErr := cmd.Run()
		closeErr := out.Close()
		if runErr != nil {
			os.Remove(dst)
			return "", fmt.Errorf("%s: %s", conv.name, firstLine(stderr.String(), runErr))
		}
		if closeErr != nil {
			return "", closeErr
		}
		return nonEmptyFile(dst, conv.name)
	}

	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%s: %s", conv.name, firstLine(string(output), err))
	}
	for _, candidate := range conv.candidates(path, dst) {
		if found, err := nonEmptyFile(candidate, conv.name); err == nil {
			return found, nil
		}
	}
	return "", fmt.Errorf("%s produced no output", conv.name)
}

// nonEmptyFile reports path when it exists and has content.
func nonEmptyFile(path, tool string) (string, error) {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return "", fmt.Errorf("%s produced no output", tool)
	}
	return path, nil
}

func rawConverterByName(name string) (rawConverter, bool) {
	for _, conv := range rawConverters {
		if conv.name == name {
			return conv, true
		}
	}
	return rawConverter{}, false
}

// firstLine trims tool output down to something fit for a one-line UI message.
func firstLine(output string, fallback error) string {
	for _, line := range strings.Split(output, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > 200 {
				line = line[:200]
			}
			return line
		}
	}
	return fallback.Error()
}
