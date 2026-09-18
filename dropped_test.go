package main

import (
	"path/filepath"
	"testing"
)

// realPTO is a project file as pto_gen actually writes it, taken from a run
// where a HEIC input was silently ignored: two "i" lines for three inputs.
const realPTO = `# hugin project file
#hugin_ptoversion 2
p f2 w3000 h1500 v360  k0 E0 R0 n"TIFF_m c:LZW r:CROP"
m i0

i w640 h480 f0 v50 Ra0 Rb0 Rc0 Rd0 Re0 Eev0 Er1 Eb1 r0 p0 y0 TrX0 TrY0 TrZ0 Tpy0 Tpp0 j0 a0 b0 c0 d0 e0 g0 t0 Va1 Vb0 Vc0 Vd0 Vx0 Vy0  Vm5 n"a.jpg"
i w640 h480 f0 v=0 Ra=0 Rb=0 Rc=0 Rd=0 Re=0 Eev0 Er1 Eb1 r0 p0 y0 TrX0 TrY0 TrZ0 Tpy0 Tpp0 j0 a=0 b=0 c=0 d=0 e=0 g=0 t=0 Va=0 Vb=0 Vc=0 Vd=0 Vx=0 Vy=0  Vm5 n"b.jpg"

v p0 r0 y0
v
`

func TestPTOImageNames(t *testing.T) {
	got, err := ptoImageNames(writePTO(t, realPTO))
	if err != nil {
		t.Fatalf("ptoImageNames: %v", err)
	}
	want := []string{"a.jpg", "b.jpg"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("name %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A file name with spaces must survive: the "n" value is quoted precisely so
// splitting the line on whitespace is not safe.
func TestPTOImageNamesKeepsSpaces(t *testing.T) {
	body := "i w1 h1 f0 v50 n\"holiday shot 1.jpg\"\n"
	got, err := ptoImageNames(writePTO(t, body))
	if err != nil {
		t.Fatalf("ptoImageNames: %v", err)
	}
	if len(got) != 1 || got[0] != "holiday shot 1.jpg" {
		t.Fatalf("got %v, want [holiday shot 1.jpg]", got)
	}
}

// The bug this guards: pto_gen ignores an image it cannot read and still
// exits 0, so a source that never reached the project file was dropped.
func TestMissingFromPTOReportsDroppedSource(t *testing.T) {
	pto := writePTO(t, realPTO)
	dir := filepath.Dir(pto)
	sources := []string{
		filepath.Join(dir, "a.jpg"),
		filepath.Join(dir, "sample.heic"),
		filepath.Join(dir, "b.jpg"),
	}

	dropped, err := missingFromPTO(pto, sources)
	if err != nil {
		t.Fatalf("missingFromPTO: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != sources[1] {
		t.Fatalf("got %v, want [%s]", dropped, sources[1])
	}
}

func TestMissingFromPTOClean(t *testing.T) {
	pto := writePTO(t, realPTO)
	dir := filepath.Dir(pto)
	sources := []string{filepath.Join(dir, "a.jpg"), filepath.Join(dir, "b.jpg")}

	dropped, err := missingFromPTO(pto, sources)
	if err != nil {
		t.Fatalf("missingFromPTO: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("got %v, want none", dropped)
	}
}

func TestMissingFromPTOUnreadableFile(t *testing.T) {
	if _, err := missingFromPTO(filepath.Join(t.TempDir(), "absent.pto"), nil); err == nil {
		t.Fatal("expected an error for a missing project file")
	}
}

// Hugin records the path it was handed. A project written on Windows stores
// backslashes, so the base name has to be taken the same way uploadName takes
// it rather than by relying on the host's separator.
func TestMissingFromPTOWindowsPaths(t *testing.T) {
	body := "i w1 h1 f0 v50 n\"C:\\jobs\\abc\\a.jpg\"\n"
	pto := writePTO(t, body)

	dropped, err := missingFromPTO(pto, []string{filepath.Join("/srv/jobs/abc", "a.jpg")})
	if err != nil {
		t.Fatalf("missingFromPTO: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("got %v, want none: the pto records the same file", dropped)
	}
}
