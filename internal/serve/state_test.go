package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
// route pattern).
func stateReq(method, seed, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/profile/"+seed+"/state", nil)
	} else {
		r = httptest.NewRequest(method, "/profile/"+seed+"/state", strings.NewReader(body))
	}
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
	// The live extract refreshes the snapshot store.
	if e, ok := pool.store.get("s1"); !ok || len(e.State.Cookies) != 1 {
		t.Fatalf("live GET did not refresh snapshot: %+v ok=%v", e, ok)
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
