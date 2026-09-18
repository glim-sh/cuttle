package serve

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeClockTable is a lease table on a clock the test moves by hand.
func fakeClockTable() (*leaseTable, *time.Time) {
	now := time.Unix(1_700_000_000, 0)
	t := newLeaseTable()
	t.now = func() time.Time { return now }
	return t, &now
}

func TestLeaseTable(t *testing.T) {
	t.Parallel()
	const seed = "s"
	type step struct {
		advance time.Duration
		// op is "acquire", "renew" (with the token of holder), "release", "force".
		op, owner, holder string
		wantErr           error
		wantOwner         string // the lease returned: the grant, or the holder refusing
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{name: "a free slot is granted", steps: []step{
			{op: "acquire", owner: "a", wantOwner: "a"},
		}},
		{name: "a live lease refuses another owner and names the holder", steps: []step{
			{op: "acquire", owner: "a", wantOwner: "a"},
			{advance: 30 * time.Second, op: "acquire", owner: "b", wantErr: errLeaseHeld, wantOwner: "a"},
		}},
		{name: "renew with the token extends past the original ttl", steps: []step{
			{op: "acquire", owner: "a", wantOwner: "a"},
			{advance: 100 * time.Second, op: "renew", owner: "a", holder: "a", wantOwner: "a"},
			{advance: 100 * time.Second, op: "acquire", owner: "b", wantErr: errLeaseHeld, wantOwner: "a"},
		}},
		{name: "an expired lease is granted to the next owner", steps: []step{
			{op: "acquire", owner: "a", wantOwner: "a"},
			{advance: leaseTTL, op: "acquire", owner: "b", wantOwner: "b"},
		}},
		{name: "an expired lease nobody took is renewed by its holder", steps: []step{
			{op: "acquire", owner: "a", wantOwner: "a"},
			{advance: 2 * leaseTTL, op: "renew", owner: "a", holder: "a", wantOwner: "a"},
		}},
		{name: "renew after another owner took the expired slot is refused", steps: []step{
			{op: "acquire", owner: "a", wantOwner: "a"},
			{advance: leaseTTL, op: "acquire", owner: "b", wantOwner: "b"},
			{op: "renew", owner: "a", holder: "a", wantErr: errLeaseHeld, wantOwner: "b"},
		}},
		{name: "a forced release makes the old holder's renew fail naming the taker", steps: []step{
			{op: "acquire", owner: "a", wantOwner: "a"},
			{op: "force", owner: "pw"},
			{op: "renew", owner: "a", holder: "a", wantErr: errLeaseLost, wantOwner: "pw"},
			{op: "acquire", owner: "b", wantOwner: "b"},
		}},
		{name: "steal: force then acquire, and the old holder is refused", steps: []step{
			{op: "acquire", owner: "a", wantOwner: "a"},
			{op: "force", owner: "b"},
			{op: "acquire", owner: "b", wantOwner: "b"},
			{op: "renew", owner: "a", holder: "a", wantErr: errLeaseHeld, wantOwner: "b"},
		}},
		{name: "release is idempotent and frees the slot", steps: []step{
			{op: "acquire", owner: "a", wantOwner: "a"},
			{op: "release", holder: "a"},
			{op: "release", holder: "a"},
			{op: "acquire", owner: "b", wantOwner: "b"},
		}},
		{name: "a stale token cannot release the current holder", steps: []step{
			{op: "acquire", owner: "a", wantOwner: "a"},
			{advance: leaseTTL, op: "acquire", owner: "b", wantOwner: "b"},
			{op: "release", holder: "a"},
			{op: "acquire", owner: "c", wantErr: errLeaseHeld, wantOwner: "b"},
		}},
		{name: "an owner label is required", steps: []step{
			{op: "acquire", owner: "", wantErr: errLeaseNoOwner},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			table, now := fakeClockTable()
			tokens := map[string]string{}
			for i, s := range tt.steps {
				*now = now.Add(s.advance)
				var got lease
				var err error
				switch s.op {
				case "acquire":
					got, err = table.acquire(seed, s.owner, "")
					if err == nil {
						tokens[s.owner] = got.token
					}
				case "renew":
					got, err = table.acquire(seed, s.owner, tokens[s.holder])
				case "release":
					table.release(seed, tokens[s.holder])
					continue
				case "force":
					table.forceRelease(seed, s.owner)
					continue
				}
				if !errors.Is(err, s.wantErr) {
					t.Fatalf("step %d (%s %s): err=%v want %v", i, s.op, s.owner, err, s.wantErr)
				}
				if got.owner != s.wantOwner {
					t.Fatalf("step %d (%s %s): owner=%q want %q", i, s.op, s.owner, got.owner, s.wantOwner)
				}
			}
		})
	}
}

