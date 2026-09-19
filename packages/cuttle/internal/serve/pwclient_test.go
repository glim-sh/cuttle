package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// pwMux is leaseMux with a pw client that can never start (its workdir does not
// exist), so every verb that passes the gate answers the fallback.
func pwMux(t *testing.T) (http.Handler, *multiplexer) {
	t.Helper()
	m := &multiplexer{pool: newTestPool(t, serveConfig{mode: modeSession}, (&fakeLauncher{port: 5100}).toLauncher()), port: 9222}
	m.pw = &pwClient{ctx: context.Background(), workdir: filepath.Join(t.TempDir(), "missing")}
	return m.routes(), m
}

func pwDo(t *testing.T, h http.Handler, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/pw", strings.NewReader(body))
	r.Host = "127.0.0.1:9222"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON: %q", rec.Body.String())
	}
	return rec.Code, out
}

func TestPWGatesOnTheLease(t *testing.T) {
	t.Parallel()
	h, m := pwMux(t)
	if _, err := m.pool.leases.acquire(reservedSeed, "jev-browse 41@host", ""); err != nil {
		t.Fatal(err)
	}

	code, body := pwDo(t, h, `{"args":["click","e1"],"drive":true,"owner":"cuttle pw"}`)
	if code != http.StatusConflict || body["owner"] != "jev-browse 41@host" || body["held"] != true {
		t.Fatalf("a driving verb under a foreign lease should 409 naming the holder: %d %v", code, body)
	}
	if _, leaked := body["token"]; leaked {
		t.Fatalf("a refusal must not carry the holder's token: %v", body)
	}

	// A read verb passes the lease; the client cannot start, so it falls back.
	if code, body = pwDo(t, h, `{"args":["snapshot"]}`); code != http.StatusOK || body["fallback"] != true {
		t.Fatalf("a read verb should pass the lease and fall back: %d %v", code, body)
	}

	code, body = pwDo(t, h, `{"args":["click","e1"],"drive":true,"takeover":true,"owner":"cuttle pw"}`)
	if code != http.StatusOK || body["fallback"] != true {
		t.Fatalf("takeover should pass the gate: %d %v", code, body)
	}
	if cur, held := m.pool.leases.status(reservedSeed); held || cur.owner != "cuttle pw" {
		t.Fatalf("takeover should free the lease naming the taker: %+v held=%v", cur, held)
	}
}

func TestPWRefusesABadBody(t *testing.T) {
	t.Parallel()
	h, _ := pwMux(t)
	for _, body := range []string{`{}`, `{"args":[]}`, `not json`} {
		if code, _ := pwDo(t, h, body); code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", body, code)
		}
	}
	if code, _ := pwDo(t, h, `{"args":["click"],"drive":true,"takeover":true}`); code != http.StatusBadRequest {
		t.Errorf("a takeover without an owner: got %d, want 400", code)
	}
}
