package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/glim-sh/cuttle/internal/fingerprint"
)

var errFakeNoFile = errors.New("no such file")

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeProcess struct {
	mu     sync.Mutex
	alive  bool
	termed bool
	killed bool
	clean  bool
	done   chan struct{}
}

func newFakeProcess() *fakeProcess { return &fakeProcess{alive: true, done: make(chan struct{})} }

func (f *fakeProcess) running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive
}

func (f *fakeProcess) signalTerm() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.termed = true
	f.alive = false
	f.closeDoneLocked()
	return nil
}

func (f *fakeProcess) kill() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = true
	f.alive = false
	f.closeDoneLocked()
	return nil
}

func (f *fakeProcess) wait(time.Duration) bool   { return true }
func (f *fakeProcess) waitExit() <-chan struct{} { return f.done }
func (f *fakeProcess) pid() int                  { return 4242 }

// crash simulates Chrome dying on its own - an exit the pool did not initiate and
// that Chrome did not choose (a signal, a non-zero status).
func (f *fakeProcess) crash() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive = false
	f.closeDoneLocked()
}

// closeWindow simulates a person closing the browser window in the viewer: Chrome
// quits itself, cleanly, without the pool asking.
func (f *fakeProcess) closeWindow() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive = false
	f.clean = true
	f.closeDoneLocked()
}

func (f *fakeProcess) exitedCleanly() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clean
}

func (f *fakeProcess) closeDoneLocked() {
	select {
	case <-f.done:
	default:
		close(f.done)
	}
}

func (f *fakeProcess) terminated() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.termed || f.killed
}

type fakeLauncher struct {
	port    int
	mu      sync.Mutex
	started [][]string
	procs   []*fakeProcess
}

func (f *fakeLauncher) toLauncher() launcher {
	return launcher{
		allocPort: func() (int, error) { return f.port, nil },
		start: func(_ string, args []string) (processHandle, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.started = append(f.started, args)
			p := newFakeProcess()
			f.procs = append(f.procs, p)
			return p, nil
		},
		waitReady: func(context.Context, int) bool { return true },
	}
}

func (f *fakeLauncher) lastArgs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.started) == 0 {
		return nil
	}
	return f.started[len(f.started)-1]
}

func (f *fakeLauncher) launchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.procs)
}

func newTestPool(t *testing.T, cfg serveConfig, l launcher) *chromePool {
	t.Helper()
	cfg.dataDir = t.TempDir()
	cfg.headless = true
	// Most tests drive named seeds, which only pool mode admits; the session-mode
	// tests set cfg.mode explicitly.
	if cfg.mode == "" {
		cfg.mode = modePool
	}
	pool := newChromePool(cfg, "/fake/chrome", nil, l, fingerprint.GeoResolver{})
	// Default to a CDP state seam whose extract fails, so lifecycle triggers
	// (disconnect/shutdown capture, launch re-inject) never reach chromedp against
	// a fake launcher's dead port NOR persist a snapshot into the test's TempDir
	// after the test ends. Tests that assert on captured state install their own
	// fake with a result.
	pool.state = (&fakeStateOps{err: errFakeNoExtract}).toStateOps()
	return pool
}

// ---------------------------------------------------------------------------
// Pool behavior
// ---------------------------------------------------------------------------

