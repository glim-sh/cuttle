//go:build !windows

package backend

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// withStateLock serializes tunnel ensure/stop for one context across concurrent
// cuttle processes via an advisory flock on a per-context lockfile, so two
// invocations cannot both spawn a forward and orphan one behind a stale pidfile.
// Closing the fd releases the lock. If the state dir or lockfile cannot be
// opened it degrades to running fn unlocked (fn then surfaces the same dir error
// itself), so the lock is a safety net, never a hard dependency.
func withStateLock(contextName string, fn func() error) error {
	dir, err := stateDir()
	if err != nil {
		return fn()
	}
	f, err := os.OpenFile(filepath.Join(dir, "tunnel-"+safeToken(contextName)+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fn()
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fn()
	}
	return fn()
}

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
