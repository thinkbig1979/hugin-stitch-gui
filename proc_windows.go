//go:build windows

package main

import (
	"os"
	"os/exec"
	"strconv"
)

// useProcessGroup arranges for cancelling a job to tear down the whole tool
// tree. hugin_executor starts nona and enblend as separate processes, and Go
// can only kill the one it launched, so defer to taskkill, whose /T flag kills
// a process together with everything it started.
func useProcessGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		kill := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid))
		if err := kill.Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}

// processAlive reports whether a process id still exists. On Windows
// os.FindProcess opens a handle to the process and fails when there is none,
// unlike on Unix where it always succeeds.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