func TestGetOrLaunchDefaultProxyInheritance(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{proxy: "http://bob:secret@proxy.example:8080"}, fl.toLauncher())

	inst, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"})
	if err != nil {
		t.Fatalf("getOrLaunch: %v", err)
	}
	if inst.proxy != "http://bob:secret@proxy.example:8080" {
		t.Errorf("instance proxy=%q (should inherit server default with creds)", inst.proxy)
	}
	if !slices.Contains(fl.lastArgs(), "--proxy-server=http://proxy.example:8080") {
		t.Errorf("chrome args missing cred-stripped proxy-server: %v", fl.lastArgs())
	}
	// The credentials must NOT reach the argv (answered over CDP instead).
	for _, a := range fl.lastArgs() {
		if strings.Contains(a, "secret") {
			t.Errorf("credentials leaked into argv: %q", a)
		}
	}
	// Without these ICE enumerates the real interfaces and STUN leaks the real
	// egress, contradicting the seed's proxy-derived geo. The fingerprint golden
	// does not cover them (they are appended here, not in BuildArgs), so this is
	// their only tripwire.
	for _, want := range []string{
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--webrtc-ip-handling-policy=disable_non_proxied_udp",
	} {
		if !slices.Contains(fl.lastArgs(), want) {
			t.Errorf("proxied seed missing %s: %v", want, fl.lastArgs())
		}
	}
}

// A direct-egress seed has no proxy to confine WebRTC to, so the policy flags
// must stay off: they would be a behavioral difference no real browser shows.
func TestGetOrLaunchNoWebRTCPolicyWithoutProxy(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{}, fl.toLauncher())

	if _, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"}); err != nil {
		t.Fatalf("getOrLaunch: %v", err)
	}
	for _, a := range fl.lastArgs() {
		if strings.Contains(a, "webrtc-ip-handling-policy") {
			t.Errorf("unproxied seed must not pin a WebRTC policy: %q", a)
		}
	}
}

func TestGetOrLaunchExplicitProxyOverridesDefault(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{proxy: "http://default:pw@def.example:8080"}, fl.toLauncher())

	inst, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1", proxy: "socks5://per.example:1080"})
	if err != nil {
		t.Fatalf("getOrLaunch: %v", err)
	}
	if inst.proxy != "socks5://per.example:1080" {
		t.Errorf("per-connection proxy should win, got %q", inst.proxy)
	}
}

func TestGetOrLaunchInvalidSeed(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{}, fl.toLauncher())

	_, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "bad seed!"})
	var le *launchError
	if !errors.As(err, &le) || le.status != http.StatusBadRequest {
		t.Fatalf("want 400 launchError, got %v", err)
	}
	if fl.launchCount() != 0 {
		t.Errorf("invalid seed must not launch a process")
	}
}

func TestGetOrLaunchReuseFirstLaunchWins(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{}, fl.toLauncher())

	a, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1", proxy: "http://ignored:1"})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("second call should return the same running instance")
	}
	if fl.launchCount() != 1 {
		t.Errorf("launchCount=%d want 1 (first-launch wins)", fl.launchCount())
	}
	if a.proxy != "" {
		t.Errorf("first launch had no proxy; later proxy must be ignored, got %q", a.proxy)
	}
}

func TestDefaultSeedAutoRelaunch(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{mode: modeSession}, fl.toLauncher())

	inst, err := pool.getOrLaunch(context.Background(), connectRequest{}) // reserved default seed
	if err != nil {
		t.Fatal(err)
	}
	if fl.launchCount() != 1 {
		t.Fatalf("launchCount=%d want 1", fl.launchCount())
	}

	// Chrome dies on its own; the pool did not initiate it, so the default seed
	// must self-heal.
	inst.process.(*fakeProcess).crash()

	var cur *chromeInstance
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pool.mu.Lock()
		cur = pool.processes[reservedSeed]
		pool.mu.Unlock()
		if cur != nil && cur != inst {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if cur == nil || cur == inst {
		t.Fatalf("default browser was not auto-relaunched (launchCount=%d)", fl.launchCount())
	}
}

func TestNamedSeedNoAutoRelaunch(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{}, fl.toLauncher())

	inst, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	inst.process.(*fakeProcess).crash()

	time.Sleep(100 * time.Millisecond)
	if fl.launchCount() != 1 {
		t.Fatalf("named agent seed must not auto-relaunch (launchCount=%d)", fl.launchCount())
	}
}

