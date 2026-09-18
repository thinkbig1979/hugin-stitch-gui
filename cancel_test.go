package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCancelRequiresPost(t *testing.T) {
	server := newTestServer(t, true)
	server.jobs.Add(&Job{ID: "j", state: StateRunning})

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cancel/j", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /cancel = %d, want 405", rec.Code)
	}
}

func TestCancelUnknownJob(t *testing.T) {
	server := newTestServer(t, true)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/cancel/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// Cancelling a job that already finished is reported, not silently accepted,
// so the UI does not claim to have stopped something that had already landed.
func TestCancelFinishedJobConflicts(t *testing.T) {
	server := newTestServer(t, true)
	server.jobs.Add(&Job{ID: "done", state: StateDone})

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/cancel/done", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var payload struct {
		State string `json:"state"`
	}
	json.Unmarshal(rec.Body.Bytes(), &payload)
	if payload.State != StateDone {
		t.Errorf("state = %q, want the job's real state", payload.State)
	}
}

func TestCancelStopsARunningJob(t *testing.T) {
	server := newTestServer(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	job := &Job{ID: "run", state: StateRunning, cancel: cancel}
	server.jobs.Add(job)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/cancel/run", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancelling the job did not cancel its context")
	}
}

// A second cancel is harmless; the job has already stopped running.
func TestCancelTwiceIsSafe(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := &Job{state: StateRunning, cancel: cancel}

	if !job.Cancel() {
		t.Fatal("first cancel should report that it stopped something")
	}
	job.markCancelled()
	if job.Cancel() {
		t.Error("second cancel should report that there was nothing to stop")
	}
}

// A cancelled job reports its own state rather than masquerading as a failure,
// and its half-written output is discarded.
func TestAbortedRecordsCancellationAndClearsTheJobDir(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "job")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "result.tif"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := NewPipeline(Toolchain{Optional: map[string]bool{}})
	job := &Job{Dir: work, state: StateRunning}

	ctx, cancel := context.WithCancel(context.Background())
	if p.aborted(ctx, job) {
		t.Fatal("a live context must not look like a cancellation")
	}
	cancel()

	if !p.aborted(ctx, job) {
		t.Fatal("a cancelled context should be reported")
	}
	if got := job.Status().State; got != StateCancelled {
		t.Errorf("state = %q, want %q", got, StateCancelled)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Error("the job directory should be removed, discarding partial output")
	}
}

// Cancelling before any tool starts must stop the run, not stitch anyway.
func TestPipelineStopsWhenCancelledUpFront(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.jpg", "b.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tools := Toolchain{Ready: true, Optional: map[string]bool{}}
	p := NewPipeline(tools)
	job := &Job{Dir: dir, state: StateRunning}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Run(ctx, job, []string{filepath.Join(dir, "a.jpg"), filepath.Join(dir, "b.jpg")})

	if got := job.Status().State; got != StateCancelled {
		t.Errorf("state = %q, want %q", got, StateCancelled)
	}
}
