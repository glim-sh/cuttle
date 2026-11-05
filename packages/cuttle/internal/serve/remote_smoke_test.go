package serve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestRemoteSmoke drives a running image with the remote listener on, the way
// a remote agent does. Opt in with CUTTLE_REMOTE_SMOKE_URL (the remote port as
// this host reaches it, e.g. http://127.0.0.1:18080) and
// CUTTLE_REMOTE_SMOKE_TOKEN (a bearer whose hash is in the principals file).
// CUTTLE_REMOTE_SMOKE_SELF adds comma-separated URLs the agent's own tab must
// not read, such as the remote port by the container's address or by the
// public host pointed at the container.
func TestRemoteSmoke(t *testing.T) {
	base, token := os.Getenv("CUTTLE_REMOTE_SMOKE_URL"), os.Getenv("CUTTLE_REMOTE_SMOKE_TOKEN")
	if base == "" || token == "" {
		t.Skip("set CUTTLE_REMOTE_SMOKE_URL and CUTTLE_REMOTE_SMOKE_TOKEN to run against a live image")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/mcp", strings.NewReader("{}"))
	req.Host = "127.0.0.1"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no bearer with a loopback Host answered %d", resp.StatusCode)
	}
	// This host is not the public host, so it is no trusted peer whatever the
	// daemon's setting: CDP is refused, and the rest is the viewer's.
	for path, want := range map[string]int{"/json/version": 403, "/devtools/browser": 403, "/secret": 401, "/": 401} {
		probe, perr := http.Get(base + path)
		if perr != nil {
			t.Fatal(perr)
		}
		_ = probe.Body.Close()
		if probe.StatusCode != want {
			t.Fatalf("%s on the remote port answered %d, want %d", path, probe.StatusCode, want)
		}
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "smoke", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   base + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{token: token, base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		start := time.Now()
		res, cerr := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if cerr != nil {
			t.Fatalf("%s: %v", name, cerr)
		}
		if res.IsError {
			t.Fatalf("%s: %s", name, resultText(res))
		}
		t.Logf("%s (%s): %.300s", name, time.Since(start).Round(time.Millisecond), resultText(res))
		return res
	}
	call("browser_navigate", map[string]any{"url": "https://example.com/"})
	if text := resultText(call("browser_snapshot", nil)); !strings.Contains(text, "Example Domain") {
		t.Fatalf("snapshot %q", text)
	}
	// The agent's own tab must not reach the loopback services: by address the
	// URL policy blocks it, by a DNS name for 127.0.0.1 the daemon's Host check
	// and the MCP child's allowed host do, and the viewer has no port at all.
	targets := []string{
		"http://127.0.0.1:9222/json/version", "http://localhost:8931/mcp",
		"http://127.0.0.1.nip.io:9222/json/version", "http://127.0.0.1.nip.io:8931/mcp",
		"http://127.0.0.1.nip.io:6080/api/status", "http://127.0.0.1.nip.io:6081/api/status",
	}
	if self := os.Getenv("CUTTLE_REMOTE_SMOKE_SELF"); self != "" {
		targets = append(targets, strings.Split(self, ",")...)
	}
	for _, target := range targets {
		_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "browser_navigate", Arguments: map[string]any{"url": target}})
		res, serr := cs.CallTool(ctx, &mcp.CallToolParams{Name: "browser_snapshot", Arguments: map[string]any{}})
		if serr != nil {
			t.Fatal(serr)
		}
		if text := resultText(res); strings.Contains(text, "webSocketDebuggerUrl") || strings.Contains(text, "jsonrpc") || strings.Contains(text, "current_mode") || strings.Contains(text, "Selkies") {
			t.Fatalf("the agent's tab read %s: %.300s", target, text)
		}
	}
	call("browser_navigate", map[string]any{"url": "https://example.com/"})
	var status statusOut
	b, _ := json.Marshal(call("cuttle_status", nil).StructuredContent)
	if json.Unmarshal(b, &status) != nil || len(status.Tabs) == 0 || !status.Lease.Held {
		t.Fatalf("status %s", b)
	}
	var link linkOut
	b, _ = json.Marshal(call("cuttle_viewer_link", map[string]any{"reason": "smoke"}).StructuredContent)
	if jerr := json.Unmarshal(b, &link); jerr != nil {
		t.Fatal(jerr)
	}
	u, _ := url.Parse(link.URL)
	page := func(method, path, cookie string) (int, string, *http.Response) {
		req, _ := http.NewRequestWithContext(ctx, method, base+path, nil)
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		got, gerr := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if gerr != nil {
			t.Fatal(gerr)
		}
		defer func() { _ = got.Body.Close() }()
		body, _ := io.ReadAll(got.Body)
		return got.StatusCode, string(body), got
	}
	if code, _, _ := page(http.MethodGet, u.Path, ""); code != http.StatusOK {
		t.Fatalf("interstitial %d", code)
	}
	code, _, resp := page(http.MethodPost, u.Path, "")
	if code != http.StatusSeeOther {
		t.Fatalf("redeem %d", code)
	}
	cookie := ""
	for _, c := range resp.Cookies() {
		if c.Name == viewerCookie {
			cookie = c.Name + "=" + c.Value
		}
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("redeem lands on %q", loc)
	}
	for _, path := range []string{"/", "/v/"} {
		if code, body, _ := page(http.MethodGet, path, cookie); code != http.StatusOK || body == "" {
			t.Fatalf("viewer at %s through the proxy %d", path, code)
		}
	}
	// The Selkies page opens two things relative to /v/: its stream socket and
	// the phone keyboard's focus stream.
	ws, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/v/api/websockets", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {u.Scheme + "://" + u.Host}, "Cookie": {cookie}},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("viewer socket through the proxy: %v", err)
	}
	_, hello, err := ws.Read(ctx)
	_ = ws.CloseNow()
	if err != nil || !strings.HasPrefix(string(hello), "MODE ") {
		t.Fatalf("the viewer socket did not speak Selkies: %.80q %v", hello, err)
	}
	focus, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v/api/cuttle-focus", nil)
	focus.Header.Set("Cookie", cookie)
	focus.Header.Set("Sec-Fetch-Site", "same-origin")
	stream, err := http.DefaultClient.Do(focus)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Body.Close() }()
	if stream.StatusCode != http.StatusOK || !strings.HasPrefix(stream.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("focus stream through the proxy %d %q", stream.StatusCode, stream.Header.Get("Content-Type"))
	}
	event := make([]byte, 5)
	if _, err := io.ReadFull(stream.Body, event); err != nil || string(event) != "data:" {
		t.Fatalf("focus stream sent %q %v", event, err)
	}
}

