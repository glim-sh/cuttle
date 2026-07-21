package backend

import (
	"context"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/glim-sh/cuttle/internal/config"
)

// mockRunner records every command and answers Output via a programmable hook.
type mockRunner struct {
	mu      sync.Mutex
	calls   [][]string
	started [][]string
	respond func(name string, args []string) Result
	absent  map[string]bool
}

func (m *mockRunner) Output(_ context.Context, name string, args ...string) (Result, error) {
	m.mu.Lock()
	m.calls = append(m.calls, append([]string{name}, args...))
	m.mu.Unlock()
	if m.respond != nil {
		return m.respond(name, args), nil
	}
	return Result{}, nil
}

func (m *mockRunner) Start(_ context.Context, name string, args ...string) (Process, error) {
	m.mu.Lock()
	m.started = append(m.started, append([]string{name}, args...))
	m.mu.Unlock()
	return noopProcess{}, nil
}

func (m *mockRunner) LookPath(name string) (string, error) {
	if m.absent != nil && m.absent[name] {
		return "", errFakeMissing
	}
	return "/usr/bin/" + name, nil
}

type noopProcess struct{}

func (noopProcess) Stop() error { return nil }

var errFakeMissing = &missingError{}

type missingError struct{}

func (*missingError) Error() string { return "not found" }

// lastCall returns the most recent recorded call whose first two tokens match
// the given verb path, e.g. lastCall("docker", "run").
func (m *mockRunner) lastCall(prefix ...string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range slices.Backward(m.calls) {
		if hasPrefixTokens(v, prefix) {
			return v
		}
	}
	return nil
}

func hasPrefixTokens(call, prefix []string) bool {
	if len(call) < len(prefix) {
		return false
	}
	// prefix matches the command name plus the first meaningful verb, allowing
	// intervening global flags (e.g. kubectl --context X get ...).
	if call[0] != prefix[0] {
		return false
	}
	rest := call[1:]
	for _, want := range prefix[1:] {
		if !slices.Contains(rest, want) {
			return false
		}
	}
	return true
}

