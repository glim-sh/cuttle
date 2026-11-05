package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glim-sh/cuttle/internal/backend"
	"github.com/glim-sh/cuttle/internal/config"
)

// A password with a stray newline fails a login in a way nothing explains, and
// every natural way to pipe one in appends exactly one.
func TestReadSecretStdinStripsOneTrailingNewline(t *testing.T) {
	for in, want := range map[string]string{
		"hunter2\n":      "hunter2",
		"hunter2\r\n":    "hunter2",
		"hunter2":        "hunter2",
		"two words\n":    "two words",
		"keeps\ninner\n": "keeps\ninner",
	} {
		got, err := readSecretStdin(strings.NewReader(in))
		if err != nil {
			t.Fatalf("readSecretStdin(%q): %v", in, err)
		}
		if string(got) != want {
			t.Errorf("readSecretStdin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadSecretStdinRefusesNothing(t *testing.T) {
	for _, in := range []string{"", "\n", "\r\n"} {
		if _, err := readSecretStdin(strings.NewReader(in)); !errors.Is(err, errSecretNoInput) {
			t.Errorf("readSecretStdin(%q) error = %v, want errSecretNoInput", in, err)
		}
	}
}

// `set` with no source must not fall back to reading a value from argv - that is
// the whole point of the verb - and it must not silently pick one of two.
func TestSecretSetNeedsExactlyOneSource(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want error
	}{
		"no source": {[]string{"GH_PASS"}, errSecretNeedSource},
		"both":      {[]string{"GH_PASS", "--stdin", "--exec", "echo hi"}, errSecretBothInputs},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := newSecretSetCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(&strings.Builder{})
			cmd.SetErr(&strings.Builder{})
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			if err := cmd.Execute(); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// A resolver runs on the host and its stdout IS the value; its stderr never
// propagates, because vault error text quotes item names and partial values.
func TestResolveExec(t *testing.T) {
	got, err := resolveExec(t.Context(), "printf 'hunter2\n'")
	if err != nil {
		t.Fatalf("resolveExec: %v", err)
	}
	if string(got) != "hunter2" {
		t.Fatalf("value = %q, want hunter2 with the trailing newline stripped", got)
	}
	if _, emptyErr := resolveExec(t.Context(), "printf ''"); !errors.Is(emptyErr, errSecretExecEmpty) {
		t.Fatalf("empty resolver error = %v, want errSecretExecEmpty", emptyErr)
	}
	_, err = resolveExec(t.Context(), "echo 'ERROR: item \"prod db\" has secret abc' >&2; exit 1")
	if !errors.Is(err, errSecretExecFailed) {
		t.Fatalf("failing resolver error = %v, want errSecretExecFailed", err)
	}
	if strings.Contains(err.Error(), "prod db") || strings.Contains(err.Error(), "abc") {
		t.Fatalf("the resolver's stderr reached the error text: %v", err)
	}
}

// `secret prompt` must never fall back to reading a pipe: "ask the human" that
// silently reads whatever was piped is a different verb, and --stdin is it.
//
// Asserted against promptTTY rather than the whole verb: the verb resolves a
// session before it asks anyone anything, so a command-level assertion here
// would pass or fail on whether the machine running the tests happens to have a
// daemon up - which is exactly how this test went green locally and red in CI.
func TestSecretPromptRefusesAPipe(t *testing.T) {
	cmd := newSecretPromptCmd()
	cmd.SetIn(strings.NewReader("123456\n"))
	if _, err := promptTTY(cmd); !errors.Is(err, errSecretNotATTY) {
		t.Fatalf("error = %v, want errSecretNotATTY", err)
	}

	// A file that is not a terminal is the same refusal - a redirect, not a pipe.
	f, err := os.CreateTemp(t.TempDir(), "in")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer func() { _ = f.Close() }()
	cmd.SetIn(f)
	if _, err := promptTTY(cmd); !errors.Is(err, errSecretNotATTY) {
		t.Fatalf("error = %v, want errSecretNotATTY for a redirected file", err)
	}
}

// A credential written into a repo is one `git add -A` from being published, and
// driver scratch state has been swept into a commit before.
func TestCaptureSinkRefusesAGitWorkingTree(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatalf("seeding a repo: %v", err)
	}
	nested := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("seeding a subdir: %v", err)
	}
	if _, _, err := parseSink("file:"+filepath.Join(nested, "key.txt"), false); !errors.Is(err, errCaptureInRepo) {
		t.Fatalf("error = %v, want errCaptureInRepo", err)
	}
	if _, _, err := parseSink("file:"+filepath.Join(nested, "key.txt"), true); err != nil {
		t.Fatalf("--force must override: %v", err)
	}

	// A `git worktree` checkout has .git as a FILE, and this feature was built in
	// one - recognizing only the directory form would miss exactly that case.
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
		t.Fatalf("seeding a worktree: %v", err)
	}
	if _, _, err := parseSink("file:"+filepath.Join(worktree, "key.txt"), false); !errors.Is(err, errCaptureInRepo) {
		t.Fatalf("worktree error = %v, want errCaptureInRepo", err)
	}
}

func TestCaptureSinkParsing(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "key.txt")
	for _, tc := range []struct{ to, wantSink, wantArg string }{
		{"", sinkMemory, ""},
		{"memory", sinkMemory, ""},
		{"file:" + outside, sinkFile, outside},
		{"exec:gh secret set X", sinkExec, "gh secret set X"},
	} {
		sink, arg, err := parseSink(tc.to, false)
		if err != nil {
			t.Fatalf("parseSink(%q): %v", tc.to, err)
		}
		if sink != tc.wantSink || arg != tc.wantArg {
			t.Errorf("parseSink(%q) = %q, %q; want %q, %q", tc.to, sink, arg, tc.wantSink, tc.wantArg)
		}
	}
	for _, bad := range []string{"stdout", "file:", "exec:"} {
		if _, _, err := parseSink(bad, false); !errors.Is(err, errCaptureSink) {
			t.Errorf("parseSink(%q) error = %v, want errCaptureSink", bad, err)
		}
	}
}