// A person closing the browser window in the viewer must not be undone: Chrome
// quits cleanly with nothing attached, and respawning it made the browser
// impossible to close.
func TestCleanCloseWithNoClientStaysClosed(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{mode: modeSession}, fl.toLauncher())

	inst, err := pool.getOrLaunch(context.Background(), connectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// A browser nobody has touched for a while: past the window in which a
	// just-launched or just-detached client still counts as using it.
	inst.startedAt = time.Now().Add(-time.Hour)
	inst.process.(*fakeProcess).closeWindow()

	time.Sleep(100 * time.Millisecond)
	if fl.launchCount() != 1 {
		t.Fatalf("a hand-closed browser must stay closed (launchCount=%d)", fl.launchCount())
	}

	// ...and it comes back on the next attach.
	next, err := pool.getOrLaunch(context.Background(), connectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if next == inst || fl.launchCount() != 2 {
		t.Fatalf("the next attach must relaunch it (launchCount=%d)", fl.launchCount())
	}
}

// A driver that closes its last tab exits Chrome just as cleanly, but it is still
// attached - that is a client bug to heal, not an intent to close.
func TestCleanExitWithClientAttachedRelaunches(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{mode: modeSession}, fl.toLauncher())

	inst, err := pool.getOrLaunch(context.Background(), connectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	inst.startedAt = time.Now().Add(-time.Hour)
	pool.connect(reservedSeed)
	inst.process.(*fakeProcess).closeWindow()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fl.launchCount() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if fl.launchCount() != 2 {
		t.Fatalf("an attached client's browser must self-heal (launchCount=%d)", fl.launchCount())
	}
}

// Chrome's death is what ends the driver's WebSocket, so the disconnect and this
// supervisor race: if disconnect lands first the refcount is already zero. A
// just-detached client therefore still counts as using the browser, or a driver
// bug would masquerade as a deliberate close and the session would stay dead.
func TestCleanExitJustAfterDetachStillRelaunches(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{mode: modeSession}, fl.toLauncher())

	inst, err := pool.getOrLaunch(context.Background(), connectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	inst.startedAt = time.Now().Add(-time.Hour)
	pool.connect(reservedSeed)
	pool.disconnect(reservedSeed) // the refcount is back to zero, moments ago
	inst.process.(*fakeProcess).closeWindow()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fl.launchCount() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if fl.launchCount() != 2 {
		t.Fatalf("an exit right after a detach must still self-heal (launchCount=%d)", fl.launchCount())
	}
}

// The mirror window: connect() runs after getOrLaunch returns, so the refcount is
// zero for the whole launch. A clean exit there is a failed startup, not a close.
func TestCleanExitDuringLaunchWindowRelaunches(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{mode: modeSession}, fl.toLauncher())

	inst, err := pool.getOrLaunch(context.Background(), connectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	inst.process.(*fakeProcess).closeWindow() // startedAt is now; nobody attached yet

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fl.launchCount() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if fl.launchCount() != 2 {
		t.Fatalf("an exit inside the launch window must self-heal (launchCount=%d)", fl.launchCount())
	}
}

func TestDefaultSeedNoRelaunchOnShutdown(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{mode: modeSession}, fl.toLauncher())

	if _, err := pool.getOrLaunch(context.Background(), connectRequest{}); err != nil {
		t.Fatal(err)
	}
	pool.shutdown() // intentional teardown terminates the default; it must NOT self-heal

	time.Sleep(100 * time.Millisecond)
	if fl.launchCount() != 1 {
		t.Fatalf("shutdown must not trigger auto-relaunch (launchCount=%d)", fl.launchCount())
	}
}