func assertArgv(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("argv mismatch:\n got: %v\nwant: %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// local
// ---------------------------------------------------------------------------

func TestLocalStartFreshRun(t *testing.T) {
	tests := []struct {
		name     string
		opts     StartOpts
		wantTail []string
	}{
		{
			name: "default keep-profile and vnc",
			opts: StartOpts{Image: "img:1"},
			wantTail: []string{
				"docker", "run", "-d", "--platform", "linux/amd64", "--init", "--name", "cuttle",
				"-p", "127.0.0.1:9222:9222", "--shm-size=2g",
				"-p", "127.0.0.1:6080:6080", "-e", "CUTTLE_VNC=1",
				"img:1", "cuttle", "serve", "--keep-profile",
			},
		},
		{
			name: "no-vnc and proxy, keep-profile off",
			opts: StartOpts{Image: "img:1", NoVNC: true, Proxy: "http://p:1", KeepProfile: new(bool)},
			wantTail: []string{
				"docker", "run", "-d", "--platform", "linux/amd64", "--init", "--name", "cuttle",
				"-p", "127.0.0.1:9222:9222", "--shm-size=2g",
				"-e", "CUTTLE_PROXY=http://p:1",
				"img:1", "cuttle", "serve",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &mockRunner{respond: dockerAbsent}
			l := &Local{runner: r, name: "cuttle", cdpPort: 9222, vncPort: 6080, image: "fallback:0"}
			if err := l.Start(context.Background(), tt.opts); err != nil {
				t.Fatalf("Start: %v", err)
			}
			assertArgv(t, r.lastCall("docker", "run"), tt.wantTail)
		})
	}
}

func TestLocalStartRestartsExited(t *testing.T) {
	r := &mockRunner{respond: func(_ string, args []string) Result {
		if slices.Contains(args, "inspect") {
			return Result{Stdout: "exited\n"}
		}
		return Result{}
	}}
	l := &Local{runner: r, name: "cuttle", cdpPort: 9222, vncPort: 6080}
	if err := l.Start(context.Background(), StartOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertArgv(t, r.lastCall("docker", "start"), []string{"docker", "start", "cuttle"})
	if r.lastCall("docker", "run") != nil {
		t.Fatal("exited container should restart, not run")
	}
}

func TestLocalStopArgv(t *testing.T) {
	tests := []struct {
		name  string
		purge bool
		want  [][]string
	}{
		{"graceful", false, [][]string{{"docker", "stop", "-t", "15", "cuttle"}}},
		{"purge", true, [][]string{{"docker", "stop", "-t", "15", "cuttle"}, {"docker", "rm", "-f", "cuttle"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &mockRunner{respond: func(_ string, args []string) Result {
				if slices.Contains(args, "inspect") {
					return Result{Stdout: "running\n"}
				}
				return Result{}
			}}
			l := &Local{runner: r, name: "cuttle"}
			if err := l.Stop(context.Background(), tt.purge); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			assertArgv(t, r.lastCall("docker", "stop"), tt.want[0])
			if tt.purge {
				assertArgv(t, r.lastCall("docker", "rm"), tt.want[1])
			}
		})
	}
}

func TestLocalStateMapping(t *testing.T) {
	tests := []struct {
		status string
		code   int
		want   State
	}{
		{"", 1, StateAbsent},
		{"running", 0, StateRunning},
		{"exited", 0, StateStopped},
		{"created", 0, StateStopped},
	}
	for _, tt := range tests {
		t.Run(string(tt.want)+"_"+tt.status, func(t *testing.T) {
			r := &mockRunner{respond: func(string, []string) Result {
				return Result{Stdout: tt.status, Code: tt.code}
			}}
			l := &Local{runner: r, name: "cuttle"}
			got, err := l.State(context.Background())
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestLocalMissingDocker(t *testing.T) {
	r := &mockRunner{absent: map[string]bool{"docker": true}}
	l := &Local{runner: r, name: "cuttle"}
	if _, err := l.State(context.Background()); err == nil || !strings.Contains(err.Error(), "docker") {
		t.Fatalf("want docker-not-found error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// k8s
// ---------------------------------------------------------------------------

func k8sContext() config.Context {
	return config.Context{
		Backend:      config.BackendK8s,
		Namespace:    "browser",
		Release:      "cuttle",
		KubeContext:  "kind",
		NodeSelector: map[string]string{"glim.sh/browser": "true"},
	}
}

func TestK8sStartArgv(t *testing.T) {
	r := &mockRunner{}
	k := newK8s(k8sContext(), r)
	if err := k.Start(context.Background(), StartOpts{Proxy: "http://u:p@proxy.example:8080"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := []string{
		"helm", "--kube-context", "kind", "--namespace", "browser",
		"upgrade", "--install", "cuttle", "ops/helm/cuttle", "--create-namespace",
		"--set", "replicaCount=1",
		"--set-string", "proxy=http://u:p@proxy.example:8080",
		"--set-string", "profileStorage=local",
		"--set-string", `nodeSelector.glim\.sh/browser=true`,
	}
	assertArgv(t, r.lastCall("helm", "upgrade"), want)
}

func TestK8sStopArgv(t *testing.T) {
	t.Run("scale down", func(t *testing.T) {
		r := &mockRunner{}
		k := newK8s(k8sContext(), r)
		if err := k.Stop(context.Background(), false); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		assertArgv(t, r.lastCall("helm", "upgrade"), []string{
			"helm", "--kube-context", "kind", "--namespace", "browser",
			"upgrade", "--install", "cuttle", "ops/helm/cuttle", "--reuse-values", "--set", "replicaCount=0",
		})
	})
	t.Run("purge uninstalls and deletes pvc", func(t *testing.T) {
		r := &mockRunner{}
		k := newK8s(k8sContext(), r)
		if err := k.Stop(context.Background(), true); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		assertArgv(t, r.lastCall("helm", "uninstall"), []string{
			"helm", "--kube-context", "kind", "--namespace", "browser", "uninstall", "cuttle",
		})
		assertArgv(t, r.lastCall("kubectl", "delete"), []string{
			"kubectl", "--context", "kind", "-n", "browser",
			"delete", "pvc", "-l", "app.kubernetes.io/instance=cuttle",
		})
	})
}

func TestK8sStatePhase(t *testing.T) {
	tests := []struct {
		phases string
		code   int
		want   State
	}{
		{"", 1, StateAbsent},
		{"Running", 0, StateRunning},
		{"Pending", 0, StateStopped},
	}
	for _, tt := range tests {
		t.Run(string(tt.want), func(t *testing.T) {
			r := &mockRunner{respond: func(string, []string) Result {
				return Result{Stdout: tt.phases, Code: tt.code}
			}}
			k := newK8s(k8sContext(), r)
			got, err := k.State(context.Background())
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
	// verify the jsonpath query argv
	r := &mockRunner{}
	k := newK8s(k8sContext(), r)
	_, _ = k.State(context.Background())
	assertArgv(t, r.lastCall("kubectl", "get"), []string{
		"kubectl", "--context", "kind", "-n", "browser",
		"get", "pod", "-l", "app.kubernetes.io/instance=cuttle", "-o", "jsonpath={.items[*].status.phase}",
	})
}

func TestK8sReachPortForward(t *testing.T) {
	r := &mockRunner{}
	k := newK8s(k8sContext(), r)
	ep, release, err := k.Reach(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Reach: %v", err)
	}
	defer release()
	if ep.CDPHost != "127.0.0.1" || ep.CDPPort == 0 || ep.VNCPort == 0 || ep.CDPPort == ep.VNCPort {
		t.Fatalf("bad endpoint: %+v", ep)
	}
	if len(r.started) != 1 {
		t.Fatalf("want one port-forward, got %d", len(r.started))
	}
	pf := r.started[0]
	if !slices.Equal(pf[:6], []string{"kubectl", "--context", "kind", "-n", "browser", "port-forward"}) {
		t.Fatalf("port-forward prefix: %v", pf)
	}
	if !slices.Contains(pf, "svc/cuttle") {
		t.Fatalf("missing svc target: %v", pf)
	}
	if !strings.HasSuffix(pf[len(pf)-2], ":9222") || !strings.HasSuffix(pf[len(pf)-1], ":6080") {
		t.Fatalf("port mappings: %v", pf[len(pf)-2:])
	}
}

func TestK8sReachPinnedPorts(t *testing.T) {
	r := &mockRunner{}
	k := newK8s(k8sContext(), r)
	ep, release, err := k.Reach(context.Background(), 9333, 6081)
	if err != nil {
		t.Fatalf("Reach: %v", err)
	}
	defer release()
	if ep.CDPPort != 9333 || ep.VNCPort != 6081 {
		t.Fatalf("pinned ports not honored: %+v", ep)
	}
	pf := r.started[0]
	if pf[len(pf)-2] != "9333:9222" || pf[len(pf)-1] != "6081:6080" {
		t.Fatalf("port-forward should use pinned local ports: %v", pf[len(pf)-2:])
	}
}

// ---------------------------------------------------------------------------
// ssh
// ---------------------------------------------------------------------------

func sshBackend(r Runner) *SSH {
	return &SSH{runner: r, host: "user@box.example", name: "cuttle", cdpPort: 9222, vncPort: 6080, image: "img:1"}
}

func TestSSHStateArgv(t *testing.T) {
	r := &mockRunner{respond: func(string, []string) Result { return Result{Stdout: "running"} }}
	s := sshBackend(r)
	got, err := s.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != StateRunning {
		t.Fatalf("state: %q", got)
	}
	call := r.lastCall("ssh")
	cp := s.controlPath()
	want := []string{
		"ssh", "-o", "ControlMaster=auto", "-o", "ControlPath=" + cp, "user@box.example",
		"docker", "inspect", "-f", "{{.State.Status}}", "cuttle",
	}
	assertArgv(t, call, want)
}

func TestSSHStartArgv(t *testing.T) {
	r := &mockRunner{}
	s := sshBackend(r)
	if err := s.Start(context.Background(), StartOpts{NoVNC: true, KeepProfile: new(bool)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cp := s.controlPath()
	want := []string{
		"ssh", "-o", "ControlMaster=auto", "-o", "ControlPath=" + cp, "user@box.example",
		"docker", "run", "-d", "--platform", "linux/amd64", "--init", "--name", "cuttle",
		"-p", "127.0.0.1:9222:9222", "--shm-size=2g",
		"img:1", "cuttle", "serve",
	}
	assertArgv(t, r.lastCall("ssh"), want)
}

// sshInspect makes the mock answer `docker inspect` over ssh with the given raw
// status; every other ssh command succeeds with empty output.
func sshInspect(status string) func(string, []string) Result {
	return func(_ string, args []string) Result {
		if slices.Contains(args, "inspect") {
			if status == "" {
				return Result{Code: 1}
			}
			return Result{Stdout: status}
		}
		return Result{}
	}
}

func TestSSHStartRunningNoOp(t *testing.T) {
	r := &mockRunner{respond: sshInspect("running")}
	if err := sshBackend(r).Start(context.Background(), StartOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if r.lastCall("ssh", "run") != nil || r.lastCall("ssh", "start") != nil {
		t.Fatal("running container should be a no-op, no run/start issued")
	}
}

func TestSSHStartRestartsExited(t *testing.T) {
	r := &mockRunner{respond: sshInspect("exited")}
	if err := sshBackend(r).Start(context.Background(), StartOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	start := r.lastCall("ssh", "start")
	if start == nil || !slices.Equal(start[len(start)-3:], []string{"docker", "start", "cuttle"}) {
		t.Fatalf("exited container should restart over ssh, got %v", start)
	}
	if r.lastCall("ssh", "run") != nil {
		t.Fatal("exited container should restart, not run")
	}
}

func TestSSHStartZombieRemovesAndRuns(t *testing.T) {
	r := &mockRunner{respond: sshInspect("created")}
	if err := sshBackend(r).Start(context.Background(), StartOpts{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rm := r.lastCall("ssh", "rm")
	if rm == nil || !slices.Equal(rm[len(rm)-3:], []string{"rm", "-f", "cuttle"}) {
		t.Fatalf("zombie should be removed, got %v", rm)
	}
	if r.lastCall("ssh", "run") == nil {
		t.Fatal("zombie should be re-run after removal")
	}
}

func TestSSHStartRecreateRemovesAndRuns(t *testing.T) {
	r := &mockRunner{respond: sshInspect("running")}
	if err := sshBackend(r).Start(context.Background(), StartOpts{Recreate: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if r.lastCall("ssh", "rm") == nil {
		t.Fatal("--recreate should remove the running container")
	}
	if r.lastCall("ssh", "run") == nil {
		t.Fatal("--recreate should start a fresh container")
	}
}

func TestSSHStartPortConflictHint(t *testing.T) {
	r := &mockRunner{respond: func(_ string, args []string) Result {
		if slices.Contains(args, "inspect") {
			return Result{Code: 1}
		}
		if slices.Contains(args, dockerRunSub) {
			return Result{Code: 1, Stderr: "Bind for 0.0.0.0:9222 failed: port is already allocated"}
		}
		return Result{}
	}}
	err := sshBackend(r).Start(context.Background(), StartOpts{})
	if err == nil {
		t.Fatal("expected a port-conflict error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "remote host port") || !strings.Contains(msg, "docker ps") {
		t.Fatalf("expected honest remote-host remedy, got %q", msg)
	}
	if strings.Contains(msg, "--cdp-port") {
		t.Fatalf("remote hint should not recommend --cdp-port, got %q", msg)
	}
	// The failed run must be cleaned up so the next `up` does not see a zombie.
	if r.lastCall("ssh", "rm") == nil {
		t.Fatal("a failed run should be removed")
	}
}

func TestSSHReachTunnelArgv(t *testing.T) {
	r := &mockRunner{}
	s := sshBackend(r)
	ep, release, err := s.Reach(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Reach: %v", err)
	}
	defer release()
	if ep.CDPPort == 0 || ep.VNCPort == 0 || ep.CDPPort == ep.VNCPort {
		t.Fatalf("bad endpoint: %+v", ep)
	}
	tun := r.started[0]
	if tun[0] != "ssh" || tun[len(tun)-1] != "user@box.example" {
		t.Fatalf("tunnel argv: %v", tun)
	}
	if !slices.Contains(tun, "-N") {
		t.Fatalf("tunnel missing -N: %v", tun)
	}
	// two -L forwards ending in the remote container ports
	var forwards []string
	for i, a := range tun {
		if a == "-L" && i+1 < len(tun) {
			forwards = append(forwards, tun[i+1])
		}
	}
	if len(forwards) != 2 || !strings.HasSuffix(forwards[0], ":127.0.0.1:9222") || !strings.HasSuffix(forwards[1], ":127.0.0.1:6080") {
		t.Fatalf("forwards: %v", forwards)
	}
}

// ---------------------------------------------------------------------------
// direct
// ---------------------------------------------------------------------------

func TestDirectStartStopError(t *testing.T) {
	d, err := newDirect(config.Context{Backend: config.BackendDirect, CDPURL: "http://cuttle.example:9222"})
	if err != nil {
		t.Fatalf("newDirect: %v", err)
	}
	if err := d.Start(context.Background(), StartOpts{}); err == nil {
		t.Fatal("Start should error for direct")
	}
	if err := d.Stop(context.Background(), false); err == nil {
		t.Fatal("Stop should error for direct")
	}
}

func TestDirectReachUsesConfigURLs(t *testing.T) {
	d, err := newDirect(config.Context{
		Backend: config.BackendDirect,
		CDPURL:  "http://cuttle.example:9222",
		VNCURL:  "https://cuttle.example:6080",
	})
	if err != nil {
		t.Fatalf("newDirect: %v", err)
	}
	ep, release, err := d.Reach(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Reach: %v", err)
	}
	defer release()
	if ep.CDPHost != "cuttle.example" || ep.CDPPort != 9222 || ep.VNCPort != 6080 {
		t.Fatalf("endpoint: %+v", ep)
	}
}

func TestDirectRequiresCDPURL(t *testing.T) {
	if _, err := newDirect(config.Context{Backend: config.BackendDirect}); err == nil {
		t.Fatal("expected error when cdp_url missing")
	}
}

func TestDirectStateProbe(t *testing.T) {
	d, err := newDirect(config.Context{Backend: config.BackendDirect, CDPURL: "http://cuttle.example:9222"})
	if err != nil {
		t.Fatalf("newDirect: %v", err)
	}
	d.probe = func(context.Context, string) bool { return true }
	if s, _ := d.State(context.Background()); s != StateRunning {
		t.Fatalf("want running, got %q", s)
	}
	d.probe = func(context.Context, string) bool { return false }
	if s, _ := d.State(context.Background()); s != StateAbsent {
		t.Fatalf("want absent, got %q", s)
	}
}

// ---------------------------------------------------------------------------
// shared
// ---------------------------------------------------------------------------

func TestFreePortDistinctAndUsable(t *testing.T) {
	seen := map[int]bool{}
	for range 5 {
		p, err := freePort()
		if err != nil {
			t.Fatalf("freePort: %v", err)
		}
		if p <= 0 || p > 65535 {
			t.Fatalf("bad port %d", p)
		}
		seen[p] = true
	}
	if len(seen) < 2 {
		t.Fatalf("expected varied ports, got %v", seen)
	}
}

func TestEscapeHelm(t *testing.T) {
	segTests := []struct{ in, want string }{
		{"replicaCount", "replicaCount"},
		{"glim.sh/browser", `glim\.sh/browser`},
		{"a,b", `a\,b`},
	}
	for _, tt := range segTests {
		if got := escapeHelmSegment(tt.in); got != tt.want {
			t.Fatalf("escapeHelmSegment(%q)=%q want %q", tt.in, got, tt.want)
		}
	}
	// Values keep dots/slashes/colons; only commas are structural.
	if got := escapeHelmValue("http://u:p@proxy.example:8080"); got != "http://u:p@proxy.example:8080" {
		t.Fatalf("escapeHelmValue dropped/added escaping: %q", got)
	}
	if got := escapeHelmValue("a,b"); got != `a\,b` {
		t.Fatalf("escapeHelmValue(comma)=%q", got)
	}
}

func TestNewDispatch(t *testing.T) {
	r := &mockRunner{}
	tests := []struct {
		backend string
		ctx     config.Context
		want    string
	}{
		{"local", config.Context{Backend: config.BackendLocal}, "*backend.Local"},
		{"k8s", config.Context{Backend: config.BackendK8s}, "*backend.K8s"},
		{"ssh", config.Context{Backend: config.BackendSSH, Host: "h"}, "*backend.SSH"},
		{"direct", config.Context{Backend: config.BackendDirect, CDPURL: "http://x:9222"}, "*backend.Direct"},
	}
	for _, tt := range tests {
		t.Run(tt.backend, func(t *testing.T) {
			b, err := New(tt.backend, tt.backend, tt.ctx, r, 9222, 6080, "img")
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got := typeName(b); got != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// tunnel (persistent standing forward)
// ---------------------------------------------------------------------------

// deadPid is a pid no live process owns, so processAlive reports false and
// stopTunnel never signals a real process (least of all this test runner).
const deadPid = 0x7FFFFFFF

func TestTunnelPidfilePath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	got, err := tunnelPidfile("my box")
	if err != nil {
		t.Fatalf("tunnelPidfile: %v", err)
	}
	if !strings.HasSuffix(got, "/cuttle/tunnel-my_box.pid") {
		t.Fatalf("unexpected pidfile path: %s", got)
	}
}

func TestTunnelHealthy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	port := l.Addr().(*net.TCPAddr).Port

	if err := writePidfile("ctx", os.Getpid()); err != nil {
		t.Fatalf("writePidfile: %v", err)
	}
	if !tunnelHealthy(context.Background(), "ctx", port) {
		t.Fatal("expected healthy: own pid alive and port listening")
	}
	_ = l.Close()
	if tunnelHealthy(context.Background(), "ctx", port) {
		t.Fatal("expected unhealthy once the port stops listening")
	}
	if err := writePidfile("ctx", deadPid); err != nil {
		t.Fatalf("writePidfile: %v", err)
	}
	if tunnelHealthy(context.Background(), "ctx", port) {
		t.Fatal("expected unhealthy for a dead pid")
	}
}

func TestTunnelStopCleansStalePidfile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := writePidfile("ctx", deadPid); err != nil {
		t.Fatalf("writePidfile: %v", err)
	}
	if _, ok := readPidfile("ctx"); !ok {
		t.Fatal("pidfile should exist before stop")
	}
	if err := stopTunnel("ctx"); err != nil {
		t.Fatalf("stopTunnel: %v", err)
	}
	if _, ok := readPidfile("ctx"); ok {
		t.Fatal("stopTunnel should remove the pidfile")
	}
}

func dockerAbsent(_ string, args []string) Result {
	if slices.Contains(args, "inspect") {
		return Result{Code: 1}
	}
	return Result{}
}

func typeName(v any) string {
	switch v.(type) {
	case *Local:
		return "*backend.Local"
	case *K8s:
		return "*backend.K8s"
	case *SSH:
		return "*backend.SSH"
	case *Direct:
		return "*backend.Direct"
	default:
		return "unknown"
	}
}
