package main

import (
	"bufio"
	"math"
	"os"
	"strconv"
	"strings"
)

// ptoImage holds the few "i" line parameters that bear on the output geometry.
type ptoImage struct {
	width, height int
	fovType       string  // pto "f": 0 = horizontal fov, 1 = vertical fov
	fov           float64 // pto "v"
	yaw           float64 // pto "y"
	ok            bool
}

// hfov returns the image's horizontal field of view in degrees. Hugin stores
// the angle as vertical for some lens types ("f1"), so derive it from the
// aspect ratio in that case.
func (im ptoImage) hfov() float64 {
	if im.fovType == "1" && im.height != 0 {
		half := math.Tan(im.fov*math.Pi/180/2) * float64(im.width) / float64(im.height)
		return 2 * math.Atan(half) * 180 / math.Pi
	}
	return im.fov
}

// ptoField pulls a single-letter parameter out of an "i" line's tokens.
//
// Tokens are letter-prefixed values such as "w800", "v50" or "y-12.5". Matching
// on the prefix rather than the token's position keeps this working across
// Hugin versions, which have changed the order of the trailing parameters.
// Only exact single-letter matches count, so the lowercase "v" (field of view)
// is never confused with "Va"/"Vb" (vignetting) or "TrX" (translation).
func ptoField(tokens []string, name string) (string, bool) {
	for _, tok := range tokens {
		if len(tok) > len(name) && strings.HasPrefix(tok, name) {
			rest := tok[len(name):]
			// The remainder must look like a number or a link, not more letters.
			switch rest[0] {
			case '=', '-', '+', '.', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
				return rest, true
			}
		}
	}
	return "", false
}

// resolveLink reads a pto value that may be linked to an earlier image.
//
// Hugin writes "v=0" to mean "this image's field of view is the same as image
// 0's", which is how it represents a shared lens. Reading that as the literal
// number 0 makes a linked frame look like it covers no angle at all.
func resolveLink(raw string, prior []ptoImage, pick func(ptoImage) float64) (float64, bool) {
	if strings.HasPrefix(raw, "=") {
		idx, err := strconv.Atoi(raw[1:])
		if err != nil || idx < 0 || idx >= len(prior) || !prior[idx].ok {
			return 0, false
		}
		return pick(prior[idx]), true
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parsePTOImages reads the "i" (image) lines out of a Hugin project file.
func parsePTOImages(r *bufio.Scanner) []ptoImage {
	var images []ptoImage
	for r.Scan() {
		line := r.Text()
		if !strings.HasPrefix(line, "i ") {
			continue
		}
		tokens := strings.Fields(line)
		im := ptoImage{}

		w, okW := ptoField(tokens, "w")
		h, okH := ptoField(tokens, "h")
		v, okV := ptoField(tokens, "v")
		if !okW || !okH || !okV {
			images = append(images, im) // keep the index aligned for links
			continue
		}
		wi, errW := strconv.Atoi(w)
		hi, errH := strconv.Atoi(h)
		if errW != nil || errH != nil {
			images = append(images, im)
			continue
		}
		im.width, im.height = wi, hi

		fov, okFov := resolveLink(v, images, func(p ptoImage) float64 { return p.fov })
		if !okFov {
			images = append(images, im)
			continue
		}
		im.fov = fov

		if f, ok := ptoField(tokens, "f"); ok {
			if strings.HasPrefix(f, "=") {
				if idx, err := strconv.Atoi(f[1:]); err == nil && idx >= 0 && idx < len(images) {
					im.fovType = images[idx].fovType
				}
			} else {
				im.fovType = f
			}
		}
		if y, ok := ptoField(tokens, "y"); ok {
			if yaw, ok := resolveLink(y, images, func(p ptoImage) float64 { return p.yaw }); ok {
				im.yaw = yaw
			}
		}

		im.ok = true
		images = append(images, im)
	}
	return images
}

// computeHFOV reports the total horizontal field of view covered by the
// aligned images, in degrees. The second return value is false when the
// project has no usable image lines.
func computeHFOV(ptoPath string) (float64, bool) {
	f, err := os.Open(ptoPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	images := parsePTOImages(scanner)

	var lo, hi float64
	seen := false
	for _, im := range images {
		if !im.ok {
			continue
		}
		half := im.hfov() / 2
		start, end := im.yaw-half, im.yaw+half
		if !seen {
			lo, hi = start, end
			seen = true
			continue
		}
		lo = math.Min(lo, start)
		hi = math.Max(hi, end)
	}
	if !seen {
		return 0, false
	}
	return hi - lo, true
}

// resolveProjection picks the projection code for the finished panorama.
//
// An explicit choice always wins. "auto" measures the sweep the aligned images
// actually cover and picks the projection that distorts it least.
func resolveProjection(ptoPath, projection string) string {
	if code := projections[projection]; code != "" {
		return code
	}
	hfov, ok := computeHFOV(ptoPath)
	if !ok {
		return projCylindrical // Hugin's own default
	}
	switch {
	case hfov <= 100: // moderate sweep: everything stays straight
		return projRectilinear
	case hfov <= 240: // wide panorama: cylindrical kills edge stretch
		return projCylindrical
	default: // near-360
		return projEquirectangular
	}
}
