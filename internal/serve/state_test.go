package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glim-sh/cuttle/internal/cdp"
)

// errFakeNoExtract makes a fake extract fail, so the default test pool captures
// nothing (no snapshot written) unless a test opts in with a real result.
var errFakeNoExtract = errors.New("fake: no extract")

// fakeStateOps substitutes the daemon's real cdp.Extract/Inject so supervision and
// the state API are exercised without a live Chrome.
type fakeStateOps struct {
	mu       sync.Mutex
	result   *cdp.StorageState
	err      error
	injected []*cdp.StorageState
}

func (f *fakeStateOps) toStateOps() stateOps {
	return stateOps{
		extract: func(context.Context, string, []string) (*cdp.StorageState, []string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.err != nil {
				return nil, nil, f.err
			}
			return cloneState(f.result), nil, nil
		},
		inject: func(_ context.Context, _ string, st *cdp.StorageState) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.injected = append(f.injected, st)
			return nil
		},
	}
}

func (f *fakeStateOps) injectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.injected)
}

func cloneState(st *cdp.StorageState) *cdp.StorageState {
	if st == nil {
		return &cdp.StorageState{}
	}
	out := &cdp.StorageState{Cookies: append([]cdp.Cookie(nil), st.Cookies...)}
	out.Origins = append([]cdp.Origin(nil), st.Origins...)
	return out
}

func cookieState(name, val string) *cdp.StorageState {
	return &cdp.StorageState{Cookies: []cdp.Cookie{{Name: name, Value: val, Domain: "example.com", Path: "/", Expires: -1}}}
}

// ---------------------------------------------------------------------------
// stateStore
// ---------------------------------------------------------------------------

func TestStateStorePutGetAndETag(t *testing.T) {
	t.Parallel()
	s := newStateStore(t.TempDir())
	etag, conflict, err := s.put("s1", cookieState("a", "1"), true, "")
	if err != nil || conflict {
		t.Fatalf("put: err=%v conflict=%v", err, conflict)
	}
	if etag == "" {
		t.Fatal("empty etag")
	}
	e, ok := s.get("s1")
	if !ok || e.ETag != etag || !e.Supervised {
		t.Fatalf("get mismatch: %+v ok=%v", e, ok)
	}
}

func TestStateStoreIfMatch(t *testing.T) {
	t.Parallel()
	s := newStateStore(t.TempDir())
	etag, _, _ := s.put("s1", cookieState("a", "1"), true, "")

	if _, conflict, _ := s.put("s1", cookieState("b", "2"), true, `"wrong"`); !conflict {
		t.Fatal("stale If-Match should conflict")
	}
	newTag, conflict, err := s.put("s1", cookieState("b", "2"), true, etag)
	if err != nil || conflict {
		t.Fatalf("matched If-Match should apply: conflict=%v err=%v", conflict, err)
	}
	if newTag == etag {
		t.Fatal("etag should rotate on a changed body")
	}
	// RFC 9110: "*" matches any existing entry, and fails when none exists.
	if _, conflict, _ := s.put("s1", cookieState("c", "3"), true, "*"); conflict {
		t.Fatal(`If-Match "*" should apply against an existing entry`)
	}
	if _, conflict, _ := s.put("absent", cookieState("c", "3"), true, "*"); !conflict {
		t.Fatal(`If-Match "*" should conflict when no entry exists`)
	}
}

func TestStateStorePersistAndReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newStateStore(dir)
	if _, _, err := s.put("linkedin", cookieState("li", "x"), true, ""); err != nil {
		t.Fatalf("put: %v", err)
	}
	// A fresh store over the same dir must recover the snapshot and its mark.
	s2 := newStateStore(dir)
	e, ok := s2.get("linkedin")
	if !ok || len(e.State.Cookies) != 1 || e.State.Cookies[0].Value != "x" || !e.Supervised {
		t.Fatalf("reload mismatch: %+v ok=%v", e, ok)
	}
}

