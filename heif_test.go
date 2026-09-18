package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// Real "magick -list format" rows. AVCI is listed with the HEIC module but
// carries no delegate, which is exactly the case a name-only check gets wrong.
const magickListing = `   Format  Module    Mode  Description
     AVCI  HEIC      ---   AVC Image File Format (1.19.8)
     AVIF  HEIC      rw+   AV1 Image File Format (1.19.8)
     HEIC  HEIC      rw+   High Efficiency Image Format (1.19.8)
     HEIF  HEIC      rw+   High Efficiency Image Format (1.19.8)
    JPEG* JPEG      rw-   Joint Photographic Experts Group JFIF format
`

func TestHEIFIsReadable(t *testing.T) {
	if !heifIsReadable(magickListing) {
		t.Error("a build listing HEIC as rw+ should be usable")
	}
}

func TestHEIFIsReadableRejectsDelegatelessBuild(t *testing.T) {
	// The same table from a build without the delegate: listed, not readable.
	listing := strings.Replace(magickListing,
		"     HEIC  HEIC      rw+", "     HEIC  HEIC      ---", 1)
	if heifIsReadable(listing) {
		t.Error("HEIC listed as --- means the delegate is missing")
	}
}

func TestHEIFIsReadableHandlesNativeSuffix(t *testing.T) {
	listing := strings.Replace(magickListing,
		"     HEIC  HEIC ", "    HEIC* HEIC ", 1)
	if !heifIsReadable(listing) {
		t.Error(`a "HEIC*" row should still count`)
	}
}

func TestHEIFIsReadableWithoutHEIC(t *testing.T) {
	if heifIsReadable("   Format  Module    Mode  Description\n    JPEG* JPEG      rw-   JFIF\n") {
		t.Error("a build with no HEIC row cannot decode HEIC")
	}
}

func TestConverterForStaysWithinItsFamily(t *testing.T) {
	if _, ok := converterFor(familyHEIF, "dcraw"); ok {
		t.Error("a RAW decoder must not be offered for HEIF")
	}
	if _, ok := converterFor(familyRaw, "dcraw"); !ok {
		t.Error("dcraw belongs to the RAW family")
	}
	if _, ok := converterFor(familyHEIF, ""); ok {
		t.Error("an empty name means no decoder was found")
	}
}

// heif-convert cannot write TIFF, so its entry has to ask for PNG while the
// rest of the chain takes the default.
func TestConverterOutputExtensions(t *testing.T) {
	conv, ok := converterFor(familyHEIF, "heif-convert")
	if !ok {
		t.Fatal("heif-convert should be in the HEIF chain")
	}
	if got := conv.outExt(); got != ".png" {
		t.Errorf("heif-convert writes %q, want .png", got)
	}
	raw, ok := converterFor(familyRaw, "dcraw")
	if !ok {
		t.Fatal("dcraw should be in the RAW chain")
	}
	if got := raw.outExt(); got != ".tif" {
		t.Errorf("default extension is %q, want .tif", got)
	}
}

func TestNeedsDecodeCoversHEIF(t *testing.T) {
	for _, ext := range []string{".heic", ".heif"} {
		if needsDecode[ext] != familyHEIF {
			t.Errorf("%s should belong to the HEIF family", ext)
		}
		if !allowedExts[ext] {
			t.Errorf("%s should be an accepted upload", ext)
		}
	}
	if _, needed := needsDecode[".jpg"]; needed {
		t.Error("Hugin reads JPEG directly")
	}
}

func TestCanDecode(t *testing.T) {
	none := Toolchain{Decoders: map[string]string{}}
	if none.CanDecode(".heic") {
		t.Error("no decoder means HEIC cannot be read")
	}
	if !none.CanDecode(".jpg") {
		t.Error("JPEG needs no decoder")
	}

	withHEIF := Toolchain{Decoders: map[string]string{string(familyHEIF): "heif-convert"}}
	if !withHEIF.CanDecode(".HEIC") {
		t.Error("the check should ignore case")
	}
	if withHEIF.CanDecode(".cr2") {
		t.Error("a HEIF decoder does not make RAW readable")
	}
}

// A machine with no decoder must say which format it cannot read, rather than
// letting the file through to fail deep in the stitch.
func TestPrepareSourceReportsTheRightMissingDecoder(t *testing.T) {
	bare := Toolchain{Decoders: map[string]string{}}

	if _, err := prepareSource(filepath.Join(t.TempDir(), "a.heic"), t.TempDir(), bare); !errors.Is(err, ErrNoHEIFConverter) {
		t.Errorf("HEIC error = %v, want ErrNoHEIFConverter", err)
	}
	if _, err := prepareSource(filepath.Join(t.TempDir(), "a.cr2"), t.TempDir(), bare); !errors.Is(err, ErrNoRawConverter) {
		t.Errorf("RAW error = %v, want ErrNoRawConverter", err)
	}

	// A format Hugin reads itself is passed through untouched.
	jpg := filepath.Join(t.TempDir(), "a.jpg")
	got, err := prepareSource(jpg, t.TempDir(), bare)
	if err != nil || got != jpg {
		t.Errorf("prepareSource(%q) = %q, %v; want it passed through", jpg, got, err)
	}
}
