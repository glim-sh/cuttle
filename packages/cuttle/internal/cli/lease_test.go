package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glim-sh/cuttle/internal/config"
)

// hostCurl runs the in-container curl on this host, aimed at a test server in
// place of the daemon.
type hostCurl struct{ base string }

func (h hostCurl) ExecCommand(_ string, argv []string) (string, []string) {
	args := make([]string, 0, len(argv)-1)
	for _, a := range argv[1:] {
		args = append(args, strings.Replace(a, playwrightCDPEndpoint, h.base, 1))
	}
	return argv[0], args
}

// leaseStub records the requests it sees and answers each with reply.
type leaseStub struct {
	mu    sync.Mutex
	seen  []string
	reply func(r *http.Request) (int, string)
}

func (s *leaseStub) start(t *testing.T) hostCurl {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.Method+" "+r.URL.RequestURI())
		s.mu.Unlock()
		code, body := s.reply(r)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return hostCurl{base: srv.URL}
}

func (s *leaseStub) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

const heldBody = `{"held":true,"owner":"jev-browse 41@host","age_seconds":42,"ttl_seconds":120}`

func TestPlaywrightReadOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"snapshot"}, true},
		{[]string{"console", "error"}, true},
		{[]string{"tab-list"}, true},
		{[]string{"click", "--help"}, true},
		{[]string{"--version"}, true},
		{[]string{"-s=cuttle", "snapshot"}, true},
		{[]string{"click", "e5"}, false},
		{[]string{"fill", "e5", "snapshot"}, false},
		{[]string{"goto", "https://example.com"}, false},
		{[]string{"eval", "document.title"}, false},
		{[]string{"attach"}, false},
		{[]string{"some-future-verb"}, false},
		{[]string{"--help", "fill"}, true},
		{[]string{"fill", "e5", "-h", "--", "x"}, true},
		{[]string{"fill", "e5", "--", "--help"}, false},
		{[]string{"type", "--", "-h"}, false},
		{[]string{"help"}, false},
		{[]string{"docs"}, false},
		{[]string{"-s", "snapshot", "click", "e5"}, false},
		{[]string{"--filename", "snapshot", "click", "e5"}, false},
		{[]string{"--json", "snapshot"}, true},
		{[]string{"-json", "snapshot", "click", "e5"}, false},
		{[]string{"--", "snapshot"}, false},
	}
	for _, tt := range tests {
		if got := playwrightReadOnly(tt.args); got != tt.want {
			t.Errorf("playwrightReadOnly(%v)=%v want %v", tt.args, got, tt.want)
		}
	}
}

func TestGatePlaywright(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		takeover bool
		code     int
		body     string
		wantErr  bool
		wantSeen string // the one request expected, or "" for none
	}{
		{name: "held refuses a mutating verb", args: []string{"fill", "e5", "hunter2"}, code: 200, body: heldBody, wantErr: true, wantSeen: "GET /lease"},
		{name: "held lets a read verb through without asking", args: []string{"snapshot"}, code: 200, body: heldBody},
		{name: "a free lease passes", args: []string{"click", "e5"}, code: 200, body: `{"held":false}`, wantSeen: "GET /lease"},
		{name: "a daemon without leases passes", args: []string{"click", "e5"}, code: 404, body: "404 page not found", wantSeen: "GET /lease"},
		{name: "takeover force-releases and proceeds", args: []string{"click", "e5"}, takeover: true, code: 200, body: `{"status":"ok"}`, wantSeen: "DELETE /lease?force=true&owner=cuttle+pw+"},
		{name: "takeover in front of a read verb evicts nobody", args: []string{"snapshot"}, takeover: true, code: 200, body: heldBody},
		{name: "takeover in front of a help request evicts nobody", args: []string{"click", "--help"}, takeover: true, code: 200, body: heldBody},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stub := &leaseStub{reply: func(*http.Request) (int, string) { return tt.code, tt.body }}
			ex := stub.start(t)
			err := gatePlaywright(context.Background(), ex, "cuttle", tt.args, tt.takeover)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if err != nil {
				msg := err.Error()
				if !errors.Is(err, errSessionLeased) || !strings.Contains(msg, "jev-browse 41@host") ||
					!strings.Contains(msg, "42s") || !strings.Contains(msg, flagTakeover) {
					t.Fatalf("refusal should name holder, age and the takeover flag: %q", msg)
				}
				if strings.Contains(msg, "hunter2") {
					t.Fatalf("refusal must not echo the args: %q", msg)
				}
			}
			seen := stub.requests()
			if tt.wantSeen == "" {
				if len(seen) != 0 {
					t.Fatalf("expected no lease request, saw %v", seen)
				}
				return
			}
			if len(seen) != 1 || !strings.HasPrefix(seen[0], tt.wantSeen) {
				t.Fatalf("requests=%v want one starting %q", seen, tt.wantSeen)
			}
		})
	}
}