// The sink runs the command with the value on ITS STDIN: a value in argv is
// world-readable in /proc and lands in shell history.
func TestCaptureExecSinkFeedsStdin(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	if err := writeToSink(t.Context(), sinkExec, "cat > "+dest, []byte("s3cret"), false); err != nil {
		t.Fatalf("writeToSink: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading what the sink got: %v", err)
	}
	if string(got) != "s3cret" {
		t.Fatalf("the sink received %q, want the value on stdin", got)
	}
}

func TestCaptureFileSinkIs0600(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "key.txt")
	if err := writeToSink(t.Context(), sinkFile, dest, []byte("s3cret"), false); err != nil {
		t.Fatalf("writeToSink: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

// Exactly one source, and never a silent default: a capture that quietly read
// the wrong thing has already touched the credential.
func TestCaptureNeedsExactlyOneSource(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want error
	}{
		"no source": {[]string{"API_KEY"}, errCaptureNoSelector},
		"both":      {[]string{"API_KEY", "--selector", "#x", "--from-clipboard"}, errCaptureTwoSources},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := newSecretCaptureCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(&strings.Builder{})
			cmd.SetErr(&strings.Builder{})
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			if err := cmd.Execute(); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// os.WriteFile's mode applies only on create, so writing a credential over an
// existing world-readable scratch file would leave it world-readable. Replacing
// one at all needs --force now, which is what the true here stands for.
func TestWriteSecretFileTightensAnExistingFile(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "key.txt")
	if err := os.WriteFile(dest, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := writeSecretFile(dest, []byte("s3cret"), true); err != nil {
		t.Fatalf("writeSecretFile: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600 even over an existing file", info.Mode().Perm())
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "s3cret" {
		t.Fatalf("content = %q, want the new value", got)
	}
}

// Nothing irreversible may happen before the name is checked: a resolver spends
// a one-time code, and `prompt` asks a human to type one. A 400 from the daemon
// after the fact wastes exactly what this feature protects.
func TestSecretVerbsRefuseABadNameBeforeTakingAValue(t *testing.T) {
	for _, name := range []string{"GH-PASS", "9lives", "has space", "", strings.Repeat("x", 65)} {
		if err := checkSecretName(name); !errors.Is(err, errSecretBadName) {
			t.Errorf("checkSecretName(%q) = %v, want errSecretBadName", name, err)
		}
	}
	for _, name := range []string{"GH_PASS", "_x", "a1"} {
		if err := checkSecretName(name); err != nil {
			t.Errorf("checkSecretName(%q) = %v, want it accepted", name, err)
		}
	}

	// End to end, and the assertion that matters is that the value was never
	// TAKEN: a reader that records being read stands in for the human at a prompt
	// and for a resolver spending a one-time code, neither of which a test can
	// invoke directly. Asserting only on the error would pass with the ordering
	// reverted, since reading a pipe succeeds either way.
	pipe := &recordingReader{data: "hunter2\n"}
	cmd := newSecretSetCmd()
	cmd.SetArgs([]string{"GH-PASS", "--stdin"})
	cmd.SetIn(pipe)
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); !errors.Is(err, errSecretBadName) {
		t.Fatalf("error = %v, want errSecretBadName", err)
	}
	if pipe.read {
		t.Fatal("the value was taken before the name was checked - for `prompt` that is a human's one-time code")
	}
}

// recordingReader reports whether anything ever read from it.
type recordingReader struct {
	data string
	read bool
}

func (r *recordingReader) Read(p []byte) (int, error) {
	r.read = true
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, io.EOF
	}
	return n, nil
}

// Every path that writes browser bytes to a local path goes through checkDest.
// Under `downloads --latest` the destination is the BROWSER's filename, so
// without this a page picks which file in the working directory is replaced.
func TestCheckDestRefusesClobberAndMissingDir(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(existing, []byte("mine"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := checkDest(existing, false); !errors.Is(err, errDestExists) {
		t.Fatalf("error = %v, want errDestExists", err)
	}
	if err := checkDest(existing, true); err != nil {
		t.Fatalf("--force must allow it: %v", err)
	}
	if err := checkDest(filepath.Join(dir, "nope", "x.txt"), true); !errors.Is(err, errDestUnwritable) {
		t.Fatalf("error = %v, want errDestUnwritable even with --force", err)
	}
	// The file the refusal protected is untouched.
	if got, _ := os.ReadFile(existing); string(got) != "mine" {
		t.Fatalf("existing file = %q, want it left alone", got)
	}
}

// parseSink runs BEFORE the page is read, so an unusable destination costs
// nothing. Discovering it after the read has already spent a one-time value.
func TestParseSinkVetsTheDestinationBeforeAnyRead(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := parseSink(sinkFile+filepath.Join(dir, "nope", "k.txt"), false); !errors.Is(err, errDestUnwritable) {
		t.Fatalf("error = %v, want errDestUnwritable", err)
	}
	existing := filepath.Join(dir, "k.txt")
	if err := os.WriteFile(existing, []byte("mine"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, _, err := parseSink(sinkFile+existing, false); !errors.Is(err, errDestExists) {
		t.Fatalf("error = %v, want errDestExists", err)
	}
}

func TestSecretRemoveOfAnUnknownNameFails(t *testing.T) {
	t.Parallel()
	if _, err := removeReport("GH_PAS", false, false); !errors.Is(err, errSecretNotHeld) {
		t.Fatalf("removing a name nothing held = %v, want errSecretNotHeld (a non-zero exit)", err)
	}
	for _, c := range [][2]bool{{true, false}, {false, true}, {true, true}} {
		if _, err := removeReport("GH_PASS", c[0], c[1]); err != nil {
			t.Errorf("removeReport(held=%v, dropped=%v) = %v, want success", c[0], c[1], err)
		}
	}
}

// TestSecretTTLRefusesWhatTheDaemonWouldIgnore: a negative or sub-second --ttl
// used to come back as the 15m default. Refused before any value is taken.
func TestSecretTTLRefusesWhatTheDaemonWouldIgnore(t *testing.T) {
	t.Parallel()
	for _, ttl := range []string{"-5s", "500ms"} {
		pipe := &recordingReader{data: "hunter2\n"}
		cmd := newSecretSetCmd()
		cmd.SetArgs([]string{"GH_PASS", "--stdin", "--ttl", ttl})
		cmd.SetIn(pipe)
		cmd.SetOut(&strings.Builder{})
		cmd.SetErr(&strings.Builder{})
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if err := cmd.Execute(); !errors.Is(err, errSecretBadTTL) {
			t.Errorf("--ttl %s: error = %v, want errSecretBadTTL", ttl, err)
		}
		if pipe.read {
			t.Errorf("--ttl %s: the value was taken before the TTL was checked", ttl)
		}
	}
	for _, ttl := range []time.Duration{0, time.Second, time.Hour} {
		if err := checkSecretTTL(ttl); err != nil {
			t.Errorf("checkSecretTTL(%s) = %v, want it accepted", ttl, err)
		}
	}
}

// argvRecorder is hostCurl that also keeps every argv it was asked to run.
type argvRecorder struct {
	hostCurl
	mu   sync.Mutex
	argv [][]string
}

func (r *argvRecorder) ExecCommand(workdir string, argv []string) (string, []string) {
	r.mu.Lock()
	r.argv = append(r.argv, append([]string(nil), argv...))
	r.mu.Unlock()
	return r.hostCurl.ExecCommand(workdir, argv)
}

// The docker backends reach the loopback-only routes through curl in the
// container, because their published port arrives from the bridge gateway.
// The transport must keep a secret value off curl's argv, map any daemon
// status back the way a direct request does, and stream a large or chunked
// body - including one the caller stops reading.
func TestExecTransportCarriesDaemonRequestsOverCurl(t *testing.T) {
	big := strings.Repeat("0123456789abcdef", 1<<16) // 1 MiB, sent chunked
	var gotBody, gotType atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/secret/GH_PASS":
			b, _ := io.ReadAll(r.Body)
			gotBody.Store(string(b))
			gotType.Store(r.Header.Get("Content-Type"))
			_, _ = w.Write([]byte(`{"name":"GH_PASS","length":7,"ttl_seconds":900}`))
		case "/downloads/big.bin":
			for i := 0; i < len(big); i += 1 << 14 {
				_, _ = io.WriteString(w, big[i:i+1<<14])
				w.(http.Flusher).Flush()
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such thing"}`))
		}
	}))
	t.Cleanup(srv.Close)
	ex := &argvRecorder{base: srv.URL}
	// The published port is never dialled: every request goes through the exec.
	d := daemonAPI{base: "http://127.0.0.1:1", client: &http.Client{Transport: execTransport{ex: ex}}}

	var reply struct {
		Name string `json:"name"`
	}
	if err := d.json(t.Context(), http.MethodPut, "/secret/GH_PASS?fingerprint=s1", map[string]any{"value": "hunter2"}, &reply); err != nil || reply.Name != "GH_PASS" {
		t.Fatalf("PUT over exec = %+v, %v", reply, err)
	}
	if body, _ := gotBody.Load().(string); body != `{"value":"hunter2"}` {
		t.Fatalf("daemon got body %q", body)
	}
	if ct, _ := gotType.Load().(string); ct != "application/json" {
		t.Fatalf("daemon got Content-Type %q", ct)
	}
	for _, argv := range ex.argv {
		if strings.Contains(strings.Join(argv, " "), "hunter2") {
			t.Fatalf("the secret value reached an argv: %q", argv)
		}
		if last := argv[len(argv)-1]; last != playwrightCDPEndpoint+"/secret/GH_PASS?fingerprint=s1" {
			t.Fatalf("curl target = %q, want the in-container endpoint", last)
		}
	}

	if err := d.json(t.Context(), http.MethodGet, "/nope", nil, nil); !errors.Is(err, errDaemonNotFound) || !strings.Contains(err.Error(), "no such thing") {
		t.Fatalf("404 over exec = %v, want errDaemonNotFound with the daemon's message", err)
	}

	dest := filepath.Join(t.TempDir(), "big.bin")
	var out strings.Builder
	if err := pullDownload(t.Context(), &out, d, "big.bin", dest, false); err != nil {
		t.Fatalf("pull over exec: %v", err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != big {
		t.Fatalf("pulled %d bytes, %v; want the %d the daemon sent", len(got), err, len(big))
	}

	// A caller that stops reading must still get its Close back.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, d.base+"/downloads/big.bin", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := d.do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { _ = resp.Body.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("closing a half-read body hung on the exec")
	}

	// Nothing listening in the container: the error says where it failed.
	dead := daemonAPI{base: d.base, client: &http.Client{Transport: execTransport{ex: hostCurl{base: "http://127.0.0.1:1"}}}}
	if err := dead.json(t.Context(), http.MethodGet, "/secret", nil, nil); err == nil || !strings.Contains(err.Error(), "reaching the daemon in the container") {
		t.Fatalf("unreachable daemon = %v", err)
	}
}

// Which backends must go through the exec: only the docker ones, whose
// published port presents the bridge gateway as the peer.
func TestDaemonForRoutesDockerBackendsThroughExec(t *testing.T) {
	ep := backend.Endpoint{CDPHost: "127.0.0.1", CDPPort: 9333}
	for _, tc := range []struct {
		backend string
		b       backend.Backend
		exec    bool
	}{
		{"", execBackend{}, true},
		{config.BackendLocal, execBackend{}, true},
		{config.BackendSSH, execBackend{}, true},
		{config.BackendK8s, execBackend{}, false},
		{config.BackendDirect, plainBackend{}, false},
	} {
		d := daemonFor(config.Context{Backend: tc.backend}, tc.b, ep)
		if d.base != "http://127.0.0.1:9333" {
			t.Errorf("%q: base = %q", tc.backend, d.base)
		}
		if got := d.client != nil; got != tc.exec {
			t.Errorf("%q: through exec = %v, want %v", tc.backend, got, tc.exec)
		}
	}
}

type plainBackend struct{ backend.Backend }

type execBackend struct{ plainBackend }

func (execBackend) ExecCommand(string, []string) (string, []string) { return "true", nil }

func TestSecretRefreshRetainsOrigin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	setSelectorEnv(t, config.EnvContext, "")
	setSelectorEnv(t, config.EnvName, "")
	withInstance(t, instanceFlags{})
	var origins []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json/version":
			_, _ = io.WriteString(w, `{"Browser":"Chrome","webSocketDebuggerUrl":"ws://127.0.0.1/devtools/browser/test"}`)
		case "/secret/BOUND":
			var body struct {
				Origin string `json:"origin"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			origins = append(origins, body.Origin)
			_, _ = io.WriteString(w, `{"name":"BOUND","length":10}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg := &config.Config{DefaultContext: "test", Contexts: map[string]config.Context{"test": {Backend: config.BackendDirect, CDPURL: srv.URL}}}
	if err := cfg.Save(config.DefaultPath()); err != nil {
		t.Fatal(err)
	}
	cmd := newSecretSetCmd()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"BOUND", "--exec", "printf test-value", "--origin", "https://example.com"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	refresh := newSecretRefreshCmd()
	refresh.SetOut(io.Discard)
	refresh.SetArgs([]string{"BOUND"})
	if err := refresh.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(origins) != 2 || origins[0] != "https://example.com" || origins[1] != origins[0] {
		t.Fatalf("origin was not retained: %v", origins)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	loaded.RemoveSecret("BOUND")
	if len(loaded.SecretOrigins) != 0 {
		t.Fatal("removed recipe retained its origin")
	}
}

func TestValidOriginArg(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", true},
		{"https://example.com", true},
		{"http://example.com:8080/", true},
		{"example.com", false},
		{"https://example.com/login", false},
		{"ftp://example.com", false},
		{"https://user@example.com", false},
		{"https://example.com?x", false},
	} {
		if got := validOriginArg(tc.in); got != tc.want {
			t.Errorf("validOriginArg(%q) = %v", tc.in, got)
		}
	}
}
