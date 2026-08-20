package serve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The cap is what stops a long-running daemon from filling the profile volume
// with its own log; one previous generation is kept so a rotation mid-incident
// does not erase the lines that explain it.
func TestRotatingFileRotatesAtTheCap(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "serve.log")
	rf, err := openRotatingFile(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rf.Close() }()

	for range 8 {
		if _, werr := rf.Write([]byte("0123456789\n")); werr != nil {
			t.Fatal(werr)
		}
	}

	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cur) > 32 {
		t.Fatalf("current log is %d bytes, past the 32-byte cap", len(cur))
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("the previous generation must be kept: %v", err)
	}
}

// Reopening must append to the existing log, not truncate it: a container
// restart that wiped the log would defeat the reason the file exists.
func TestRotatingFileAppendsAcrossReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "serve.log")
	for _, line := range []string{"first\n", "second\n"} {
		rf, err := openRotatingFile(path, logMaxBytes)
		if err != nil {
			t.Fatal(err)
		}
		if _, werr := rf.Write([]byte(line)); werr != nil {
			t.Fatal(werr)
		}
		_ = rf.Close()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "first\nsecond\n" {
		t.Fatalf("log = %q, want both lines", got)
	}
}

// The file exists to outlive the container, so it is written only where it can:
// a session daemon whose profile dir is durable. Anywhere else it would land in
// the container's writable layer and die with it.
func TestFileLoggingOnlyForDurableSessions(t *testing.T) {
	// Not parallel, and restored: this swaps the package logger for a file-backed
	// one, which every other test in the package writes through.
	original := logger
	t.Cleanup(func() { logger = original })

	for _, cfg := range []serveConfig{
		{mode: modePool, keepProfile: true},                     // stdout already collected
		{mode: modeSession},                                     // no volume to survive in
		{mode: modeSession, keepProfile: true, ephemeral: true}, // scratch dir, discarded
	} {
		dir := t.TempDir()
		cfg.dataDir = dir
		startFileLogging(cfg)()
		if _, err := os.Stat(filepath.Join(dir, logsDirName)); !os.IsNotExist(err) {
			t.Fatalf("no log file expected for %+v", cfg)
		}
	}

	dir := t.TempDir()
	stop := startFileLogging(serveConfig{mode: modeSession, keepProfile: true, dataDir: dir})
	logInfo("hello from the session daemon")
	stop()
	data, err := os.ReadFile(serveLogPath(dir))
	if err != nil {
		t.Fatalf("a durable session daemon must persist its log: %v", err)
	}
	if !strings.Contains(string(data), "hello from the session daemon") {
		t.Fatalf("log file missing the line: %q", data)
	}
}
