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
// Session mode only. A pool daemon is a fleet server whose stdout is already
// collected by compose/k8s, and whose data dir is often a scratch or shared
// volume - a log file there is overhead that buys nothing.
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
}

func openRotatingFile(path string, maxBytes int64) (*rotatingFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	size := int64(0)
	if info, serr := f.Stat(); serr == nil {
		size = info.Size()
	}
	return &rotatingFile{path: path, max: maxBytes, f: f, size: size}, nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > r.max {
		r.rotateLocked()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err //nolint:wrapcheck
}

// rotateLocked swaps in a fresh file. Every failure is silent by design: a log
// that cannot rotate must not take the daemon down, and reporting the failure
// through the logger it is rotating would recurse.
func (r *rotatingFile) rotateLocked() {
	_ = r.f.Close()
	_ = os.Rename(r.path, r.path+".1")
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	r.f = f
	r.size = 0
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close() //nolint:wrapcheck
}

// startFileLogging tees the daemon's log into the data dir when the daemon runs
// in session mode, and returns the func that closes it. Any failure downgrades to
// stderr-only rather than failing the daemon: losing the log file is not worth
// refusing to run a browser.
func startFileLogging(cfg serveConfig) func() {
	noop := func() {}
	if cfg.mode != modeSession {
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
	logger = slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, rf), nil))
	logInfo("daemon log also written to %s - it lives in the profile volume, so it survives a container recreate", path)
	return func() { _ = rf.Close() }
}
