package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Job directories are temporary directories full of very large files, and on
// Linux /tmp is usually tmpfs, which means a leaked job directory sits in RAM
// until the machine reboots. Two things keep them from piling up: finished
// jobs are removed once nobody has asked about them for a while, and a sweep
// at startup clears directories left behind by a run that was killed.

const (
	// jobDirPrefix is the os.MkdirTemp pattern job directories are made with.
	jobDirPrefix = "hugin_"

	// markerName identifies a job directory as belonging to a running server
	// and records which one. The sweep uses both its contents and its
	// modification time, which the retention loop keeps fresh.
	markerName = ".hugin-stitch-gui"

	// orphanAge is how long an unclaimed job directory must have sat
	// untouched before the startup sweep will remove it. Only directories
	// with no marker at all are judged this way.
	orphanAge = time.Hour

	// reuseAge is the backstop for a marker whose process id has been
	// recycled by something unrelated, which would otherwise make the
	// directory look owned forever. It is deliberately far longer than
	// orphanAge because it is guessing rather than detecting: a live server
	// refreshes its markers every heartbeatInterval, so it can never reach
	// this even after days of uptime.
	reuseAge = 24 * time.Hour

	// heartbeatInterval is how often retention runs: expired jobs are removed
	// and the markers of the surviving ones are touched.
	heartbeatInterval = time.Minute
)

// DefaultRetention is how long a finished job is kept after the last time the
// browser asked about it. It is measured from the last request rather than
// from completion, so it only has to cover someone walking away from a
// finished result page, not the whole time they might leave the server up.
const DefaultRetention = 2 * time.Hour

// writeMarker claims a job directory for this process.
func writeMarker(dir string) error {
	return os.WriteFile(filepath.Join(dir, markerName),
		[]byte(strconv.Itoa(os.Getpid())), 0o600)
}

// touchMarker tells any future sweep that this directory still has an owner.
func touchMarker(dir string) {
	marker := filepath.Join(dir, markerName)
	now := time.Now()
	if err := os.Chtimes(marker, now, now); err != nil {
		// The marker is missing (or was never written), so write it again
		// rather than leaving the directory looking abandoned.
		_ = writeMarker(dir)
	}
}

// markerPID reads the process id recorded in a job directory.
func markerPID(dir string) (pid int, at time.Time, ok bool) {
	marker := filepath.Join(dir, markerName)
	info, err := os.Stat(marker)
	if err != nil {
		return 0, time.Time{}, false
	}
	body, err := os.ReadFile(marker)
	if err != nil {
		return 0, time.Time{}, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		return 0, time.Time{}, false
	}
	return pid, info.ModTime(), true
}

// sweepOrphans removes job directories under root that no running server can
// still be using, and returns the ones it removed.
//
// Running two servers at once is a supported setup, so the sweep has to leave
// the other one's directories alone. Liveness decides that, not time: a
// directory whose marker names a process that still exists is kept, however
// old it looks and whatever retention the other server was started with. Time
// only comes into it where there is nothing to detect - a directory with no
// marker at all, from a version before markers or from a crash between
// creating the directory and claiming it - and there the test is whether
// anything inside has been written lately, so a stitch in progress is safe.
func sweepOrphans(root string, now time.Time, age time.Duration) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var removed []string
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), jobDirPrefix) {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if !orphaned(dir, now, age) {
			continue
		}
		if err := os.RemoveAll(dir); err == nil {
			removed = append(removed, dir)
		}
	}
	return removed
}

// orphaned reports whether a job directory has lost its owner.
func orphaned(dir string, now time.Time, age time.Duration) bool {
	pid, at, ok := markerPID(dir)
	if !ok {
		// Nobody claimed it, so fall back to whether it is still being used.
		return now.Sub(newestMTime(dir)) > age
	}
	if !processAlive(pid) {
		return true
	}
	// The owner is alive. Keep the directory, unless the marker is so stale
	// that the process id has plainly been recycled by something else.
	return now.Sub(at) > reuseAge
}

// newestMTime returns the most recent modification time of a directory or
// anything directly inside it. A stitch in progress keeps writing, so its
// directory always looks recent even when the entries themselves are old.
func newestMTime(dir string) time.Time {
	newest := time.Time{}
	if info, err := os.Stat(dir); err == nil {
		newest = info.ModTime()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return newest
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

// Retain runs the retention loop until ctx is cancelled: finished jobs nobody
// has asked about for ttl are deleted, and the jobs that survive have their
// markers refreshed so a sweep elsewhere can tell they are still owned. A ttl
// of zero keeps jobs for the life of the process; the heartbeat still runs,
// because whether a directory has an owner is not a retention question.
func (s *Server) Retain(ctx context.Context, ttl time.Duration) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			for _, dir := range s.jobs.Reap(now, ttl) {
				log.Printf("removed expired job directory %s", dir)
			}
			for _, dir := range s.jobs.Dirs() {
				touchMarker(dir)
			}
		}
	}
}

// pruneIntermediates deletes the working files a finished job no longer needs:
// the uploaded frames, the copies made of them, and the Hugin projects. The
// panorama, its preview and any encoded download are named in keep and stay.
//
// It runs only after a successful stitch. The EXIF copy and the download name
// both read the first uploaded frame, and both happen before the job is
// finished; nothing afterwards reads anything but the finished panorama.
func pruneIntermediates(dir string, disposable []string, keep ...string) {
	kept := make(map[string]bool, len(keep))
	for _, name := range keep {
		if name != "" {
			kept[name] = true
		}
	}
	for _, name := range disposable {
		if name == "" || kept[name] {
			continue
		}
		if filepath.Dir(name) != dir {
			continue // never reach outside the job directory
		}
		_ = os.Remove(name)
	}

	projects, err := filepath.Glob(filepath.Join(dir, "*.pto"))
	if err != nil {
		return
	}
	for _, project := range projects {
		if !kept[project] {
			_ = os.Remove(project)
		}
	}
}