func TestStateStoreRejectsUnsafeKey(t *testing.T) {
	t.Parallel()
	s := newStateStore(t.TempDir())
	if _, _, err := s.put("../evil", cookieState("a", "1"), true, ""); err == nil {
		t.Fatal("a key with path separators must be rejected")
	}
}

// ---------------------------------------------------------------------------
// state API handlers
// ---------------------------------------------------------------------------

func newStatePool(t *testing.T, cfg serveConfig, ops *fakeStateOps) *chromePool {
	t.Helper()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, cfg, fl.toLauncher())
	pool.state = ops.toStateOps()
	return pool
}

// stateReq builds a request for the state handlers, setting the {seed} path value
// the way http.ServeMux would (the handlers are called directly, bypassing the
// route pattern) and a loopback Host so the anti-rebinding guard admits it.
func stateReq(method, seed, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/profile/"+seed+"/state", nil)
	} else {
		r = httptest.NewRequest(method, "/profile/"+seed+"/state", strings.NewReader(body))
	}
	r.Host = "127.0.0.1:9222"
	r.SetPathValue("seed", seed)
	return r
}

func TestHandleGetStateLiveExtract(t *testing.T) {
	t.Parallel()
	ops := &fakeStateOps{result: cookieState("sess", "live")}
	pool := newStatePool(t, serveConfig{}, ops)
	if _, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"}); err != nil {
		t.Fatal(err)
	}
	m := &multiplexer{pool: pool, port: 9222}

	rec := httptest.NewRecorder()
	m.handleGetState(rec, stateReq(http.MethodGet, "s1", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"sess"`) || rec.Header().Get("ETag") == "" {
		t.Fatalf("body=%s etag=%q", rec.Body.String(), rec.Header().Get("ETag"))
	}
	// GET is side-effect-free: a live extract must NOT write the store.
	if _, ok := pool.store.get("s1"); ok {
		t.Fatal("live GET must not mutate the snapshot store")
	}
}

func TestStateHandlersRejectNonLoopbackHost(t *testing.T) {
	t.Parallel()
	pool := newStatePool(t, serveConfig{}, &fakeStateOps{})
	if _, _, err := pool.store.put("s1", cookieState("c", "snap"), true, ""); err != nil {
		t.Fatal(err)
	}
	m := &multiplexer{pool: pool, port: 9222}
	// A DNS-rebinding page keeps its own Host after the DNS flips to 127.0.0.1.
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		rec := httptest.NewRecorder()
		req := stateReq(method, "s1", `{"cookies":[],"origins":[]}`)
		req.Host = "attacker.com:9222"
		if method == http.MethodGet {
			m.handleGetState(rec, req)
		} else {
			m.handlePutState(rec, req)
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s from rebound Host want 403, got %d", method, rec.Code)
		}
	}
}

func TestStateHandlersRejectCrossOrigin(t *testing.T) {
	t.Parallel()
	pool := newStatePool(t, serveConfig{}, &fakeStateOps{})
	if _, _, err := pool.store.put("s1", cookieState("c", "snap"), true, ""); err != nil {
		t.Fatal(err)
	}
	m := &multiplexer{pool: pool, port: 9222}
	rec := httptest.NewRecorder()
	req := stateReq(http.MethodGet, "s1", "")
	req.Header.Set("Origin", "http://evil.example")
	m.handleGetState(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("loopback Host but cross-origin Origin want 403, got %d", rec.Code)
	}
}

func TestHandleGetStateSnapshotFallbackAnd404(t *testing.T) {
	t.Parallel()
	ops := &fakeStateOps{}
	pool := newStatePool(t, serveConfig{}, ops)
	m := &multiplexer{pool: pool, port: 9222}

	// Not running, no snapshot -> 404.
	rec := httptest.NewRecorder()
	m.handleGetState(rec, stateReq(http.MethodGet, "s1", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	// Not running, snapshot present -> served from the store.
	if _, _, err := pool.store.put("s1", cookieState("c", "snap"), false, ""); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	m.handleGetState(rec, stateReq(http.MethodGet, "s1", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"snap"`) {
		t.Fatalf("snapshot fallback: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleGetStateInvalidSeed(t *testing.T) {
	t.Parallel()
	m := &multiplexer{pool: newStatePool(t, serveConfig{}, &fakeStateOps{}), port: 9222}
	rec := httptest.NewRecorder()
	m.handleGetState(rec, stateReq(http.MethodGet, "__default__", ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reserved seed want 400, got %d", rec.Code)
	}
}

func TestHandlePutStateStoresInjectsAndETag(t *testing.T) {
	t.Parallel()
	ops := &fakeStateOps{}
	pool := newStatePool(t, serveConfig{}, ops)
	if _, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"}); err != nil {
		t.Fatal(err)
	}
	m := &multiplexer{pool: pool, port: 9222}

	body, _ := json.Marshal(cookieState("put", "v"))
	rec := httptest.NewRecorder()
	m.handlePutState(rec, stateReq(http.MethodPut, "s1", string(body)))
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") == "" {
		t.Fatalf("status=%d etag=%q body=%s", rec.Code, rec.Header().Get("ETag"), rec.Body.String())
	}
	if e, ok := pool.store.get("s1"); !ok || !e.Supervised {
		t.Fatalf("PUT must store and mark supervised: %+v ok=%v", e, ok)
	}
	if ops.injectCount() == 0 {
		t.Fatal("PUT into a running seed must inject")
	}
}

func TestHandlePutStateIfMatchConflict(t *testing.T) {
	t.Parallel()
	pool := newStatePool(t, serveConfig{}, &fakeStateOps{})
	m := &multiplexer{pool: pool, port: 9222}
	body, _ := json.Marshal(cookieState("a", "1"))

	rec := httptest.NewRecorder()
	req := stateReq(http.MethodPut, "s1", string(body))
	req.Header.Set("If-Match", `"stale"`)
	m.handlePutState(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match want 412, got %d", rec.Code)
	}
}

func TestHandlePutStateBadBody(t *testing.T) {
	t.Parallel()
	m := &multiplexer{pool: newStatePool(t, serveConfig{}, &fakeStateOps{}), port: 9222}
	rec := httptest.NewRecorder()
	m.handlePutState(rec, stateReq(http.MethodPut, "s1", "not json"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body want 400, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// supervision lifecycle
// ---------------------------------------------------------------------------

func TestDisconnectCapturesState(t *testing.T) {
	t.Parallel()
	ops := &fakeStateOps{result: cookieState("sess", "captured")}
	pool := newStatePool(t, serveConfig{}, ops) // keepProfile=false -> disposable/supervised
	if _, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"}); err != nil {
		t.Fatal(err)
	}
	pool.connect("s1")
	pool.disconnect("s1")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := pool.store.get("s1"); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("last-client-disconnect did not capture state")
}

func TestShutdownCapturesState(t *testing.T) {
	t.Parallel()
	ops := &fakeStateOps{result: cookieState("sess", "final")}
	pool := newStatePool(t, serveConfig{}, ops)
	if _, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"}); err != nil {
		t.Fatal(err)
	}
	pool.shutdown()
	if e, ok := pool.store.get("s1"); !ok || e.State.Cookies[0].Value != "final" {
		t.Fatalf("shutdown must snapshot supervised seeds: %+v ok=%v", e, ok)
	}
}

// TestIdleReapWaitsForInFlightCapture guards the fix for the disconnect-vs-reap
// race: an idle reap must not kill Chrome (wiping the ephemeral profile) while a
// disconnect-triggered capture is still extracting, or a fresh login is lost.
func TestIdleReapWaitsForInFlightCapture(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	captured := cookieState("sess", "in-flight")
	gated := stateOps{
		extract: func(context.Context, string, []string) (*cdp.StorageState, []string, error) {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
			}
			return cloneState(captured), nil, nil
		},
		inject: func(context.Context, string, *cdp.StorageState) error { return nil },
	}
	pool := newStatePool(t, serveConfig{}, &fakeStateOps{})
	pool.state = gated
	inst, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	fp := inst.process.(*fakeProcess)

	go pool.captureSupervised("s1", inst) // holds the capture lock, blocks in extract
	<-entered

	reaped := make(chan struct{})
	go func() { pool.idleReap("s1"); close(reaped) }()

	select {
	case <-reaped:
		t.Fatal("idleReap terminated Chrome before the in-flight capture finished")
	case <-time.After(50 * time.Millisecond):
	}
	if fp.terminated() {
		t.Fatal("Chrome was terminated while a capture was in flight")
	}
	close(release)
	<-reaped

	if !fp.terminated() {
		t.Fatal("idleReap must terminate after capturing")
	}
	if e, ok := pool.store.get("s1"); !ok || e.State.Cookies[0].Value != "in-flight" {
		t.Fatalf("in-flight capture must survive the reap: %+v ok=%v", e, ok)
	}
}

func TestReinjectStoredStateAtLaunch(t *testing.T) {
	t.Parallel()
	ops := &fakeStateOps{}
	pool := newStatePool(t, serveConfig{}, ops)
	if _, _, err := pool.store.put("s1", cookieState("seed", "preloaded"), true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"}); err != nil {
		t.Fatal(err)
	}
	if ops.injectCount() != 1 {
		t.Fatalf("a stored snapshot must be re-injected at launch, injects=%d", ops.injectCount())
	}
}

func TestKeepProfileSupervisesOnlyMarked(t *testing.T) {
	t.Parallel()
	ops := &fakeStateOps{result: cookieState("sess", "x")}
	pool := newStatePool(t, serveConfig{keepProfile: true}, ops)
	if _, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"}); err != nil {
		t.Fatal(err)
	}
	pool.connect("s1")
	pool.disconnect("s1")

	time.Sleep(80 * time.Millisecond)
	if _, ok := pool.store.get("s1"); ok {
		t.Fatal("with --keep-profile an unmarked seed must not be auto-captured")
	}
}