// TestRemoteSmokeNetworkPeer checks one public URL from a caller outside the
// container, through the ingress in front of it. Opt in with
// CUTTLE_REMOTE_SMOKE_PUBLIC_URL (the public URL, reached from a container on
// the ingress's network) and CUTTLE_REMOTE_SMOKE_TRUSTED (1 when the daemon runs
// with --trust-network-peers, 0 when not).
func TestRemoteSmokeNetworkPeer(t *testing.T) {
	base, trustedEnv := os.Getenv("CUTTLE_REMOTE_SMOKE_PUBLIC_URL"), os.Getenv("CUTTLE_REMOTE_SMOKE_TRUSTED")
	if base == "" || trustedEnv == "" {
		t.Skip("set CUTTLE_REMOTE_SMOKE_PUBLIC_URL and CUTTLE_REMOTE_SMOKE_TRUSTED to run through a live ingress")
	}
	trusted := trustedEnv == "1"
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		resp, gerr := http.DefaultClient.Do(req)
		if gerr != nil {
			t.Fatal(gerr)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	want := func(ok bool, okCode, refused int) int {
		if ok {
			return okCode
		}
		return refused
	}
	if code, body := get("/"); code != want(trusted, 200, 401) || trusted && !strings.Contains(body, "<") {
		t.Fatalf("viewer at / answered %d %.120q", code, body)
	}
	if code, _ := get("/mcp"); code != 401 {
		t.Fatalf("/mcp without a bearer answered %d", code)
	}
	code, body := get("/json/version")
	if code != want(trusted, 200, 403) {
		t.Fatalf("/json/version answered %d %.200q", code, body)
	}
	wsBase := "wss://" + u.Host + "/devtools/"
	if !trusted {
		c, resp, derr := websocket.Dial(ctx, wsBase+"browser", nil)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if derr == nil {
			_ = c.CloseNow()
			t.Fatal("a CDP socket opened without trusted peers")
		}
		return
	}
	var version struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if jerr := json.Unmarshal([]byte(body), &version); jerr != nil || !strings.HasPrefix(version.WS, wsBase+"browser/") {
		t.Fatalf("/json/version advertises %q", version.WS)
	}
	if code, body := get("/json/list"); code != 200 || !strings.Contains(body, wsBase+"page/") || strings.Contains(body, "127.0.0.1") {
		t.Fatalf("/json/list %d %.300q", code, body)
	}
	c, resp, err := websocket.Dial(ctx, version.WS, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("CDP socket through the ingress: %v", err)
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(-1)
	if werr := c.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"Browser.getVersion"}`)); werr != nil {
		t.Fatal(werr)
	}
	for {
		_, msg, rerr := c.Read(ctx)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if strings.Contains(string(msg), `"id":1`) {
			if !strings.Contains(string(msg), "product") {
				t.Fatalf("Browser.getVersion answered %.200q", msg)
			}
			t.Logf("CDP through the ingress: %.160s", msg)
			break
		}
	}
	// The viewer's own socket, with the owner's network position and no cookie.
	v, resp, err := websocket.Dial(ctx, "wss://"+u.Host+"/api/websockets", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {base}}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("viewer socket through the ingress: %v", err)
	}
	_, hello, err := v.Read(ctx)
	_ = v.CloseNow()
	if err != nil || !strings.HasPrefix(string(hello), "MODE ") {
		t.Fatalf("the viewer socket did not speak Selkies: %.80q %v", hello, err)
	}
	// A page's socket, the viewer's own origin included, still never gets CDP.
	if bad, resp, derr := websocket.Dial(ctx, version.WS, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {base}}}); derr == nil {
		_ = bad.CloseNow()
		t.Fatal("a CDP socket from the public origin's pages was accepted")
	} else if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}
