package serve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestParseConnectionParams(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		query string
		want  connectRequest
	}{
		{
			name:  "all recognized params",
			query: "fingerprint=seed1&timezone=America/New_York&locale=en-US&proxy=http://p:1&geoip=true",
			want: connectRequest{
				seed: "seed1", timezone: "America/New_York", locale: "en-US",
				proxy: "http://p:1", geoip: true,
			},
		},
		{
			name:  "generic param becomes fingerprint flag",
			query: "fingerprint=s&platform=windows&webrtc-ip=auto",
			want: connectRequest{
				seed:      "s",
				extraArgs: []string{"--fingerprint-platform=windows", "--fingerprint-webrtc-ip=auto"},
			},
		},
		{
			name:  "blank values dropped",
			query: "fingerprint=&timezone=&locale=en-GB",
			want:  connectRequest{locale: "en-GB"},
		},
		{
			name:  "geoip falsey",
			query: "fingerprint=s&geoip=no",
			want:  connectRequest{seed: "s", geoip: false},
		},
		{
			name:  "first value wins on duplicate key",
			query: "fingerprint=first&fingerprint=second",
			want:  connectRequest{seed: "first"},
		},
		{
			name:  "url-encoded value",
			query: "proxy=http%3A%2F%2Fu%3Ap%40h%3A8080",
			want:  connectRequest{proxy: "http://u:p@h:8080"},
		},
		{
			name:  "empty query",
			query: "",
			want:  connectRequest{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseConnectionParams(tc.query)
			if got.seed != tc.want.seed || got.timezone != tc.want.timezone ||
				got.locale != tc.want.locale || got.proxy != tc.want.proxy || got.geoip != tc.want.geoip {
				t.Errorf("scalar mismatch: got %+v want %+v", got, tc.want)
			}
			if !slices.Equal(got.extraArgs, tc.want.extraArgs) {
				t.Errorf("extraArgs got %v want %v", got.extraArgs, tc.want.extraArgs)
			}
		})
	}
}

func TestOriginIsAllowed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		origin  string
		present bool
		host    string
		scheme  string
		want    bool
	}{
		{name: "absent origin allowed (non-browser client)", present: false, host: "127.0.0.1:9222", scheme: "http", want: true},
		{name: "empty origin rejected", origin: "", present: true, host: "127.0.0.1:9222", scheme: "http", want: false},
		{name: "null origin rejected", origin: "null", present: true, host: "127.0.0.1:9222", scheme: "http", want: false},
		{name: "devtools trusted", origin: "devtools://devtools", present: true, host: "example:9222", scheme: "http", want: true},
		{name: "loopback same-origin allowed (port-forward)", origin: "http://127.0.0.1:9222", present: true, host: "127.0.0.1:9222", scheme: "http", want: true},
		{name: "localhost same-origin allowed", origin: "http://localhost:9222", present: true, host: "localhost:9222", scheme: "http", want: true},
		{name: "port mismatch rejected", origin: "http://127.0.0.1:9223", present: true, host: "127.0.0.1:9222", scheme: "http", want: false},
		{name: "non-loopback host rejected", origin: "http://evil.example", present: true, host: "evil.example", scheme: "http", want: false},
		{name: "cross-origin against loopback rejected", origin: "http://evil.example", present: true, host: "127.0.0.1:9222", scheme: "http", want: false},
		{name: "origin with path rejected", origin: "http://127.0.0.1:9222/x", present: true, host: "127.0.0.1:9222", scheme: "http", want: false},
		{name: "origin with userinfo rejected", origin: "http://u@127.0.0.1:9222", present: true, host: "127.0.0.1:9222", scheme: "http", want: false},
		{name: "bad scheme rejected", origin: "ftp://127.0.0.1", present: true, host: "127.0.0.1", scheme: "http", want: false},
		{name: "default ports match (http 80)", origin: "http://localhost", present: true, host: "localhost:80", scheme: "http", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := originIsAllowed(tc.origin, tc.present, tc.host, tc.scheme); got != tc.want {
				t.Errorf("originIsAllowed(%q,%v,%q,%q)=%v want %v", tc.origin, tc.present, tc.host, tc.scheme, got, tc.want)
			}
		})
	}
}

