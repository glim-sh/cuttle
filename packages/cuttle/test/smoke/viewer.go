package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const (
	viewerPrefix         = "/viewer/"
	viewerConnectTimeout = 30 * time.Second
)

var errViewerTimeout = errors.New("RFB did not connect")

type viewerState struct {
	Connection string `json:"connection"`
	SocketURL  string `json:"socketURL"`
	Status     string `json:"status"`
}

// viewerChecks loads the shipped viewer page in a real Chrome through a root
// proxy and a prefixing one: page and asset paths are stripped of the prefix,
// websocket paths are forwarded intact. RFB can only report "connected" after
// KasmVNC accepted the upgrade, which it does only with Origin and the binary
// subprotocol, so the socket URL plus that state cover the whole handshake.
func viewerChecks(ctx context.Context, cuttleURL, viewerURL, browserHost, runID string) []checkResult {
	upstream, err := url.Parse(viewerURL)
	if err != nil {
		return []checkResult{viewerFailure("viewer-proxy", "parse CUTTLE_VIEWER_URL: %v", err)}
	}
	rootProxy, rootURL, err := startViewerProxy(ctx, upstream, browserHost, false)
	if err != nil {
		return []checkResult{viewerFailure("viewer-proxy", "start HTTP proxy: %v", err)}
	}
	defer rootProxy.Close()
	prefixProxy, prefixURL, err := startViewerProxy(ctx, upstream, browserHost, true)
	if err != nil {
		return []checkResult{viewerFailure("viewer-proxy", "start HTTPS proxy: %v", err)}
	}
	defer prefixProxy.Close()

	client, err := dialSeed(ctx, cuttleURL, "viewer-"+runID)
	if err != nil {
		return []checkResult{viewerFailure("viewer-browser", "%v", err)}
	}
	defer func() { _ = client.conn.CloseNow() }()

	// Browser-wide, so it is in place before the HTTPS tab's first request; the
	// seed is this run's own throwaway.
	certCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err = client.send(certCtx, "Security.setIgnoreCertificateErrors", map[string]any{"ignore": true}, ""); err != nil {
		return []checkResult{viewerFailure("viewer-browser", "allow local test certificate: %v", err)}
	}

	return []checkResult{
		viewerBrowserCheck(ctx, client, "viewer-root-websocket", rootURL+"/"),
		viewerBrowserCheck(ctx, client, "viewer-prefixed-websocket", prefixURL+viewerPrefix),
		viewerLockKeysCheck(ctx, client, rootURL+"/"),
	}
}

// startViewerProxy returns the proxy and its base URL as the containerized
// browser reaches it: browserHost stands in for the listener's own address.
func startViewerProxy(ctx context.Context, upstream *url.URL, browserHost string, secure bool) (*httptest.Server, string, error) {
	// The browser under test runs in Docker and must reach this host listener.
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", "0.0.0.0:0") // #nosec G102 -- test proxy for the local container
	if err != nil {
		return nil, "", fmt.Errorf("listen: %w", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(upstream) // #nosec G704 -- local smoke-test upstream
	stripped := http.StripPrefix(strings.TrimSuffix(viewerPrefix, "/"), proxy)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.EqualFold(req.Header.Get("Upgrade"), "websocket") || !strings.HasPrefix(req.URL.Path, viewerPrefix) {
			proxy.ServeHTTP(w, req)
			return
		}
		stripped.ServeHTTP(w, req)
	}))
	server.Listener = listener
	server.Config.ReadHeaderTimeout = 5 * time.Second
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	if secure {
		server.StartTLS()
	} else {
		server.Start()
	}

	base, err := url.Parse(server.URL)
	if err != nil {
		server.Close()
		return nil, "", fmt.Errorf("parse proxy URL: %w", err)
	}
	base.Host = net.JoinHostPort(browserHost, base.Port())
	return server, base.String(), nil
}

