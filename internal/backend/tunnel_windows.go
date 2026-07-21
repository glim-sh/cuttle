//go:build windows

package backend

import (
	"os"
	"os/exec"
)

// Standing tunnels are not a supported deployment on windows (the CLI hosts are
// darwin/linux); these keep the package compiling there with best-effort process
// handling and no session detach.

func processAlive(pid int) bool {
	_, err := os.FindProcess(pid)
	return err == nil
}

func detach(_ *exec.Cmd) {}

// withStateLock is a no-op on windows (standing tunnels are unsupported there).
func withStateLock(_ string, fn func() error) error { return fn() }

func killTunnel(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err //nolint:wrapcheck // best-effort teardown; caller ignores it
	}
	return proc.Kill() //nolint:wrapcheck // best-effort teardown; caller ignores it
}
