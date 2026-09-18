package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// jobDir makes a directory standing in for a job's working directory.
func jobDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "result.tif"), []byte("pano"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// deadPID returns a process id that has certainly exited. Running the test
// binary itself and waiting for it is the portable way to get one.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestNothingMatchesThisName")
	cmd.Env = append(os.Environ(), "GO_CLEANUP_CHILD=1")
	if err := cmd.Run(); err != nil && cmd.ProcessState == nil {
		t.Fatalf("could not start a throwaway process: %v", err)
	}
	return cmd.ProcessState.Pid()
}

func TestReapRemovesFinishedJobsAndTheirDirectories(t *testing.T) {
	store := NewJobStore()
	dir := jobDir(t)
	job := &Job{ID: "old", Dir: dir, state: StateDone}
	store.Add(job)
	job.lastUsed = time.Now().Add(-3 * time.Hour)

	removed := store.Reap(time.Now(), time.Hour)

	if len(removed) != 1 || removed[0] != dir {
		t.Fatalf("removed = %v, want [%s]", removed, dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("job directory still exists: %v", err)
	}
	if _, ok := store.Get("old"); ok {
		t.Error("expired job is still in the store")
	}
}

func TestReapKeepsJobsThatAreStillWanted(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		setup func(j *Job)
	}{
		{"running", func(j *Job) { j.state = StateRunning; j.lastUsed = now.Add(-3 * time.Hour) }},
		{"recently used", func(j *Job) { j.state = StateDone; j.lastUsed = now.Add(-time.Minute) }},
		{"being downloaded", func(j *Job) {
			j.state = StateDone
			j.Acquire()
			j.lastUsed = now.Add(-3 * time.Hour)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewJobStore()
			dir := jobDir(t)
			job := &Job{ID: "keep", Dir: dir}
			store.Add(job)
			tc.setup(job)

			if removed := store.Reap(now, time.Hour); len(removed) != 0 {
				t.Fatalf("removed = %v, want nothing", removed)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("job directory was deleted: %v", err)
			}
			if _, ok := store.Get("keep"); !ok {
				t.Error("job was dropped from the store")
			}
		})
	}
}

func TestReapWithoutRetentionKeepsEverything(t *testing.T) {
	store := NewJobStore()
	dir := jobDir(t)
	job := &Job{ID: "forever", Dir: dir, state: StateDone}
	store.Add(job)
	job.lastUsed = time.Now().Add(-1000 * time.Hour)

	if removed := store.Reap(time.Now(), 0); len(removed) != 0 {
		t.Fatalf("removed = %v, want nothing when retention is off", removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("job directory was deleted: %v", err)
	}
}

// A job that has been expired must not be servable afterwards, or a handler
// would read from a directory that is already gone.
func TestExpiredJobCannotBeClaimed(t *testing.T) {
	job := &Job{ID: "gone", Dir: t.TempDir(), state: StateDone}
	job.lastUsed = time.Now().Add(-3 * time.Hour)

	if !job.expire(time.Now(), time.Hour) {
		t.Fatal("expire() = false, want true for an untouched finished job")
	}
	if job.Acquire() {
		t.Error("Acquire() = true after the job was expired")
	}
}

func TestAcquireAndReapDoNotRace(t *testing.T) {
	store := NewJobStore()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		job := &Job{ID: strconv.Itoa(i), Dir: jobDir(t), state: StateDone}
		store.Add(job)
		job.lastUsed = time.Now().Add(-3 * time.Hour)

		wg.Add(1)
		go func() {
			defer wg.Done()
			if job.Acquire() {
				// Holding a claim means the directory must still be there.
				if _, err := os.Stat(job.Dir); err != nil {
					t.Errorf("claimed job directory is gone: %v", err)
				}
				job.Release()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		store.Reap(time.Now(), time.Hour)
	}()
	wg.Wait()
}

func TestDownloadKeepsTheJobAlive(t *testing.T) {
	server := newTestServer(t, true)
	dir := jobDir(t)
	job := &Job{ID: "abc", Dir: dir, Format: "tif", state: StateDone}
	job.setResult("", filepath.Join(dir, "result.tif"), "pano.tif")
	server.jobs.Add(job)
	job.lastUsed = time.Now().Add(-3 * time.Hour)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/download/abc", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if removed := server.jobs.Reap(time.Now(), time.Hour); len(removed) != 0 {
		t.Fatalf("removed = %v, want nothing right after a download", removed)
	}
}

func TestPreviewFetchKeepsTheJobAlive(t *testing.T) {
	server := newTestServer(t, true)
	dir := jobDir(t)
	preview := filepath.Join(dir, "preview.png")
	if err := os.WriteFile(preview, []byte("preview"), 0o600); err != nil {
		t.Fatal(err)
	}
	job := &Job{ID: "abc", Dir: dir, state: StateDone}
	job.setResult(preview, "", "")
	server.jobs.Add(job)
	job.lastUsed = time.Now().Add(-3 * time.Hour)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/result/abc", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// The browser stops polling /status once a job is done, so the preview and
	// the download are the only things that can keep a result page alive.
	if removed := server.jobs.Reap(time.Now(), time.Hour); len(removed) != 0 {
		t.Fatalf("removed = %v, want nothing right after a preview fetch", removed)
	}
}

func TestStatusPollKeepsTheJobAlive(t *testing.T) {
	server := newTestServer(t, true)
	job := &Job{ID: "abc", Dir: jobDir(t), state: StateDone}
	server.jobs.Add(job)
	job.lastUsed = time.Now().Add(-3 * time.Hour)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status/abc", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if removed := server.jobs.Reap(time.Now(), time.Hour); len(removed) != 0 {
		t.Fatalf("removed = %v, want nothing right after a status poll", removed)
	}
}