func TestAcquireLease(t *testing.T) {
	t.Parallel()
	t.Run("conflict names holder, age and takeover", func(t *testing.T) {
		t.Parallel()
		stub := &leaseStub{reply: func(*http.Request) (int, string) { return http.StatusConflict, heldBody }}
		_, err := acquireLease(context.Background(), stub.start(t), "jev-browse 7@me", false)
		if !errors.Is(err, errSessionLeased) || !strings.Contains(err.Error(), "jev-browse 41@host (for 42s)") ||
			!strings.Contains(err.Error(), flagTakeover) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("grant carries token and ttl", func(t *testing.T) {
		t.Parallel()
		stub := &leaseStub{reply: func(*http.Request) (int, string) {
			return http.StatusOK, `{"owner":"jev-browse 7@me","token":"abc","ttl_seconds":120}`
		}}
		l, err := acquireLease(context.Background(), stub.start(t), "jev-browse 7@me", false)
		if err != nil || l.token != "abc" || l.ttl != 120*time.Second {
			t.Fatalf("lease=%+v err=%v", l, err)
		}
		l.release()
		if seen := stub.requests(); len(seen) != 2 || seen[1] != "DELETE /lease?token=abc" {
			t.Fatalf("release should DELETE with the token: %v", seen)
		}
	})
	t.Run("takeover force-releases before acquiring", func(t *testing.T) {
		t.Parallel()
		stub := &leaseStub{reply: func(*http.Request) (int, string) {
			return http.StatusOK, `{"token":"abc","ttl_seconds":120}`
		}}
		if _, err := acquireLease(context.Background(), stub.start(t), "jev-browse 7@me", true); err != nil {
			t.Fatal(err)
		}
		seen := stub.requests()
		if len(seen) != 2 || !strings.HasPrefix(seen[0], "DELETE /lease?force=true") || !strings.HasPrefix(seen[1], "POST /lease") {
			t.Fatalf("requests=%v", seen)
		}
	})
	t.Run("a daemon without leases runs unleased", func(t *testing.T) {
		t.Parallel()
		stub := &leaseStub{reply: func(*http.Request) (int, string) { return http.StatusNotFound, "404 page not found" }}
		l, err := acquireLease(context.Background(), stub.start(t), "jev-browse 7@me", false)
		if err != nil || l.token != "" {
			t.Fatalf("lease=%+v err=%v", l, err)
		}
		l.release()
		if seen := stub.requests(); len(seen) != 1 {
			t.Fatalf("an unleased run must not release: %v", seen)
		}
	})
}

func TestLeaseHeartbeatStopsTheRunOnTakeover(t *testing.T) {
	t.Parallel()
	stub := &leaseStub{reply: func(r *http.Request) (int, string) {
		if r.URL.Query().Get("token") != "abc" {
			return http.StatusBadRequest, `{}`
		}
		return http.StatusConflict, `{"owner":"cuttle pw 9@there","age_seconds":0}`
	}}
	l := &sessionLease{ex: stub.start(t), owner: "jev-browse 7@me", token: "abc", ttl: 90 * time.Millisecond}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go l.heartbeat(ctx, cancel)
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("heartbeat never noticed the takeover")
	}
	cause := context.Cause(ctx)
	if !errors.Is(cause, errSessionTakenOver) || !strings.Contains(cause.Error(), "cuttle pw 9@there") {
		t.Fatalf("cause=%v", cause)
	}
}

func TestLeaseGuardStopsBeforeTheNextActionOnTakeover(t *testing.T) {
	t.Parallel()
	stub := &leaseStub{reply: func(*http.Request) (int, string) {
		return http.StatusConflict, `{"owner":"cuttle pw 9@there","age_seconds":0}`
	}}
	l := &sessionLease{ex: stub.start(t), owner: "jev-browse 7@me", token: "abc", ttl: time.Hour}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var ran []string
	drive := l.guard(func(_ context.Context, args ...string) (string, error) {
		ran = append(ran, args[0])
		return "", nil
	}, cancel)

	if _, err := drive(ctx, "snapshot"); err != nil || len(stub.requests()) != 0 {
		t.Fatalf("a read verb should pass without a lease call: err=%v requests=%v", err, stub.requests())
	}
	if _, err := drive(ctx, "click", "e3"); !errors.Is(err, errSessionTakenOver) {
		t.Fatalf("err=%v, want the takeover", err)
	}
	if len(ran) != 1 || ran[0] != "snapshot" {
		t.Fatalf("the click must not run after a takeover: ran=%v", ran)
	}
	if cause := context.Cause(ctx); !strings.Contains(cause.Error(), "cuttle pw 9@there") {
		t.Fatalf("cause=%v", cause)
	}
}

// A renew is a process exec, so the guard skips its own while the last grant is
// fresh, and renews once it is not.
func TestLeaseGuardSkipsARenewWhileTheGrantIsFresh(t *testing.T) {
	t.Parallel()
	stub := &leaseStub{reply: func(*http.Request) (int, string) {
		return http.StatusOK, `{"owner":"jev-browse 7@me","token":"abc","ttl_seconds":120}`
	}}
	l := &sessionLease{ex: stub.start(t), owner: "jev-browse 7@me", token: "abc", ttl: time.Hour}
	l.renewedAt.Store(time.Now().UnixNano())
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	drive := l.guard(func(context.Context, ...string) (string, error) { return "", nil }, cancel)

	for range 3 {
		if _, err := drive(ctx, "click", "e3"); err != nil {
			t.Fatalf("click: %v", err)
		}
	}
	if n := len(stub.requests()); n != 0 {
		t.Fatalf("renewed %d times on a fresh grant, want none", n)
	}
	stale := time.Now().Add(-guardRenewEvery).UnixNano()
	l.renewedAt.Store(stale)
	for range 3 {
		if _, err := drive(ctx, "click", "e3"); err != nil {
			t.Fatalf("click: %v", err)
		}
	}
	if n := len(stub.requests()); n != 1 {
		t.Fatalf("renewed %d times on a stale grant, want once - the grant it got is fresh", n)
	}
	if l.renewedAt.Load() <= stale {
		t.Error("a granted renew did not refresh the grant time")
	}
}

// TestLeaseHeartbeatAndGuardShareOneLease drives the two goroutines a real run
// has on one sessionLease - the heartbeat and the per-verb guard - at once, so
// -race covers the sharing, and then takes the lease away to show the heartbeat
// still ends the run once the verbs have stopped.
func TestLeaseHeartbeatAndGuardShareOneLease(t *testing.T) {
	t.Parallel()
	const (
		drivers = 4
		laps    = 10
		// one driving verb and one read verb per lap
		wantDrives = drivers * laps * 2
	)
	var taken atomic.Bool
	stub := &leaseStub{reply: func(*http.Request) (int, string) {
		if taken.Load() {
			return http.StatusConflict, `{"owner":"cuttle pw 9@there"}`
		}
		return http.StatusOK, `{"owner":"jev-browse 7@me","token":"abc","ttl_seconds":120}`
	}}
	l := &sessionLease{ex: stub.start(t), owner: "jev-browse 7@me", token: "abc", ttl: 120 * time.Millisecond}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go l.heartbeat(ctx, cancel)

	var drives atomic.Int64
	drive := l.guard(func(_ context.Context, _ ...string) (string, error) {
		drives.Add(1)
		return "", nil
	}, cancel)
	var wg sync.WaitGroup
	for range drivers {
		wg.Go(func() {
			for range laps {
				if _, err := drive(ctx, "click", "e5"); err != nil {
					t.Errorf("a verb was refused while the lease was held: %v", err)
					return
				}
				if _, err := drive(ctx, "snapshot"); err != nil {
					t.Errorf("a read verb was refused: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	if got := drives.Load(); got != wantDrives {
		t.Fatalf("ran %d verbs, want %d", got, wantDrives)
	}

	taken.Store(true)
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the heartbeat never noticed the takeover")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, errSessionTakenOver) {
		t.Fatalf("cause=%v, want the takeover", cause)
	}
}

// fakeDocker is a `docker` that logs every call on one line, reports any container running,
// and runs an exec on this host, past the in-container workdir wrapper, with
// fakeCurl and fakeDriver standing in for the image's curl and driver.
const fakeDocker = `#!/bin/sh
printf '%s\n' "$*" | tr '\n' ' ' >> "$FAKE_DOCKER_LOG" && echo >> "$FAKE_DOCKER_LOG"
case "$1" in
inspect) echo running ;;
exec) shift 8; exec "$@" ;; # exec -i <container> sh -c <script> sh <workdir>: backend.inWorkdir, whose mkdir is the container's
esac
`

// fakeCurl answers a lease GET with FAKE_DOCKER_LEASE and FAKE_DOCKER_LEASE_CODE
// (default 200), refuses a POST with FAKE_DOCKER_LEASE, and releases on DELETE,
// ending the body in a newline as the daemon's JSON does and honoring -w.
const fakeCurl = `#!/bin/sh
body=$FAKE_DOCKER_LEASE code=${FAKE_DOCKER_LEASE_CODE:-200}
case "$*" in
*"-X POST"*) code=409 ;;
*"-X DELETE"*) body='{"status":"ok"}' code=200 ;;
esac
while [ $# -gt 0 ]; do [ "$1" = -w ] && w=$2; shift; done
printf '%s\n' "$body"
printf '%s' "$w" | sed "s/%{http_code}/$code/"
`

// fakeDriver echoes its verb, and fails one whose target is "missing".
const fakeDriver = `#!/bin/sh
echo "ran $*"
case "$*" in *missing*) exit 1 ;; esac
`

// fakeDockerOnPath puts fakeDocker, fakeCurl and fakeDriver on PATH with lease
// as the daemon's lease reply, and returns the path of the docker call log.
func fakeDockerOnPath(t *testing.T, lease string) string {
	t.Helper()
	bin := t.TempDir()
	for name, body := range map[string]string{"docker": fakeDocker, "curl": fakeCurl, driverPlaywright: fakeDriver} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(t.TempDir(), "docker.log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_LOG", log)
	t.Setenv("FAKE_DOCKER_LEASE", lease)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return log
}

// TestLeaseFollowsTheSelectedInstance runs the real command tree against a fake
// docker: every lease call - the pw gate, its takeover, jev-browse's acquire -
// must exec into the instance --name or CUTTLE_NAME selected, never the default
// "cuttle", and the takeover the refusal suggests must reach that instance too.
func TestLeaseFollowsTheSelectedInstance(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		env       string
		wantErr   string // substring of the error, "" for success
		wantCurls int
		wantCall  string // a lease call that must appear in the docker log
	}{
		{name: "pw gate with --name", args: []string{"--name", "leased", "pw", "click", "e5"}, wantErr: "`cuttle --name leased pw --takeover <verb> ...`", wantCurls: 1},
		{name: "pw gate with CUTTLE_NAME", args: []string{"pw", "click", "e5"}, env: "leased", wantErr: "`cuttle --name leased pw --takeover <verb> ...`", wantCurls: 1},
		{name: "pw takeover with --name", args: []string{"--name", "leased", "pw", "--takeover", "click", "e5"}, wantCurls: 1, wantCall: "-X DELETE"},
		{name: "pw takeover before --name", args: []string{"pw", "--takeover", "--name", "leased", "click", "e5"}, wantCurls: 1, wantCall: "-X DELETE"},
		{name: "jev-browse acquire with --name", args: []string{"--name", "leased", "jev-browse", "--mock", "a task"}, wantErr: "rerun with --takeover", wantCurls: 1},
		{name: "jev-browse acquire with CUTTLE_NAME", args: []string{"jev-browse", "--mock", "a task"}, env: "leased", wantErr: "rerun with --takeover", wantCurls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := fakeDockerOnPath(t, heldBody)
			setSelectorEnv(t, config.EnvContext, "")
			setSelectorEnv(t, config.EnvName, tc.env)
			withInstance(t, instanceFlags{})
			var out bytes.Buffer
			rootCmd.SetOut(&out)
			rootCmd.SetErr(&out)
			rootCmd.SetArgs(tc.args)
			err := rootCmd.Execute()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v, want success", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}

			raw, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			curls := 0
			for line := range strings.Lines(string(raw)) {
				f := strings.Fields(line)
				var target string
				switch f[0] {
				case "inspect":
					target = f[len(f)-1]
				case "exec": // exec -i <container> sh -c <script> sh <workdir> argv...
					target = f[2]
					if strings.Contains(line, "curl -s") {
						curls++
					}
				default:
					continue
				}
				if target != "leased" {
					t.Fatalf("docker %q went to %q, not the selected instance", strings.TrimSpace(line), target)
				}
			}
			if !strings.Contains(string(raw), tc.wantCall) {
				t.Fatalf("docker log lacks %q:\n%s", tc.wantCall, raw)
			}
			if curls < tc.wantCurls {
				t.Fatalf("saw %d lease calls, want at least %d; docker log:\n%s", curls, tc.wantCurls, raw)
			}
		})
	}
}

// The in-exec lease check runs the verb only on a lease the daemon calls free or
// a daemon predating leases; every other answer leaves the verb unrun for
// gatePlaywright.
func TestLeaseGatedArgv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		code    int
		body    string
		wantRan bool
	}{
		{name: "free", code: 200, body: "{\"held\":false}\n", wantRan: true},
		{name: "daemon without leases", code: 404, body: "404 page not found\n", wantRan: true},
		{name: "held", code: 200, body: heldBody + "\n"},
		{name: "daemon error", code: 500, body: "{\"held\":false}\n"},
		{name: "unexpected body", code: 200, body: "{}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stub := &leaseStub{reply: func(*http.Request) (int, string) { return tt.code, tt.body }}
			var out, errOut bytes.Buffer
			err := execIn(context.Background(), nil, stub.start(t), "/", leaseGatedArgv([]string{"echo", "ran"}), &out, &errOut)
			if ran := out.String() == "ran\n"; ran != tt.wantRan || (err == nil) != tt.wantRan {
				t.Fatalf("out=%q err=%v, want ran=%v", out.String(), err, tt.wantRan)
			}
			if !tt.wantRan && !leaseUnsettled(err, "", errOut.String()) {
				t.Fatalf("err=%v stderr=%q, want the unsettled marker", err, errOut.String())
			}
			if seen := stub.requests(); len(seen) != 1 || seen[0] != "GET /lease" {
				t.Fatalf("requests=%v, want one GET /lease", seen)
			}
		})
	}
	var errOut bytes.Buffer
	err := execIn(context.Background(), nil, hostCurl{base: "http://127.0.0.1:1"}, "/", leaseGatedArgv([]string{"echo", "ran"}), io.Discard, &errOut)
	if !leaseUnsettled(err, "", errOut.String()) {
		t.Fatalf("unreachable daemon: err=%v stderr=%q, want the unsettled marker", err, errOut.String())
	}
	// A verb that ran and happened to exit the same way is not the gate: reading
	// it as one would run it a second time.
	script := `echo "$1"; echo "` + leaseUnsettledMarker + `" >&2; exit ` + strconv.Itoa(leaseUnsettledExit)
	for _, stdout := range []string{"", "page text"} {
		var o, e bytes.Buffer
		c := exec.Command("sh", "-c", script, "sh", stdout)
		c.Stdout, c.Stderr = &o, &e
		err := c.Run()
		stderr := e.String()
		if stdout == "" {
			stderr += "trailing driver output\n"
		}
		if leaseUnsettled(err, o.String(), stderr) {
			t.Fatalf("stdout=%q stderr=%q read as the gate", o.String(), stderr)
		}
	}
}

