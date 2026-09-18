package cli

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every test here drives the real wrapper against a stub masker, because the
// three properties that matter - the output is masked, it still streams, and an
// old image degrades instead of failing - are all properties of the wiring, not
// of any one function.

// stubMasker answers /mask by replacing the fake secret with a placeholder, and
// records each batch it was given so a test can assert on the batching.
type stubMasker struct {
	mu      sync.Mutex
	batches []string
	status  int
}

func (s *stubMasker) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.batches = append(s.batches, string(body))
		s.mu.Unlock()
		if s.status != 0 {
			w.WriteHeader(s.status)
			return
		}
		_, _ = io.WriteString(w, strings.ReplaceAll(string(body), "hunter2-not-a-real-password", "<secret:PASS>"))
	}))
	t.Cleanup(srv.Close)
	previous := maskEndpoint
	maskEndpoint = srv.URL + "/mask"
	t.Cleanup(func() { maskEndpoint = previous })
}

func (s *stubMasker) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.batches...)
}

// runMaskExecCmd runs the wrapper over a shell command, returning its streams.
func runMaskExecCmd(t *testing.T, stdin io.Reader, script string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := newMaskExecCmd()
	cmd.SetArgs([]string{"--", "sh", "-c", script})
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(stdin)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

func TestMaskExecMasksBothStreams(t *testing.T) {
	(&stubMasker{}).start(t)
	out, errOut, err := runMaskExecCmd(t, nil,
		`echo "filled hunter2-not-a-real-password"; echo "failed for hunter2-not-a-real-password" >&2`)
	if err != nil {
		t.Fatalf("wrapper: %v", err)
	}
	for name, got := range map[string]string{"stdout": out, "stderr": errOut} {
		if strings.Contains(got, "hunter2-not-a-real-password") {
			t.Errorf("%s carried the value: %q", name, got)
		}
		if !strings.Contains(got, "<secret:PASS>") {
			t.Errorf("%s was not masked: %q", name, got)
		}
	}
}

// The driver's stdin and exit code are its own; masking sits only on the way out.
func TestMaskExecPassesStdinAndTheExitCode(t *testing.T) {
	(&stubMasker{}).start(t)
	out, _, err := runMaskExecCmd(t, strings.NewReader("piped\n"), "cat; exit 3")
	if out != "piped\n" {
		t.Errorf("stdin did not reach the child: %q", out)
	}
	var exit *ExitCodeError
	if !errors.As(err, &exit) || exit.Code != 3 {
		t.Fatalf("err = %v, want exit status 3", err)
	}
}

// A streaming verb must stay streaming: a line printed now has to be readable
// now, not when the process ends. `cuttle pw` has verbs that run for minutes.
func TestMaskExecKeepsStreaming(t *testing.T) {
	(&stubMasker{}).start(t)
	first := make(chan string, 1)
	var out bytes.Buffer
	cmd := newMaskExecCmd()
	cmd.SetArgs([]string{"--", "sh", "-c", "echo early; sleep 3; echo late"})
	cmd.SetOut(writerFunc(func(p []byte) (int, error) {
		select {
		case first <- string(p):
		default:
		}
		return out.Write(p)
	}))
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(t.Context()) }()

	select {
	case got := <-first:
		if !strings.Contains(got, "early") {
			t.Fatalf("first write = %q, want the first line", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first line was still buffered a second after it was printed")
	}
	if err := <-done; err != nil {
		t.Fatalf("wrapper: %v", err)
	}
}

// A prompt has no newline to batch on, so it leaves on the idle timer instead.
func TestMaskExecFlushesALineWithNoNewline(t *testing.T) {
	stub := &stubMasker{}
	stub.start(t)
	out, _, err := runMaskExecCmd(t, nil, `printf 'value: '; sleep 1`)
	if err != nil {
		t.Fatalf("wrapper: %v", err)
	}
	if out != "value: " {
		t.Fatalf("stdout = %q, want the unterminated prompt", out)
	}
	if got := stub.seen(); len(got) != 1 || got[0] != "value: " {
		t.Fatalf("batches = %q, want the prompt on its own", got)
	}
}

// A daemon that cannot mask - down, slow, erroring - must not turn masking off:
// the batch is withheld, the caller told once, and the run reports failure.
func TestMaskExecFailsClosed(t *testing.T) {
	(&stubMasker{status: http.StatusInternalServerError}).start(t)
	out, errOut, err := runMaskExecCmd(t, nil, `echo hunter2-not-a-real-password; echo again`)
	if !errors.Is(err, errMaskWithheld) {
		t.Fatalf("err = %v, want the withheld error", err)
	}
	if out != "" {
		t.Errorf("unmasked output reached stdout: %q", out)
	}
	if n := strings.Count(errOut, "withheld"); n != 1 {
		t.Errorf("want exactly one warning, got %d: %q", n, errOut)
	}
}

// The wrapper is only reachable because `cuttle pw` execs it instead of the
// driver - and only safely so because an older image can answer the probe with
// "no".
func TestMaskedArgvWrapsTheDriverWithAProbe(t *testing.T) {
	t.Parallel()
	argv := maskedArgv([]string{driverPlaywright, "snapshot"})
	if argv[0] != "sh" || argv[1] != "-c" || argv[3] != "sh" {
		t.Fatalf("argv = %q, want a shell wrapper", argv)
	}
	if !strings.Contains(argv[2], "__mask-exec "+maskCheckFlag) {
		t.Errorf("no capability probe in the script: %q", argv[2])
	}
	if got := argv[4:]; got[0] != driverPlaywright || got[1] != "snapshot" {
		t.Errorf("the driver argv was rewritten: %q", got)
	}
}

func TestMaskExecCheckFlagSucceeds(t *testing.T) {
	t.Parallel()
	cmd := newMaskExecCmd()
	cmd.SetArgs([]string{maskCheckFlag})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("the probe must succeed on an image that masks: %v", err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
