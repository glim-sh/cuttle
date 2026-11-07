package serve

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestOnionCertificateURL(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://example.onion/", true},
		{"https://sub.example.onion:443/", true},
		{"https://EXAMPLE.ONION/", true},
		{"wss://example.onion/", true},
		{"https://example.onion./", false},
		{"https://.onion/", false},
		{"https://onion/", false},
		{"https://example.onion.evil.test/", false},
		{"https://example.onion@evil.test/", false},
		{"https://evil.test/example.onion", false},
		{"https://evil.test/#example.onion", false},
		{"http://example.onion/", false},
		{"https://example%2eonion/", false},
		{"//example.onion/", false},
		{"garbage", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			if got := onionCertificateURL(tc.url); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOnionCertificateHandler(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	recorder, endpoint := startCDPBrowser(t, nil, func(cmd map[string]any) ([]map[string]any, []map[string]any) {
		if cmd[cdpMethod] != "Target.setAutoAttach" {
			return nil, nil
		}
		return []map[string]any{
			{cdpMethod: "Security.certificateError", cdpParams: map[string]any{"eventId": 1, "requestURL": "https://x.onion/"}},
			{cdpMethod: "Security.certificateError", cdpParams: map[string]any{"eventId": 2, "requestURL": "https://x.test/"}},
			{cdpMethod: "Target.attachedToTarget", cdpParams: map[string]any{"sessionId": "popup", "waitingForDebugger": false}},
		}, nil
	})
	ws, _, err := websocket.Dial(ctx, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &cdpConn{ws: ws}
	defer c.close()
	if err := enableOnionCertificates(ctx, c); err != nil {
		t.Fatal(err)
	}
	// A round trip drains both replies without a timing-dependent sleep.
	if _, err := c.call(ctx, "", "Browser.getVersion", nil); err != nil {
		t.Fatal(err)
	}
	got := recorder.received()
	if len(got) != 7 {
		t.Fatalf("commands: %v", got)
	}
	if got[5][cdpMethod] != "Runtime.runIfWaitingForDebugger" || got[5][cdpSessionID] != "popup" {
		t.Fatalf("popup not resumed: %v", got[5])
	}
	for i, action := range []string{"continue", "cancel"} {
		params := got[i+3][cdpParams].(map[string]any)
		if got[i+3][cdpMethod] != "Security.handleCertificateError" || params["action"] != action {
			t.Fatalf("command: %v", got[i+3])
		}
	}
}

func TestServeOnionCertificates(t *testing.T) {
	t.Setenv("CUTTLE_ALLOW_ONION_SELF_SIGNED", "")
	if cfg, _ := parseServeArgs(t, nil); cfg.allowOnionSelfSigned {
		t.Fatal("default enabled")
	}
	t.Setenv("CUTTLE_ALLOW_ONION_SELF_SIGNED", "1")
	if cfg, _ := parseServeArgs(t, nil); !cfg.allowOnionSelfSigned {
		t.Fatal("env ignored")
	}
	if cfg, _ := parseServeArgs(t, []string{"--allow-onion-self-signed=false"}); cfg.allowOnionSelfSigned {
		t.Fatal("flag must override env")
	}
}

func TestOnionCertificateSetupRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_, _, _ = ws.Read(ctx)
		_ = ws.Write(ctx, websocket.MessageText, []byte(`{"id":1,"error":{"message":"unsupported"}}`))
		_, _, _ = ws.Read(ctx)
	}))
	defer server.Close()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &cdpConn{ws: ws}
	defer c.close()
	if err := enableOnionCertificates(ctx, c); err == nil {
		t.Fatal("unsupported Security domain accepted")
	}
}

func TestOnionCertificateFailureStopsSeed(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	fl := &fakeLauncher{port: server.Listener.Addr().(*net.TCPAddr).Port}
	pool := newTestPool(t, serveConfig{allowOnionSelfSigned: true}, fl.toLauncher())
	_, err := pool.getOrLaunch(t.Context(), connectRequest{seed: "12345"})
	if err == nil {
		t.Fatal("seed exposed without certificate policy")
	}
	if len(fl.procs) != 1 || fl.procs[0].running() {
		t.Fatal("failed seed left running")
	}
	if len(pool.processes) != 0 {
		t.Fatal("failed seed published")
	}
}