// TestKeepProfileSupervisesReservedDefaultSeed proves the reserved default seed
// is auto-captured even under --keep-profile, so its CDP-only cookies survive a
// recreate (the profile-dir volume carries everything except the never-flushed
// Cookies DB).
func TestKeepProfileSupervisesReservedDefaultSeed(t *testing.T) {
	t.Parallel()
	ops := &fakeStateOps{result: cookieState("sess", "x")}
	pool := newStatePool(t, serveConfig{keepProfile: true}, ops)
	if _, err := pool.getOrLaunch(context.Background(), connectRequest{seed: ""}); err != nil {
		t.Fatal(err)
	}
	pool.connect(reservedSeed)
	pool.disconnect(reservedSeed)

	time.Sleep(80 * time.Millisecond)
	if _, ok := pool.store.get(reservedSeed); !ok {
		t.Fatal("the reserved default seed must be auto-captured even with --keep-profile")
	}
}

// TestStatePersistAllowsReservedSeed proves the reserved default seed snapshot is
// written to disk and survives a store reload - the durability the volume/PVC then
// carries across a recreate.
func TestStatePersistAllowsReservedSeed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newStateStore(dir)
	if _, _, err := s.put(reservedSeed, cookieState("k", "v"), true, ""); err != nil {
		t.Fatalf("reserved seed must persist to disk: %v", err)
	}
	if _, ok := newStateStore(dir).get(reservedSeed); !ok {
		t.Fatal("reserved seed snapshot must survive a store reload (disk persistence)")
	}
}

