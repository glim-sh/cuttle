//go:build !windows

package backend

import (
	"os"
	"os/exec"
	"syscall"
)

// processAlive reports whether pid names a live process, via signal 0.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// detach puts the spawned forward in its own session (setsid), so it becomes a
// process-group leader and a terminal close or CLI exit does not signal it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// killTunnel signals the whole process group (negative pid): setsid made pid the
// leader, so an ssh master and its children all go down together.
func killTunnel(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		return err //nolint:wrapcheck // best-effort teardown; caller ignores it
	}
	return nil
}
