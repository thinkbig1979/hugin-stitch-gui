package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
	"os"
	"sync"
	"time"
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

	// convertMu serialises download-time encoding. It is deliberately not mu:
	// encoding a large panorama takes a second or more, and status polls must
	// not queue behind it.
	convertMu sync.Mutex

	// lastUsed is when the browser last asked about this job: a status poll,
	// the preview, or a download. Retention is measured from it rather than
	// from completion, so a result page someone is still using stays alive.
	lastUsed time.Time
	// inUse counts handlers currently reading the job directory. The
	// directory is never removed while it is above zero.
	inUse int
	// removed is set once the job has been expired. It is set under mu before
	// the directory goes, so no handler can start serving a condemned job.
	removed bool

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

// touchLocked records that the job was just used. Callers already hold mu.
func (j *Job) touchLocked() {
	j.lastUsed = time.Now()
}

// Touch marks the job as still wanted, pushing back its expiry.
func (j *Job) Touch() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.touchLocked()
}

// Acquire claims the job directory for the duration of one request, reporting
// false if the job has already been expired. Every caller that reads or writes
// inside Job.Dir must hold a claim, because expiry deletes the whole directory
// and a download may be encoding or streaming out of it for many seconds.
func (j *Job) Acquire() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.removed {
		return false
	}
	j.inUse++
	j.touchLocked()
	return true
}

// Release gives up a claim taken by Acquire.
func (j *Job) Release() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.inUse > 0 {
		j.inUse--
	}
	j.touchLocked()
}

// expire marks a finished, unused and untouched job for deletion, reporting
// whether the caller now owns its directory. Deciding under the same mutex
// Acquire takes is what makes the delete safe: after this returns true no
// handler can claim the job, and it only returns true when none holds it.
func (j *Job) expire(now time.Time, ttl time.Duration) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.removed || j.inUse > 0 || j.state == StateRunning {
		return false
	}
	if now.Sub(j.lastUsed) < ttl {
		return false
	}
	j.removed = true
	return true
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
	j.Touch()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[j.ID] = j
}

// Reap deletes the working directory of every job that has gone untouched for
// ttl, and forgets the job itself so the store cannot grow without bound. A
// ttl of zero disables expiry. It returns the directories it removed.
func (s *JobStore) Reap(now time.Time, ttl time.Duration) []string {
	if ttl <= 0 {
		return nil
	}
	var removed []string
	for _, j := range s.list() {
		if !j.expire(now, ttl) {
			continue
		}
		os.RemoveAll(j.Dir)
		removed = append(removed, j.Dir)
		s.mu.Lock()
		delete(s.jobs, j.ID)
		s.mu.Unlock()
	}
	return removed
}

// DiscardAll removes every job directory. It is called once the server has
// stopped, where nothing can reach a job any more, so unlike Reap it does not
// wait for a claim to be given up.
func (s *JobStore) DiscardAll() {
	for _, j := range s.list() {
		j.mu.Lock()
		j.removed = true
		j.mu.Unlock()
		os.RemoveAll(j.Dir)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = make(map[string]*Job)
}

// Dirs returns the working directory of every job the store still holds.
func (s *JobStore) Dirs() []string {
	jobs := s.list()
	dirs := make([]string, 0, len(jobs))
	for _, j := range jobs {
		dirs = append(dirs, j.Dir)
	}
	return dirs
}

// list snapshots the jobs so the store lock is not held across file removal.
func (s *JobStore) list() []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	jobs := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}
	return jobs
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
