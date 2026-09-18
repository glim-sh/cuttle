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

	"github.com/coder/websocket"
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

	wsURL, err := browserWSForSeed(ctx, cuttleURL, "viewer-"+runID)
	if err != nil {
		return []checkResult{viewerFailure("viewer-browser", "resolve CDP websocket: %v", err)}
	}
	dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		return []checkResult{viewerFailure("viewer-browser", "dial CDP websocket: %v", err)}
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(-1)
	client := &cdpClient{conn: conn}

	return []checkResult{
		viewerBrowserCheck(ctx, client, "viewer-root-websocket", rootURL+"/", false),
		viewerBrowserCheck(ctx, client, "viewer-prefixed-websocket", prefixURL+viewerPrefix, true),
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

func viewerBrowserCheck(ctx context.Context, client *cdpClient, name, pageURL string, ignoreCertificateErrors bool) checkResult {
	ctx, cancel := context.WithTimeout(ctx, viewerConnectTimeout+30*time.Second)
	defer cancel()

	targetID, sessionID, err := client.openTab(ctx)
	if targetID != "" {
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = client.send(closeCtx, "Target.closeTarget", map[string]any{cdpTargetID: targetID}, "")
		}()
	}
	if err != nil {
		return viewerFailure(name, "open viewer tab: %v", err)
	}

	if ignoreCertificateErrors {
		if _, err = client.send(ctx, "Security.setIgnoreCertificateErrors", map[string]any{"ignore": true}, sessionID); err != nil {
			return viewerFailure(name, "allow local test certificate: %v", err)
		}
	}
	if _, err = client.send(ctx, "Page.navigate", map[string]any{cdpURL: pageURL}, sessionID); err != nil {
		return viewerFailure(name, "navigate viewer tab: %v", err)
	}

	state, err := waitForViewerConnection(ctx, client, sessionID)
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

func waitForViewerConnection(ctx context.Context, client *cdpClient, sessionID string) (viewerState, error) {
	deadline := time.Now().Add(viewerConnectTimeout)
	var last viewerState
	for time.Now().Before(deadline) {
		evaluated, err := client.send(ctx, "Runtime.evaluate", map[string]any{
			"expression": `JSON.stringify({
              connection: window.cuttle?.rfb?._rfbConnectionState || "",
              socketURL: window.cuttle?.rfb?._url || "",
              status: document.getElementById("status")?.textContent || ""
            })`,
			"returnByValue": true,
		}, sessionID)
		if err != nil {
			return viewerState{}, fmt.Errorf("evaluate viewer state: %w", err)
		}
		var result struct {
			Result struct {
				Value string `json:"value"`
			} `json:"result"`
		}
		if err = json.Unmarshal(evaluated, &result); err != nil {
			return viewerState{}, fmt.Errorf("decode viewer evaluation: %w", err)
		}
		var state viewerState
		if result.Result.Value != "" {
			if err = json.Unmarshal([]byte(result.Result.Value), &state); err != nil {
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
