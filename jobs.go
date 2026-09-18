package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
	"sync"
)

// Job state values reported to the browser.
const (
	StateRunning   = "running"
	StateDone      = "done"
	StateError     = "error"
	StateCancelled = "cancelled"
)

// maxLogLines bounds the per-job log tail kept for error reporting.
const maxLogLines = 400

// Job is one stitch request: its settings, its progress, and where its files
// live. Every field is guarded by mu because the pipeline goroutine writes
// while status requests read.
type Job struct {
	mu sync.Mutex

	ID  string
	Dir string

	// Settings, fixed once the job is created.
	Projection string
	Format     string
	Quality    int
	Filename   string
	// Level rotates the finished panorama so the horizon is level. It is on
	// unless the client explicitly turns it off.
	Level bool
	// Photometric solves exposure, vignetting and white balance across the
	// frames instead of trusting each frame's EXIF. Also on by default.
	Photometric bool
	// Rotation is a manual nudge applied on top of the solved orientation.
	Rotation Rotation

	// cancel stops the pipeline and every tool it has running. It is nil
	// once the job has finished.
	cancel context.CancelFunc

	// Progress, updated as the pipeline runs.
	state    string
	progress float64
	message  string
	log      []string

	// Results, set when the pipeline succeeds.
	preview      string
	download     string
	downloadName string
	projCode     string
}

// Status is the snapshot the browser polls for.
type Status struct {
	State    string  `json:"state"`
	Progress float64 `json:"progress"`
	Message  string  `json:"message"`
}

func (j *Job) Status() Status {
	j.mu.Lock()
	defer j.mu.Unlock()
	return Status{State: j.state, Progress: round4(j.progress), Message: j.message}
}

// setProgress updates the bar and the phase caption together.
func (j *Job) setProgress(progress float64, message string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.progress = progress
	j.message = message
}

// fail marks the job as failed with a message shown verbatim in the UI.
func (j *Job) fail(message string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.state = StateError
	j.progress = 1
	j.message = message
}

// Cancel stops a running job, reporting whether there was anything to stop.
// Cancelling an already finished job is not an error, it just does nothing.
func (j *Job) Cancel() bool {
	j.mu.Lock()
	if j.state != StateRunning || j.cancel == nil {
		j.mu.Unlock()
		return false
	}
	cancel := j.cancel
	j.mu.Unlock()

	cancel()
	return true
}

// markCancelled records that the job stopped because the user asked it to,
// which the UI presents differently from a stitch that failed on its own.
func (j *Job) markCancelled() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.state = StateCancelled
	j.progress = 0
	j.message = "Stitch cancelled."
}

func (j *Job) finish(message string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.state = StateDone
	j.progress = 1
	j.message = message
}

// pushLine keeps a bounded recent-log tail for error reporting.
func (j *Job) pushLine(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, line)
	if n := len(j.log) - maxLogLines; n > 0 {
		j.log = append(j.log[:0], j.log[n:]...)
	}
}

func (j *Job) setResult(preview, download, downloadName string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.preview = preview
	j.download = download
	j.downloadName = downloadName
}

func (j *Job) results() (preview, download, downloadName string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.preview, j.download, j.downloadName
}

func (j *Job) setProjCode(code string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.projCode = code
}

func (j *Job) projName() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return projectionFriendly[j.projCode]
}

// JobStore holds running and finished jobs for the lifetime of the process.
type JobStore struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

func NewJobStore() *JobStore {
	return &JobStore{jobs: make(map[string]*Job)}
}

func (s *JobStore) Add(j *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[j.ID] = j
}

func (s *JobStore) Get(id string) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	return j, ok
}

// newJobID returns a random 32-character hex id.
func newJobID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("hugin-stitch-gui: no randomness available: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}
