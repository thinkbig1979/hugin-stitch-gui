package main

import (
	"image"
	"image/color"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/image/tiff"
)

// writeMaster writes a lossless TIFF master with a transparent margin, the
// shape the blender actually produces.
func writeMaster(t *testing.T, dir string) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 40, 30))
	for y := 0; y < 30; y++ {
		for x := 0; x < 40; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(x * 6), G: uint8(y * 8), B: 120, A: 255})
		}
	}
	img.Set(0, 0, color.NRGBA{R: 255, G: 0, B: 0, A: 0}) // outside the crop

	path := filepath.Join(dir, "result.tif")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := tiff.Encode(f, img, &tiff.Options{Compression: tiff.Deflate}); err != nil {
		t.Fatal(err)
	}
	return path
}

// Every LDR stitch produces a TIFF master, whatever format was asked for, so
// the choice can be changed later without re-stitching.
func TestMasterForAlwaysLosslessForLDR(t *testing.T) {
	for _, key := range []string{"tif", "png", "jpg"} {
		_, f := lookupFormat(key)
		if got := masterFor(f); got.Ext != ".tif" {
			t.Errorf("masterFor(%s) = %s, want the TIFF master", key, got.Ext)
		}
	}
	// HDR jobs stitch to their own format; there is nothing to convert from.
	for _, key := range []string{"tif_hdr", "exr"} {
		_, f := lookupFormat(key)
		if got := masterFor(f); got.Label != f.Label {
			t.Errorf("masterFor(%s) = %s, want it left alone", key, got.Label)
		}
	}
}

func TestConvertible(t *testing.T) {
	for key, want := range map[string]bool{
		"tif": true, "png": true, "jpg": true,
		"tif_hdr": false, "exr": false,
	} {
		if _, f := lookupFormat(key); f.Convertible() != want {
			t.Errorf("%s convertible = %v, want %v", key, f.Convertible(), want)
		}
	}
}

// A TIFF download is the master itself, so it keeps the blender's own LZW
// compression rather than being re-written by a package whose TIFF writer
// cannot produce readable LZW.
func TestEncodeToServesTheMasterUnchangedForItsOwnFormat(t *testing.T) {
	dir := t.TempDir()
	master := writeMaster(t, dir)

	got, err := encodeTo(master, dir, "tif", 90)
	if err != nil {
		t.Fatal(err)
	}
	if got != master {
		t.Errorf("encodeTo returned %q, want the master %q re-used", got, master)
	}
}

func TestEncodeToProducesReadableOutput(t *testing.T) {
	dir := t.TempDir()
	master := writeMaster(t, dir)

	for _, key := range []string{"png", "jpg"} {
		path, err := encodeTo(master, dir, key, 85)
		if err != nil {
			t.Fatalf("encodeTo(%s): %v", key, err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		img, format, err := image.Decode(f)
		f.Close()
		if err != nil {
			t.Fatalf("%s output is not readable: %v", key, err)
		}
		if img.Bounds().Dx() != 40 || img.Bounds().Dy() != 30 {
			t.Errorf("%s output is %v, want the master's size", key, img.Bounds())
		}
		if key == "jpg" && format != "jpeg" {
			t.Errorf("expected jpeg, got %s", format)
		}
	}
}

// JPEG cannot carry alpha, so the transparent margin has to be flattened
// rather than coming out as garbage.
func TestJPEGOutputIsOpaque(t *testing.T) {
	dir := t.TempDir()
	master := writeMaster(t, dir)

	path, err := encodeTo(master, dir, "jpg", 90)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0xffff {
		t.Errorf("corner alpha = %d, want fully opaque", a)
	}
}

// Re-encoding is cached, so switching back and forth costs nothing.
func TestEncodeToCachesByFormatAndQuality(t *testing.T) {
	dir := t.TempDir()
	master := writeMaster(t, dir)

	first, err := encodeTo(master, dir, "jpg", 60)
	if err != nil {
		t.Fatal(err)
	}
	again, err := encodeTo(master, dir, "jpg", 60)
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Errorf("same request produced %q then %q", first, again)
	}

	other, err := encodeTo(master, dir, "jpg", 95)
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Error("a different quality must not reuse the cached file")
	}
}

func TestEncodeToRefusesHDR(t *testing.T) {
	dir := t.TempDir()
	master := writeMaster(t, dir)

	for _, key := range []string{"tif_hdr", "exr"} {
		if _, err := encodeTo(master, dir, key, 90); err == nil {
			t.Errorf("encodeTo(%s) should refuse: HDR cannot be re-encoded", key)
		}
	}
}

// downloadHeaders runs a request and returns the filename and content type.
func downloadHeaders(t *testing.T, server *Server, url string) (string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, rec.Code, rec.Body.String())
	}
	_, params, err := mime.ParseMediaType(rec.Header().Get("Content-Disposition"))
	if err != nil {
		t.Fatal(err)
	}
	return params["filename"], rec.Header().Get("Content-Type")
}

// The whole point: change the format after the stitch and get that format.
func TestDownloadConvertsToTheRequestedFormat(t *testing.T) {
	server := newTestServer(t, true)
	dir := t.TempDir()
	master := writeMaster(t, dir)

	job := &Job{ID: "conv", Dir: dir, Format: "tif", Quality: 90, state: StateDone}
	job.setResult("", master, "lake.tif")
	server.jobs.Add(job)

	for _, tc := range []struct{ query, wantName, wantType string }{
		{"", "lake.tif", "image/tiff"},
		{"?format=tif", "lake.tif", "image/tiff"},
		{"?format=png", "lake.png", "image/png"},
		{"?format=jpg", "lake.jpg", "image/jpeg"},
		{"?format=jpg&quality=50", "lake.jpg", "image/jpeg"},
		{"?format=png&name=sunset", "sunset.png", "image/png"},
	} {
		name, ctype := downloadHeaders(t, server, "/download/conv"+tc.query)
		if name != tc.wantName || ctype != tc.wantType {
			t.Errorf("GET %q gave %q/%q, want %q/%q", tc.query, name, ctype, tc.wantName, tc.wantType)
		}
	}
}

// Asking for HDR after an LDR stitch is refused with an explanation rather
// than silently handing back the wrong thing.
func TestDownloadRefusesHDRAfterAnLDRStitch(t *testing.T) {
	server := newTestServer(t, true)
	dir := t.TempDir()
	master := writeMaster(t, dir)

	job := &Job{ID: "ldr", Dir: dir, Format: "tif", Quality: 90, state: StateDone}
	job.setResult("", master, "lake.tif")
	server.jobs.Add(job)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/download/ldr?format=exr", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "before stitching") {
		t.Errorf("body = %q, want it to explain that a new stitch is needed", rec.Body.String())
	}
}

func TestDownloadRejectsAnUnknownFormat(t *testing.T) {
	server := newTestServer(t, true)
	dir := t.TempDir()
	master := writeMaster(t, dir)
	job := &Job{ID: "bad", Dir: dir, Format: "tif", state: StateDone}
	job.setResult("", master, "lake.tif")
	server.jobs.Add(job)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/download/bad?format=gif", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown format", rec.Code)
	}
}
