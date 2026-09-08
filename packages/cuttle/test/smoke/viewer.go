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
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const viewerPrefix = "/v/smoke/platform/"

type viewerUpgrade struct {
	path       string
	origin     string
	protocol   string
	statusCode int
}

type viewerTestProxy struct {
	server     *httptest.Server
	attempts   <-chan viewerUpgrade
	browserURL string
}

type viewerUpgradeKey struct{}

type viewerState struct {
	Connection string `json:"connection"`
	SocketURL  string `json:"socketURL"`
	Status     string `json:"status"`
}

// viewerChecks exercises the shipped viewer through the same asymmetric proxy
// shape used by a prefixed deployment: page and asset paths are stripped, while
// websocket paths are forwarded intact. A real Chrome page runs noVNC and must
// complete the RFB handshake at both the origin root and a path prefix.
func viewerChecks(ctx context.Context, cuttleURL, viewerURL, browserHost, runID string) []checkResult {
	results := []checkResult{incompleteViewerHandshake(ctx, viewerURL)}

	upstream, err := url.Parse(viewerURL)
	if err != nil {
		return append(results, viewerFailure("viewer-proxy", "parse CUTTLE_VIEWER_URL: %v", err))
	}
	directProxy, err := startViewerProxy(ctx, upstream, browserHost, false)
	if err != nil {
		return append(results, viewerFailure("viewer-proxy", "start HTTP proxy: %v", err))
	}
	defer directProxy.server.Close()
	secureProxy, err := startViewerProxy(ctx, upstream, browserHost, true)
	if err != nil {
		return append(results, viewerFailure("viewer-proxy", "start HTTPS proxy: %v", err))
	}
	defer secureProxy.server.Close()

	wsURL, err := browserWSForSeed(ctx, cuttleURL, "viewer-"+runID)
	if err != nil {
		return append(results, viewerFailure("viewer-browser", "resolve CDP websocket: %v", err))
	}
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return append(results, viewerFailure("viewer-browser", "dial CDP websocket: %v", err))
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(-1)
	client := &cdpClient{conn: conn}

	results = append(results,
		viewerBrowserCheck(
			ctx, client, directProxy.attempts, "viewer-root-websocket",
			directProxy.browserURL+"/", "/websockify", false,
		),
		viewerBrowserCheck(
			ctx, client, secureProxy.attempts, "viewer-prefixed-websocket",
			secureProxy.browserURL+viewerPrefix, viewerPrefix+"websockify", true,
		),
	)
	return results
}

func startViewerProxy(ctx context.Context, upstream *url.URL, browserHost string, secure bool) (*viewerTestProxy, error) {
	// The browser under test runs in Docker and must reach this host listener.
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", "0.0.0.0:0") // #nosec G102 -- test proxy for the local container
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	attempts := make(chan viewerUpgrade, 8)
	server := httptest.NewUnstartedServer(prefixedViewerProxy(upstream, attempts))
	server.Listener = listener
	server.Config.ReadHeaderTimeout = 5 * time.Second
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	scheme := schemeHTTP
	if secure {
		server.StartTLS()
		scheme = schemeHTTPS
	} else {
		server.Start()
	}

	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		server.Close()
		return nil, fmt.Errorf("%w: got %T", errUnexpectedListener, listener.Addr())
	}
	return &viewerTestProxy{
		server:     server,
		attempts:   attempts,
		browserURL: scheme + "://" + net.JoinHostPort(browserHost, strconv.Itoa(address.Port)),
	}, nil
}

// KasmVNC 1.3.3 returns 404 for a syntactically valid upgrade that omits both
// Origin and Sec-WebSocket-Protocol: binary. Pin that misleading response so a
// future diagnosis does not treat every websocket 404 as evidence of a bad path.
func incompleteViewerHandshake(ctx context.Context, viewerURL string) checkResult {
	name := "viewer-incomplete-handshake"
	endpoint, err := url.Parse(viewerURL)
	if err != nil {
		return viewerFailure(name, "parse viewer URL: %v", err)
	}
	endpoint.Scheme = map[string]string{"http": "ws", "https": "wss"}[endpoint.Scheme]
	endpoint.Path = "/websockify"

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(checkCtx, endpoint.String(), nil)
	if conn != nil {
		_ = conn.CloseNow()
	}
	if resp == nil {
		return viewerFailure(name, "upgrade returned no HTTP response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		return viewerFailure(name, "status=%d, want 404 without Origin and binary subprotocol", resp.StatusCode)
	}
	return checkResult{name, statusPass, "404 without Origin and Sec-WebSocket-Protocol: binary"}
}

func prefixedViewerProxy(upstream *url.URL, attempts chan<- viewerUpgrade) http.Handler {
	// The caller supplies the explicitly scoped local KasmVNC endpoint.
	proxy := httputil.NewSingleHostReverseProxy(upstream) // #nosec G704 -- local smoke-test upstream
	proxy.ModifyResponse = func(resp *http.Response) error {
		if attempt, ok := resp.Request.Context().Value(viewerUpgradeKey{}).(*viewerUpgrade); ok {
			attempt.statusCode = resp.StatusCode
			attempts <- *attempt
		}
		return nil
	}

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		isUpgrade := strings.EqualFold(req.Header.Get("Upgrade"), "websocket")
		if isUpgrade {
			attempt := &viewerUpgrade{
				path:     req.URL.Path,
				origin:   req.Header.Get("Origin"),
				protocol: req.Header.Get("Sec-WebSocket-Protocol"),
			}
			req = req.WithContext(context.WithValue(req.Context(), viewerUpgradeKey{}, attempt))
		} else if strings.HasPrefix(req.URL.Path, viewerPrefix) {
			clone := req.Clone(req.Context())
			clone.URL = cloneURL(req.URL)
			clone.URL.Path = "/" + strings.TrimPrefix(req.URL.Path, viewerPrefix)
			req = clone
		}
		proxy.ServeHTTP(w, req)
	})
}