func viewerBrowserCheck(ctx context.Context, client *cdpClient, name, pageURL string) checkResult {
	ctx, cancel := context.WithTimeout(ctx, viewerConnectTimeout+30*time.Second)
	defer cancel()

	state, _, closeTab, err := openViewer(ctx, client, pageURL)
	defer closeTab()
	if err != nil {
		return viewerFailure(name, "%v", err)
	}
	// http -> ws, https -> wss, resolved in the page's own directory.
	wantSocketURL := "ws" + strings.TrimPrefix(pageURL, "http") + "websockify"
	if state.SocketURL != wantSocketURL {
		return viewerFailure(name, "socket URL=%q, want %q", state.SocketURL, wantSocketURL)
	}
	return checkResult{name, statusPass, "RFB=" + state.Connection + " socket=" + state.SocketURL}
}

// openViewer opens the viewer page in a new tab and waits for RFB to connect.
// The returned close func is always safe to call.
func openViewer(ctx context.Context, client *cdpClient, pageURL string) (viewerState, string, func(), error) {
	// Opened at the page itself, not navigated there: a navigation away from
	// about:blank swaps renderer processes, and Chrome can drop a command sent
	// during that swap without ever answering it.
	targetID, sessionID, err := client.openTab(ctx, pageURL)
	closeTab := func() {
		if targetID == "" {
			return
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = client.send(closeCtx, "Target.closeTarget", map[string]any{cdpTargetID: targetID}, "")
	}
	if err != nil {
		return viewerState{}, "", closeTab, fmt.Errorf("open viewer tab: %w", err)
	}
	state, err := waitForViewerConnection(ctx, client, sessionID)
	return state, sessionID, closeTab, err
}

const (
	lockCheckKeys = 10
	xkCapsLock    = 0xffe5
	xkNumLock     = 0xff7f
	xkX           = 0x78
)

// The viewer must not inject lock-key taps into the remote session while a
// person types: a CapsLock and NumLock tap before every key is a behavioural
// fingerprint any page can read. The recorder sits on rfb.sendKey, the one
// path every keysym takes to the socket, and remembers the server's lock state
// (LED pseudo-encoding) before typing: a mismatch with the local state earns
// noVNC exactly one legitimate resync tap, anything more is injected.
const lockRecorderJS = `(() => {
  const r = window.cuttle.rfb;
  window.__lockCheck = { sent: [], caps: r._remoteCapsLock, num: r._remoteNumLock };
  const send = r.sendKey.bind(r);
  r.sendKey = (keysym, code, down) => { if (down) window.__lockCheck.sent.push(keysym); send(keysym, code, down); };
  r.focus();
  return "";
})()`

// Waits (bounded) for the server to re-report its lock state, which noVNC
// clears after each resync tap, so a regression taps on every key instead of
// only the first one before the LED echo lands.
const lockStateKnownJS = `new Promise((done) => {
  const t0 = performance.now();
  (function poll() {
    if (window.cuttle.rfb._remoteCapsLock !== null || performance.now() - t0 > %d) return done(String(window.cuttle.rfb._remoteCapsLock !== null));
    setTimeout(poll, 10);
  })();
})`

const lockReportJS = `JSON.stringify((() => {
  const c = window.__lockCheck, count = (k) => c.sent.filter((s) => s === k).length;
  return { typed: count(%d), caps: count(%d), num: count(%d), remoteCaps: c.caps, remoteNum: c.num };
})())`

type lockReport struct {
	Typed      int   `json:"typed"`
	Caps       int   `json:"caps"`
	Num        int   `json:"num"`
	RemoteCaps *bool `json:"remoteCaps"`
	RemoteNum  *bool `json:"remoteNum"`
}

// viewerLockKeysCheck types into the viewer with real (CDP) key events and
// asserts that only the typed keys - plus at most one resync per lock whose
// remote state differs from the local one - reach the RFB socket.
func viewerLockKeysCheck(ctx context.Context, client *cdpClient, pageURL string) checkResult {
	const name = "viewer-lock-keys"
	ctx, cancel := context.WithTimeout(ctx, viewerConnectTimeout+60*time.Second)
	defer cancel()

	_, sessionID, closeTab, err := openViewer(ctx, client, pageURL)
	defer closeTab()
	if err != nil {
		return viewerFailure(name, "%v", err)
	}
	known, err := client.evaluate(ctx, sessionID, fmt.Sprintf(lockStateKnownJS, 10000))
	if err != nil {
		return viewerFailure(name, "wait for server lock state: %v", err)
	}
	if known != "true" {
		return viewerFailure(name, "server never reported its lock state, so lock sync cannot be exercised")
	}
	if _, err = client.send(ctx, "Page.bringToFront", nil, sessionID); err != nil {
		return viewerFailure(name, "bring viewer to front: %v", err)
	}
	if _, err = client.evaluate(ctx, sessionID, lockRecorderJS); err != nil {
		return viewerFailure(name, "install key recorder: %v", err)
	}
	for range lockCheckKeys {
		for _, typ := range []string{"keyDown", "keyUp"} {
			params := map[string]any{"type": typ, "key": "x", "code": "KeyX", "windowsVirtualKeyCode": 88}
			if typ == "keyDown" {
				params["text"] = "x"
			}
			if _, err = client.send(ctx, "Input.dispatchKeyEvent", params, sessionID); err != nil {
				return viewerFailure(name, "dispatch %s: %v", typ, err)
			}
		}
		if _, err = client.evaluate(ctx, sessionID, fmt.Sprintf(lockStateKnownJS, 1000)); err != nil {
			return viewerFailure(name, "wait for server lock state: %v", err)
		}
	}
	raw, err := client.evaluate(ctx, sessionID, fmt.Sprintf(lockReportJS, xkX, xkCapsLock, xkNumLock))
	if err != nil {
		return viewerFailure(name, "read key recorder: %v", err)
	}
	var got lockReport
	if err = json.Unmarshal([]byte(raw), &got); err != nil {
		return viewerFailure(name, "decode key recorder: %v", err)
	}
	// Local lock state from CDP key events is off; a remote lock left on earns one tap.
	wantCaps, wantNum := resyncTaps(got.RemoteCaps), resyncTaps(got.RemoteNum)
	detail := fmt.Sprintf("typed=%d CapsLock=%d (allowed %d) NumLock=%d (allowed %d)",
		got.Typed, got.Caps, wantCaps, got.Num, wantNum)
	if got.Typed != lockCheckKeys || got.Caps > wantCaps || got.Num > wantNum {
		return viewerFailure(name, "%s", detail)
	}
	return checkResult{name, statusPass, detail}
}

func resyncTaps(remoteOn *bool) int {
	if remoteOn != nil && *remoteOn {
		return 1
	}
	return 0
}

func waitForViewerConnection(ctx context.Context, client *cdpClient, sessionID string) (viewerState, error) {
	deadline := time.Now().Add(viewerConnectTimeout)
	var last viewerState
	for time.Now().Before(deadline) {
		value, err := client.evaluate(ctx, sessionID, `JSON.stringify({
          connection: window.cuttle?.rfb?._rfbConnectionState || "",
          socketURL: window.cuttle?.rfb?._url || "",
          status: document.getElementById("status")?.textContent || ""
        })`)
		if err != nil {
			return viewerState{}, fmt.Errorf("evaluate viewer state: %w", err)
		}
		var state viewerState
		if value != "" {
			if err = json.Unmarshal([]byte(value), &state); err != nil {
				return viewerState{}, fmt.Errorf("decode viewer state: %w", err)
			}
		}
		last = state
		if state.Connection == "connected" {
			return state, nil
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return viewerState{}, fmt.Errorf("waiting for RFB connection: %w", ctx.Err())
		}
	}
	return viewerState{}, fmt.Errorf("%w within %s (connection=%q status=%q socket=%q)",
		errViewerTimeout, viewerConnectTimeout, last.Connection, last.Status, last.SocketURL)
}

func viewerFailure(name, format string, args ...any) checkResult {
	return checkResult{name, statusFail, fmt.Sprintf(format, args...)}
}
