package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestParseAngle(t *testing.T) {
	for raw, want := range map[string]float64{
		"0":     0,
		"":      0,
		"2.5":   2.5,
		"-1.25": -1.25,
		" 3 ":   3,
		"abc":   0,   // unparseable is no rotation, never an error
		"400":   180, // clamped
		"-400":  -180,
		"180":   180,
	} {
		if got := parseAngle(raw); got != want {
			t.Errorf("parseAngle(%q) = %v, want %v", raw, got, want)
		}
	}
	if got := parseAngle("NaN"); got != 0 || math.IsNaN(got) {
		t.Errorf("parseAngle(NaN) = %v, want 0", got)
	}
}

func TestRotationIsZero(t *testing.T) {
	if !(Rotation{}).IsZero() {
		t.Error("the zero rotation should report itself as zero")
	}
	for _, r := range []Rotation{{Roll: 0.5}, {Pitch: -1}, {Yaw: 90}} {
		if r.IsZero() {
			t.Errorf("%+v should not report as zero", r)
		}
	}
}

// pano_modify takes the angles in yaw,pitch,roll order; getting that wrong
// would tilt a panorama sideways when the user asked to nudge it up.
func TestRotationArgOrder(t *testing.T) {
	got := Rotation{Yaw: 1, Pitch: 2, Roll: 3}.Arg()
	if want := "--rotate=1,2,3"; got != want {
		t.Errorf("Arg() = %q, want %q", got, want)
	}
	if got := (Rotation{Roll: -2.5}).Arg(); got != "--rotate=0,0,-2.5" {
		t.Errorf("Arg() = %q", got)
	}
}

func TestParseRotationFromFields(t *testing.T) {
	got := parseRotation(map[string]string{"yaw": "10", "pitch": "-2", "roll": "0.5"})
	want := Rotation{Yaw: 10, Pitch: -2, Roll: 0.5}
	if got != want {
		t.Errorf("parseRotation = %+v, want %+v", got, want)
	}
	if got := parseRotation(map[string]string{}); !got.IsZero() {
		t.Errorf("missing fields should give no rotation, got %+v", got)
	}
}

func TestOptimiserArgsCombinations(t *testing.T) {
	for _, tc := range []struct {
		level, photometric bool
		want               string
	}{
		{false, false, "-a"},
		{true, false, "-a -l"},
		{false, true, "-a -m"},
		{true, true, "-a -l -m"}, // what Hugin's own Align button runs
	} {
		args := optimiserArgs(tc.level, tc.photometric, "/tmp/o.pto", "/tmp/i.pto")
		flags := strings.Join(args[:len(args)-3], " ")
		if flags != tc.want {
			t.Errorf("level=%v photometric=%v gave %q, want %q",
				tc.level, tc.photometric, flags, tc.want)
		}
		if args[len(args)-3] != "-o" || args[len(args)-1] != "/tmp/i.pto" {
			t.Errorf("args tail is malformed: %v", args)
		}
	}
}

func TestStitchRecordsPhotometricAndRotation(t *testing.T) {
	server := newTestServer(t, true)
	req := stitchRequest(t,
		map[string][]byte{"a.jpg": []byte("x"), "b.jpg": []byte("y")},
		map[string]string{
			"photometric": "0",
			"roll":        "1.5",
			"pitch":       "-3",
			"yaw":         "0",
		})
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	job, ok := server.jobs.Get(payload.JobID)
	if !ok {
		t.Fatal("job was not registered")
	}
	t.Cleanup(func() { os.RemoveAll(job.Dir) })

	if job.Photometric {
		t.Error("photometric should be off when the client sends 0")
	}
	if want := (Rotation{Roll: 1.5, Pitch: -3}); job.Rotation != want {
		t.Errorf("rotation = %+v, want %+v", job.Rotation, want)
	}
}

// Both corrections stay on for a client that does not know about them.
func TestStitchDefaultsBothCorrectionsOn(t *testing.T) {
	server := newTestServer(t, true)
	req := stitchRequest(t, map[string][]byte{"a.jpg": []byte("x"), "b.jpg": []byte("y")}, nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	var payload struct {
		JobID string `json:"job_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &payload)
	job, ok := server.jobs.Get(payload.JobID)
	if !ok {
		t.Fatal("job was not registered")
	}
	t.Cleanup(func() { os.RemoveAll(job.Dir) })

	if !job.Level || !job.Photometric {
		t.Errorf("level = %v, photometric = %v; both should default on", job.Level, job.Photometric)
	}
	if !job.Rotation.IsZero() {
		t.Errorf("rotation should default to none, got %+v", job.Rotation)
	}
}