// Chrome's /json endpoints that act on a tab are not proxied, and must say so.
func TestUnproxiedJSONEndpointsAre404(t *testing.T) {
	t.Parallel()
	cdp := newFakeCDP(t)
	pool := newTestPool(t, serveConfig{}, (&fakeLauncher{port: cdp.port}).toLauncher())
	front := httptest.NewServer((&multiplexer{pool: pool, port: 9222}).routes())
	t.Cleanup(front.Close)
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/json/close/PAGE9", http.StatusNotFound},
		{http.MethodGet, "/json/activate/PAGE9", http.StatusNotFound},
		{http.MethodPut, "/json/new", http.StatusNotFound},
		{http.MethodGet, "/json/", http.StatusOK},
		{http.MethodGet, "/json", http.StatusOK},
	} {
		// Pool mode, so the list needs a seed; the 404s must not care.
		req, err := http.NewRequestWithContext(t.Context(), tc.method, front.URL+tc.path+"?fingerprint=seedX", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s = %d %s, want %d", tc.method, tc.path, resp.StatusCode, body, tc.want)
		}
		if tc.want == http.StatusNotFound && !strings.Contains(string(body), "Target.closeTarget") {
			t.Errorf("%s %s: 404 does not name the CDP alternative: %s", tc.method, tc.path, body)
		}
	}
}

func TestIsLoopbackPeer(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]bool{
		"127.0.0.1:40000":           true,
		"127.8.9.10:1":              true,
		"[::1]:40000":               true,
		"[::ffff:127.0.0.1]:40000":  true,
		"192.168.215.1:40000":       false, // a docker -p mapping arrives from the bridge gateway
		"10.0.0.7:1":                false,
		"[::ffff:10.0.0.7]:1":       false,
		"[fe80::1%eth0]:1":          false,
		"0.0.0.0:1":                 false,
		"127.0.0.1":                 false, // no port: not a RemoteAddr net/http produces
		"localhost:40000":           false, // a name is never a peer address
		"":                          false,
		"@":                         false,
		"not an address":            false,
		"[::1]:40000/extra garbage": false,
	} {
		if got := isLoopbackPeer(addr); got != want {
			t.Errorf("isLoopbackPeer(%q) = %v, want %v", addr, got, want)
		}
	}
}

// A loopback Host and a missing Origin are what any client writes, so the
// routes behind rejectUntrustedLoopback must also see a loopback peer: a
// request from another container or pod is refused whatever its headers say,
// while a loopback peer still meets the Host and Origin checks.
func TestLoopbackOnlyRoutesRequireALoopbackPeer(t *testing.T) {
	t.Parallel()
	m, _ := downloadsPool(t, nil)
	h := m.routes()
	routes := []struct {
		family, method, target, body string
		allowed                      int // status past the guard
	}{
		{"secret", http.MethodGet, "/secret?fingerprint=s1", "", http.StatusOK},
		{"secret", http.MethodPut, "/secret/GH_PASS?fingerprint=s1", `{"value":"hunter2"}`, http.StatusOK},
		{"lease", http.MethodGet, "/lease?fingerprint=s1", "", http.StatusOK},
		{"lease", http.MethodDelete, "/lease?fingerprint=s1&force=true&owner=t", "", http.StatusOK},
		{"snapshot", http.MethodGet, "/snapshot?fingerprint=s1&file=.playwright-cli/page-1.yml", "", http.StatusNotFound},
		{"state", http.MethodGet, "/profile/s2/state", "", http.StatusNotFound},
		{"downloads", http.MethodGet, "/downloads?fingerprint=s1", "", http.StatusOK},
	}
	peers := []struct {
		addr     string
		loopback bool
	}{
		{"127.0.0.1:40000", true},
		{"[::1]:40000", true},
		{"[::ffff:127.0.0.1]:40000", true},
		{"192.168.215.1:40000", false},
		{"10.0.0.7:40000", false},
		{"[::ffff:10.0.0.7]:40000", false},
	}
	hosts := []struct {
		host     string
		loopback bool
	}{
		{"127.0.0.1:9222", true},
		{"localhost:9222", true},
		{"attacker.example:9222", false},
	}
	for _, rt := range routes {
		for _, peer := range peers {
			for _, host := range hosts {
				for _, origin := range []string{"", "same", "http://attacker.example"} {
					name := rt.family + " " + rt.method + " peer=" + peer.addr + " host=" + host.host + " origin=" + origin
					var body io.Reader
					if rt.body != "" {
						body = strings.NewReader(rt.body)
					}
					req := httptest.NewRequest(rt.method, rt.target, body)
					req.RemoteAddr = peer.addr
					req.Host = host.host
					switch origin {
					case "":
					case "same":
						req.Header.Set("Origin", "http://"+host.host)
					default:
						req.Header.Set("Origin", origin)
					}
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					admit := peer.loopback && host.loopback && origin != "http://attacker.example"
					switch {
					case admit && rec.Code != rt.allowed:
						t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body.String(), rt.allowed)
					case !admit && rec.Code != http.StatusForbidden:
						t.Errorf("%s: %d %s, want 403", name, rec.Code, rec.Body.String())
					case !peer.loopback && !strings.Contains(rec.Body.String(), "loopback peer"):
						t.Errorf("%s: a non-loopback peer must be refused as such, got %q", name, rec.Body.String())
					}
				}
			}
		}
	}
}