// TestExtractSeedStateCarriesForwardClosedOrigin proves the non-invasive extract's
// carry-forward contract at the daemon seam: when a prior-known origin has no open
// tab this pass (reported failed), its last-known localStorage survives while
// cookies still refresh - a checkpoint that no longer navigates must not clear an
// origin just because its tab was closed.
func TestExtractSeedStateCarriesForwardClosedOrigin(t *testing.T) {
	t.Parallel()
	prior := &cdp.StorageState{
		Cookies: []cdp.Cookie{{Name: "sid", Value: "old", Domain: ".example.com", Path: "/", Expires: -1}},
		Origins: []cdp.Origin{{Origin: "https://example.com", LocalStorage: []cdp.LocalStorageItem{{Name: "tok", Value: "keep"}}}},
	}
	// The extract refreshes cookies but returns no localStorage for example.com and
	// marks it failed: its tab is closed, so localStorage was not readable in place.
	ops := stateOps{
		extract: func(_ context.Context, _ string, origins []string) (*cdp.StorageState, []string, error) {
			if !slices.Contains(origins, "https://example.com") {
				t.Errorf("prior origin must be requested for carry-forward, got %v", origins)
			}
			return &cdp.StorageState{
				Cookies: []cdp.Cookie{{Name: "sid", Value: "new", Domain: ".example.com", Path: "/", Expires: -1}},
			}, []string{"https://example.com"}, nil
		},
		inject: func(context.Context, string, *cdp.StorageState) error { return nil },
	}
	pool := newStatePool(t, serveConfig{}, &fakeStateOps{})
	pool.state = ops

	st, ok := pool.extractSeedState(context.Background(), "http://127.0.0.1:1", prior)
	if !ok {
		t.Fatal("extract should succeed")
	}
	if len(st.Cookies) != 1 || st.Cookies[0].Value != "new" {
		t.Fatalf("cookies must refresh: %+v", st.Cookies)
	}
	if len(st.Origins) != 1 || st.Origins[0].Origin != "https://example.com" ||
		len(st.Origins[0].LocalStorage) != 1 || st.Origins[0].LocalStorage[0].Value != "keep" {
		t.Fatalf("closed origin's localStorage must carry forward: %+v", st.Origins)
	}
}