func TestCleanupRemovesEveryJobDirectory(t *testing.T) {
	server := newTestServer(t, true)
	first, second := jobDir(t), jobDir(t)
	server.jobs.Add(&Job{ID: "a", Dir: first, state: StateDone})
	server.jobs.Add(&Job{ID: "b", Dir: second, state: StateRunning})

	server.Cleanup(time.Second)

	for _, dir := range []string{first, second} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s survived shutdown: %v", dir, err)
		}
	}
	if _, ok := server.jobs.Get("a"); ok {
		t.Error("job store still holds a job after shutdown")
	}
}

func TestSweepOrphans(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * time.Hour)

	// Each case prepares one directory under a shared root and says whether
	// the sweep should have removed it.
	cases := []struct {
		name   string
		setup  func(t *testing.T, dir string)
		remove bool
	}{
		{"owned by a running server", func(t *testing.T, dir string) {
			mustWriteMarker(t, dir, os.Getpid(), old)
			mustChtimes(t, dir, old)
		}, false},
		{"owner has gone", func(t *testing.T, dir string) {
			mustWriteMarker(t, dir, deadPID(t), now)
		}, true},
		{"owner alive but the marker is ancient", func(t *testing.T, dir string) {
			mustWriteMarker(t, dir, os.Getpid(), now.Add(-48*time.Hour))
		}, true},
		{"unclaimed and untouched", func(t *testing.T, dir string) {
			mustChtimes(t, dir, old)
		}, true},
		{"unclaimed but still being written", func(t *testing.T, dir string) {
			mustChtimes(t, dir, old)
			recent := filepath.Join(dir, "result.tif")
			if err := os.WriteFile(recent, []byte("growing"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false},
	}

	root := t.TempDir()
	dirs := make([]string, len(cases))
	for i, tc := range cases {
		dir := filepath.Join(root, jobDirPrefix+strconv.Itoa(i))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		tc.setup(t, dir)
		dirs[i] = dir
	}
	// A directory that is not one of ours must never be touched.
	other := filepath.Join(root, "unrelated")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	mustChtimes(t, other, old)

	sweepOrphans(root, now, orphanAge)

	for i, tc := range cases {
		_, err := os.Stat(dirs[i])
		if tc.remove && !os.IsNotExist(err) {
			t.Errorf("%s: directory survived the sweep", tc.name)
		}
		if !tc.remove && err != nil {
			t.Errorf("%s: directory was swept away: %v", tc.name, err)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("sweep removed a directory that is not ours: %v", err)
	}
}

func mustWriteMarker(t *testing.T, dir string, pid int, at time.Time) {
	t.Helper()
	marker := filepath.Join(dir, markerName)
	if err := os.WriteFile(marker, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(marker, at, at); err != nil {
		t.Fatal(err)
	}
}

func mustChtimes(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestWriteMarkerNamesThisProcess(t *testing.T) {
	dir := t.TempDir()
	if err := writeMarker(dir); err != nil {
		t.Fatal(err)
	}
	pid, _, ok := markerPID(dir)
	if !ok || pid != os.Getpid() {
		t.Fatalf("markerPID() = %d, %v, want %d, true", pid, ok, os.Getpid())
	}
	if !processAlive(pid) {
		t.Error("processAlive() = false for this very process")
	}
}

func TestTouchMarkerRewritesAMissingMarker(t *testing.T) {
	dir := t.TempDir()
	touchMarker(dir)
	if pid, _, ok := markerPID(dir); !ok || pid != os.Getpid() {
		t.Fatalf("markerPID() = %d, %v, want %d, true", pid, ok, os.Getpid())
	}
}

func TestPruneIntermediatesKeepsWhatTheDownloadNeeds(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	source := write("IMG_0001.jpg")
	converted := write("IMG_0001_converted.tif")
	write("project.pto")
	write("pp.pto")
	master := write("result.tif")
	preview := write("preview.png")
	cached := write("download-jpg-q90.jpg")

	pruneIntermediates(dir, []string{source, converted}, master, preview)

	for _, gone := range []string{source, converted, filepath.Join(dir, "project.pto"), filepath.Join(dir, "pp.pto")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s was kept, want it pruned", filepath.Base(gone))
		}
	}
	for _, kept := range []string{master, preview, cached} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s was pruned, want it kept: %v", filepath.Base(kept), err)
		}
	}
}

func TestPruneIntermediatesIgnoresPathsOutsideTheJob(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "keep.jpg")
	if err := os.WriteFile(elsewhere, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}

	pruneIntermediates(dir, []string{elsewhere})

	if _, err := os.Stat(elsewhere); err != nil {
		t.Errorf("a file outside the job directory was removed: %v", err)
	}
}

func TestResolveRetention(t *testing.T) {
	cases := []struct {
		name  string
		flag  time.Duration
		given bool
		env   string
		want  time.Duration
		fails bool
	}{
		{name: "default", want: DefaultRetention},
		{name: "flag wins", flag: 30 * time.Minute, given: true, env: "5m", want: 30 * time.Minute},
		{name: "flag can switch expiry off", flag: 0, given: true, env: "5m", want: 0},
		{name: "environment", env: "90m", want: 90 * time.Minute},
		{name: "bad environment", env: "soon", fails: true},
		{name: "negative flag", flag: -time.Minute, given: true, fails: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RETENTION", tc.env)
			got, err := resolveRetention(tc.flag, tc.given)
			if tc.fails {
				if err == nil {
					t.Fatalf("resolveRetention() = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("resolveRetention() = %v, want %v", got, tc.want)
			}
		})
	}
}