func TestIdleReap(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{idleTimeout: 30 * time.Millisecond}, fl.toLauncher())

	inst, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	fp := inst.process.(*fakeProcess)

	pool.connect("s1")
	pool.disconnect("s1")

	// Reap removes the seed from the pool and SIGTERMs its process on separate,
	// non-atomic steps, so the terminate can lag the map removal under load.
	// Wait for BOTH before asserting, or the terminate check races the reap.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pool.mu.Lock()
		_, present := pool.processes["s1"]
		pool.mu.Unlock()
		if !present && fp.terminated() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	pool.mu.Lock()
	_, present := pool.processes["s1"]
	pool.mu.Unlock()
	if present {
		t.Fatal("idle process was not reaped")
	}
	if !fp.terminated() {
		t.Error("reaped process was not terminated (SIGTERM)")
	}
}

func TestNoReapWhenIdleTimeoutZero(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{idleTimeout: 0}, fl.toLauncher())

	inst, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	pool.connect("s1")
	pool.disconnect("s1")

	time.Sleep(60 * time.Millisecond)
	pool.mu.Lock()
	_, present := pool.processes["s1"]
	pool.mu.Unlock()
	if !present {
		t.Fatal("process must not be reaped when idle-timeout is 0")
	}
	if inst.process.(*fakeProcess).terminated() {
		t.Error("process must stay alive with idle-timeout 0")
	}
}

func TestShutdownTerminatesAll(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{}, fl.toLauncher())

	for _, seed := range []string{"a", "b", "c"} {
		if _, err := pool.getOrLaunch(context.Background(), connectRequest{seed: seed}); err != nil {
			t.Fatal(err)
		}
	}
	pool.shutdown()
	fl.mu.Lock()
	procs := slices.Clone(fl.procs)
	fl.mu.Unlock()
	for _, p := range procs {
		if !p.terminated() {
			t.Error("shutdown left a process running")
		}
	}
	pool.mu.Lock()
	n := len(pool.processes)
	pool.mu.Unlock()
	if n != 0 {
		t.Errorf("processes not cleared: %d", n)
	}
}

func TestEphemeralProfileDir(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{ephemeral: true}, fl.toLauncher())

	inst, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	// Ephemeral dir is a fresh scratch under dataDir, not the stable seed path.
	if inst.userDataDir == pool.dataDir+"/s1" {
		t.Errorf("ephemeral dir should be a scratch path, got %q", inst.userDataDir)
	}
	if !strings.HasPrefix(inst.userDataDir, pool.dataDir) {
		t.Errorf("ephemeral dir %q not under dataDir %q", inst.userDataDir, pool.dataDir)
	}
}

// ---------------------------------------------------------------------------
// HTTP endpoints against a fake CDP backend
// ---------------------------------------------------------------------------

// The Chrome passthrough is appended past BuildArgs' dedupe, so this is the only
// guard that keeps a VNC-mode --start-maximized from overriding the seed's
// coherent window size.
func TestDropMaximizeIfSized(t *testing.T) {
	t.Parallel()
	maximized := []string{"about:blank", "--start-maximized", "--test-type"}

	t.Run("dropped when the seed pins a window size", func(t *testing.T) {
		t.Parallel()
		got, dropped := dropMaximizeIfSized([]string{"--window-size=1366,720"}, maximized)
		if !dropped {
			t.Error("want dropped=true")
		}
		if slices.Contains(got, "--start-maximized") {
			t.Errorf("--start-maximized survived: %q", got)
		}
		if !slices.Contains(got, "--test-type") || !slices.Contains(got, "about:blank") {
			t.Errorf("unrelated passthrough args must survive: %q", got)
		}
	})

	t.Run("kept when no window size is pinned", func(t *testing.T) {
		t.Parallel()
		got, dropped := dropMaximizeIfSized([]string{"--fingerprint=abc"}, maximized)
		if dropped {
			t.Error("want dropped=false")
		}
		if !slices.Contains(got, "--start-maximized") {
			t.Errorf("--start-maximized must survive: %q", got)
		}
	})

	t.Run("caller slice not mutated", func(t *testing.T) {
		t.Parallel()
		global := slices.Clone(maximized)
		dropMaximizeIfSized([]string{"--window-size=800,600"}, global)
		if !slices.Equal(global, maximized) {
			t.Errorf("input mutated: %q", global)
		}
	})
}

