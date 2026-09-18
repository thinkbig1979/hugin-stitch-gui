package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestServer builds a server with a fake-but-complete toolchain so handler
// tests do not depend on Hugin being installed.
func newTestServer(t *testing.T, ready bool) *Server {
	t.Helper()
	tools := Toolchain{
		Ready:    ready,
		Tools:    map[string]bool{},
		Optional: map[string]bool{},
		Missing:  []string{},
	}
	for _, name := range toolchain {
		tools.Tools[name] = ready
		if !ready {
			tools.Missing = append(tools.Missing, name)
		}
	}
	server, err := NewServer(context.Background(), tools, "")
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// stitchRequest builds a multipart body shaped like the one the UI sends.
func stitchRequest(t *testing.T, files map[string][]byte, fields map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for name, content := range files {
		part, err := w.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range fields {
		if err := w.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()

	req := httptest.NewRequest(http.MethodPost, "/stitch", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

func TestServeIndex(t *testing.T) {
	server := newTestServer(t, true)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Hugin Stitch GUI") {
		t.Error("index.html did not come back from the embedded assets")
	}
}

// The UI is embedded, so the binary must serve it with no files beside it.
func TestEmbeddedAssetsArePresent(t *testing.T) {
	server := newTestServer(t, true)
	for _, path := range []string{"/index.html", "/app.js", "/style.css"} {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s returned an empty body", path)
		}
	}
}

func TestHealthReportsToolchain(t *testing.T) {
	server := newTestServer(t, false)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload Toolchain
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Ready {
		t.Error("engine_ready should be false when tools are missing")
	}
	if len(payload.Missing) != len(toolchain) {
		t.Errorf("missing = %v, want all %d tools", payload.Missing, len(toolchain))
	}
}

func TestStatusAndResultRejectUnknownJobs(t *testing.T) {
	server := newTestServer(t, true)
	for _, path := range []string{"/status/nope", "/result/nope", "/download/nope"} {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestStitchRequiresTwoImages(t *testing.T) {
	server := newTestServer(t, true)
	req := stitchRequest(t, map[string][]byte{"only.jpg": []byte("data")}, nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "at least 2") {
		t.Errorf("body = %q, want a message about needing two images", rec.Body.String())
	}
}

func TestStitchRefusesWhenToolchainMissing(t *testing.T) {
	server := newTestServer(t, false)
	req := stitchRequest(t, map[string][]byte{"a.jpg": []byte("x"), "b.jpg": []byte("y")}, nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "hugin") {
		t.Errorf("body = %q, want install guidance", rec.Body.String())
	}
}

func TestStitchRejectsNonMultipart(t *testing.T) {
	server := newTestServer(t, true)
	req := httptest.NewRequest(http.MethodPost, "/stitch", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// Files the toolchain cannot read are dropped rather than handed to Hugin.
func TestStitchIgnoresDisallowedExtensions(t *testing.T) {
	server := newTestServer(t, true)
	req := stitchRequest(t, map[string][]byte{
		"a.jpg":       []byte("x"),
		"payload.exe": []byte("y"),
	}, nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (only one usable image remained)", rec.Code)
	}
}

// A job's settings must survive the trip through the multipart form.
func TestStitchRecordsJobSettings(t *testing.T) {
	server := newTestServer(t, true)
	req := stitchRequest(t,
		map[string][]byte{"a.jpg": []byte("x"), "b.jpg": []byte("y")},
		map[string]string{
			"projection": "cylindrical",
			"format":     "jpg",
			"quality":    "72",
			"filename":   "dunes",
		})
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
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

	if job.Projection != "cylindrical" {
		t.Errorf("projection = %q", job.Projection)
	}
	if job.Format != "jpg" {
		t.Errorf("format = %q", job.Format)
	}
	if job.Quality != 72 {
		t.Errorf("quality = %d", job.Quality)
	}
	if job.Filename != "dunes" {
		t.Errorf("filename = %q", job.Filename)
	}
}

// Unknown values must not reach the Hugin command line.
func TestStitchNormalisesBadSettings(t *testing.T) {
	server := newTestServer(t, true)
	req := stitchRequest(t,
		map[string][]byte{"a.jpg": []byte("x"), "b.jpg": []byte("y")},
		map[string]string{"projection": "fisheye", "format": "gif", "quality": "9999"})
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

	if job.Projection != "auto" {
		t.Errorf("projection = %q, want auto", job.Projection)
	}
	if job.Format != DefaultFormat {
		t.Errorf("format = %q, want %q", job.Format, DefaultFormat)
	}
	if job.Quality != 100 {
		t.Errorf("quality = %d, want it clamped to 100", job.Quality)
	}
}

func TestDownloadServesTheResult(t *testing.T) {
	server := newTestServer(t, true)
	dir := t.TempDir()
	output := filepath.Join(dir, "result.tif")
	if err := os.WriteFile(output, []byte("stitched bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	job := &Job{ID: "abc", Dir: dir, Format: "tif", state: StateDone}
	job.setResult("", output, "my pano.tif")
	server.jobs.Add(job)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/download/abc", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/tiff" {
		t.Errorf("content type = %q, want image/tiff", got)
	}
	// A space in the filename has to survive the header intact.
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "my pano.tif") {
		t.Errorf("content-disposition = %q, want the download name", got)
	}
	if rec.Body.String() != "stitched bytes" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestDownloadBeforeTheJobFinishes(t *testing.T) {
	server := newTestServer(t, true)
	server.jobs.Add(&Job{ID: "pending", state: StateRunning})

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/download/pending", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 while the job is still running", rec.Code)
	}
}

func TestStatusReportsProgress(t *testing.T) {
	server := newTestServer(t, true)
	job := &Job{ID: "running", state: StateRunning}
	job.setProgress(0.42, "Finding control points (cpfind)...")
	server.jobs.Add(job)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status/running", nil))

	var got Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != StateRunning || got.Progress != 0.42 || !strings.Contains(got.Message, "cpfind") {
		t.Errorf("status = %+v", got)
	}
}

// makePreview has to turn whatever Hugin produced into a PNG the browser can
// show, including the 16-bit TIFF that is the default output format.
func TestMakePreviewFromTIFF(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "result.png")

	img := image.NewNRGBA(image.Rect(0, 0, 4, 3))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	img.Set(1, 0, color.NRGBA{G: 255, A: 0}) // transparent crop margin
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	f.Close()

	out := makePreview(src, dir)
	if out == "" {
		t.Fatal("makePreview returned nothing")
	}
	if filepath.Base(out) != "preview.png" {
		t.Errorf("preview path = %q", out)
	}

	decoded, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.Close()
	got, err := png.Decode(decoded)
	if err != nil {
		t.Fatalf("preview is not a readable PNG: %v", err)
	}
	if got.Bounds() != img.Bounds() {
		t.Errorf("preview bounds = %v, want %v", got.Bounds(), img.Bounds())
	}
	// Transparent pixels must be flattened onto black, not left see-through.
	if _, _, _, a := got.At(1, 0).RGBA(); a != 0xffff {
		t.Errorf("preview pixel is still transparent (alpha %d)", a)
	}
}

func TestMakePreviewRejectsUndecodableInput(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "result.exr")
	if err := os.WriteFile(junk, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := makePreview(junk, dir); got != "" {
		t.Errorf("makePreview = %q, want empty for an undecodable file", got)
	}
}

// prepareSource passes non-RAW files through untouched.
func TestPrepareSourcePassesThroughNormalImages(t *testing.T) {
	got, err := prepareSource("/tmp/job/a.jpg", "/tmp/job", Toolchain{})
	if err != nil || got != "/tmp/job/a.jpg" {
		t.Errorf("prepareSource = %q, %v; want the path unchanged", got, err)
	}
}

func TestPrepareSourceNeedsAConverterForRAW(t *testing.T) {
	_, err := prepareSource("/tmp/job/IMG.CR2", "/tmp/job", Toolchain{})
	if err == nil {
		t.Fatal("expected an error when no RAW converter is configured")
	}
	if !strings.Contains(err.Error(), "decoder") {
		t.Errorf("error = %q, want it to name the missing decoder", err)
	}
}

// Uploads stream to disk; the body must never have to fit in memory.
func TestUploadStreamsToDisk(t *testing.T) {
	server := newTestServer(t, true)
	large := bytes.Repeat([]byte("a"), 3<<20) // 3 MB
	req := stitchRequest(t, map[string][]byte{"a.jpg": large, "b.jpg": large}, nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		JobID string `json:"job_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &payload)
	job, _ := server.jobs.Get(payload.JobID)
	t.Cleanup(func() { os.RemoveAll(job.Dir) })

	entries, err := os.ReadDir(job.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, e := range entries {
		info, _ := e.Info()
		total += info.Size()
	}
	if total < int64(len(large))*2 {
		t.Errorf("job dir holds %d bytes, want both uploads written to disk", total)
	}
}