// TestDefaultFingerprintSeedStable proves the reserved default seed's fingerprint
// is stable across a "recreate" (a new pool on the same durable dataDir) when the
// profile is persistent - so a login kept across recreate is not paired with a
// rotating device fingerprint (a returning-session correlation signal).
func TestDefaultFingerprintSeedStable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := &chromePool{dataDir: dir, keepProfile: true}
	first := p.defaultFingerprintSeed()
	if !validSeed(first) {
		t.Fatalf("default seed %q must be a valid fingerprint seed", first)
	}
	if got := p.defaultFingerprintSeed(); got != first {
		t.Fatalf("seed changed within a session: %q -> %q", first, got)
	}
	// A fresh pool on the same dataDir is what a container/pod recreate looks like
	// to the daemon; the persisted seed must be read back, not regenerated.
	if got := (&chromePool{dataDir: dir, keepProfile: true}).defaultFingerprintSeed(); got != first {
		t.Fatalf("seed not persisted across recreate: %q -> %q", first, got)
	}
}

// TestDefaultFingerprintSeedEphemeralNotPersisted proves a non-durable run keeps
// the fingerprint random per launch and writes nothing to disk.
func TestDefaultFingerprintSeedEphemeralNotPersisted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := &chromePool{dataDir: dir, ephemeral: true, keepProfile: true}
	if !validSeed(p.defaultFingerprintSeed()) {
		t.Fatal("ephemeral default seed must still be a valid fingerprint seed")
	}
	if _, err := os.Stat(filepath.Join(dir, reservedSeed+".seed")); !os.IsNotExist(err) {
		t.Fatal("an ephemeral run must not persist a default-seed file")
	}
}
