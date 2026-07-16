package profile

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLockSingleWriter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lk, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, serr := acquireLock(dir); !errors.Is(serr, errCheckedOut) {
		t.Fatalf("second acquire: want errCheckedOut, got %v", serr)
	}
	if rerr := lk.release(); rerr != nil {
		t.Fatalf("release: %v", rerr)
	}
	lk2, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	_ = lk2.release()
}

func TestLockStealsStaleLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, lockName)
	host, _ := os.Hostname()
	info := lockInfo{PID: deadPID(t), Host: host, Acquired: time.Now().UTC()}
	data, _ := json.Marshal(info)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("seed stale lock: %v", err)
	}
	if lockHeld(path) {
		t.Fatal("dead-PID lock should read as not held")
	}
	lk, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("acquire over stale lock: %v", err)
	}
	_ = lk.release()
}

func TestLockHeldForLiveProcess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, lockName)
	host, _ := os.Hostname()
	data, _ := json.Marshal(lockInfo{PID: os.Getpid(), Host: host, Acquired: time.Now().UTC()})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	if !lockHeld(path) {
		t.Fatal("live-PID lock should read as held")
	}
}

func TestLockHeldForeignHost(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, lockName)
	data, _ := json.Marshal(lockInfo{PID: deadPID(t), Host: "some-other-host", Acquired: time.Now().UTC()})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	// A lock owned on a different machine cannot have its liveness checked, so it
	// is treated as held rather than stolen.
	if !lockHeld(path) {
		t.Fatal("foreign-host lock should read as held")
	}
}

// deadPID returns the PID of a process that has already exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	return pid
}
