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
	"regexp"
	"runtime"
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

// decodeFamily names a group of upload formats that share a decoder chain.
type decodeFamily string

const (
	familyRaw  decodeFamily = "raw"
	familyHEIF decodeFamily = "heif"
)

// sourceConverter is an external decoder the pipeline can shell out to for a
// format Hugin cannot read on its own.
//
// Go has no pure-Go demosaicer and no pure-Go HEIF decoder, so these formats
// work the same way the rest of the pipeline does: by driving a command-line
// tool. The binary itself stays free of cgo, so it still cross-compiles to
// every platform; a machine with none of these tools installed simply cannot
// accept those uploads.
type sourceConverter struct {
	name string
	// args builds the command line.
	args func(src, dst string) []string
	// ext is the extension the decoded file should carry, because most of
	// these tools pick their output format from it. Empty means ".tif".
	ext string
	// stdout is true when the decoded image arrives on the tool's standard
	// output rather than being written to a file.
	stdout bool
	// candidates lists the paths the tool may have written to. Decoders
	// disagree on whether they replace the source extension or append to it,
	// so look in every plausible place rather than assuming one convention.
	candidates func(src, dst string) []string
	// verify reports whether the tool found at path can actually decode this
	// family. Nil means that finding the binary is proof enough.
	verify func(path string, env []string) bool
}

// outExt is the extension the decoded file should be given.
func (c sourceConverter) outExt() string {
	if c.ext == "" {
		return ".tif"
	}
	return c.ext
}

// rawConverters are probed in order: libraw's dcraw_emu first because it
// tracks new camera models, then dcraw, then darktable's batch tool.
var rawConverters = []sourceConverter{
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

// heifConverters decode HEIC/HEIF, which iPhones shoot by default.
//
// Unlike the RAW chain, finding the binary is not enough for ImageMagick: it
// is routinely installed without the HEIC delegate, so those entries carry a
// verify hook and are only used when the build can genuinely read the format.
var heifConverters = buildHEIFConverters()

func buildHEIFConverters() []sourceConverter {
	convs := []sourceConverter{
		{
			// Built into every macOS since High Sierra, so a Mac needs no
			// install at all. It exists nowhere else, so probing it first
			// costs other platforms one failed lookup.
			name: "sips",
			args: func(src, dst string) []string {
				return []string{"-s", "format", "tiff", src, "--out", dst}
			},
			candidates: func(src, dst string) []string { return []string{dst} },
		},
		{
			// libheif's own converter, and the usual one on Linux. It picks
			// the output format from the suffix and does not know TIFF, so
			// this is the one entry that writes PNG.
			name:       "heif-convert",
			ext:        ".png",
			args:       func(src, dst string) []string { return []string{src, dst} },
			candidates: func(src, dst string) []string { return []string{dst} },
		},
		{
			// 8 bits on purpose: Hugin refuses to blend a UINT16 image with
			// the UINT8 JPEGs it is usually stitched beside, and the RAW
			// chain writes 8-bit TIFFs for the same reason. It costs the
			// extra range of a 10-bit HDR HEIC, which is the rarer case.
			name: "magick",
			args: func(src, dst string) []string {
				return []string{src, "-depth", "8", dst}
			},
			candidates: func(src, dst string) []string { return []string{dst} },
			verify:     magickReadsHEIF,
		},
	}
	// ImageMagick 6 kept the work under "convert". On Windows that name
	// belongs to the system's own filesystem conversion tool, so it is only
	// safe to probe anywhere else.
	if runtime.GOOS != "windows" {
		convs = append(convs, sourceConverter{
			name: "convert",
			args: func(src, dst string) []string {
				return []string{src, "-depth", "8", dst}
			},
			candidates: func(src, dst string) []string { return []string{dst} },
			verify:     magickReadsHEIF,
		})
	}
	return convs
}

// decodeFamilies pairs each family with its decoder chain, in a fixed order so
// that detection and the /health report are deterministic.
var decodeFamilies = []struct {
	family     decodeFamily
	converters []sourceConverter
}{
	{familyRaw, rawConverters},
	{familyHEIF, heifConverters},
}

// magickMode matches ImageMagick's three-character mode column, such as "rw+"
// or "---".
var magickMode = regexp.MustCompile(`^[r-][w-][+-]$`)

// magickReadsHEIF reports whether this ImageMagick build can actually decode
// HEIF, rather than merely knowing the name of the format.
//
// ImageMagick is often installed without the HEIC delegate, so finding the
// binary says nothing on its own. A build can even list the format and still
// not read it, the way "AVCI HEIC ---" is listed here without a delegate, so
// only the mode column settles it.
func magickReadsHEIF(path string, env []string) bool {
	cmd := exec.Command(path, "-list", "format")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return heifIsReadable(string(out))
}

// heifIsReadable parses "magick -list format" output, which is tabular:
// name, module, mode, description. A native format carries a "*" suffix.
func heifIsReadable(listing string) bool {
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if !strings.EqualFold(strings.TrimSuffix(fields[0], "*"), "HEIC") {
			continue
		}
		for _, field := range fields[1:] {
			if magickMode.MatchString(field) {
				return strings.HasPrefix(field, "r")
			}
		}
	}
	return false
}

// ErrNoRawConverter is reported when a RAW file is uploaded to a machine with
// no decoder installed.
var ErrNoRawConverter = errors.New(
	"RAW files need a decoder on PATH: install libraw-bin (dcraw_emu), dcraw, or darktable")

// ErrNoHEIFConverter is reported when a HEIC/HEIF file is uploaded to a
// machine with no decoder installed.
var ErrNoHEIFConverter = errors.New(
	"HEIC files need a decoder: install libheif (heif-convert) or ImageMagick built with HEIC support")

// missingDecoder explains a family the machine cannot decode.
func missingDecoder(family decodeFamily) error {
	if family == familyHEIF {
		return ErrNoHEIFConverter
	}
	return ErrNoRawConverter
}

// prepareSource passes images Hugin can read straight through and decodes the
// ones it cannot into a file that it can. It returns the path to use as a
// stitch input.
//
// tools supplies the decoder: its name selects the command line to build, and
// its resolved location is what actually gets run, since a decoder installed
// alongside Hugin may not be on PATH.
func prepareSource(path, dir string, tools Toolchain) (string, error) {
	family, needed := needsDecode[strings.ToLower(filepath.Ext(path))]
	if !needed {
		return path, nil
	}
	conv, ok := converterFor(family, tools.DecoderFor(family))
	if !ok {
		return "", missingDecoder(family)
	}

	base := filepath.Base(path)
	dst := filepath.Join(dir,
		strings.TrimSuffix(base, filepath.Ext(base))+"_decoded"+conv.outExt())
	cmd := exec.Command(tools.Path(conv.name), conv.args(path, dst)...)
	cmd.Env = tools.Env()

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

// converterFor finds the named converter within a family's chain.
func converterFor(family decodeFamily, name string) (sourceConverter, bool) {
	if name == "" {
		return sourceConverter{}, false
	}
	for _, entry := range decodeFamilies {
		if entry.family != family {
			continue
		}
		for _, conv := range entry.converters {
			if conv.name == name {
				return conv, true
			}
		}
	}
	return sourceConverter{}, false
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
