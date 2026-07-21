package serve

import (
	"context"
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
}

func newFakeProcess() *fakeProcess { return &fakeProcess{alive: true} }

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
	return nil
}

func (f *fakeProcess) kill() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = true
	f.alive = false
	return nil
}

func (f *fakeProcess) wait(time.Duration) bool { return true }
func (f *fakeProcess) pid() int                { return 4242 }

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

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pool.mu.Lock()
		_, present := pool.processes["s1"]
		pool.mu.Unlock()
		if !present {
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
			name:   "no seed uses default devtools path",
			target: "/json/version",
			host:   "10.1.2.3:9222",
			want:   "ws://10.1.2.3:9222/devtools/browser/GUID123",
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
