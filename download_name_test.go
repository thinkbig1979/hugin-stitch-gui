package main

import (
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// finishedJob registers a job that has already produced a result.
func finishedJob(t *testing.T, server *Server, id, format, storedName string) *Job {
	t.Helper()
	dir := t.TempDir()
	output := filepath.Join(dir, "result"+formats[format].Ext)
	if err := os.WriteFile(output, []byte("stitched bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	job := &Job{ID: id, Dir: dir, Format: format, state: StateDone}
	job.setResult("", output, storedName)
	server.jobs.Add(job)
	return job
}

// downloadedName returns the filename the server puts in Content-Disposition.
func downloadedName(t *testing.T, server *Server, url string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, rec.Code)
	}
	_, params, err := mime.ParseMediaType(rec.Header().Get("Content-Disposition"))
	if err != nil {
		t.Fatalf("bad Content-Disposition %q: %v", rec.Header().Get("Content-Disposition"), err)
	}
	return params["filename"]
}

// Editing the filename after the stitch finishes must change the downloaded
// file's name. The name used to be fixed when the job was created, so a late
// edit was silently ignored and the old name came back.
func TestDownloadHonoursALateFilenameChange(t *testing.T) {
	server := newTestServer(t, true)
	finishedJob(t, server, "late", "tif", "20240531_140309_pano.tif")

	if got := downloadedName(t, server, "/download/late?name=sunset+over+the+lake"); got != "sunset_over_the_lake.tif" {
		t.Errorf("filename = %q, want the name supplied at download time", got)
	}
}

// The extension always comes from the format that was actually stitched, never
// from whatever the user typed.
func TestDownloadForcesTheStitchedExtension(t *testing.T) {
	server := newTestServer(t, true)
	finishedJob(t, server, "ext", "tif", "auto_pano.tif")

	if got := downloadedName(t, server, "/download/ext?name=lake.jpg"); got != "lake.tif" {
		t.Errorf("filename = %q, want the .tif the job actually produced", got)
	}
}

// Clearing the field falls back to the name derived when the job finished.
func TestDownloadFallsBackWhenNoNameIsSupplied(t *testing.T) {
	server := newTestServer(t, true)
	finishedJob(t, server, "empty", "tif", "20240531_140309_pano.tif")

	for _, url := range []string{"/download/empty", "/download/empty?name=", "/download/empty?name=+++"} {
		if got := downloadedName(t, server, url); got != "20240531_140309_pano.tif" {
			t.Errorf("GET %s filename = %q, want the stored name", url, got)
		}
	}
}

// A name arriving at download time goes through the same sanitising as one
// supplied at upload time.
func TestDownloadSanitisesTheSuppliedName(t *testing.T) {
	server := newTestServer(t, true)
	finishedJob(t, server, "evil", "jpg", "auto_pano.jpg")

	if got := downloadedName(t, server, "/download/evil?name=..%2F..%2Fetc%2Fpasswd"); got != ".._.._etc_passwd.jpg" {
		t.Errorf("filename = %q, want the separators neutralised", got)
	}
}
