package backend

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/glim-sh/cuttle/internal/config"
)

func keepProfileOn() *bool { on := true; return &on }

// persistServeTail is the container command dockerRunArgs emits for a persistent
// profile: a plain `cuttle serve` (the daemon clears any stale SingletonLock and
// the argv carries no shell metacharacters, so the ssh backend forwards it intact).
var persistServeTail = []string{"cuttle", "serve"}

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
	m.calls = append(m.calls, withoutRunLabel(append([]string{name}, args...)))
	m.mu.Unlock()
	if m.respond != nil {
		return m.respond(name, args), nil
	}
	return Result{}, nil
}

// withoutRunLabel drops the per-run label from a recorded `docker run`: it is
// random, and the argv assertions are about everything else.
func withoutRunLabel(argv []string) []string {
	if i := slices.Index(argv, "--label"); i >= 0 && i+1 < len(argv) && strings.HasPrefix(argv[i+1], runLabel+"=") {
		return slices.Delete(argv, i, i+2)
	}
	return argv
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

// hasCall reports whether any recorded call exactly equals the given argv.
func (m *mockRunner) hasCall(argv ...string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.calls {
		if slices.Equal(v, argv) {
			return true
		}
	}
	return false
}

// hasCallSuffix reports whether any recorded call ends with the given tokens
// (used for ssh-wrapped docker argv, where global ssh flags precede the docker
// subcommand).
func (m *mockRunner) hasCallSuffix(suffix ...string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.calls {
		if len(v) >= len(suffix) && slices.Equal(v[len(v)-len(suffix):], suffix) {
			return true
		}
	}
	return false
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
			name: "default persists (named volume + keep-profile env + singleton cleanup)",
			opts: StartOpts{Image: "img:1"},
			wantTail: append([]string{
				"docker", "run", "-d", "--init", "--name", "cuttle",
				"-p", "127.0.0.1:9222:9222", "--shm-size=2g",
				"-p", "127.0.0.1:6080:6080", "-e", "CUTTLE_VNC=1",
				"-v", "cuttle-cuttle-profile:/data", "-e", "CUTTLE_KEEP_PROFILE=1", "img:1",
			}, persistServeTail...),
		},
		{
			name: "explicit --keep-profile persists (same as default)",
			opts: StartOpts{Image: "img:1", KeepProfile: keepProfileOn()},
			wantTail: append([]string{
				"docker", "run", "-d", "--init", "--name", "cuttle",
				"-p", "127.0.0.1:9222:9222", "--shm-size=2g",
				"-p", "127.0.0.1:6080:6080", "-e", "CUTTLE_VNC=1",
				"-v", "cuttle-cuttle-profile:/data", "-e", "CUTTLE_KEEP_PROFILE=1", "img:1",
			}, persistServeTail...),
		},
		{
			name: "--ephemeral opts out (no volume, no keep-profile)",
			opts: StartOpts{Image: "img:1", Ephemeral: true},
			wantTail: []string{
				"docker", "run", "-d", "--init", "--name", "cuttle",
				"-p", "127.0.0.1:9222:9222", "--shm-size=2g",
				"-p", "127.0.0.1:6080:6080", "-e", "CUTTLE_VNC=1",
				"img:1", "cuttle", "serve",
			},
		},
		{
			name: "legacy --keep-profile=false opts out",
			opts: StartOpts{Image: "img:1", KeepProfile: new(bool)},
			wantTail: []string{
				"docker", "run", "-d", "--init", "--name", "cuttle",
				"-p", "127.0.0.1:9222:9222", "--shm-size=2g",
				"-p", "127.0.0.1:6080:6080", "-e", "CUTTLE_VNC=1",
				"img:1", "cuttle", "serve",
			},
		},
		{
			name: "proxy, persistent",
			opts: StartOpts{Image: "img:1", Proxy: "http://p:1"},
			wantTail: append([]string{
				"docker", "run", "-d", "--init", "--name", "cuttle",
				"-p", "127.0.0.1:9222:9222", "--shm-size=2g",
				"-p", "127.0.0.1:6080:6080", "-e", "CUTTLE_VNC=1",
				"-e", "CUTTLE_PROXY=http://p:1",
				"-v", "cuttle-cuttle-profile:/data", "-e", "CUTTLE_KEEP_PROFILE=1", "img:1",
			}, persistServeTail...),
		},
		{
			name: "idle-timeout, ephemeral",
			opts: StartOpts{Image: "img:1", IdleTimeout: "30", Ephemeral: true},
			wantTail: []string{
				"docker", "run", "-d", "--init", "--name", "cuttle",
				"-p", "127.0.0.1:9222:9222", "--shm-size=2g",
				"-p", "127.0.0.1:6080:6080", "-e", "CUTTLE_VNC=1",
				"-e", "CUTTLE_IDLE_TIMEOUT=30",
				"img:1", "cuttle", "serve",
			},
		},
		{
			// Humanize is on by the daemon default, so the enabled/nil case adds no
			// env; only --humanize=false emits CUTTLE_HUMANIZE=0.
			name: "humanize disabled, ephemeral",
			opts: StartOpts{Image: "img:1", Humanize: new(bool), Ephemeral: true},
			wantTail: []string{
				"docker", "run", "-d", "--init", "--name", "cuttle",
				"-p", "127.0.0.1:9222:9222", "--shm-size=2g",
				"-p", "127.0.0.1:6080:6080", "-e", "CUTTLE_VNC=1",
				"-e", "CUTTLE_HUMANIZE=0",
				"img:1", "cuttle", "serve",
			},
		},
		{
			// Third-party cookies are allowed by the daemon default (stock Chrome
			// parity), so only the opt-in emits an env.
			name: "third-party cookies blocked, ephemeral",
			opts: StartOpts{Image: "img:1", BlockThirdPartyCookies: true, Ephemeral: true},
			wantTail: []string{
				"docker", "run", "-d", "--init", "--name", "cuttle",
				"-p", "127.0.0.1:9222:9222", "--shm-size=2g",
				"-p", "127.0.0.1:6080:6080", "-e", "CUTTLE_VNC=1",
				"-e", "CUTTLE_BLOCK_THIRD_PARTY_COOKIES=1",
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

// ephemeralPort returns a currently-free loopback TCP port.
func ephemeralPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}

// A fresh start whose host CDP/VNC port is already held by a foreign process
// (another context's ssh tunnel, a stale container) must fail with a conflict -
// OrbStack does not error on the colliding publish, so cuttle detects it itself.
func TestLocalStartDetectsHostPortCollision(t *testing.T) {
	t.Parallel()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	busyPort := busy.Addr().(*net.TCPAddr).Port

	l := &Local{runner: &mockRunner{respond: dockerAbsent}, name: "cuttle", cdpPort: busyPort, vncPort: ephemeralPort(t), image: "img:1", portInUse: hostPortInUse}
	if err := l.ensureHostPortsFree(context.Background(), false); err == nil {
		t.Fatal("expected a conflict when the CDP host port is already bound")
	}

	// Both ports free: the check passes.
	l.cdpPort = ephemeralPort(t)
	if err := l.ensureHostPortsFree(context.Background(), false); err != nil {
		t.Fatalf("free ports should pass: %v", err)
	}
}

var errPortBound = errors.New("bound")

// A rebuild tears the running container down only once the new run can go
// ahead: an image that cannot be pulled, or a new host port another process
// holds, fails it with the old container and its profile untouched. The ports
// the container holds itself are not a clash - its teardown frees them.
func TestLocalRebuildChecksBeforeTearingDown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		opts     StartOpts
		cdpPort  int
		imageOK  bool
		wantErr  bool
		wantPull bool
	}{
		{name: "image cannot be pulled", opts: StartOpts{Recreate: true}, cdpPort: 9222, wantErr: true, wantPull: true},
		{name: "purge-profile, image cannot be pulled", opts: StartOpts{PurgeProfile: true}, cdpPort: 9222, wantErr: true, wantPull: true},
		{name: "a new port another process holds", opts: StartOpts{Recreate: true}, cdpPort: 9555, imageOK: true, wantErr: true},
		{name: "its own ports", opts: StartOpts{Recreate: true}, cdpPort: 9222, imageOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &mockRunner{respond: func(_ string, args []string) Result {
				switch {
				case slices.Contains(args, "image"):
					if tc.imageOK {
						return Result{Stdout: "sha256:1\n"}
					}
					return Result{Code: 1, Stderr: "No such image"}
				case slices.Contains(args, "pull"):
					return Result{Code: 1, Stderr: "pull access denied"}
				case slices.Contains(args, "inspect"):
					return Result{Stdout: "running\n"}
				case slices.Contains(args, "port"):
					return Result{Stdout: "9222/tcp -> 127.0.0.1:9222\n6080/tcp -> 127.0.0.1:6080\n"}
				}
				return Result{}
			}}
			held := func(context.Context, int) error { return errPortBound } // every port is bound by someone
			l := &Local{runner: r, name: "cuttle", cdpPort: tc.cdpPort, vncPort: 6080, image: "img:1", portInUse: held}
			err := l.Start(context.Background(), tc.opts)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Start: err = %v, want error %v", err, tc.wantErr)
			}
			if got := r.lastCall("docker", "pull") != nil; got != tc.wantPull {
				t.Errorf("pulled = %v, want %v", got, tc.wantPull)
			}
			tornDown := r.lastCall("docker", "stop") != nil || r.lastCall("docker", "rm") != nil || r.lastCall("docker", "volume") != nil
			if tornDown == tc.wantErr {
				t.Errorf("torn down = %v on a rebuild that %s", tornDown, map[bool]string{true: "cannot proceed", false: "can proceed"}[tc.wantErr])
			}
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
	t.Run("graceful keeps the volume", func(t *testing.T) {
		r := runningDocker()
		l := &Local{runner: r, name: "cuttle"}
		if err := l.Stop(context.Background(), false); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		assertArgv(t, r.lastCall("docker", "stop"), []string{"docker", "stop", "-t", "15", "cuttle"})
		if r.hasCall("docker", "rm", "-f", "cuttle") {
			t.Fatal("graceful stop must not remove the container")
		}
		if r.hasCall("docker", "volume", "rm", "-f", "cuttle-cuttle-profile") {
			t.Fatal("graceful stop must not remove the profile volume")
		}
	})
	t.Run("purge removes container and volume", func(t *testing.T) {
		r := runningDocker()
		l := &Local{runner: r, name: "cuttle"}
		if err := l.Stop(context.Background(), true); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		assertArgv(t, r.lastCall("docker", "stop"), []string{"docker", "stop", "-t", "15", "cuttle"})
		if !r.hasCall("docker", "rm", "-f", "cuttle") {
			t.Fatal("purge must remove the container")
		}
		if !r.hasCall("docker", "volume", "rm", "-f", "cuttle-cuttle-profile") {
			t.Fatal("purge must remove the profile volume")
		}
	})
}

// TestLocalStopPurgeAbsentRemovesVolume covers `down --purge` after the container
// was already removed: the lingering profile volume must still be dropped.
func TestLocalStopPurgeAbsentRemovesVolume(t *testing.T) {
	r := &mockRunner{respond: dockerAbsent}
	l := &Local{runner: r, name: "cuttle"}
	if err := l.Stop(context.Background(), true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if r.hasCall("docker", "rm", "-f", "cuttle") {
		t.Fatal("absent container needs no docker rm")
	}
	if !r.hasCall("docker", "volume", "rm", "-f", "cuttle-cuttle-profile") {
		t.Fatal("purge must remove a lingering volume even when the container is absent")
	}
}

// runningDocker answers `docker inspect` as a running container; every other call
// succeeds with empty output.
func runningDocker() *mockRunner {
	return &mockRunner{respond: func(_ string, args []string) Result {
		if slices.Contains(args, "inspect") {
			return Result{Stdout: "running\n"}
		}
		return Result{}
	}}
}

func TestLocalPurgeProfilePurgesVolumeBeforeRun(t *testing.T) {
	r := &mockRunner{respond: func(_ string, args []string) Result {
		if slices.Contains(args, "inspect") {
			return Result{Stdout: "running\n"}
		}
		return Result{}
	}}
	l := &Local{runner: r, name: "cuttle", cdpPort: 9222, vncPort: 6080, image: "img:1"}
	if err := l.Start(context.Background(), StartOpts{PurgeProfile: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !r.hasCall("docker", "rm", "-f", "cuttle") {
		t.Fatal("purge-profile must remove the container so the volume detaches")
	}
	if !r.hasCall("docker", "volume", "rm", "-f", "cuttle-cuttle-profile") {
		t.Fatal("purge-profile must remove the volume")
	}
	if r.lastCall("docker", "run") == nil {
		t.Fatal("purge-profile must start a fresh container")
	}
}

func TestLocalPurgeProfileVolume(t *testing.T) {
	r := &mockRunner{}
	l := &Local{runner: r, name: "cuttle"}
	if err := l.PurgeProfileVolume(context.Background()); err != nil {
		t.Fatalf("PurgeProfileVolume: %v", err)
	}
	if !r.hasCall("docker", "volume", "rm", "-f", "cuttle-cuttle-profile") {
		t.Fatal("PurgeProfileVolume must remove the named volume")
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
		"--set-string", "profileStorage=remote",
		"--set-string", `nodeSelector.glim\.sh/browser=true`,
	}
	assertArgv(t, r.lastCall("helm", "upgrade"), want)
}

// TestK8sImagePin checks that the k8s backend pins the deployed image tag to the
// CLI-resolved default image (parity with docker/ssh), while leaving a dev build's
// local-only ref to the chart default the cluster can actually pull.
func TestK8sImagePin(t *testing.T) {
	t.Run("release default pins the tag", func(t *testing.T) {
		r := &mockRunner{}
		k := newK8s(k8sContext(), r)
		k.image = "ghcr.io/glim-sh/cuttle:0.9.0"
		if err := k.Start(context.Background(), StartOpts{}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !slices.Contains(r.lastCall("helm", "upgrade"), "image.tag=0.9.0") {
			t.Fatalf("expected image.tag=0.9.0 in %v", r.lastCall("helm", "upgrade"))
		}
	})
	t.Run("explicit --image override wins", func(t *testing.T) {
		r := &mockRunner{}
		k := newK8s(k8sContext(), r)
		k.image = "ghcr.io/glim-sh/cuttle:0.9.0"
		if err := k.Start(context.Background(), StartOpts{Image: "ghcr.io/glim-sh/cuttle:0.8.3"}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !slices.Contains(r.lastCall("helm", "upgrade"), "image.tag=0.8.3") {
			t.Fatalf("expected image.tag=0.8.3 in %v", r.lastCall("helm", "upgrade"))
		}
	})
	t.Run("dev local ref falls back to chart default", func(t *testing.T) {
		r := &mockRunner{}
		k := newK8s(k8sContext(), r)
		k.image = "cuttle:local"
		if err := k.Start(context.Background(), StartOpts{}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		for _, a := range r.lastCall("helm", "upgrade") {
			if strings.HasPrefix(a, "image.tag=") {
				t.Fatalf("dev build must not pin image.tag, got %q", a)
			}
		}
	})
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
			"helm", "--kube-context", "kind", "--namespace", "browser", "uninstall", "cuttle", "--ignore-not-found",
		})
		assertArgv(t, r.lastCall("kubectl", "delete"), []string{
			"kubectl", "--context", "kind", "-n", "browser",
			"delete", "pvc", "-l", "app.kubernetes.io/instance=cuttle", "--ignore-not-found",
		})
	})
}

func TestK8sPurgeProfileDeletesPVCBeforeInstall(t *testing.T) {
	r := &mockRunner{}
	k := newK8s(k8sContext(), r)
	if err := k.Start(context.Background(), StartOpts{PurgeProfile: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The reset path uninstalls the release and deletes the PVC, then reinstalls.
	if r.lastCall("helm", "uninstall") == nil {
		t.Fatal("purge-profile must uninstall to release the RWO PVC")
	}
	if r.lastCall("kubectl", "delete") == nil {
		t.Fatal("purge-profile must delete the PVC")
	}
	if r.lastCall("helm", "upgrade") == nil {
		t.Fatal("purge-profile must reinstall a fresh release")
	}
}

func TestK8sPurgeProfileVolume(t *testing.T) {
	r := &mockRunner{}
	k := newK8s(k8sContext(), r)
	if err := k.PurgeProfileVolume(context.Background()); err != nil {
		t.Fatalf("PurgeProfileVolume: %v", err)
	}
	assertArgv(t, r.lastCall("kubectl", "delete"), []string{
		"kubectl", "--context", "kind", "-n", "browser",
		"delete", "pvc", "-l", "app.kubernetes.io/instance=cuttle", "--ignore-not-found",
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
	if err := s.Start(context.Background(), StartOpts{KeepProfile: new(bool)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cp := s.controlPath()
	want := []string{
		"ssh", "-o", "ControlMaster=auto", "-o", "ControlPath=" + cp, "user@box.example",
		"docker", "run", "-d", "--init", "--name", "cuttle",
		"-p", "127.0.0.1:9222:9222", "--shm-size=2g",
		"-p", "127.0.0.1:6080:6080", "-e", "CUTTLE_VNC=1",
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
	r := &mockRunner{respond: failedRun("Bind for 0.0.0.0:9222 failed: port is already allocated", true, Result{})}
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
	if !r.hasCallSuffix("docker", "rm", "-f", "c0ffee") {
		t.Fatal("a failed run should be removed")
	}
}

// sshVolumeRmSeen reports whether any recorded call ends with the docker volume
// rm sub-argv that the ssh backend wraps.
func sshVolumeRmSeen(r *mockRunner) bool {
	return r.hasCallSuffix("docker", "volume", "rm", "-f", "cuttle-cuttle-profile")
}

func TestSSHStopPurgeRemovesVolume(t *testing.T) {
	r := &mockRunner{respond: sshInspect("running")}
	s := sshBackend(r)
	if err := s.Stop(context.Background(), true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if r.hasCallSuffix("docker", "stop", "-t", stopGrace, "cuttle") == false {
		t.Fatal("ssh purge must gracefully stop a running container")
	}
	if r.hasCallSuffix("docker", "rm", "-f", "cuttle") == false {
		t.Fatal("ssh purge must remove the container")
	}
	if !sshVolumeRmSeen(r) {
		t.Fatal("ssh purge must remove the profile volume over ssh")
	}
}

// TestSSHStopPurgeAbsentRemovesVolume covers the `down --purge` after a plain
// `down` case: the container is already gone, but a lingering profile volume must
// still be removed.
func TestSSHStopPurgeAbsentRemovesVolume(t *testing.T) {
	r := &mockRunner{respond: sshInspect("")} // absent container
	s := sshBackend(r)
	if err := s.Stop(context.Background(), true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if r.hasCallSuffix("docker", "stop", "-t", stopGrace, "cuttle") {
		t.Fatal("absent container needs no docker stop")
	}
	if !sshVolumeRmSeen(r) {
		t.Fatal("purge must remove a lingering volume even when the container is absent")
	}
}

func TestSSHPurgeProfileVolume(t *testing.T) {
	r := &mockRunner{}
	s := sshBackend(r)
	if err := s.PurgeProfileVolume(context.Background()); err != nil {
		t.Fatalf("PurgeProfileVolume: %v", err)
	}
	if !sshVolumeRmSeen(r) {
		t.Fatal("PurgeProfileVolume must remove the volume over ssh")
	}
}

func TestSSHStopGracefulKeepsVolume(t *testing.T) {
	r := &mockRunner{respond: sshInspect("running")}
	s := sshBackend(r)
	if err := s.Stop(context.Background(), false); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if r.hasCallSuffix("docker", "stop", "-t", stopGrace, "cuttle") == false {
		t.Fatal("graceful ssh stop must stop the running container")
	}
	if sshVolumeRmSeen(r) {
		t.Fatal("graceful ssh stop must not remove the volume")
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
	if !slices.Contains(tun, "ServerAliveInterval=15") {
		t.Fatalf("tunnel missing keepalive (ServerAliveInterval): %v", tun)
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

// The standing tunnel runs under the self-re-exec supervisor, which must own the
// ssh -N process outright. ControlPath=none gives it a dedicated, un-multiplexed
// connection so no persistent master - an ambient ~/.ssh/config ControlPersist or
// cuttle's own docker-control master - can daemonize the forward to PPID 1 and
// orphan it beyond a process-group kill. It must therefore NOT join the shared
// ControlMaster.
func TestSSHStandingTunnelArgs(t *testing.T) {
	s := sshBackend(&mockRunner{})
	args := s.standingTunnelArgs(9222, 6080)
	if !slices.Contains(args, "ControlPath=none") {
		t.Fatalf("standing tunnel missing ControlPath=none (forward would attach to a shared master and orphan): %v", args)
	}
	if slices.Contains(args, "ControlMaster=auto") {
		t.Fatalf("standing tunnel must not share a master (ControlMaster=auto present): %v", args)
	}
	if !slices.Contains(args, "ServerAliveInterval=15") {
		t.Fatalf("standing tunnel missing keepalive: %v", args)
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
	// An unsafe rune ('my box') gets a hash suffix so it cannot collide with a
	// distinct 'my_box' context.
	if !strings.Contains(got, "/cuttle/tunnel-my_box-") || !strings.HasSuffix(got, ".pid") {
		t.Fatalf("unexpected pidfile path: %s", got)
	}
	safe, err := tunnelPidfile("my_box")
	if err != nil {
		t.Fatalf("tunnelPidfile: %v", err)
	}
	if got == safe {
		t.Fatalf("'my box' and 'my_box' must not collide: %s == %s", got, safe)
	}
	if !strings.HasSuffix(safe, "/cuttle/tunnel-my_box.pid") {
		t.Fatalf("already-safe name should pass through unchanged: %s", safe)
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

// TestShellQuoteRoundTrip proves a token carrying shell metacharacters survives
// ssh's remote re-parse: quoting it and running the result through `sh -c` must
// reproduce the original token exactly, while safe tokens pass through unquoted
// (so the readable docker argv the other ssh tests assert is preserved).
func TestShellQuoteRoundTrip(t *testing.T) {
	safe := []string{"docker", "run", "cuttle-cuttle-profile:/data", "{{.State.Status}}", "127.0.0.1:9222:9222", "img:1"}
	for _, tok := range safe {
		if got := shellQuote(tok); got != tok {
			t.Errorf("shellQuote(%q) = %q, want unchanged", tok, got)
		}
	}
	unsafe := []string{
		`find /data -name 'Singleton*' -delete 2>/dev/null || true; exec cuttle serve "$@"`,
		"a b c", "$HOME", "x;y", "a|b", "we'ird", "",
	}
	for _, tok := range unsafe {
		quoted := shellQuote(tok)
		// Emulate ssh: the remote login shell runs `sh -c <joined>` where the
		// quoted token is one word. Echo it back verbatim to confirm it arrives
		// as a single, unmangled argument.
		out, err := exec.Command("sh", "-c", "printf %s "+quoted).Output()
		if err != nil {
			t.Fatalf("sh -c on %q: %v", quoted, err)
		}
		if string(out) != tok {
			t.Errorf("round-trip of %q via %q = %q", tok, quoted, out)
		}
	}
}

// TestLocalRecreateEphemeralRemovesVolume covers `up --recreate --ephemeral`: the
// old persistent volume must be dropped so it does not linger unreferenced, while
// a plain --recreate (still persistent) keeps it (asserted elsewhere).
func TestLocalRecreateEphemeralRemovesVolume(t *testing.T) {
	r := runningDocker()
	l := &Local{runner: r, name: "cuttle", cdpPort: 9222, vncPort: 6080, image: "img:1"}
	if err := l.Start(context.Background(), StartOpts{Recreate: true, Ephemeral: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !r.hasCall("docker", "volume", "rm", "-f", "cuttle-cuttle-profile") {
		t.Fatal("--recreate --ephemeral must remove the orphaned persistent volume")
	}
}

// TestLocalRecreatePersistentKeepsVolume is the counterpart: a plain --recreate
// stays persistent and must NOT remove the volume (the profile re-attaches).
func TestLocalRecreatePersistentKeepsVolume(t *testing.T) {
	r := runningDocker()
	l := &Local{runner: r, name: "cuttle", cdpPort: 9222, vncPort: 6080, image: "img:1"}
	if err := l.Start(context.Background(), StartOpts{Recreate: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if r.hasCall("docker", "volume", "rm", "-f", "cuttle-cuttle-profile") {
		t.Fatal("a plain --recreate must keep the persistent volume")
	}
}

// TestLocalNameOverride proves --name (via the container name) keys both the
// container and its profile volume, so distinct instances do not collide.
func TestLocalNameOverride(t *testing.T) {
	r := &mockRunner{respond: dockerAbsent}
	l := &Local{runner: r, name: "persist-test", cdpPort: 9340, vncPort: 6100, image: "img:1"}
	if err := l.Start(context.Background(), StartOpts{Image: "img:1"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	run := r.lastCall("docker", "run")
	if !slices.Contains(run, "persist-test") || !slices.Contains(run, "cuttle-persist-test-profile:/data") {
		t.Fatalf("run argv %v must name the container and its per-name volume", run)
	}
}

// TestK8sDefaultStorageClassDetection covers the persistent-install preflight
// against real kubectl annotation output. Current kubectl renders the annotations
// map as JSON (`"is-default-class":"true"`); an earlier bug matched only the older
// Go-map form, so a cluster that HAS a default class was misread as having none and
// the install was wrongly blocked. Both render formats must be understood.
func TestK8sDefaultStorageClassDetection(t *testing.T) {
	// A cluster WITH a default class: the install must proceed.
	proceed := []string{
		`{"storageclass.kubernetes.io/is-default-class":"false"} {"storageclass.kubernetes.io/is-default-class":"true"}`, // kubectl JSON
		"map[storageclass.kubernetes.io/is-default-class:true]",                                                          // legacy Go-map
	}
	for _, out := range proceed {
		r := &mockRunner{respond: func(_ string, args []string) Result {
			if slices.Contains(args, "storageclass") {
				return Result{Stdout: out}
			}
			return Result{}
		}}
		k := newK8s(k8sContext(), r)
		if err := k.Start(context.Background(), StartOpts{}); err != nil {
			t.Fatalf("a default StorageClass (%q) must let the install proceed: %v", out, err)
		}
		if r.lastCall("helm", "upgrade") == nil {
			t.Fatalf("install must run when a default StorageClass exists (%q)", out)
		}
	}

	// A cluster with classes but NONE default: fail fast before provisioning a PVC
	// that would hang Pending.
	r := &mockRunner{respond: func(_ string, args []string) Result {
		if slices.Contains(args, "storageclass") {
			return Result{Stdout: `{"storageclass.kubernetes.io/is-default-class":"false"}`}
		}
		return Result{}
	}}
	k := newK8s(k8sContext(), r)
	err := k.Start(context.Background(), StartOpts{})
	if err == nil {
		t.Fatal("persistent install must fail fast when no default StorageClass exists")
	}
	if !strings.Contains(err.Error(), "default StorageClass") {
		t.Fatalf("error %v must explain the missing default StorageClass", err)
	}
	if r.lastCall("helm", "upgrade") != nil {
		t.Fatal("preflight must abort before the helm install")
	}
}

// ---------------------------------------------------------------------------
// logs
// ---------------------------------------------------------------------------

func TestLogsCommandArgv(t *testing.T) {
	t.Parallel()
	local := &Local{runner: &mockRunner{}, name: "cuttle"}
	if exe, args := local.LogsCommand(false); exe != "docker" || !slices.Equal(args, []string{"logs", "cuttle"}) {
		t.Errorf("local plain: %s %v", exe, args)
	}
	if exe, args := local.LogsCommand(true); exe != "docker" || !slices.Equal(args, []string{"logs", "--follow", "cuttle"}) {
		t.Errorf("local follow: %s %v", exe, args)
	}

	ssh := sshBackend(&mockRunner{})
	exe, args := ssh.LogsCommand(true)
	if exe != "ssh" {
		t.Errorf("ssh exe=%s", exe)
	}
	// The remote docker argv must ride after the host, shell-safe and in order.
	tail := args[len(args)-4:]
	if args[len(args)-5] != "user@box.example" || !slices.Equal(tail, []string{"docker", "logs", "--follow", "cuttle"}) {
		t.Errorf("ssh args=%v", args)
	}

	k := newK8s(k8sContext(), &mockRunner{})
	if exe, args := k.LogsCommand(false); exe != "kubectl" || !slices.Equal(args, []string{
		"--context", "kind", "-n", "browser", "logs", "-l", "app.kubernetes.io/instance=cuttle", "--tail=-1",
	}) {
		t.Errorf("k8s plain: %s %v", exe, args)
	}
	if _, args := k.LogsCommand(true); args[len(args)-1] != "-f" {
		t.Errorf("k8s follow: %v", args)
	}
}

func TestDiscoverPortsParsesDockerPort(t *testing.T) {
	t.Parallel()
	r := &mockRunner{respond: func(_ string, args []string) Result {
		// `docker port <name>` (no port arg) lists every mapping in one call.
		if slices.Contains(args, "port") {
			return Result{Stdout: "6080/tcp -> 127.0.0.1:6099\n9222/tcp -> 127.0.0.1:9333\n"}
		}
		return Result{Code: 1}
	}}
	local := &Local{runner: r, name: "cuttle-dltest"}
	if cdp, vnc, ok := local.DiscoverPorts(context.Background()); !ok || cdp != 9333 || vnc != 6099 {
		t.Fatalf("local discover: cdp=%d vnc=%d ok=%v", cdp, vnc, ok)
	}
	ssh := sshBackend(r)
	if cdp, vnc, ok := ssh.DiscoverPorts(context.Background()); !ok || cdp != 9333 || vnc != 6099 {
		t.Fatalf("ssh discover: cdp=%d vnc=%d ok=%v", cdp, vnc, ok)
	}
}

// A stopped container publishes nothing, so its ports come from the bindings
// `docker start` will rebind.
func TestDiscoverPortsReadsAStoppedContainersBindings(t *testing.T) {
	t.Parallel()
	r := &mockRunner{respond: func(_ string, args []string) Result {
		if strings.Contains(strings.Join(args, " "), ".HostConfig.PortBindings") {
			return Result{Stdout: `{"6080/tcp":[{"HostIp":"127.0.0.1","HostPort":"6741"}],"9222/tcp":[{"HostIp":"127.0.0.1","HostPort":"9741"}]}` + "\n"}
		}
		return Result{Code: 1} // docker port: not running
	}}
	if cdp, vnc, ok := (&Local{runner: r, name: "x"}).DiscoverPorts(context.Background()); !ok || cdp != 9741 || vnc != 6741 {
		t.Fatalf("local discover: cdp=%d vnc=%d ok=%v", cdp, vnc, ok)
	}
	if cdp, vnc, ok := sshBackend(r).DiscoverPorts(context.Background()); !ok || cdp != 9741 || vnc != 6741 {
		t.Fatalf("ssh discover: cdp=%d vnc=%d ok=%v", cdp, vnc, ok)
	}
}

func TestDiscoverPortsUnpublishedIsNotOK(t *testing.T) {
	t.Parallel()
	r := &mockRunner{respond: func(string, []string) Result { return Result{Code: 1} }} // no such container
	if _, _, ok := (&Local{runner: r, name: "x"}).DiscoverPorts(context.Background()); ok {
		t.Fatal("want ok=false when neither docker port nor the configured bindings resolve")
	}
}

// ExecCommand is what `cuttle pw` runs, so its argv encodes two contracts a
// silent break would turn into a driver argument being re-parsed remotely: ssh
// shell-quotes every remote token, and kubectl (which has no workdir flag) hands
// the argv to a shell as "$@" rather than interpolating it.
func TestExecCommandArgv(t *testing.T) {
	t.Parallel()
	argv := []string{"playwright-cli", "click", "a b"}
	workdir := "/data/__default__/Downloads"

	local := &Local{runner: &mockRunner{}, name: "cuttle"}
	exe, args := local.ExecCommand(workdir, argv)
	wrapped := slices.Concat([]string{"sh", "-c", `(umask 077 && mkdir -p -- "$0") && cd -- "$0" && exec "$@"`, workdir}, argv)
	if exe != "docker" || !slices.Equal(args, slices.Concat([]string{"exec", "-i", "cuttle"}, wrapped)) {
		t.Errorf("local: %s %v", exe, args)
	}

	ssh := sshBackend(&mockRunner{})
	exe, args = ssh.ExecCommand(workdir, argv)
	if exe != "ssh" {
		t.Errorf("ssh exe=%s", exe)
	}
	// The remote docker argv rides after the host, in order, with the one token
	// carrying a space quoted so the remote login shell does not split it.
	tail := args[len(args)-11:]
	if !slices.Equal(tail, []string{
		"docker", "exec", "-i", "cuttle", "sh", "-c", `'(umask 077 && mkdir -p -- "$0") && cd -- "$0" && exec "$@"'`, workdir, "playwright-cli", "click", "'a b'",
	}) {
		t.Errorf("ssh args=%v", args)
	}

	k := newK8s(k8sContext(), &mockRunner{})
	exe, args = k.ExecCommand(workdir, argv)
	if exe != "kubectl" || !slices.Equal(args, slices.Concat([]string{
		"--context", "kind", "-n", "browser", "exec", "-i", "deploy/cuttle", "--",
	}, wrapped)) {
		t.Errorf("k8s: %s %v", exe, args)
	}

	// The direct backend has no container, so it must NOT satisfy Execer - the CLI
	// branches on that to explain itself instead of running a meaningless command.
	d, err := newDirect(config.Context{Backend: config.BackendDirect, CDPURL: "http://127.0.0.1:9222"})
	if err != nil {
		t.Fatalf("newDirect: %v", err)
	}
	var direct Backend = d
	if _, ok := direct.(Execer); ok {
		t.Error("direct backend must not implement Execer")
	}
}

// Right after a restart the driver's workdir is not there yet: the exec must
// create it and run the verb in it (where --filename output lands), with the
// verb's arguments reaching it verbatim.
func TestExecCreatesAMissingWorkdir(t *testing.T) {
	t.Parallel()
	workdir := filepath.Join(t.TempDir(), "__default__", "Downloads")
	argv := inWorkdir(workdir, []string{"sh", "-c", `pwd -P; printf '%s\n' "$1"`, "sh", "a b;$HOME"})
	out, err := exec.CommandContext(context.Background(), argv[0], argv[1:]...).Output()
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(workdir)
	if lines := strings.Split(strings.TrimSpace(string(out)), "\n"); len(lines) != 2 || lines[0] != resolved || lines[1] != "a b;$HOME" {
		t.Fatalf("ran as:\n%s", out)
	}
	if fi, err := os.Stat(workdir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("workdir: %v %v, want a 0700 dir", fi, err)
	}
}

// The chart folds the chart name into the Deployment name, and `kubectl exec
// deploy/<name>` is how ExecCommand reaches a pod - a wrong name is a command
// that only fails against a non-default release.
func TestK8sDeploymentNameMatchesChartFullname(t *testing.T) {
	t.Parallel()
	for release, want := range map[string]string{
		"cuttle":     "cuttle",     // release carries the chart name already
		"cuttle-dev": "cuttle-dev", // ... as a prefix
		"browsers":   "browsers-cuttle",
	} {
		ctx := k8sContext()
		ctx.Release = release
		if got := newK8s(ctx, &mockRunner{}).deploymentName(); got != want {
			t.Errorf("release %q: deploymentName=%q want %q", release, got, want)
		}
	}
}

// failedRun answers a `docker run` with runErr, and the label query after it
// with the id of the container that run created - none when created is false,
// as for a run that failed before creating one while a concurrent `up` holds the
// name. Any other inspect reads as an absent container.
func failedRun(runErr string, created bool, volumeInspect Result) func(string, []string) Result {
	var mu sync.Mutex
	label := ""
	return func(_ string, args []string) Result {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case slices.Contains(args, "volume") && slices.Contains(args, "inspect"):
			return volumeInspect
		case slices.Contains(args, "inspect"):
			return Result{Code: 1}
		case slices.Contains(args, dockerRunSub):
			if i := slices.Index(args, "--label"); i >= 0 {
				label = "label=" + args[i+1]
			}
			return Result{Code: 125, Stderr: runErr}
		case slices.Contains(args, "ps") && created && label != "" && slices.Contains(args, label):
			return Result{Stdout: "c0ffee\n"}
		}
		return Result{}
	}
}

// A failed `docker run` is cleaned up only as far as that run created anything:
// its half-made container, found by the label that run set, and the profile
// volume only if the run made it. By name, the container could be a concurrent
// `up`'s - a name conflict, or a run that failed before creating one.
func TestLocalFailedRunRemovesOnlyWhatItCreated(t *testing.T) {
	const portClash = "Bind for 127.0.0.1:9222 failed: port is already allocated"
	const nameClash = `Conflict. The container name "/cuttle" is already in use by container "abc"`
	noVolume := Result{Code: 1, Stderr: "Error response from daemon: get cuttle-cuttle-profile: no such volume"}
	for _, tc := range []struct {
		name          string
		runErr        string
		created       bool
		volumeInspect Result
		wantVolumeRm  bool
	}{
		{name: "port clash, fresh volume", runErr: portClash, created: true, volumeInspect: noVolume, wantVolumeRm: true},
		{name: "port clash, profile volume kept", runErr: portClash, created: true},
		// An inspect that fails for another reason must not read as "no volume".
		{name: "port clash, volume check failed", runErr: portClash, created: true, volumeInspect: Result{Code: 255, Stderr: "ssh: connection reset"}},
		{name: "name clash with a concurrent up", runErr: nameClash, volumeInspect: noVolume},
		{name: "name clash, podman wording", runErr: `creating container storage: the container name "cuttle" is already in use by abc. You have to remove that container to be able to reuse that name: that name is already in use`, volumeInspect: noVolume},
		{name: "pull failed while a concurrent up won", runErr: "Unable to find image 'img:1' locally: pull access denied", volumeInspect: noVolume},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &mockRunner{respond: failedRun(tc.runErr, tc.created, tc.volumeInspect)}
			l := &Local{runner: r, name: "cuttle", cdpPort: 9222, vncPort: 6080, image: "img:1"}
			if err := l.Start(context.Background(), StartOpts{}); err == nil {
				t.Fatal("a failed run must fail Start")
			}
			if r.hasCall("docker", "rm", "-f", "cuttle") {
				t.Error("removed a container by name, which may be another run's")
			}
			if got := r.hasCall("docker", "rm", "-f", "c0ffee"); got != tc.created {
				t.Errorf("container removed = %v, want %v", got, tc.created)
			}
			if got := r.hasCall("docker", "volume", "rm", "-f", "cuttle-cuttle-profile"); got != tc.wantVolumeRm {
				t.Errorf("volume removed = %v, want %v", got, tc.wantVolumeRm)
			}
		})
	}
}

// A standing forward is healthy only when the daemon on its local port is the
// instance it forwards to: a live pid and a listening port are also what another
// cuttle holding that port looks like.
func TestTunnelReachesInstance(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"active":0,"hostname":"abc123"}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	for _, tc := range []struct {
		name string
		want string
		ok   bool
	}{
		{name: "the forwarded instance", want: "abc123", ok: true},
		{name: "another instance on the port", want: "def456", ok: false},
		{name: "remote hostname unknown", want: "", ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := tunnelSpec{cdpPort: port, instanceHost: func(context.Context) string { return tc.want }}
			if got := tunnelReachesInstance(context.Background(), spec); got != tc.ok {
				t.Fatalf("tunnelReachesInstance = %v, want %v", got, tc.ok)
			}
		})
	}
	t.Run("a daemon that names no host cannot be told apart", func(t *testing.T) {
		t.Parallel()
		spec := tunnelSpec{cdpPort: ephemeralPort(t), instanceHost: func(context.Context) string { return "abc123" }}
		if !tunnelReachesInstance(context.Background(), spec) {
			t.Fatal("a silent port must not be reported as foreign")
		}
	})
}