type fakeCDP struct {
	server *httptest.Server
	port   int
}

func newFakeCDP(t *testing.T) *fakeCDP {
	t.Helper()
	f := &fakeCDP{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /json/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Browser":"Chrome/148","webSocketDebuggerUrl":"ws://127.0.0.1:` +
			strconv.Itoa(f.port) + `/devtools/browser/GUID123"}`))
	})
	mux.HandleFunc("GET /json/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:` +
			strconv.Itoa(f.port) + `/devtools/page/PAGE9"}]`))
	})
	mux.HandleFunc("GET /devtools/{path...}", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		c.SetReadLimit(-1)
		ctx := r.Context()
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"method":"CDP.greeting"}`))
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			// Answer the launch-path keep-alive tab creation like real Chrome; echo
			// everything else so the piping test can assert round-trips.
			var m struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(data, &m) == nil && m.Method == "Target.createTarget" {
				ack, _ := json.Marshal(map[string]any{"id": m.ID, "result": map[string]any{"targetId": "KEEPALIVE"}})
				_ = c.Write(ctx, websocket.MessageText, ack)
				continue
			}
			_ = c.Write(ctx, typ, append([]byte("echo:"), data...))
		}
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	u, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	f.port, _ = strconv.Atoi(u.Port())
	return f
}

func TestHandleJSONVersionHostRewrite(t *testing.T) {
	t.Parallel()
	cdp := newFakeCDP(t)
	fl := &fakeLauncher{port: cdp.port}
	pool := newTestPool(t, serveConfig{}, fl.toLauncher())
	m := &multiplexer{pool: pool, port: 9222}

	tests := []struct {
		name    string
		target  string
		host    string
		headers map[string]string
		want    string
	}{
		{
			name:   "seed rewrites to request host",
			target: "/json/version?fingerprint=seedX",
			host:   "myhost:1234",
			want:   "ws://myhost:1234/fingerprint/seedX/devtools/browser/GUID123",
		},
		{
			name:    "x-forwarded-host and https -> wss",
			target:  "/json/version?fingerprint=seedX",
			host:    "internal:9222",
			headers: map[string]string{"X-Forwarded-Host": "public.example", "X-Forwarded-Proto": "https"},
			want:    "wss://public.example/fingerprint/seedX/devtools/browser/GUID123",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			req.Host = tc.host
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			m.handleJSONVersion(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"webSocketDebuggerUrl":"`+tc.want+`"`) {
				t.Errorf("body=%s\nwant ws url %q", rec.Body.String(), tc.want)
			}
		})
	}

	t.Run("session mode: no seed uses default devtools path", func(t *testing.T) {
		sp := newTestPool(t, serveConfig{mode: modeSession}, (&fakeLauncher{port: cdp.port}).toLauncher())
		sm := &multiplexer{pool: sp, port: 9222}
		req := httptest.NewRequest(http.MethodGet, "/json/version", nil)
		req.Host = "10.1.2.3:9222"
		rec := httptest.NewRecorder()
		sm.handleJSONVersion(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if want := "ws://10.1.2.3:9222/devtools/browser/GUID123"; !strings.Contains(rec.Body.String(), `"webSocketDebuggerUrl":"`+want+`"`) {
			t.Errorf("body=%s\nwant ws url %q", rec.Body.String(), want)
		}
	})
}

func TestHandleJSONListHostRewrite(t *testing.T) {
	t.Parallel()
	cdp := newFakeCDP(t)
	fl := &fakeLauncher{port: cdp.port}
	pool := newTestPool(t, serveConfig{}, fl.toLauncher())
	m := &multiplexer{pool: pool, port: 9222}

	req := httptest.NewRequest(http.MethodGet, "/json/list?fingerprint=seedX", nil)
	req.Host = "myhost:1234"
	rec := httptest.NewRecorder()
	m.handleJSONList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	want := "ws://myhost:1234/fingerprint/seedX/devtools/page/PAGE9"
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body=%s\nwant %q", rec.Body.String(), want)
	}
}

