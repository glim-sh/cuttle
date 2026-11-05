package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeMaskServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/mask" {
			t.Errorf("mask request = %s %s", r.Method, r.URL)
		}
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		scanner := bufio.NewScanner(r.Body)
		for scanner.Scan() {
			_, _ = io.WriteString(w, strings.ReplaceAll(scanner.Text(), "test-sensitive-value", "<masked>")+"\n")
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestMaskExecStreamsBothOutputsAndPreservesExit(t *testing.T) {
	t.Parallel()
	s := fakeMaskServer(t)
	var out, errOut bytes.Buffer
	err := runMaskExec(t.Context(), s.URL+"/mask", "upgrade", []string{"sh", "-c", "printf test-sensitive-; printf 'value\\n'; printf 'test-sensitive-value\\n' >&2; exit 23"}, nil, &out, &errOut)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 23 {
		t.Fatalf("exit = %v", err)
	}
	if out.String() != "<masked>\n" || errOut.String() != "<masked>\n" {
		t.Fatalf("outputs = %q, %q", out.String(), errOut.String())
	}
}

func TestMaskExecPreservesStatusWithInheritedOutputPipes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		argv []string
		code int
	}{
		{"success", []string{"sh", "-c", "sleep 3 & exit 0"}, 0},
		{"failure", []string{"sh", "-c", "sleep 3 & exit 23"}, 23},
		{"signal", []string{"sh", "-c", "sleep 3 & kill -TERM $$"}, 143},
		{"start failure", []string{filepath.Join(t.TempDir(), "missing-driver")}, execNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := runMaskExec(ctx, ":", "upgrade", tc.argv, nil, io.Discard, io.Discard)
			if tc.code == 0 {
				if err != nil {
					t.Fatalf("successful driver: %v", err)
				}
				return
			}
			var ec *ExitCodeError
			if !errors.As(err, &ec) || ec.Code != tc.code {
				t.Fatalf("exit = %v, want %d", err, tc.code)
			}
		})
	}
}

func TestMaskExecFailClosed(t *testing.T) {
	t.Parallel()
	for code, says := range map[int]string{404: "bump its chart pin", 500: "HTTP 500", 302: "HTTP 302"} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
				_, _ = io.WriteString(w, "test-sensitive-value")
			}))
			defer s.Close()
			var out, errOut bytes.Buffer
			err := runMaskExec(t.Context(), s.URL, "bump its chart pin", []string{"sh", "-c", "printf test-sensitive-value; printf test-sensitive-value >&2; exit 19"}, nil, &out, &errOut)
			var ec *ExitCodeError
			if !errors.As(err, &ec) || ec.Code != 19 {
				t.Fatalf("exit = %v", err)
			}
			if out.Len() != 0 || strings.Contains(errOut.String(), "test-sensitive-value") || !strings.Contains(errOut.String(), says) {
				t.Fatalf("outputs = %q, %q", out.String(), errOut.String())
			}
		})
	}
}

type signalWriter struct{ wrote chan struct{} }

func (w signalWriter) Write(p []byte) (int, error) {
	select {
	case w.wrote <- struct{}{}:
	default:
	}
	return len(p), nil
}

func TestMaskExecStreamsBeforeDriverExits(t *testing.T) {
	t.Parallel()
	s := fakeMaskServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	wrote := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- runMaskExec(ctx, s.URL+"/mask", "upgrade", []string{"sh", "-c", "echo test-sensitive-value; sleep 30"}, nil, signalWriter{wrote}, io.Discard)
	}()
	select {
	case <-wrote:
		cancel()
	case <-ctx.Done():
		t.Fatal("no live masked output")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled driver succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("driver group survived cancellation")
	}
}

func TestMaskExecClosedOutputKillsQuietDriver(t *testing.T) {
	t.Parallel()
	s := fakeMaskServer(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	_ = r.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err = runMaskExec(ctx, s.URL+"/mask", "upgrade", []string{"sh", "-c", "sleep 30 & wait"}, nil, w, io.Discard)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("quiet process did not stop promptly: %v", err)
	}
}

