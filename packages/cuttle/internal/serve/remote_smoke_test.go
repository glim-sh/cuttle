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
	for _, path := range []string{"/json/version", "/secret", "/devtools/browser"} {
		probe, perr := http.Get(base + path)
		if perr != nil {
			t.Fatal(perr)
		}
		_ = probe.Body.Close()
		if probe.StatusCode != http.StatusNotFound {
			t.Fatalf("%s on the remote port answered %d", path, probe.StatusCode)
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
	if code, body, _ := page(http.MethodGet, "/v/", cookie); code != http.StatusOK || body == "" {
		t.Fatalf("viewer through the proxy %d", code)
	}
	ws, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/v/websockify", &websocket.DialOptions{
		HTTPHeader:   http.Header{"Origin": {u.Scheme + "://" + u.Host}, "Cookie": {cookie}},
		Subprotocols: []string{"binary"},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("viewer socket through the proxy: %v", err)
	}
	_, hello, err := ws.Read(ctx)
	_ = ws.CloseNow()
	if err != nil || !strings.HasPrefix(string(hello), "RFB ") {
		t.Fatalf("the viewer socket did not speak RFB: %q %v", hello, err)
	}
}
