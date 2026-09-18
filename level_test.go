package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Levelling is on unless the client explicitly disables it, so an older client
// or a bare curl request still gets a level horizon.
func TestParseLevel(t *testing.T) {
	for raw, want := range map[string]bool{
		"":      true, // field absent
		"1":     true,
		"true":  true,
		"on":    true,
		"0":     false,
		"false": false,
		"off":   false,
		"no":    false,
		"OFF":   false,
		" 0 ":   false,
	} {
		if got := parseLevel(raw); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestStitchRecordsLevelChoice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]string
		want   bool
	}{
		{"explicitly on", map[string]string{"level": "1"}, true},
		{"explicitly off", map[string]string{"level": "0"}, false},
		{"omitted defaults to on", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer(t, true)
			req := stitchRequest(t,
				map[string][]byte{"a.jpg": []byte("x"), "b.jpg": []byte("y")}, tc.fields)
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)

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

			if job.Level != tc.want {
				t.Errorf("Level = %v, want %v", job.Level, tc.want)
			}
		})
	}
}

// With no cleanup tools installed the pipeline must hand the original project
// straight to the optimiser rather than losing the stitch.
func TestCleanControlPointsWithoutTools(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "cp.pto")
	if err := os.WriteFile(input, []byte("# hugin project file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := NewPipeline(Toolchain{Optional: map[string]bool{}})
	job := &Job{Dir: dir}

	if got := p.cleanControlPoints(context.Background(), job, input); got != input {
		t.Errorf("cleanControlPoints = %q, want the untouched input %q", got, input)
	}
	if got := job.Status().Progress; got != phases[phaseClean].end {
		t.Errorf("progress = %v, want the phase to still complete at %v",
			got, phases[phaseClean].end)
	}
}

// A cleanup tool that is present but fails must not abort the job.
func TestCleanControlPointsSurvivesAFailingTool(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "cp.pto")
	if err := os.WriteFile(input, []byte("# hugin project file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The tools are advertised as present but are not really on PATH, so both
	// invocations fail the way a broken install would.
	p := NewPipeline(Toolchain{Optional: map[string]bool{
		"cpclean": true, "celeste_standalone": true,
	}})
	job := &Job{Dir: dir}

	if got := p.cleanControlPoints(context.Background(), job, input); got != input {
		t.Errorf("cleanControlPoints = %q, want the input to be kept on failure", got)
	}
	if job.Status().State == StateError {
		t.Error("a failing cleanup tool must not fail the job")
	}
}

// The optimiser arguments are what actually fix the user-visible tilt, so
// assert on them directly.
func TestLevellingChangesOptimiserArguments(t *testing.T) {
	for _, tc := range []struct {
		level    bool
		wantFlag bool
	}{
		{true, true},
		{false, false},
	} {
		args := optimiserArgs(tc.level, false, "/tmp/out.pto", "/tmp/in.pto")
		found := false
		for _, a := range args {
			if a == "-l" {
				found = true
			}
		}
		if found != tc.wantFlag {
			t.Errorf("level=%v produced %v; -l present = %v, want %v",
				tc.level, args, found, tc.wantFlag)
		}
		if args[0] != "-a" {
			t.Errorf("args = %v, want auto-align first", args)
		}
	}
}

func TestToolchainReportsCleanupTools(t *testing.T) {
	tc := DetectToolchain()
	for _, name := range []string{"cpclean", "celeste_standalone"} {
		if _, ok := tc.Optional[name]; !ok {
			t.Errorf("/health does not report %q", name)
		}
	}
}

// The phase table gained a step; it must still tile the bar without gaps.
func TestPhaseIndicesMatchTheTable(t *testing.T) {
	if phases[phaseClean].message == "" || phaseStitch != len(phases)-1 {
		t.Fatalf("phase indices are out of step with the table (%d phases)", len(phases))
	}
	if phaseClean >= phaseOptimise || phaseOptimise >= phaseModify || phaseModify >= phaseStitch {
		t.Error("phase indices are not in pipeline order")
	}
}
