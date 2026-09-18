package main

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// enblend redraws one progress line with carriage returns rather than writing
// a line per update. Splitting only on LF would hide every percentage until
// the tool exited, freezing the bar through the longest phase of the job.
func TestScanProgressLinesSplitsOnCarriageReturn(t *testing.T) {
	input := "loading\r10%\r50%\r100%\ndone\r\ntrailing"
	scanner := bufio.NewScanner(strings.NewReader(input))
	scanner.Split(scanProgressLines)

	var got []string
	for scanner.Scan() {
		got = append(got, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"loading", "10%", "50%", "100%", "done", "trailing"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestReportPercentMapsIntoBlendPhase(t *testing.T) {
	p := NewPipeline(Toolchain{})
	blend := phases[phaseStitch]

	for _, tc := range []struct {
		line string
		want float64
	}{
		{"0%", blend.start},
		{"50%", blend.start + (blend.end-blend.start)*0.5},
		{"100%", blend.end},
		{"  83.5 % done", blend.start + (blend.end-blend.start)*0.835},
	} {
		job := &Job{}
		p.reportPercent(job, phaseStitch, tc.line)
		if got := job.Status().Progress; math.Abs(got-round4(tc.want)) > 1e-9 {
			t.Errorf("line %q -> progress %v, want %v", tc.line, got, round4(tc.want))
		}
	}
}

// Progress must never escape the slice of the bar the phase owns, or a tool
// reporting "150%" would drive the bar past the phases that follow it.
func TestReportPercentClampsAndIgnoresOtherPhases(t *testing.T) {
	p := NewPipeline(Toolchain{})

	job := &Job{}
	p.reportPercent(job, phaseStitch, "150%")
	if got := job.Status().Progress; got > phases[phaseStitch].end {
		t.Errorf("progress %v exceeded the phase ceiling %v", got, phases[phaseStitch].end)
	}

	quiet := &Job{}
	p.reportPercent(quiet, 2, "50%") // cpfind prints percentages we cannot trust
	if got := quiet.Status().Progress; got != 0 {
		t.Errorf("progress = %v, want 0 for a non-blend phase", got)
	}

	noNumber := &Job{}
	p.reportPercent(noNumber, phaseStitch, "no percentage here")
	if got := noNumber.Status().Progress; got != 0 {
		t.Errorf("progress = %v, want 0 when there is nothing to parse", got)
	}
}

func TestPhasesCoverTheBarInOrder(t *testing.T) {
	for i, ph := range phases {
		if ph.start > ph.end {
			t.Errorf("phase %d runs backwards: %v -> %v", i, ph.start, ph.end)
		}
		if i > 0 && phases[i-1].end != ph.start {
			t.Errorf("gap between phase %d (ends %v) and %d (starts %v)",
				i-1, phases[i-1].end, i, ph.start)
		}
	}
	if phases[0].start != 0 {
		t.Errorf("first phase starts at %v, want 0", phases[0].start)
	}
	if last := phases[len(phases)-1].end; last >= 1 {
		t.Errorf("last phase ends at %v; the bar should only reach 1 when the job finishes", last)
	}
}

// touch writes a file of the given size so locateOutput can weigh it.
func touch(t *testing.T, dir, name string, size int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLocateOutputPrefersTheExactName(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "result.tif", 10)
	touch(t, dir, "result_hdr.tif", 5000) // larger, but not what TIFF asked for

	_, format := lookupFormat("tif")
	if got := locateOutput(dir, format); filepath.Base(got) != "result.tif" {
		t.Errorf("locateOutput = %q, want result.tif", got)
	}
}

func TestLocateOutputFallsBackToLargest(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "result_0000.tif", 100)
	touch(t, dir, "result_0001.tif", 900)

	_, format := lookupFormat("tif")
	if got := locateOutput(dir, format); filepath.Base(got) != "result_0001.tif" {
		t.Errorf("locateOutput = %q, want the largest stitch file", got)
	}
}

func TestLocateOutputIgnoresEmptyFiles(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "result.tif", 0)

	_, format := lookupFormat("tif")
	if got := locateOutput(dir, format); got != "" {
		t.Errorf("locateOutput = %q, want empty for a zero-byte result", got)
	}
}

// An HDR job previews from the LDR companion Hugin writes beside the float
// image, because a 32-bit float TIFF cannot be decoded for the browser.
func TestLocatePreviewSource(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "result_hdr.exr", 4000)
	touch(t, dir, "result.jpg", 200)

	_, hdr := lookupFormat("exr")
	if got := locatePreviewSource(dir, hdr); filepath.Base(got) != "result.jpg" {
		t.Errorf("locatePreviewSource = %q, want result.jpg", got)
	}

	_, ldr := lookupFormat("tif")
	if got := locatePreviewSource(dir, ldr); got != "" {
		t.Errorf("locatePreviewSource = %q, want empty for an LDR format", got)
	}
}

func TestRingBufferKeepsTheTail(t *testing.T) {
	r := newRingBuffer(3)
	for _, line := range []string{"a", "b", "c", "d", "e"} {
		r.push(line)
	}
	got := strings.Join(r.last(3), ",")
	if got != "c,d,e" {
		t.Errorf("tail = %q, want c,d,e", got)
	}
	if got := strings.Join(r.last(10), ","); got != "c,d,e" {
		t.Errorf("asking for more lines than were kept = %q, want c,d,e", got)
	}
}
