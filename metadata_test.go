package main

import (
	"context"
	"image"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The metadata path is the shelling out to ExifTool itself, which a fake tool
// cannot exercise, so these tests use the real one and skip without it. That
// mirrors the product: a machine with no ExifTool stitches fine and simply
// gets no metadata.
func exifToolchain(t *testing.T) Toolchain {
	t.Helper()
	path, err := exec.LookPath("exiftool")
	if err != nil {
		t.Skip("exiftool is not installed")
	}
	tools := Toolchain{
		Ready:    true,
		Tools:    map[string]bool{},
		Optional: map[string]bool{"exiftool": true},
		paths:    map[string]string{"exiftool": path},
	}
	for _, name := range toolchain {
		tools.Tools[name] = true
	}
	return tools
}

// testCaptureTime is the DateTimeOriginal the fixtures carry.
const testCaptureTime = "2026:08:12 13:10:24"

// tagMaster puts on the master the metadata copyEXIF transfers into it from
// the first source frame at the end of a stitch.
func tagMaster(t *testing.T, tools Toolchain, master string) {
	t.Helper()
	cmd := exec.Command(tools.Path("exiftool"), "-q", "-overwrite_original",
		"-DateTimeOriginal="+testCaptureTime, "-Make=Canon", "-Model=Canon EOS 7D", master)
	cmd.Env = tools.Env()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tagging the master: %v: %s", err, out)
	}
}

// readTag reads one tag back, empty when the file carries none.
func readTag(t *testing.T, tools Toolchain, path, tag string) string {
	t.Helper()
	cmd := exec.Command(tools.Path("exiftool"), "-s", "-s", "-s", "-"+tag, path)
	cmd.Env = tools.Env()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("reading %s from %s: %v", tag, path, err)
	}
	return strings.TrimSpace(string(out))
}

// A JPEG or PNG download is re-encoded from the TIFF master rather than being
// what Hugin wrote, and the re-encode has to carry the master's metadata over.
// Keeping the capture time is the whole reason the stitch copies EXIF at all.
func TestDownloadKeepsTheCaptureTimeWhenReEncoding(t *testing.T) {
	tools := exifToolchain(t)
	server, err := NewServer(context.Background(), tools, "")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	master := writeMaster(t, dir)
	tagMaster(t, tools, master)

	job := &Job{ID: "meta", Dir: dir, Format: "tif", Quality: 90, state: StateDone}
	job.setResult("", master, "lake.tif")
	server.jobs.Add(job)

	for _, key := range []string{"tif", "jpg", "png"} {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec,
			httptest.NewRequest(http.MethodGet, "/download/meta?format="+key, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET format=%s = %d: %s", key, rec.Code, rec.Body.String())
		}
		downloaded := filepath.Join(t.TempDir(), "downloaded."+key)
		if err := os.WriteFile(downloaded, rec.Body.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := readTag(t, tools, downloaded, "DateTimeOriginal"); got != testCaptureTime {
			t.Errorf("%s download DateTimeOriginal = %q, want %q", key, got, testCaptureTime)
		}
	}
}

// Writing metadata into a finished file must not damage the picture in it.
// ExifTool rewrites the container to make room, so this checks the image still
// decodes at the right size afterwards.
func TestTaggedDownloadsAreStillReadableImages(t *testing.T) {
	tools := exifToolchain(t)
	dir := t.TempDir()
	master := writeMaster(t, dir)
	tagMaster(t, tools, master)

	for _, key := range []string{"jpg", "png"} {
		path, err := encodeTo(tools, master, dir, key, 85)
		if err != nil {
			t.Fatalf("encodeTo(%s): %v", key, err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		img, _, err := image.Decode(f)
		f.Close()
		if err != nil {
			t.Fatalf("%s output is not readable after tagging: %v", key, err)
		}
		if img.Bounds().Dx() != 40 || img.Bounds().Dy() != 30 {
			t.Errorf("%s output is %v, want the master's size", key, img.Bounds())
		}
		if got := readTag(t, tools, path, "DateTimeOriginal"); got != testCaptureTime {
			t.Errorf("%s DateTimeOriginal = %q, want %q", key, got, testCaptureTime)
		}
	}
}

// The XMP a panorama viewer looks for has to survive the re-encode too, or the
// download stops being recognised as a panorama even though it still looks
// like one.
func TestReEncodingKeepsThePanoramaXMP(t *testing.T) {
	tools := exifToolchain(t)
	dir := t.TempDir()
	master := writeMaster(t, dir)

	cmd := exec.Command(tools.Path("exiftool"), "-q", "-overwrite_original",
		"-XMP-GPano:ProjectionType=equirectangular",
		"-XMP-GPano:UsePanoramaViewer=True", master)
	cmd.Env = tools.Env()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tagging the master: %v: %s", err, out)
	}

	path, err := encodeTo(tools, master, dir, "jpg", 90)
	if err != nil {
		t.Fatal(err)
	}
	if got := readTag(t, tools, path, "XMP-GPano:ProjectionType"); got != "equirectangular" {
		t.Errorf("ProjectionType = %q, want it carried into the JPEG", got)
	}
}

// A machine with no ExifTool stitches and downloads exactly as before; it just
// gets no metadata. The transfer must never be what stands between the user
// and their panorama.
func TestEncodingSucceedsWithoutExifTool(t *testing.T) {
	dir := t.TempDir()
	master := writeMaster(t, dir)

	path, err := encodeTo(Toolchain{}, master, dir, "jpg", 90)
	if err != nil {
		t.Fatalf("encodeTo without exiftool: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Errorf("no usable download produced: %v", err)
	}
}
