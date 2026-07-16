package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// errCheckedOut is returned when a profile is already held by a live session
// elsewhere. A profile maps to a single browser seed, and one seed cannot be
// driven by two sessions at once (Chrome's own single-writer rule).
var errCheckedOut = errors.New("profile is checked out by another live session")

const lockName = "session.lock"

// lockInfo is what a held lock records, so a stale lock (from a crashed session)
// can be detected and stolen.
type lockInfo struct {
	PID      int       `json:"pid"`
	Host     string    `json:"host"`
	Acquired time.Time `json:"acquired"`
}

// lock is a held single-writer lock over a profile directory.
type lock struct {
	path string
}

// acquireLock takes the profile's single-writer lock. If the lock file exists
// and its owning process is still alive it fails with errCheckedOut; a stale
// lock (owner gone) is removed and retaken.
func acquireLock(dir string) (*lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating profile dir: %w", err)
	}
	path := filepath.Join(dir, lockName)
	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			host, _ := os.Hostname()
			info := lockInfo{PID: os.Getpid(), Host: host, Acquired: time.Now().UTC()}
			data, _ := json.Marshal(info)
			if _, werr := f.Write(data); werr != nil {
				_ = f.Close()
				_ = os.Remove(path)
				return nil, fmt.Errorf("writing lock: %w", werr)
			}
			if cerr := f.Close(); cerr != nil {
				return nil, fmt.Errorf("closing lock: %w", cerr)
			}
			return &lock{path: path}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("opening lock: %w", err)
		}
		if held := lockHeld(path); held {
			return nil, errCheckedOut
		}
		// Stale lock from a dead session: remove and retry once.
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return nil, fmt.Errorf("clearing stale lock: %w", rmErr)
		}
	}
	return nil, errCheckedOut
}

// release removes the lock file. It is safe to call more than once.
func (l *lock) release() error {
	if l == nil {
		return nil
	}
	if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("releasing lock: %w", err)
	}
	return nil
}

// lockHeld reports whether the lock file names a live process on this host. A
// malformed lock, a cross-host lock, or a dead PID is treated as not held (stale)
// so a crash never wedges a profile permanently.
func lockHeld(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var info lockInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return false
	}
	host, _ := os.Hostname()
	if info.Host != "" && host != "" && info.Host != host {
		// Owned on a different machine; we cannot check liveness, so do not steal.
		return true
	}
	return pidAlive(info.PID)
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
