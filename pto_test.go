package main

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writePTO puts a project file in a temp dir and returns its path.
func writePTO(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "project.pto")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPTOField(t *testing.T) {
	// A real "i" line from pto_gen 2023.0.
	tokens := []string{
		"i", "w800", "h600", "f0", "v50", "Ra0", "Rb0", "Rc0", "Rd0", "Re0",
		"Eev0", "Er1", "Eb1", "r0", "p0", "y-12.5", "TrX0", "TrY0", "TrZ0",
		"Tpy0", "Tpp0", "j0", "a0", "b0", "c0", "d0", "e0", "g0", "t0",
		"Va1", "Vb0", "Vc0", "Vd0", "Vx0", "Vy0", "Vm5", `n"a.jpg"`,
	}
	for _, tc := range []struct {
		name, want string
	}{
		{"w", "800"},
		{"h", "600"},
		{"f", "0"},
		{"v", "50"},    // must not match Va1/Vb0/Vm5
		{"y", "-12.5"}, // must not match Vy0/TrY0/Tpy0
	} {
		got, ok := ptoField(tokens, tc.name)
		if !ok || got != tc.want {
			t.Errorf("ptoField(%q) = %q, %v; want %q, true", tc.name, got, ok, tc.want)
		}
	}
	if _, ok := ptoField(tokens, "q"); ok {
		t.Error("ptoField found a parameter that is not present")
	}
}

func TestComputeHFOVSingleImage(t *testing.T) {
	path := writePTO(t, `# hugin project file
p f2 w3000 h1500 v360
i w800 h600 f0 v50 r0 p0 y0 n"a.jpg"
`)
	got, ok := computeHFOV(path)
	if !ok {
		t.Fatal("computeHFOV reported no usable images")
	}
	if math.Abs(got-50) > 1e-9 {
		t.Errorf("hfov = %v, want 50", got)
	}
}

// A linked lens ("v=0") means "same field of view as image 0". Reading it as
// the literal number zero makes the frame contribute no width at all, which
// silently under-reports how much of the scene the panorama covers.
func TestComputeHFOVResolvesLinkedLens(t *testing.T) {
	path := writePTO(t, `# hugin project file
i w800 h600 f0 v50 r0 p0 y0 n"a.jpg"
i w800 h600 f0 v=0 r0 p0 y40 n"b.jpg"
`)
	got, ok := computeHFOV(path)
	if !ok {
		t.Fatal("computeHFOV reported no usable images")
	}
	// Spans are [-25,25] and [15,65], so total coverage is 90 degrees.
	if math.Abs(got-90) > 1e-9 {
		t.Errorf("hfov = %v, want 90 (linked lens resolved to 50 degrees)", got)
	}
}

func TestComputeHFOVVerticalFOV(t *testing.T) {
	// f1 means the angle is vertical; a 4:3 frame widens 30 vertical degrees.
	path := writePTO(t, `i w800 h600 f1 v30 r0 p0 y0 n"a.jpg"`)
	got, ok := computeHFOV(path)
	if !ok {
		t.Fatal("computeHFOV reported no usable images")
	}
	want := 2 * math.Atan(math.Tan(30*math.Pi/180/2)*800/600) * 180 / math.Pi
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("hfov = %v, want %v", got, want)
	}
	if got <= 30 {
		t.Errorf("hfov = %v, expected the horizontal angle to exceed the vertical 30", got)
	}
}

func TestComputeHFOVNoImages(t *testing.T) {
	path := writePTO(t, "# hugin project file\np f2 w3000 h1500 v360\n")
	if _, ok := computeHFOV(path); ok {
		t.Error("computeHFOV should report failure when there are no image lines")
	}
	if _, ok := computeHFOV(filepath.Join(t.TempDir(), "missing.pto")); ok {
		t.Error("computeHFOV should report failure for a missing file")
	}
}

func TestResolveProjectionExplicitChoiceWins(t *testing.T) {
	// A 360-degree sweep that would auto-select equirectangular.
	path := writePTO(t, `i w800 h600 f0 v90 r0 p0 y-135 n"a.jpg"
i w800 h600 f0 v90 r0 p0 y135 n"b.jpg"`)
	for name, want := range map[string]string{
		"rectilinear":     projRectilinear,
		"cylindrical":     projCylindrical,
		"equirectangular": projEquirectangular,
	} {
		if got := resolveProjection(path, name); got != want {
			t.Errorf("resolveProjection(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestResolveProjectionAuto(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fov    float64
		yaw    float64
		expect string
	}{
		{"narrow sweep stays rectilinear", 60, 0, projRectilinear},
		{"exactly 100 is still rectilinear", 100, 0, projRectilinear},
		{"wide sweep goes cylindrical", 180, 0, projCylindrical},
		{"exactly 240 is still cylindrical", 240, 0, projCylindrical},
		{"near-360 goes equirectangular", 300, 0, projEquirectangular},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writePTO(t, "i w800 h600 f0 v"+formatFloat(tc.fov)+" r0 p0 y"+formatFloat(tc.yaw)+` n"a.jpg"`)
			if got := resolveProjection(path, "auto"); got != tc.expect {
				t.Errorf("resolveProjection(auto) = %q, want %q", got, tc.expect)
			}
		})
	}
}

// With no measurable images, auto falls back to Hugin's own default.
func TestResolveProjectionAutoFallback(t *testing.T) {
	path := writePTO(t, "# no image lines here\n")
	if got := resolveProjection(path, "auto"); got != projCylindrical {
		t.Errorf("resolveProjection(auto) = %q, want the cylindrical fallback", got)
	}
}

func TestNormaliseProjection(t *testing.T) {
	for input, want := range map[string]string{
		"auto":            "auto",
		"cylindrical":     "cylindrical",
		"equirectangular": "equirectangular",
		"":                "auto",
		"fisheye":         "auto",
		"CYLINDRICAL":     "auto", // callers lowercase before this point
	} {
		if got := normaliseProjection(input); got != want {
			t.Errorf("normaliseProjection(%q) = %q, want %q", input, got, want)
		}
	}
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