func TestMaskExecOldImageWithholdsOutputAndKeepsStatus(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	// Simulate a cuttle executable that predates mask-exec.
	if err := os.WriteFile(filepath.Join(dir, "cuttle"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	argv := maskExecArgv([]string{"/bin/sh", "-c", "echo test-sensitive-value; echo test-sensitive-value >&2; exit 17"})
	c := exec.CommandContext(t.Context(), "/bin/sh", argv[1:]...)
	c.Env = append(os.Environ(), "PATH="+dir)
	out, err := c.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 17 {
		t.Fatalf("exit = %v", err)
	}
	if strings.Contains(string(out), "test-sensitive-value") || !strings.Contains(string(out), "up --recreate") {
		t.Fatalf("output = %q", out)
	}
}

func TestMaskUpgradeHintsAndVersion(t *testing.T) {
	t.Parallel()
	if got := maskUpgradeHint("cuttle --context demo", false); !strings.Contains(got, "cuttle --context demo up --recreate") {
		t.Fatal(got)
	}
	if got := maskUpgradeHint("cuttle", true); !strings.Contains(got, "bump its chart pin") || strings.Contains(got, "recreate") {
		t.Fatal(got)
	}
	for _, tc := range []struct {
		daemon, cli string
		older       bool
	}{{"1.2.3", "1.2.4", true}, {"1.9.0", "1.10.0", true}, {"v1.2.3", "1.2.3", false}, {"2.0.0", "1.9.9", false}, {"", "1.0.0", false}, {"dev", "1.0.0", false}} {
		if olderDaemon(tc.daemon, tc.cli) != tc.older {
			t.Errorf("comparison = %+v", tc)
		}
	}
}

// unwrapMaskArgv strips the maskExecArgv wrapper, leaving the driver argv.
func unwrapMaskArgv(argv []string) []string {
	if len(argv) > 4 && argv[3] == maskArgv0 {
		return argv[5:]
	}
	return argv
}

type maskArgvRecorder struct{ argv []string }

func (r *maskArgvRecorder) ExecCommand(_ string, argv []string) (string, []string) {
	r.argv = argv
	return "false", nil
}

func TestDriverExecPathsUseMaskWrapper(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	recorder := &maskArgvRecorder{}
	for _, argv := range [][]string{{driverPlaywright, "snapshot"}, {driverPlaywright, helpFlag}, playwrightAttachArgv(), leaseGatedArgv([]string{driverPlaywright, "click", "e1"})} {
		_ = execPlaywright(t.Context(), nil, recorder, argv, io.Discard, io.Discard)
		if len(recorder.argv) < 6 || recorder.argv[3] != maskArgv0 || strings.Join(recorder.argv[5:], "\x00") != strings.Join(argv, "\x00") {
			t.Fatalf("unwrapped driver: %q", recorder.argv)
		}
	}
	d := &persistentDriver{ex: recorder}
	_ = d.start(t.Context())
	defer d.close()
	if len(recorder.argv) < 8 || recorder.argv[3] != maskArgv0 || recorder.argv[5] != "node" || recorder.argv[7] != persistentClientJS {
		t.Fatalf("unwrapped persistent client: %q", recorder.argv)
	}
}

func TestMaskExecStalledResponsePreservesExit(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).EnableFullDuplex()
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = io.Copy(io.Discard, r.Body)
		<-release
	}))
	defer s.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(t.Context(), 2*maskResponseTimeout)
	defer cancel()
	var out, errOut bytes.Buffer
	start := time.Now()
	err := runMaskExec(ctx, s.URL, "upgrade", []string{"sh", "-c", "echo test-sensitive-value; exit 29"}, nil, &out, &errOut)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 29 || time.Since(start) > maskResponseTimeout+3*time.Second {
		t.Fatalf("stalled response: %v after %v", err, time.Since(start))
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "output withheld") {
		t.Fatalf("outputs = %q, %q", out.String(), errOut.String())
	}
}

func TestMaskExecHeaderTimeoutDoesNotWaitForUploadEOF(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer s.Close()
	defer close(release)
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*maskResponseTimeout)
	defer cancel()
	start := time.Now()
	if streamMasked(ctx, s.URL, "hint", r, io.Discard, cancel) == "" {
		t.Fatal("stalled mask succeeded")
	}
	if time.Since(start) > maskResponseTimeout+3*time.Second {
		t.Fatal("header timeout waited for upload EOF")
	}
}

func TestMaskExecStalledUploadPreservesExit(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).EnableFullDuplex()
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer s.Close()
	defer close(release)
	// The write deadline counts from the last write that made progress, and how long the
	// loopback buffers keep absorbing the upload varies by host, so the bound is loose.
	ctx, cancel := context.WithTimeout(t.Context(), 4*maskResponseTimeout)
	defer cancel()
	var out, errOut bytes.Buffer
	start := time.Now()
	err := runMaskExec(ctx, s.URL, "upgrade", []string{"sh", "-c", "head -c 33554432 /dev/zero; exit 29"}, nil, &out, &errOut)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 29 || time.Since(start) > 3*maskResponseTimeout {
		t.Fatalf("stalled upload: %v after %v", err, time.Since(start))
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "output withheld") {
		t.Fatalf("outputs = %q, %q", out.String(), errOut.String())
	}
}

// The in-container `cuttle mask-exec --seed X` reads the root's persistent
// --seed, so it is validated and normalized like every other seed.
func TestMaskExecTakesPersistentSeed(t *testing.T) {
	withInstance(t, instanceFlags{})
	root := newRootCmd()
	root.AddCommand(newMaskExecCmd())
	cmd, _, err := root.Find([]string{"mask-exec"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.ParseFlags([]string{"--hint", "h", "--seed", "alpha", "--", "true"}); err != nil {
		t.Fatal(err)
	}
	if selectedSeed() != "alpha" || root.PersistentPreRunE(cmd, nil) != nil {
		t.Fatalf("seed %q", selectedSeed())
	}
}