func cloneURL(src *url.URL) *url.URL {
	dst := *src
	return &dst
}

func viewerBrowserCheck(
	ctx context.Context,
	client *cdpClient,
	attempts <-chan viewerUpgrade,
	name, pageURL, wantPath string,
	ignoreCertificateErrors bool,
) checkResult {
	created, err := client.send(ctx, "Target.createTarget", map[string]any{cdpURL: "about:blank"}, "")
	if err != nil {
		return viewerFailure(name, "create viewer target: %v", err)
	}
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err = json.Unmarshal(created, &target); err != nil {
		return viewerFailure(name, "decode target: %v", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = client.send(closeCtx, "Target.closeTarget", map[string]any{cdpTargetID: target.TargetID}, "")
	}()

	attached, err := client.send(ctx, "Target.attachToTarget", map[string]any{
		cdpTargetID: target.TargetID,
		"flatten":   true,
	}, "")
	if err != nil {
		return viewerFailure(name, "attach viewer target: %v", err)
	}
	var session struct {
		SessionID string `json:"sessionId"`
	}
	if err = json.Unmarshal(attached, &session); err != nil {
		return viewerFailure(name, "decode session: %v", err)
	}

	if ignoreCertificateErrors {
		if _, err = client.send(ctx, "Security.setIgnoreCertificateErrors", map[string]any{"ignore": true}, session.SessionID); err != nil {
			return viewerFailure(name, "allow local test certificate: %v", err)
		}
	}
	if _, err = client.send(ctx, "Page.navigate", map[string]any{cdpURL: pageURL}, session.SessionID); err != nil {
		return viewerFailure(name, "navigate viewer target: %v", err)
	}
	wantSocketURL, err := url.Parse(pageURL)
	if err != nil {
		return viewerFailure(name, "parse expected socket URL: %v", err)
	}
	wantSocketURL.Scheme = map[string]string{"http": "ws", "https": "wss"}[wantSocketURL.Scheme]
	wantSocketURL.Path = wantPath

	state, err := waitForViewerConnection(ctx, client, session.SessionID)
	if err != nil {
		return viewerFailure(name, "%v", err)
	}

	var attempt viewerUpgrade
	select {
	case attempt = <-attempts:
	case <-time.After(5 * time.Second):
		return viewerFailure(name, "RFB connected but proxy recorded no websocket upgrade")
	case <-ctx.Done():
		return viewerFailure(name, "waiting for websocket upgrade: %v", ctx.Err())
	}

	var problems []string
	if attempt.path != wantPath {
		problems = append(problems, fmt.Sprintf("upgrade path=%q, want %q", attempt.path, wantPath))
	}
	if state.SocketURL != wantSocketURL.String() {
		problems = append(problems, fmt.Sprintf("socket URL=%q, want %q", state.SocketURL, wantSocketURL.String()))
	}
	if attempt.statusCode != http.StatusSwitchingProtocols {
		problems = append(problems, fmt.Sprintf("upgrade status=%d, want 101", attempt.statusCode))
	}
	if attempt.origin == "" {
		problems = append(problems, "Origin header missing")
	}
	if !headerContains(attempt.protocol, "binary") {
		problems = append(problems, fmt.Sprintf("Sec-WebSocket-Protocol=%q, want binary", attempt.protocol))
	}
	if len(problems) > 0 {
		return checkResult{name, statusFail, strings.Join(problems, "; ")}
	}
	return checkResult{name, statusPass, fmt.Sprintf(
		"path=%s status=101 protocol=binary RFB=%s socket=%s",
		attempt.path, state.Connection, state.SocketURL,
	)}
}

func waitForViewerConnection(ctx context.Context, client *cdpClient, sessionID string) (viewerState, error) {
	deadline := time.Now().Add(30 * time.Second)
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
	return viewerState{}, fmt.Errorf("%w (connection=%q status=%q)", errViewerTimeout, last.Connection, last.Status)
}

func headerContains(value, want string) bool {
	for part := range strings.SplitSeq(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), want) {
			return true
		}
	}
	return false
}

var (
	errViewerTimeout      = errors.New("RFB did not connect within 30s")
	errUnexpectedListener = errors.New("listener address is not *net.TCPAddr")
)

func viewerFailure(name, format string, args ...any) checkResult {
	return checkResult{name, statusFail, fmt.Sprintf(format, args...)}
}