func TestSeedDiscoveryRoutes(t *testing.T) {
	t.Parallel()
	cdp := newFakeCDP(t)
	pool := newTestPool(t, serveConfig{}, (&fakeLauncher{port: cdp.port}).toLauncher())
	h := (&multiplexer{pool: pool, port: 9222}).routes()
	for _, suffix := range []string{"/json/version", "/json/version/", "/json/list", "/json/list/", "/json", "/json/"} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost:9222/fingerprint/alpha"+suffix+"?fingerprint=other", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ws://localhost:9222/fingerprint/alpha/devtools/") {
			t.Fatalf("%s: %d %s", suffix, rec.Code, rec.Body.String())
		}
	}
	if pool.runningInstance("alpha") == nil || pool.runningInstance("other") != nil {
		t.Fatal("the path seed must override the query seed")
	}
	for _, suffix := range []string{"/json/close/PAGE9", "/json/version/extra", "/json/list/extra"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fingerprint/alpha"+suffix, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d, want 404", suffix, rec.Code)
		}
	}
}

func TestSeedDiscoveryThenSocket(t *testing.T) {
	t.Parallel()
	for _, mode := range []serveMode{modePool, modeSession} {
		t.Run(string(mode), func(t *testing.T) {
			cdp := newFakeCDP(t)
			pool := newTestPool(t, serveConfig{mode: mode}, (&fakeLauncher{port: cdp.port}).toLauncher())
			seed, key := "alpha", "alpha"
			if mode == modeSession {
				seed, key = "main", reservedSeed
			}
			front := httptest.NewServer((&multiplexer{pool: pool, port: 9222}).routes())
			t.Cleanup(front.Close)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, front.URL+"/fingerprint/"+seed+"/json/version", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var version struct {
				Socket string `json:"webSocketDebuggerUrl"`
			}
			err = json.NewDecoder(resp.Body).Decode(&version)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("discovery: %d %v", resp.StatusCode, err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			client, _, err := websocket.Dial(ctx, version.Socket, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.CloseNow() }()
			if _, msg, err := client.Read(ctx); err != nil || !strings.Contains(string(msg), "CDP.greeting") {
				t.Fatalf("greeting: %q %v", msg, err)
			}
			pool.mu.Lock()
			defer pool.mu.Unlock()
			if pool.conns[key] != 1 || (key != seed && pool.conns[seed] != 0) {
				t.Fatalf("connections: %v, want one under %s", pool.conns, key)
			}
		})
	}
}

func TestSeedRoutesRefuseInternalBrowserRequests(t *testing.T) {
	t.Parallel()
	cdp := newFakeCDP(t)
	pool := newTestPool(t, serveConfig{}, (&fakeLauncher{port: cdp.port}).toLauncher())
	h := refuseBrowserRequests((&multiplexer{pool: pool, port: 9222}).routes())
	for _, suffix := range []string{"/json/version", "/json/list", "/json", "/devtools/browser/GUID123"} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost:9222/fingerprint/alpha"+suffix, nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: %d, want 403", suffix, rec.Code)
		}
	}
	if pool.runningInstance("alpha") != nil {
		t.Fatal("browser requests launched a seed")
	}
}