// Every docker process is a round trip an agent pays on each verb, so the
// common paths are pinned: a verb that succeeds is one exec, lease check
// included, and the state is asked only on a path that needs it.
func TestPlaywrightDockerCallsPerVerb(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		lease   string
		code    string
		wantErr string
		want    []string // the first word after `docker` of each call, in order
	}{
		{name: "read verb", args: []string{"pw", "snapshot"}, lease: heldBody, want: []string{"exec"}},
		{name: "driving verb on a free lease", args: []string{"pw", "click", "e5"}, lease: `{"held":false}`, want: []string{"exec"}},
		{name: "driving verb on a daemon without leases", args: []string{"pw", "click", "e5"}, lease: "404 page not found", code: "404", want: []string{"exec"}},
		{name: "driving verb on a held lease", args: []string{"pw", "click", "e5"}, lease: heldBody, wantErr: "already being driven", want: []string{"exec", "exec"}},
		{name: "takeover", args: []string{"pw", "--takeover", "click", "e5"}, lease: heldBody, want: []string{"inspect", "exec", "exec"}},
		{name: "driving verb that fails", args: []string{"pw", "click", "missing"}, lease: `{"held":false}`, wantErr: "exited with status 1", want: []string{"exec", "inspect"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := fakeDockerOnPath(t, tc.lease)
			t.Setenv("FAKE_DOCKER_LEASE_CODE", tc.code)
			setSelectorEnv(t, config.EnvContext, "")
			setSelectorEnv(t, config.EnvName, "")
			withInstance(t, instanceFlags{})
			var out bytes.Buffer
			rootCmd.SetOut(&out)
			rootCmd.SetErr(&out)
			rootCmd.SetArgs(tc.args)
			err := rootCmd.Execute()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v, want success", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			case tc.wantErr == "" && !strings.HasSuffix(out.String(), " "+tc.args[len(tc.args)-1]+"\n"):
				t.Fatalf("output %q lacks the verb's own", out.String())
			}
			raw, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for line := range strings.Lines(string(raw)) {
				got = append(got, strings.Fields(line)[0])
			}
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("docker calls %v, want %v:\n%s", got, tc.want, raw)
			}
		})
	}
}