func TestLeaseTokensAreUniqueAndStableAcrossRenew(t *testing.T) {
	t.Parallel()
	table, _ := fakeClockTable()
	a, _ := table.acquire("s1", "a", "")
	b, _ := table.acquire("s2", "b", "")
	if len(a.token) != 32 || a.token == b.token {
		t.Fatalf("tokens should be distinct 16-byte hex: %q %q", a.token, b.token)
	}
	renewed, err := table.acquire("s1", "a", a.token)
	if err != nil || renewed.token != a.token || !renewed.acquired.Equal(a.acquired) {
		t.Fatalf("renew should keep token and acquired time: %+v err=%v", renewed, err)
	}
}

// leaseMux is a session-mode multiplexer on a fake clock; the lease routes need
// no running browser.
func leaseMux(t *testing.T) (http.Handler, *time.Time) {
	t.Helper()
	pool := newTestPool(t, serveConfig{mode: modeSession}, (&fakeLauncher{port: 5100}).toLauncher())
	table, now := fakeClockTable()
	pool.leases = table
	m := &multiplexer{pool: pool, port: 9222}
	return m.routes(), now
}

func leaseDo(t *testing.T, h http.Handler, method, target string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Host = "127.0.0.1:9222"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s %s: non-JSON body %q", method, target, rec.Body.String())
	}
	return rec.Code, body
}

func TestLeaseHTTPAcquireConflictRenewRelease(t *testing.T) {
	t.Parallel()
	h, now := leaseMux(t)

	code, body := leaseDo(t, h, http.MethodGet, "/lease")
	if code != http.StatusOK || body["held"] != false {
		t.Fatalf("fresh status: %d %v", code, body)
	}

	code, body = leaseDo(t, h, http.MethodPost, "/lease?owner=jev-browse+41%40host")
	token, _ := body["token"].(string)
	if code != http.StatusOK || token == "" || body["owner"] != "jev-browse 41@host" {
		t.Fatalf("acquire: %d %v", code, body)
	}

	*now = now.Add(42 * time.Second)
	code, body = leaseDo(t, h, http.MethodPost, "/lease?owner=other")
	if code != http.StatusConflict || body["owner"] != "jev-browse 41@host" || body["age_seconds"] != float64(42) {
		t.Fatalf("conflict should name holder and age: %d %v", code, body)
	}
	if _, leaked := body["token"]; leaked {
		t.Fatalf("a refusal must not carry the holder's token: %v", body)
	}

	code, body = leaseDo(t, h, http.MethodGet, "/lease")
	if code != http.StatusOK || body["held"] != true || body["owner"] != "jev-browse 41@host" {
		t.Fatalf("held status: %d %v", code, body)
	}
	if _, leaked := body["token"]; leaked {
		t.Fatalf("status must not carry the token: %v", body)
	}

	if code, body = leaseDo(t, h, http.MethodPost, "/lease?owner=jev-browse+41%40host&token="+token); code != http.StatusOK {
		t.Fatalf("renew: %d %v", code, body)
	}

	for range 2 {
		if code, body = leaseDo(t, h, http.MethodDelete, "/lease?token="+token); code != http.StatusOK {
			t.Fatalf("release: %d %v", code, body)
		}
	}
	if _, body = leaseDo(t, h, http.MethodGet, "/lease"); body["held"] != false {
		t.Fatalf("released lease still held: %v", body)
	}
}

func TestLeaseHTTPForceTakeover(t *testing.T) {
	t.Parallel()
	h, _ := leaseMux(t)
	_, body := leaseDo(t, h, http.MethodPost, "/lease?owner=a")
	token, _ := body["token"].(string)

	if code, _ := leaseDo(t, h, http.MethodDelete, "/lease?force=true&owner=cuttle+pw"); code != http.StatusOK {
		t.Fatalf("force release: %d", code)
	}
	if _, body = leaseDo(t, h, http.MethodGet, "/lease"); body["held"] != false {
		t.Fatalf("forced lease still held: %v", body)
	}
	code, body := leaseDo(t, h, http.MethodPost, "/lease?owner=a&token="+token)
	if code != http.StatusConflict || body["owner"] != "cuttle pw" {
		t.Fatalf("evicted renew should 409 naming the taker: %d %v", code, body)
	}
}

func TestLeaseHTTPRefusals(t *testing.T) {
	t.Parallel()
	h, _ := leaseMux(t)
	if code, _ := leaseDo(t, h, http.MethodPost, "/lease"); code != http.StatusBadRequest {
		t.Fatalf("missing owner: %d", code)
	}
	if code, body := leaseDo(t, h, http.MethodPost, "/lease?owner="+strings.Repeat("x", leaseOwnerMax+1)); code != http.StatusBadRequest {
		t.Fatalf("oversized owner: %d %v", code, body)
	}
	if code, _ := leaseDo(t, h, http.MethodGet, "/lease?fingerprint=s1"); code != http.StatusBadRequest {
		t.Fatalf("session mode should refuse a seed: %d", code)
	}

	r := httptest.NewRequest(http.MethodPost, "/lease?owner=a", nil)
	r.Host = "attacker.example:9222"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback Host should 403: %d", rec.Code)
	}
}
