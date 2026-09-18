package main

// Output formats exposed in the UI, mapped to pano_modify arguments.
//
//   - LDR formats are encoded for display/storage (TIFF, PNG lossless; JPEG lossy).
//   - HDR formats keep the full stitch precision as linear floating point in a
//     32-bit float container. Hugin emits a companion LDR JPEG alongside them so
//     the browser still gets a real preview.
type Format struct {
	Label string
	Ext   string
	MIME  string
	// Basenames Hugin may use for this format, most likely first.
	Basenames []string
	// Args appended to the pano_modify command line.
	Args []string
	// Quality is true when the format honours the JPEG quality slider.
	Quality bool
	// HDR is true for 32-bit float output, which gets an LDR preview companion.
	HDR bool
	// EXIF is true when metadata transfer into the output makes sense.
	EXIF bool
}

// DefaultFormat is used when the client sends nothing or sends an unknown key.
const DefaultFormat = "tif"

// MasterFormat is what every LDR stitch actually produces. The chosen format
// is then applied when the panorama is downloaded, which makes format and JPEG
// quality decisions that can be changed afterwards without re-stitching and
// without ever compressing an already lossy image a second time.
const MasterFormat = "tif"

// Convertible reports whether this format can be produced by re-encoding a
// finished panorama. HDR output carries 32-bit floating point data that only
// the stitch itself produces, so it has to be chosen beforehand.
func (f Format) Convertible() bool { return !f.HDR }

// masterFor returns the format the pipeline should actually stitch to.
func masterFor(f Format) Format {
	if f.HDR {
		return f
	}
	return formats[MasterFormat]
}

var formats = map[string]Format{
	"tif": {
		Label: "TIFF", Ext: ".tif", MIME: "image/tiff",
		Basenames: []string{"result.tif", "result.tiff"},
		Args:      []string{"--ldr-file=TIF"},
		EXIF:      true,
	},
	"png": {
		Label: "PNG", Ext: ".png", MIME: "image/png",
		Basenames: []string{"result.png"},
		Args:      []string{"--ldr-file=PNG"},
		EXIF:      true,
	},
	"jpg": {
		Label: "JPEG", Ext: ".jpg", MIME: "image/jpeg",
		Basenames: []string{"result.jpg", "result.jpeg"},
		Args:      []string{"--ldr-file=JPG"},
		Quality:   true, EXIF: true,
	},
	"tif_hdr": {
		Label: "HDR TIFF", Ext: ".tif", MIME: "image/tiff",
		Basenames: []string{"result_hdr.tif"},
		Args:      []string{"--output-type=HDR,NORMAL", "--ldr-file=JPG", "--hdr-file=TIF"},
		HDR:       true,
	},
	"exr": {
		Label: "OpenEXR", Ext: ".exr", MIME: "image/x-exr",
		Basenames: []string{"result_hdr.exr"},
		Args:      []string{"--output-type=HDR,NORMAL", "--ldr-file=JPG", "--hdr-file=EXR"},
		HDR:       true,
	},
}

// lookupFormat resolves a client-supplied format key, falling back to TIFF.
func lookupFormat(key string) (string, Format) {
	if f, ok := formats[key]; ok {
		return key, f
	}
	return DefaultFormat, formats[DefaultFormat]
}

// Hugin projection codes (pto "p fX" value): rectilinear, cylindrical and
// equirectangular are the parameter-free workhorses.
const (
	projRectilinear     = "0"
	projCylindrical     = "2"
	projEquirectangular = "3"
)

// projections maps the UI's choices to a Hugin code. "auto" has no fixed code:
// it is resolved from the measured field of view once alignment is done.
var projections = map[string]string{
	"auto":            "",
	"rectilinear":     projRectilinear,
	"cylindrical":     projCylindrical,
	"equirectangular": projEquirectangular,
}

var projectionFriendly = map[string]string{
	projRectilinear:     "rectilinear",
	projCylindrical:     "cylindrical",
	projEquirectangular: "equirectangular",
}

// normaliseProjection resolves a client-supplied projection name, falling back
// to "auto".
func normaliseProjection(name string) string {
	if _, ok := projections[name]; ok {
		return name
	}
	return "auto"
}
