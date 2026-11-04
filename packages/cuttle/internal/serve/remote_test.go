package serve

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemotePrincipalAuthentication(t *testing.T) {
	p := &principalTable{entries: []remotePrincipal{{"agent", sha256.Sum256([]byte("test-bearer"))}}}
	handler := p.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(principalContextKey{}) != "agent" {
			t.Error("principal missing from context")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		name, header, query string
		want                int
	}{
		{"missing", "", "", 401},
		{"wrong", "Bearer wrong", "", 401},
		{"basic", "Basic test-bearer", "", 401},
		{"query", "", "?token=test-bearer", 401},
		{"whitespace", "Bearer test-bearer ", "", 401},
		{"valid", "Bearer test-bearer", "", 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp"+tc.query, nil)
			req.Header.Set("Authorization", tc.header)
			req.Header.Set("Cookie", "__Host-cuttle_view=test-bearer")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
			if tc.want == 401 && rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Error("missing challenge")
			}
		})
	}
}

func TestPrincipalReloadFailsClosed(t *testing.T) {
	file := filepath.Join(t.TempDir(), "principals")
	p := &principalTable{}
	valid := fmt.Sprintf("agent sha256:%x\n", sha256.Sum256([]byte("test-bearer")))
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{valid, true},
		{"", true},
		{"agent sha256:00", false},
		{valid + valid, false},
		{strings.ReplaceAll(valid, "agent", "bad:name"), false},
		{valid, true},
	} {
		if err := os.WriteFile(file, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		err := p.reload(file)
		if (err == nil) != tc.valid {
			t.Fatalf("reload validity: %v", err)
		}
		if err != nil && len(p.entries) != 0 {
			t.Fatal("invalid file retained credentials")
		}
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if p.reload(file) == nil || len(p.entries) != 0 {
		t.Fatal("missing file retained credentials")
	}
}

func TestRemoteConfigValidation(t *testing.T) {
	good := remoteConfig{":8080", "https://browser.example", "/principals", "http://127.0.0.1:6081"}
	for _, tc := range []struct {
		name   string
		change func(*remoteConfig)
		mode   serveMode
		valid  bool
	}{
		{"valid", func(*remoteConfig) {}, modeSession, true},
		{"pool", func(*remoteConfig) {}, modePool, false},
		{"http", func(c *remoteConfig) { c.publicURL = "http://browser.example" }, modeSession, false},
		{"path", func(c *remoteConfig) { c.publicURL += "/path" }, modeSession, false},
		{"credentials", func(c *remoteConfig) { c.publicURL = "https://user@browser.example" }, modeSession, false},
		{"viewer", func(c *remoteConfig) { c.viewerUpstream = "http://viewer.example" }, modeSession, false},
		{"missing file", func(c *remoteConfig) { c.principalsFile = "" }, modeSession, false},
		{"disabled", func(c *remoteConfig) { c.listen = "" }, modePool, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := good
			tc.change(&cfg)
			if (cfg.validate(tc.mode) == nil) != tc.valid {
				t.Fatal("unexpected validation result")
			}
		})
	}
}

func TestRemoteRouteBoundary(t *testing.T) {
	file := filepath.Join(t.TempDir(), "principals")
	if err := os.WriteFile(file, fmt.Appendf(nil, "agent sha256:%x\n", sha256.Sum256([]byte("test-bearer"))), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := newTestPool(t, serveConfig{mode: modeSession}, (&fakeLauncher{port: 5100}).toLauncher())
	m := &multiplexer{pool: pool}
	server, _, err := m.remoteServer(remoteConfig{principalsFile: file}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/healthz", 200}, {"/readyz", 200}, {"/mcp", 401}, {"/files/handle", 401}, {"/json/version", 404}, {"/secret/NAME", 404}, {"/", 404},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if code, _ := probe(t, server.Handler, tc.path); code != tc.want {
				t.Fatalf("status %d, want %d", code, tc.want)
			}
		})
	}
}

func TestPrincipalWatcherCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	(&principalTable{}).watch(ctx, filepath.Join(t.TempDir(), "missing"))
}