// ---------------------------------------------------------------------------
// Bidirectional WebSocket frame piping through the multiplexer
// ---------------------------------------------------------------------------

func TestWSFramePipingBothWays(t *testing.T) {
	t.Parallel()
	cdp := newFakeCDP(t)
	fl := &fakeLauncher{port: cdp.port}
	pool := newTestPool(t, serveConfig{}, fl.toLauncher())
	m := &multiplexer{pool: pool, port: 9222}

	front := httptest.NewServer(m.routes())
	t.Cleanup(front.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(front.URL, "http") + "/fingerprint/s1/devtools/browser/GUID123"
	client, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close(websocket.StatusNormalClosure, "") }()

	// cdp -> client: the fake backend pushes a greeting on connect.
	typ, data, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if typ != websocket.MessageText || string(data) != `{"method":"CDP.greeting"}` {
		t.Fatalf("unexpected greeting: %q", data)
	}

	// client -> cdp -> client: our frame reaches Chrome and its reply comes back.
	if werr := client.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"Browser.getVersion"}`)); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	_, echo, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(echo) != `echo:{"id":1,"method":"Browser.getVersion"}` {
		t.Errorf("echo=%q", echo)
	}

	// The live session is refcounted on the seed.
	pool.mu.Lock()
	conns := pool.conns["s1"]
	pool.mu.Unlock()
	if conns != 1 {
		t.Errorf("connections=%d want 1", conns)
	}
}

// TestSessionModeRefusesSeed is the one-browser-per-container invariant: in
// session mode a seeded connect is a 400, never a second Chrome.
func TestSessionModeRefusesSeed(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{mode: modeSession}, fl.toLauncher())

	if _, err := pool.getOrLaunch(context.Background(), connectRequest{}); err != nil {
		t.Fatalf("unseeded connect in session mode: %v", err)
	}
	_, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "linkedin"})
	var le *launchError
	if !errors.As(err, &le) || le.status != http.StatusBadRequest || le.msg != msgSeedInSession {
		t.Fatalf("seeded connect in session mode: want 400 %q, got %v", msgSeedInSession, err)
	}
	if fl.launchCount() != 1 {
		t.Fatalf("session mode launched %d Chromes, want exactly 1", fl.launchCount())
	}
}

// TestPoolModeRequiresSeed closes the discovery leak: an unseeded request in
// pool mode is a 400 and never spawns the direct-egress reserved default.
func TestPoolModeRequiresSeed(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{mode: modePool}, fl.toLauncher())

	_, err := pool.getOrLaunch(context.Background(), connectRequest{})
	var le *launchError
	if !errors.As(err, &le) || le.status != http.StatusBadRequest || le.msg != msgSeedRequired {
		t.Fatalf("unseeded connect in pool mode: want 400 %q, got %v", msgSeedRequired, err)
	}
	if fl.launchCount() != 0 {
		t.Fatalf("unseeded pool-mode request launched a Chrome")
	}
	if _, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"}); err != nil {
		t.Fatalf("seeded connect in pool mode: %v", err)
	}
	if pool.runningInstance(reservedSeed) != nil {
		t.Fatal("pool mode must never run the reserved default seed")
	}
}

// TestPoolModeOperatorDefaultSeed keeps --fingerprint meaningful in pool mode:
// an unseeded connect lands on the operator's named default, not the reserved one.
func TestPoolModeOperatorDefaultSeed(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{mode: modePool, defaultSeed: "ops"}, fl.toLauncher())
	if _, err := pool.getOrLaunch(context.Background(), connectRequest{}); err != nil {
		t.Fatal(err)
	}
	if pool.runningInstance("ops") == nil || pool.runningInstance(reservedSeed) != nil {
		t.Fatal("unseeded connect must key to the operator default seed")
	}
}
