//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// useProcessGroup arranges for cancelling a job to tear down the whole tool
// tree, not just the process we started.
func useProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killTree(cmd.Process.Pid)
	}
}

// killTree kills a process and everything it started.
//
// Putting the tool in its own process group is not enough for this toolchain.
// hugin_executor is a launcher, and the workers it starts put themselves into
// fresh process groups:
//
//	PID     PPID    PGID
//	529340  525168  529340   hugin_executor
//	529392  529340  529392   nona
//
// A group kill therefore reaches the launcher and leaves nona and enblend
// running, still writing into a job directory that is about to be deleted. So
// walk the actual parent/child tree instead. ps is used for the listing
// because it is the one process table available on both Linux and macOS.
func killTree(pid int) error {
	// Children first, so a launcher cannot spawn more work while its
	// existing workers are being killed.
	for _, child := range descendants(pid) {
		_ = syscall.Kill(child, syscall.SIGKILL)
	}
	// The group as well, for anything well-behaved enough to have stayed in it.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return syscall.Kill(pid, syscall.SIGKILL)
}

// descendants lists every process below root, deepest first.
func descendants(root int) []int {
	out, err := exec.Command("ps", "-eo", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}

	children := make(map[int][]int)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, errPid := strconv.Atoi(fields[0])
		ppid, errPpid := strconv.Atoi(fields[1])
		if errPid != nil || errPpid != nil || pid == ppid {
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}

	var found []int
	seen := make(map[int]bool)
	var walk func(int)
	walk = func(parent int) {
		if seen[parent] {
			return // a cycle in a malformed listing must not hang the kill
		}
		seen[parent] = true
		for _, child := range children[parent] {
			walk(child)
			found = append(found, child)
		}
	}
	walk(root)
	return found
}

// processAlive reports whether a process id still exists. Signal 0 performs
// the permission and existence checks without delivering anything, and a
// process owned by another user answers EPERM rather than ESRCH, which still
// means it is alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
