//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// alive reports whether a pid still exists.
func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// waitGone polls until the pid disappears or the deadline passes.
func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !alive(pid)
}

// killTree must reach a grandchild that escaped into its own process group,
// which is exactly what nona does under hugin_executor. Killing only the
// process group would leave this one running.
func TestKillTreeReachesAChildInItsOwnProcessGroup(t *testing.T) {
	// setsid forks, so the pid has to come from inside the escaped process
	// rather than from $!, which would name the short-lived setsid itself.
	pidFile := filepath.Join(t.TempDir(), "escaped.pid")
	script := "setsid sh -c 'echo $$ > " + pidFile + "; exec sleep 60' & sleep 60"
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = killTree(cmd.Process.Pid)
		_, _ = cmd.Process.Wait()
	})

	escaped := readPIDFile(t, pidFile)
	if !waitAlive(escaped, 2*time.Second) {
		t.Fatalf("escaped child %d never started", escaped)
	}

	// Confirm the premise: it really is outside the launcher's group.
	pgid, err := syscall.Getpgid(escaped)
	if err != nil {
		t.Fatalf("could not read the escaped child's group: %v", err)
	}
	if pgid == cmd.Process.Pid {
		t.Skip("this platform kept the child in the parent's process group")
	}

	if err := killTree(cmd.Process.Pid); err != nil {
		t.Fatalf("killTree: %v", err)
	}
	if !waitGone(escaped, 5*time.Second) {
		t.Errorf("process %d in its own group survived killTree", escaped)
	}
	_, _ = cmd.Process.Wait()
	if !waitGone(cmd.Process.Pid, 5*time.Second) {
		t.Errorf("the launcher %d survived killTree", cmd.Process.Pid)
	}
}

// readPIDFile waits for the escaped process to record its own pid.
func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the escaped child never wrote its pid to %s", path)
	return 0
}

func waitAlive(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if alive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return alive(pid)
}

func TestDescendantsFindsNestedChildren(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sh -c 'sleep 30' & sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = killTree(cmd.Process.Pid)
		_, _ = cmd.Process.Wait()
	})
	time.Sleep(300 * time.Millisecond)

	found := descendants(cmd.Process.Pid)
	if len(found) < 2 {
		t.Errorf("descendants found %d processes, want the nested children too", len(found))
	}
	for _, pid := range found {
		if pid == cmd.Process.Pid {
			t.Error("descendants must not include the root itself")
		}
	}
}

// A pid with no children is not an error and must not hang.
func TestDescendantsOfALeaf(t *testing.T) {
	if got := descendants(1 << 30); len(got) != 0 {
		t.Errorf("descendants of a nonexistent pid = %v, want none", got)
	}
}
