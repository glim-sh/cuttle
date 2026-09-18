package serve

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Daemon log persistence. Stderr is the natural home for these lines, but it dies
// with the container: `cuttle up --recreate` starts a brand-new docker log file,
// so the evidence for anything that happened before the recreate is gone exactly
// when someone wants it. The data dir is a named volume that survives, so the
// session daemon also writes its log there. Read it after the fact with
// `docker exec <name> cat /data/logs/serve.log`.
//
// Durable session profiles only. A pool daemon is a fleet server whose stdout is
// already collected by compose/k8s; a non-durable one has no volume to survive
// in. Either way a log file there is overhead that buys nothing.
const (
	logsDirName = "logs"
	logFileName = "serve.log"
	// logMaxBytes caps one log file; one previous generation is kept alongside it,
	// so the log costs at most twice this on the volume.
	logMaxBytes = 20 << 20
)

func serveLogPath(dataDir string) string {
	return filepath.Join(dataDir, logsDirName, logFileName)
}

// rotatingFile is a size-capped append writer. At the cap it renames the file to
// <name>.1 (replacing any previous generation) and opens a fresh one, so a
// long-running daemon cannot fill the profile volume with its own log.
type rotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
	// done means the file half has given up (a rotation that could not complete).
	// Writes then succeed silently: stderr is the other half of the MultiWriter and
	// still carries every line, so a broken log file must not turn into an error on
	// every log call, nor a retry storm against a filesystem that just said no.
	done bool
}

func openRotatingFile(path string, maxBytes int64) (*rotatingFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	// Size must be known: guessing 0 for an existing file would let it grow to
	// its old size plus the cap before the first rotation.
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err //nolint:wrapcheck
	}
	return &rotatingFile{path: path, max: maxBytes, f: f, size: info.Size()}, nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Checked BEFORE the size test, not after: a file half that has given up is
	// over its cap forever, so testing afterwards re-entered rotation on every
	// single line - a close on a closed handle plus a rename syscall apiece,
	// against the filesystem that just refused - and a rename that later
	// succeeded would leave a fresh handle nothing ever writes to or closes.
	if r.done {
		return len(p), nil
	}
	if r.size+int64(len(p)) > r.max {
		r.rotateLocked()
		if r.done {
			return len(p), nil
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err //nolint:wrapcheck
}

// rotateLocked swaps in a fresh file, or gives up. Failure is silent by design: a
// log that cannot rotate must not take the daemon down, and reporting it through
// the logger being rotated would recurse.
func (r *rotatingFile) rotateLocked() {
	_ = r.f.Close()
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		// The generation could not be preserved. Stop rather than opening the same
		// path O_TRUNC, which would destroy the log we just failed to roll.
		r.done = true
		return
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		r.done = true
		return
	}
	r.f = f
	r.size = 0
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return nil
	}
	r.done = true
	return r.f.Close() //nolint:wrapcheck
}

// startFileLogging tees the daemon's log into the data dir, and returns the func
// that closes it. Any failure downgrades to stderr-only rather than failing the
// daemon: losing the log file is not worth refusing to run a browser.
//
// Gated on a DURABLE session profile, because outliving the container is the
// whole point. A non-durable run mounts no volume, so the file would land in the
// container's writable layer and die with it - all cost, none of the benefit it
// is documented to provide.
func startFileLogging(cfg serveConfig) func() {
	noop := func() {}
	if cfg.mode != modeSession || !durableProfile(cfg.keepProfile, cfg.ephemeral) {
		return noop
	}
	path := serveLogPath(cfg.dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logWarn("log file: creating %s failed (%v) - logging to stderr only", filepath.Dir(path), err)
		return noop
	}
	rf, err := openRotatingFile(path, logMaxBytes)
	if err != nil {
		logWarn("log file: opening %s failed (%v) - logging to stderr only", path, err)
		return noop
	}
	logger = slog.New(newLogHandler(slog.NewTextHandler(io.MultiWriter(os.Stderr, rf), nil)))
	logInfo("daemon log also written to %s - it lives in the profile volume, so it survives a container recreate", path)
	return func() { _ = rf.Close() }
}