// exitOnceExecer fails its first exec with the given status, as curl does while
// a restarted container's daemon is not listening yet, then answers the lease.
type exitOnceExecer struct {
	dir  string
	code int
}

func (e exitOnceExecer) ExecCommand(_ string, _ []string) (string, []string) {
	const script = `cd "$0" || exit 2
if [ ! -e tried ]; then touch tried; echo "curl: (7) Failed to connect" >&2; exit "$1"; fi
printf '{"held":false}\n200'`
	return "sh", []string{"-c", script, e.dir, strconv.Itoa(e.code)}
}

// A daemon still starting after a restart is waited out; any other curl
// failure is reported at once.
func TestLeaseCallWaitsForABootingDaemon(t *testing.T) {
	t.Parallel()
	code, _, err := leaseCall(context.Background(), exitOnceExecer{dir: t.TempDir(), code: curlConnectFailed}, http.MethodGet, nil)
	if err != nil || code != http.StatusOK {
		t.Fatalf("refused connection: code=%d err=%v, want the retry to answer 200", code, err)
	}
	if _, _, err := leaseCall(context.Background(), exitOnceExecer{dir: t.TempDir(), code: 6}, http.MethodGet, nil); err == nil {
		t.Fatal("a failure other than a refused connection must not be retried")
	}
}
