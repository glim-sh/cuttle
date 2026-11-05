package serve

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
)

func TestRemotePrincipalAuthentication(t *testing.T) {
	p := &principalTable{entries: []remotePrincipal{{name: "agent", hash: sha256.Sum256([]byte("test-bearer"))}}}
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
			req.Header.Set("Cookie", viewerCookie+"=test-bearer")
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
	good := remoteConfig{listen: ":8080", publicURL: "https://browser.example", principalsFile: "/principals", viewerUpstream: "http://127.0.0.1:6081"}
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
		{"viewer socket", func(c *remoteConfig) { c.viewerUpstream = "unix:/tmp/cuttle-viewer/viewer.sock" }, modeSession, true},
		{"relative socket", func(c *remoteConfig) { c.viewerUpstream = "unix:viewer.sock" }, modeSession, false},
		{"viewer without port", func(c *remoteConfig) { c.viewerUpstream = "http://127.0.0.1" }, modeSession, false},
		{"missing file", func(c *remoteConfig) { c.principalsFile = "" }, modeSession, false},
		{"disabled", func(c *remoteConfig) { c.listen = "" }, modePool, true},
		{"trusted peers", func(c *remoteConfig) { c.trustNetworkPeers = true }, modeSession, true},
		{"trusted peers without remote", func(c *remoteConfig) { c.listen, c.trustNetworkPeers = "", true }, modeSession, false},
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

func TestPrincipalWatcherCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	(&principalTable{}).watch(ctx, filepath.Join(t.TempDir(), "missing"), nil)
}

// do sends one request to the remote surface and returns status, body and the
// response for headers.
func (h *mcpHarness) do(t *testing.T, method, path string, header http.Header, form url.Values) (int, string, *http.Response) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(t.Context(), method, testPublicURL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(req.Header, header)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	client := h.srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp
}

func TestRemoteRouteFamilies(t *testing.T) {
	h := newMCPHarness(t)
	t.Cleanup(h.r.m.pool.shutdown)
	h.r.startView(tokenHash("view-token"), "agent", time.Hour)
	bearer := http.Header{"Authorization": {"Bearer " + testAgentToken}}
	cookie := http.Header{"Cookie": {viewerCookie + "=view-token"}}
	both := http.Header{"Authorization": bearer["Authorization"], "Cookie": cookie["Cookie"]}
	mcpBody := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}}
	for _, tc := range []struct {
		name, method, path string
		header             http.Header
		want               int // 0: anything but 401
	}{
		{"healthz open", http.MethodGet, "/healthz", nil, 200},
		{"readyz open", http.MethodGet, "/readyz", nil, 200},
		{"mcp no credential", http.MethodPost, "/mcp", mcpBody, 401},
		{"mcp viewer cookie", http.MethodPost, "/mcp", cookie, 401},
		{"mcp bearer", http.MethodPost, "/mcp", bearer, 0},
		{"files no credential", http.MethodGet, "/files/abc", nil, 401},
		{"files viewer cookie", http.MethodGet, "/files/abc", cookie, 401},
		{"files bearer", http.MethodGet, "/files/abc", bearer, 404},
		{"viewer no credential", http.MethodGet, "/v/", nil, 401},
		{"viewer bearer", http.MethodGet, "/v/", bearer, 401},
		{"viewer cookie", http.MethodGet, "/v/", cookie, 0},
		{"viewer both", http.MethodGet, "/v/", both, 0},
		{"unknown ticket", http.MethodGet, "/t/nope", bearer, 410},
		{"root is the viewer", http.MethodGet, "/", cookie, 0},
		{"root needs a credential", http.MethodGet, "/", bearer, 401},
		{"no CDP without trusted peers", http.MethodGet, "/json/version", both, 403},
		{"no CDP socket without trusted peers", http.MethodGet, "/devtools/browser/x", both, 403},
		{"no secret API here", http.MethodGet, "/secret", bearer, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, resp := h.do(t, tc.method, tc.path, tc.header, nil)
			if tc.want == 0 && code == 401 || tc.want != 0 && code != tc.want {
				t.Fatalf("status %d, want %d", code, tc.want)
			}
			if code == 401 && strings.HasPrefix(tc.path, "/mcp") && resp.Header.Get("WWW-Authenticate") != "Bearer" {
				t.Error("missing bearer challenge")
			}
		})
	}
}

func TestViewerTicketLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newMCPHarness(t)
		t.Cleanup(h.r.m.pool.shutdown)
		cs := h.connect(t, testAgentToken, nil)
		callTool(t, cs, "browser_navigate", map[string]any{"url": "https://login.example/"})
		link := structured[linkOut](t, callTool(t, cs, "cuttle_viewer_link", map[string]any{"reason": "solve the <b>captcha</b>", "ttl_minutes": 10}))
		u, err := url.Parse(link.URL)
		if err != nil || u.Scheme+"://"+u.Host != testPublicURL || !strings.HasPrefix(u.Path, "/t/") {
			t.Fatalf("link %q", link.URL)
		}
		if res := callTool(t, cs, "cuttle_viewer_link", map[string]any{"reason": "x", "ttl_minutes": 31}); !res.IsError {
			t.Fatal("a 31 minute link must be refused")
		}
		same := http.Header{"Origin": {testPublicURL}}
		for range 2 { // a link preview fetches it first; the person's tap must still work
			code, body, resp := h.do(t, http.MethodGet, u.Path, nil, nil)
			if code != 200 || !strings.Contains(body, "agent") || !strings.Contains(body, "&lt;b&gt;captcha") {
				t.Fatalf("interstitial %d %q", code, body)
			}
			if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
				t.Fatal("interstitial headers")
			}
		}
		if code, _, _ := h.do(t, http.MethodPost, u.Path, http.Header{"Sec-Fetch-Site": {"cross-site"}}, url.Values{}); code != http.StatusForbidden {
			t.Fatalf("a cross-site POST answered %d", code)
		}
		code, _, resp := h.do(t, http.MethodPost, u.Path, same, url.Values{})
		if code != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
			t.Fatalf("redeem %d %q", code, resp.Header.Get("Location"))
		}
		var view *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == viewerCookie {
				view = c
			}
		}
		if view == nil || !view.Secure || !view.HttpOnly || view.SameSite != http.SameSiteLaxMode || view.Path != "/" || view.MaxAge != 600 {
			t.Fatalf("viewer cookie %+v", view)
		}
		if code, _, _ := h.do(t, http.MethodPost, u.Path, same, url.Values{}); code != http.StatusGone {
			t.Fatalf("a replayed POST answered %d", code)
		}
		if code, _, _ := h.do(t, http.MethodGet, u.Path, nil, nil); code != http.StatusGone {
			t.Fatalf("a spent link's page answered %d", code)
		}
		withCookie := http.Header{"Cookie": {viewerCookie + "=" + view.Value}}
		if code, _, _ := h.do(t, http.MethodGet, "/", withCookie, nil); code == 401 {
			t.Fatal("the viewer session did not open the viewer")
		}
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if code, _, _ := h.do(t, http.MethodGet, "/v/", withCookie, nil); code != 401 {
			t.Fatalf("an expired viewer session answered %d", code)
		}

		unused := structured[linkOut](t, callTool(t, cs, "cuttle_viewer_link", map[string]any{"reason": "later"}))
		time.Sleep(15 * time.Minute)
		up, _ := url.Parse(unused.URL)
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			if code, _, _ := h.do(t, method, up.Path, same, url.Values{}); code != http.StatusGone {
				t.Fatalf("%s on an expired link answered %d", method, code)
			}
		}
	})
}

func TestTicketCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newMCPHarness(t)
		t.Cleanup(h.r.m.pool.shutdown)
		mint := func(principal string) error {
			_, err := h.r.mintTicket(ticket{kind: ticketViewer, principal: principal, expires: time.Now().Add(time.Minute)})
			return err
		}
		for range ticketsPerPrincipal {
			if err := mint("agent"); err != nil {
				t.Fatal(err)
			}
		}
		if mint("agent") == nil {
			t.Fatal("the 51st outstanding link was minted")
		}
		if err := mint("other"); err != nil {
			t.Fatal("the cap is per principal")
		}
		time.Sleep(time.Minute)
		if err := mint("agent"); err != nil {
			t.Fatal("expired links must stop counting")
		}
	})
}

func TestSecretRequestForm(t *testing.T) {
	h := newMCPHarness(t)
	t.Cleanup(h.r.m.pool.shutdown)
	cs := h.connect(t, testAgentToken, nil)
	if res := callTool(t, cs, "cuttle_request_secret", map[string]any{"name": "GH_PASS", "origin": "https://github.com/login", "reason": "x"}); !res.IsError {
		t.Fatal("an origin with a path must be refused")
	}
	link := structured[linkOut](t, callTool(t, cs, "cuttle_request_secret", map[string]any{
		"name": "GH_PASS", "origin": "https://github.com", "reason": "sign in to the repo",
	}))
	u, _ := url.Parse(link.URL)
	code, body, _ := h.do(t, http.MethodGet, u.Path, nil, nil)
	for _, want := range []string{"agent", "GH_PASS", `value="https://github.com"`, "sign in to the repo", `type="password"`} {
		if code != 200 || !strings.Contains(body, want) {
			t.Fatalf("form page lacks %q: %d %s", want, code, body)
		}
	}
	same := http.Header{"Origin": {testPublicURL}}
	for _, bad := range []url.Values{
		{"origin": {"https://github.com"}, "ttl": {"1h"}},
		{"origin": {"github.com/login"}, "ttl": {"1h"}, "value": {"s3cret-value"}},
		{"origin": {"https://github.com"}, "ttl": {"9h"}, "value": {"s3cret-value"}},
	} {
		if code, body, _ := h.do(t, http.MethodPost, u.Path, same, bad); code != 400 || strings.Contains(body, "s3cret-value") {
			t.Fatalf("invalid form answered %d", code)
		}
	}
	form := url.Values{"origin": {"https://GitHub.com:443"}, "ttl": {"12h"}, "value": {"s3cret-value"}}
	if code, body, _ := h.do(t, http.MethodPost, u.Path, same, form); code != 200 || !strings.Contains(body, "Saved") {
		t.Fatalf("save answered %d %q", code, body)
	}
	info := h.r.m.pool.secrets.list(h.seed)
	if len(info) != 1 || info[0].Source != sourceHumanForm || info[0].BoundOrigin != "https://github.com" || info[0].TTLRemaining < 11*3600 {
		t.Fatalf("stored %+v", info)
	}
	if out := structured[waitOut](t, callTool(t, cs, "cuttle_wait", map[string]any{"until": "secret:GH_PASS", "timeout_minutes": 1})); !out.Met {
		t.Fatal("cuttle_wait does not see the saved secret")
	}
	if code, _, _ := h.do(t, http.MethodPost, u.Path, same, form); code != http.StatusGone {
		t.Fatalf("a replayed form answered %d", code)
	}
}

// fakeViewer is a viewer upstream: a page, an echoing socket on Selkies' path
// and a stream that only ends when its client goes.
func fakeViewer(t *testing.T, ln net.Listener, seen chan<- *http.Request) {
	t.Helper()
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(context.Background())
		switch r.URL.Path {
		case "/api/websockets":
			c, err := websocket.Accept(w, r, nil) // checks Origin against Host, as Selkies does
			if err != nil {
				return
			}
			defer func() { _ = c.CloseNow() }()
			for {
				typ, msg, err := c.Read(context.Background())
				if err != nil {
					return
				}
				_ = c.Write(context.Background(), typ, msg)
			}
		case "/api/cuttle-focus":
			w.Header().Set("Content-Type", "text/event-stream")
			for {
				_, _ = io.WriteString(w, "data: null\n\n")
				_ = http.NewResponseController(w).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		default:
			_, _ = io.WriteString(w, "viewer page "+r.URL.Path)
		}
	})}}
	srv.Start()
	t.Cleanup(srv.Close)
}

func TestViewerProxy(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "v.sock")
	for _, tc := range []struct{ name, network, addr string }{
		{"tcp", "tcp", "127.0.0.1:0"},
		{"unix", "unix", sock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen(tc.network, tc.addr)
			if err != nil {
				t.Fatal(err)
			}
			upstream := "unix:" + tc.addr
			if tc.network == "tcp" {
				upstream = "http://" + ln.Addr().String()
			}
			seen := make(chan *http.Request, 64)
			fakeViewer(t, ln, seen)
			testViewerProxy(t, newMCPHarnessViewer(t, upstream), seen)
		})
	}
}

func testViewerProxy(t *testing.T, h *mcpHarness, seen chan *http.Request) {
	t.Helper()
	front := httptest.NewServer(h.r.handler())
	t.Cleanup(front.Close)
	h.r.startView(tokenHash("view-token"), "agent", time.Hour)
	cookie := viewerCookie + "=view-token"

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, front.URL+"/v/index.html?x=1", nil)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Authorization", "Bearer "+testAgentToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "viewer page /index.html" {
		t.Fatalf("proxied page %q", b)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); csp != "frame-ancestors 'self'" {
		t.Fatalf("the viewer can be framed: CSP %q", csp)
	}
	if up := <-seen; up.Header.Get("Cookie") != "" || up.Header.Get("Authorization") != "" || up.URL.RawQuery != "x=1" {
		t.Fatalf("the viewer upstream saw %v %q", up.Header, up.URL.RawQuery)
	}
	for path, want := range map[string]string{"/index.html": "/index.html", "/v": "/", "/video.js": "/video.js", "/v/v/x": "/v/x"} {
		req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, front.URL+path, nil)
		req.Header.Set("Cookie", cookie)
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		<-seen
		if string(b) != "viewer page "+want {
			t.Fatalf("%s reached the viewer as %q, want %s", path, b, want)
		}
	}

	for origin, want := range map[string]int{"https://evil.example": http.StatusForbidden, testPublicURL: http.StatusOK} {
		post, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, front.URL+"/v/api/recording", nil)
		post.Header.Set("Cookie", cookie)
		post.Header.Set("Origin", origin)
		posted, perr := http.DefaultClient.Do(post)
		if perr != nil {
			t.Fatal(perr)
		}
		_ = posted.Body.Close()
		if posted.StatusCode != want {
			t.Fatalf("a POST from %s answered %d, want %d", origin, posted.StatusCode, want)
		}
		if want == http.StatusOK {
			if up := <-seen; up.Header.Get("Origin") != "http://"+up.Host {
				t.Fatalf("the viewer saw Origin %q for Host %q", up.Header.Get("Origin"), up.Host)
			}
		}
	}

	wsURL := "ws" + strings.TrimPrefix(front.URL, "http") + "/v/api/websockets"
	dial := func(origin, cookie string) (*websocket.Conn, error) {
		hdr := http.Header{"Origin": {origin}}
		if cookie != "" {
			hdr.Set("Cookie", cookie)
		}
		c, dresp, derr := websocket.Dial(t.Context(), wsURL, &websocket.DialOptions{HTTPHeader: hdr})
		if dresp != nil && dresp.Body != nil {
			_ = dresp.Body.Close()
		}
		return c, derr
	}
	if _, derr := dial("https://evil.example", cookie); derr == nil {
		t.Fatal("a cross-origin upgrade was accepted")
	}
	if _, derr := dial(testPublicURL, ""); derr == nil {
		t.Fatal("an upgrade without the viewer cookie was accepted")
	}
	c, err := dial(testPublicURL, cookie)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	if werr := c.Write(t.Context(), websocket.MessageBinary, []byte("frame")); werr != nil {
		t.Fatal(werr)
	}
	if _, msg, rerr := c.Read(t.Context()); rerr != nil || string(msg) != "frame" {
		t.Fatalf("echo %q %v", msg, rerr)
	}

	// At session expiry the daemon ends the sockets and the streams it relays.
	h.r.startView(tokenHash("short-token"), "agent", 300*time.Millisecond)
	short, err := dial(testPublicURL, viewerCookie+"=short-token")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = short.CloseNow() }()
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, front.URL+"/v/api/cuttle-focus", nil)
	req.Header.Set("Cookie", viewerCookie+"=short-token")
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Body.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, _, err := short.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("the socket outlived its viewer session: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, stream.Body); done <- err }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the stream outlived its viewer session")
	}
}

func TestRevokeEndsWhatAPrincipalMinted(t *testing.T) {
	h := newMCPHarness(t)
	t.Cleanup(h.r.m.pool.shutdown)
	cs := h.connect(t, testOtherToken, nil)
	link := structured[linkOut](t, callTool(t, cs, "cuttle_viewer_link", map[string]any{"reason": "x"}))
	h.r.startView(tokenHash("other-view"), "other", time.Hour)
	h.r.startView(tokenHash("agent-view"), "agent", time.Hour)
	file, _ := h.r.mintFile("other", t.TempDir(), "f")
	body := fmt.Sprintf("agent sha256:%x\n", sha256.Sum256([]byte(testAgentToken)))
	if err := os.WriteFile(h.r.cfg.principalsFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.r.principals.reload(h.r.cfg.principalsFile); err != nil {
		t.Fatal(err)
	}
	h.r.revoke()
	u, _ := url.Parse(link.URL)
	if _, ok := h.r.peekTicket(strings.TrimPrefix(u.Path, "/t/")); ok {
		t.Fatal("a revoked principal's link still works")
	}
	h.r.mu.Lock()
	_, otherView := h.r.views[tokenHash("other-view")]
	_, agentView := h.r.views[tokenHash("agent-view")]
	_, otherFile := h.r.files[file]
	h.r.mu.Unlock()
	if otherView || otherFile || !agentView {
		t.Fatalf("after revoking other: its view %v, its file %v, agent's view %v", otherView, otherFile, agentView)
	}
	h.r.mcp.mu.Lock()
	_, session := h.r.mcp.agents["other"]
	h.r.mcp.mu.Unlock()
	if session {
		t.Fatal("a revoked principal kept its browser session")
	}
}

func TestInternalListenerRefusesBrowserPages(t *testing.T) {
	h := refuseBrowserRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, tc := range []struct {
		host, site string
		want       int
	}{
		{"127.0.0.1:9222", "", 204},
		{"localhost:9222", "", 204},
		{"127.0.0.1:9222", "same-origin", 403},
		{"127.0.0.1:9222", "none", 403},
		{"127.0.0.1.nip.io:9222", "", 403},
		{"cuttle.example", "", 403},
	} {
		req := httptest.NewRequest(http.MethodGet, "/json/version", nil)
		req.Host = tc.host
		if tc.site != "" {
			req.Header.Set("Sec-Fetch-Site", tc.site)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("Host %q, Sec-Fetch-Site %q: %d, want %d", tc.host, tc.site, rec.Code, tc.want)
		}
	}
}

func TestPublicOriginIsCanonical(t *testing.T) {
	for _, raw := range []string{"https://Browser.Example", "https://browser.example:443/", "https://browser.example/"} {
		if got := publicOrigin(raw); got != testPublicURL {
			t.Errorf("publicOrigin(%q) = %q", raw, got)
		}
	}
}

// trustedRemote is a remote listener with trusted network peers on (or off)
// in front of a fake Chrome, the pod's own addresses pinned to podAddr.
func trustedRemote(t *testing.T, trust bool) *remote {
	t.Helper()
	cdp := newFakeCDP(t)
	file := filepath.Join(t.TempDir(), "principals")
	if err := os.WriteFile(file, fmt.Appendf(nil, "agent sha256:%x\n", sha256.Sum256([]byte(testAgentToken))), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := newTestPool(t, serveConfig{mode: modeSession}, (&fakeLauncher{port: cdp.port}).toLauncher())
	m := &multiplexer{pool: pool, port: 9222}
	r, err := newRemote(m, remoteConfig{
		listen: ":0", publicURL: testPublicURL, principalsFile: file, viewerUpstream: testViewerAddr,
		trustNetworkPeers: trust,
	}, "http://127.0.0.1:8931/mcp", "localhost:8931", &fakeBrowser{})
	if err != nil {
		t.Fatal(err)
	}
	r.mcp.client = &http.Client{Transport: noUpstream{}}
	if r.peers != nil {
		r.peers.ownAddrs = func() ([]net.Addr, error) {
			return []net.Addr{
				&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
				&net.IPNet{IP: net.ParseIP(podAddr), Mask: net.CIDRMask(24, 32)},
				&net.IPNet{IP: net.ParseIP(podAddr6), Mask: net.CIDRMask(64, 128)},
			}, nil
		}
	}
	t.Cleanup(r.close)
	return r
}

var (
	errNoUpstream  = errors.New("no upstream in this test")
	errNetlinkDown = errors.New("netlink down")
)

// noUpstream fails every call to the Playwright MCP child at once.
type noUpstream struct{}

func (noUpstream) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errNoUpstream
}

const (
	podAddr     = "10.42.0.9"
	podAddr6    = "fd00::9"
	ingressAddr = "10.42.1.5:40000"
)

// TestRemoteRoutesByCaller is the route x caller table: who reaches what on
// the remote listener with trusted network peers on and off.
func TestRemoteRoutesByCaller(t *testing.T) {
	const allow, deny = true, false
	type caller struct {
		name, remoteAddr, host string
		header                 http.Header
	}
	bearer := http.Header{"Authorization": {"Bearer " + testAgentToken}}
	cookie := http.Header{"Cookie": {viewerCookie + "=view-token"}}
	publicHost := strings.TrimPrefix(testPublicURL, "https://")
	callers := []caller{
		{"loopback", "127.0.0.1:5000", publicHost, nil},
		{"loopback v6", "[::1]:5000", publicHost, nil},
		{"mapped loopback", "[::ffff:127.0.0.1]:5000", publicHost, nil},
		{"own pod address", podAddr + ":5000", publicHost, nil},
		{"own pod v6 address", "[" + podAddr6 + "]:5000", publicHost, nil},
		{"ingress", ingressAddr, publicHost, nil},
		{"ingress, other Host", ingressAddr, "127.0.0.1.nip.io:8080", nil},
		{"ingress, bearer", ingressAddr, publicHost, bearer},
		{"loopback, cookie", "127.0.0.1:5000", publicHost, cookie},
		{"own pod address, cookie", podAddr + ":5000", publicHost, cookie},
		{"forwarded-for spoof", "127.0.0.1:5000", publicHost, http.Header{"X-Forwarded-For": {"100.64.0.1"}, "Tailscale-User-Login": {"owner@example.com"}}},
		{"ingress, cross-site fetch", ingressAddr, publicHost, http.Header{"Sec-Fetch-Site": {"cross-site"}, "Sec-Fetch-Mode": {"no-cors"}}},
		{"ingress, cross-site navigation", ingressAddr, publicHost, http.Header{"Sec-Fetch-Site": {"cross-site"}, "Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}}},
		{"ingress, cross-site frame", ingressAddr, publicHost, http.Header{"Sec-Fetch-Site": {"cross-site"}, "Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"iframe"}}},
		{"ingress, same-origin fetch", ingressAddr, publicHost, http.Header{"Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"cors"}}},
		{"ingress, Host with dot and port", ingressAddr, publicHost + ".:443", nil},
		{"ingress, other port", ingressAddr, publicHost + ":8080", nil},
	}
	routes := []struct{ method, path string }{
		{http.MethodGet, "/"},
		{http.MethodGet, "/v/"},
		{http.MethodGet, "/json/version"},
		{http.MethodGet, "/json/list"},
		{http.MethodGet, "/json"},
		{http.MethodGet, "/devtools/browser/GUID123"},
		{http.MethodPost, "/mcp"},
		{http.MethodGet, "/files/abc"},
	}
	// want[trust][caller][route]
	want := map[bool]map[string][]bool{
		true: {
			"loopback":                        {deny, deny, deny, deny, deny, deny, deny, deny},
			"loopback v6":                     {deny, deny, deny, deny, deny, deny, deny, deny},
			"mapped loopback":                 {deny, deny, deny, deny, deny, deny, deny, deny},
			"own pod address":                 {deny, deny, deny, deny, deny, deny, deny, deny},
			"own pod v6 address":              {deny, deny, deny, deny, deny, deny, deny, deny},
			"ingress":                         {allow, allow, allow, allow, allow, allow, deny, deny},
			"ingress, other Host":             {deny, deny, deny, deny, deny, deny, deny, deny},
			"ingress, bearer":                 {allow, allow, allow, allow, allow, allow, allow, allow},
			"loopback, cookie":                {allow, allow, deny, deny, deny, deny, deny, deny},
			"own pod address, cookie":         {allow, allow, deny, deny, deny, deny, deny, deny},
			"forwarded-for spoof":             {deny, deny, deny, deny, deny, deny, deny, deny},
			"ingress, cross-site fetch":       {deny, deny, deny, deny, deny, deny, deny, deny},
			"ingress, cross-site navigation":  {allow, allow, deny, deny, deny, deny, deny, deny},
			"ingress, cross-site frame":       {deny, deny, deny, deny, deny, deny, deny, deny},
			"ingress, same-origin fetch":      {allow, allow, deny, deny, deny, deny, deny, deny},
			"ingress, Host with dot and port": {allow, allow, allow, allow, allow, allow, deny, deny},
			"ingress, other port":             {deny, deny, deny, deny, deny, deny, deny, deny},
		},
		false: {
			"loopback":                        {deny, deny, deny, deny, deny, deny, deny, deny},
			"loopback v6":                     {deny, deny, deny, deny, deny, deny, deny, deny},
			"mapped loopback":                 {deny, deny, deny, deny, deny, deny, deny, deny},
			"own pod address":                 {deny, deny, deny, deny, deny, deny, deny, deny},
			"own pod v6 address":              {deny, deny, deny, deny, deny, deny, deny, deny},
			"ingress":                         {deny, deny, deny, deny, deny, deny, deny, deny},
			"ingress, other Host":             {deny, deny, deny, deny, deny, deny, deny, deny},
			"ingress, bearer":                 {deny, deny, deny, deny, deny, deny, allow, allow},
			"loopback, cookie":                {allow, allow, deny, deny, deny, deny, deny, deny},
			"own pod address, cookie":         {allow, allow, deny, deny, deny, deny, deny, deny},
			"forwarded-for spoof":             {deny, deny, deny, deny, deny, deny, deny, deny},
			"ingress, cross-site fetch":       {deny, deny, deny, deny, deny, deny, deny, deny},
			"ingress, cross-site navigation":  {deny, deny, deny, deny, deny, deny, deny, deny},
			"ingress, cross-site frame":       {deny, deny, deny, deny, deny, deny, deny, deny},
			"ingress, same-origin fetch":      {deny, deny, deny, deny, deny, deny, deny, deny},
			"ingress, Host with dot and port": {deny, deny, deny, deny, deny, deny, deny, deny},
			"ingress, other port":             {deny, deny, deny, deny, deny, deny, deny, deny},
		},
	}
	for _, trust := range []bool{true, false} {
		r := trustedRemote(t, trust)
		r.startView(tokenHash("view-token"), "agent", time.Hour)
		h := r.handler()
		for _, c := range callers {
			for i, rt := range routes {
				t.Run(fmt.Sprintf("trust=%v/%s/%s %s", trust, c.name, rt.method, rt.path), func(t *testing.T) {
					// An admitted /mcp call waits for the absent upstream; its answer
					// past the credential check is all this table needs.
					ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
					defer cancel()
					req := httptest.NewRequestWithContext(ctx, rt.method, testPublicURL+rt.path, strings.NewReader("{}"))
					req.RemoteAddr, req.Host = c.remoteAddr, c.host
					maps.Copy(req.Header, c.header)
					req.Header.Set("Content-Type", "application/json")
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					// The viewer upstream is unreachable here, so an admitted viewer
					// request answers 502; a /files handle that does not exist, 404.
					got := rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden
					if got != want[trust][c.name][i] {
						t.Fatalf("status %d (%s), want allowed=%v", rec.Code, strings.TrimSpace(rec.Body.String()), want[trust][c.name][i])
					}
				})
			}
		}
	}
}

// A page in the owner's browser never reaches discovery, not even by a
// top-level navigation, and no caller picks the browser's launch options.
func TestRemoteCDPRefusesPagesAndLaunchOptions(t *testing.T) {
	r := trustedRemote(t, true)
	h := r.handler()
	for site, want := range map[string]int{"cross-site": http.StatusForbidden, "same-site": http.StatusForbidden, "same-origin": http.StatusForbidden, "none": http.StatusOK, "": http.StatusOK} {
		req := httptest.NewRequest(http.MethodGet, testPublicURL+"/json/version?proxy=http://evil.example:3128&timezone=Pacific/Auckland", nil)
		req.RemoteAddr = ingressAddr
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
			req.Header.Set("Sec-Fetch-Mode", "navigate")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("Sec-Fetch-Site %q: %d, want %d", site, rec.Code, want)
		}
	}
	// A page's socket handshake on discovery is still a page request.
	req := httptest.NewRequest(http.MethodGet, testPublicURL+"/json/version", nil)
	req.RemoteAddr = ingressAddr
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a page's upgrade on /json/version: %d", rec.Code)
	}
	seed, _ := r.m.pool.seedKeyFor("")
	inst := r.m.pool.runningInstance(seed)
	if inst == nil || inst.proxy != "" || inst.timezone == "Pacific/Auckland" {
		t.Fatalf("the query chose the launch: %+v", inst)
	}
}

func TestRemoteCDPAdvertisesThePublicOrigin(t *testing.T) {
	r := trustedRemote(t, true)
	h := r.handler()
	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, testPublicURL+path, nil)
		req.RemoteAddr = ingressAddr
		// Forwarded headers must not steer the advertised URL.
		req.Header.Set("X-Forwarded-Host", "evil.example")
		req.Header.Set("X-Forwarded-Proto", "http")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if body := get("/json/version"); !strings.Contains(body, `"webSocketDebuggerUrl":"wss://browser.example/devtools/browser/GUID123"`) {
		t.Fatalf("version %s", body)
	}
	body := get("/json/list")
	if !strings.Contains(body, `"webSocketDebuggerUrl":"wss://browser.example/devtools/page/PAGE9"`) ||
		!strings.Contains(body, `"devtoolsFrontendUrl":"devtools://devtools/bundled/inspector.html?wss=browser.example/devtools/page/PAGE9"`) {
		t.Fatalf("list %s", body)
	}
	if body := get("/json"); !strings.Contains(body, "wss://browser.example/devtools/page/PAGE9") {
		t.Fatalf("/json %s", body)
	}
}

// TestRemoteCDPSocket drives a real WebSocket through the remote listener to
// the fake Chrome, the way connectOverCDP does after reading /json/version.
func TestRemoteCDPSocket(t *testing.T) {
	r := trustedRemote(t, true)
	h := r.handler()
	// The test client dials loopback; this stands in for the ingress proxy.
	front := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.RemoteAddr, req.Host = ingressAddr, "browser.example"
		h.ServeHTTP(w, req)
	}))
	// The remote server's read deadline, shortened: a socket must outlive it.
	front.Config.ReadTimeout = 200 * time.Millisecond
	front.Start()
	t.Cleanup(front.Close)
	wsURL := "ws" + strings.TrimPrefix(front.URL, "http") + "/devtools/browser/GUID123"
	if _, resp, err := websocket.Dial(t.Context(), wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {testPublicURL}}}); err == nil {
		t.Fatal("a socket from the public origin's own pages was accepted")
	} else if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	// DevTools' own socket is cross-site by Fetch Metadata; a page's is refused.
	crossSite := func(origin string) http.Header {
		return http.Header{"Origin": {origin}, "Sec-Fetch-Site": {"cross-site"}, "Sec-Fetch-Mode": {"websocket"}}
	}
	if _, resp, err := websocket.Dial(t.Context(), wsURL, &websocket.DialOptions{HTTPHeader: crossSite("https://evil.example")}); err == nil {
		t.Fatal("a cross-site page's socket was accepted")
	} else if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	dt, resp, err := websocket.Dial(t.Context(), wsURL, &websocket.DialOptions{HTTPHeader: crossSite("devtools://devtools")})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("the DevTools frontend's socket: %v", err)
	}
	_ = dt.CloseNow()
	c, resp, err := websocket.Dial(t.Context(), wsURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, msg, rerr := c.Read(ctx); rerr != nil || !strings.Contains(string(msg), "CDP.greeting") {
		t.Fatalf("greeting %q %v", msg, rerr)
	}
	time.Sleep(2 * front.Config.ReadTimeout)
	if werr := c.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"Browser.getVersion"}`)); werr != nil {
		t.Fatal(werr)
	}
	if _, msg, rerr := c.Read(ctx); rerr != nil || !strings.HasPrefix(string(msg), "echo:") {
		t.Fatalf("round trip after the read deadline %q %v", msg, rerr)
	}
}

func TestNetworkPeersFailClosed(t *testing.T) {
	p := &networkPeers{ownAddrs: func() ([]net.Addr, error) { return nil, errNetlinkDown }}
	if p.outside(ingressAddr) {
		t.Fatal("an unreadable address list trusted a peer")
	}
	for _, addr := range []string{"", "garbage", "[::]:1", "0.0.0.0:1"} {
		if (&networkPeers{ownAddrs: func() ([]net.Addr, error) { return nil, nil }}).outside(addr) {
			t.Fatalf("%q was trusted", addr)
		}
	}
}

func TestPrincipalSeedParsing(t *testing.T) {
	file := filepath.Join(t.TempDir(), "principals")
	for _, seed := range []string{"main", "worker_1", "a", strings.Repeat("a", 32), "state", "logs", "__default__", "../other", "Upper", "a-b", "", strings.Repeat("a", 33), "1worker"} {
		t.Run(seed, func(t *testing.T) {
			body := fmt.Sprintf("agent sha256:%x seed=%s\n", sha256.Sum256([]byte(testAgentToken)), seed)
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			p := &principalTable{}
			err := p.reload(file)
			want := seed == "main" || seed == "worker_1" || seed == "a" || len(seed) == 32
			if (err == nil) != want {
				t.Fatalf("parse %q: %v", seed, err)
			}
			handler := p.authenticate(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if principalSeedOf(req.Context()) != seed {
					t.Error("request lost its admitted seed")
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+testAgentToken)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if want && rec.Code != 204 || !want && rec.Code != 401 {
				t.Fatalf("status %d", rec.Code)
			}
		})
	}
}

func TestPrincipalSeedRebindRevokes(t *testing.T) {
	h := newMCPHarness(t)
	t.Cleanup(h.r.m.pool.shutdown)
	cs := h.connect(t, testAgentToken, nil)
	callTool(t, cs, "browser_snapshot", nil)
	tok, err := h.r.mintTicket(ticket{principal: "agent", kind: ticketViewer, expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	file, err := h.r.mintFile("agent", t.TempDir(), "file")
	if err != nil {
		t.Fatal(err)
	}
	viewHash := tokenHash("old-view")
	h.r.startView(viewHash, "agent", time.Hour)
	view := h.r.views[viewHash]
	var bound map[string]bool
	h.r.principals.bindSeeds = func(seeds map[string]bool) { bound = seeds }
	body := fmt.Sprintf("agent sha256:%x seed=worker\nother sha256:%x\nnewcomer sha256:%x seed=worker\n", sha256.Sum256([]byte(testAgentToken)), sha256.Sum256([]byte(testOtherToken)), sha256.Sum256([]byte("new-token")))
	if err := os.WriteFile(h.r.cfg.principalsFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.r.principals.reload(h.r.cfg.principalsFile); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.mintTicket(ticket{principal: "agent"}); !errors.Is(err, errRevoked) {
		t.Fatalf("stale ticket mint: %v", err)
	}
	if _, err := h.r.mintFileSeed("agent", "main", "", "file"); !errors.Is(err, errRevoked) {
		t.Fatalf("stale file mint: %v", err)
	}
	if h.r.startViewSeed(tokenHash("stale-view"), "agent", "main", time.Hour) {
		t.Fatal("stale view created")
	}
	for _, principal := range []string{"agent", "newcomer"} {
		newTicket, mintErr := h.r.mintTicket(ticket{principal: principal, seed: "worker", kind: ticketViewer, expires: time.Now().Add(time.Hour)})
		if mintErr != nil {
			t.Fatal(mintErr)
		}
		newFile, mintErr := h.r.mintFileSeed(principal, "worker", t.TempDir(), "file")
		if mintErr != nil {
			t.Fatal(mintErr)
		}
		newViewHash := tokenHash(principal + "-new-view")
		if !h.r.startViewSeed(newViewHash, principal, "worker", time.Hour) {
			t.Fatal("new view refused")
		}
		newView := h.r.views[newViewHash]
		h.r.revoke()
		if _, ok := h.r.peekTicket(newTicket); !ok {
			t.Errorf("%s: new ticket revoked", principal)
		}
		if h.r.files[newFile] == nil || h.r.views[newViewHash] != newView {
			t.Errorf("%s: new capability revoked", principal)
		}
		select {
		case <-newView.ctx.Done():
			t.Errorf("%s: new viewer ended", principal)
		default:
		}
	}
	if !bound["worker"] || bound["main"] {
		t.Fatalf("bindings: %v", bound)
	}
	if _, ok := h.r.peekTicket(tok); ok {
		t.Fatal("old ticket survived")
	}
	if h.r.files[file] != nil || h.r.views[viewHash] != nil {
		t.Fatal("old capabilities survived")
	}
	select {
	case <-view.ctx.Done():
	default:
		t.Fatal("viewer context still live")
	}
	h.r.mcp.mu.Lock()
	_, alive := h.r.mcp.agents["agent"]
	h.r.mcp.mu.Unlock()
	if alive {
		t.Fatal("upstream session survived")
	}
	if _, live := h.r.m.pool.leases.status(reservedSeed); live {
		t.Fatal("old lease survived")
	}
	if _, err := h.r.mintFileSeed("agent", "worker", t.TempDir(), "file"); err != nil {
		t.Fatal(err)
	}
}

func TestSeedBusyAndViewerRedeem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newMCPHarness(t)
		t.Cleanup(h.r.m.pool.shutdown)
		h.r.principals.mu.Lock()
		h.r.principals.entries[0].seed = "worker"
		h.r.principals.mu.Unlock()
		h.r.m.pool.mode = modePool
		raised := ""
		h.r.raiseSeed = func(_ context.Context, seed string) bool { raised = seed; return true }
		tok, err := h.r.mintTicket(ticket{principal: "agent", seed: "worker", kind: ticketViewer, expires: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if !h.r.seedBusy("worker") || h.r.seedBusy(reservedSeed) {
			t.Fatal("ticket pins wrong seed")
		}
		req := httptest.NewRequest(http.MethodPost, "/t/"+tok, nil)
		req.SetPathValue("tkt", tok)
		rec := httptest.NewRecorder()
		h.r.handleTicketPost(rec, req)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" || raised != "worker" {
			t.Fatalf("redeem: %d %q %q", rec.Code, rec.Header().Get("Location"), raised)
		}
		if !h.r.seedBusy("worker") {
			t.Fatal("view does not pin seed")
		}
		for key, view := range h.r.views {
			if view.seed != "worker" {
				t.Fatal("view lost seed")
			}
			h.r.endView(key)
		}
		if h.r.seedBusy("worker") {
			t.Fatal("closed view pins seed")
		}
		if _, err := h.r.mintFileSeed("agent", "worker", t.TempDir(), "file"); err != nil {
			t.Fatal(err)
		}
		if !h.r.seedBusy("worker") || h.r.seedBusy(reservedSeed) {
			t.Fatal("file handle pins wrong seed")
		}
	})
}

func TestSecretFormUsesTicketSeed(t *testing.T) {
	h := newMCPHarness(t)
	t.Cleanup(h.r.m.pool.shutdown)
	h.r.principals.mu.Lock()
	h.r.principals.entries[0].seed = "worker"
	h.r.principals.mu.Unlock()
	tok, err := h.r.mintTicket(ticket{principal: "agent", seed: "worker", kind: ticketSecret, name: "password", expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/t/"+tok, strings.NewReader(url.Values{"value": {"test-value"}, "origin": {"https://example.com"}, "ttl": {"1h"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("tkt", tok)
	rec := httptest.NewRecorder()
	h.r.handleTicketPost(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if len(h.r.m.pool.secrets.list("worker")) != 1 || len(h.r.m.pool.secrets.list(reservedSeed)) != 0 {
		t.Fatal("secret stored in wrong seed")
	}
}

func TestViewerLaunchKeepsPinAndRechecksSession(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(strconv.FormatBool(revoked), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newMCPHarness(t)
				t.Cleanup(h.r.m.pool.shutdown)
				raised := false
				h.r.raiseSeed = func(context.Context, string) bool { raised = true; return true }
				h.r.m.pool.launch.waitReady = func(context.Context, int) bool {
					if !h.r.seedBusy(reservedSeed) {
						t.Error("seed is not pinned during launch")
					}
					if revoked {
						if err := os.WriteFile(h.r.cfg.principalsFile, nil, 0o600); err != nil {
							t.Fatal(err)
						}
						if err := h.r.principals.reload(h.r.cfg.principalsFile); err != nil {
							t.Fatal(err)
						}
						h.r.revoke()
					} else {
						h.r.mu.Lock()
						for _, view := range h.r.views {
							view.expires = time.Now().Add(-time.Second)
						}
						h.r.mu.Unlock()
					}
					return true
				}
				tok, err := h.r.mintTicket(ticket{principal: "agent", kind: ticketViewer, expires: time.Now().Add(time.Hour)})
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPost, "/t/"+tok, nil)
				req.SetPathValue("tkt", tok)
				rec := httptest.NewRecorder()
				h.r.handleTicketPost(rec, req)
				if rec.Code != http.StatusGone || raised || rec.Header().Get("Set-Cookie") != "" {
					t.Fatalf("expired/revoked redeem: status %d raised %v", rec.Code, raised)
				}
			})
		})
	}
}

func TestRemoteSeedCDPRoutes(t *testing.T) {
	r := trustedRemote(t, true)
	r.m.pool.mode = modePool
	h := r.handler()
	for _, suffix := range []string{"/json/version", "/json/list", "/json"} {
		for _, site := range []string{"same-origin", "same-site", "cross-site", ""} {
			req := httptest.NewRequest(http.MethodGet, testPublicURL+"/fingerprint/alpha"+suffix+"?fingerprint=other&proxy=http://evil.example:3128", nil)
			req.RemoteAddr = ingressAddr
			req.Header.Set("Sec-Fetch-Site", site)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if site != "" {
				if rec.Code != http.StatusForbidden {
					t.Fatalf("%s %s: %d, want 403", suffix, site, rec.Code)
				}
				continue
			}
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "wss://browser.example/fingerprint/alpha/devtools/") {
				t.Fatalf("%s: %d %s", suffix, rec.Code, rec.Body.String())
			}
			if suffix != "/json/version" && !strings.Contains(rec.Body.String(), "inspector.html?wss=browser.example/fingerprint/alpha/devtools/page/PAGE9") {
				t.Fatalf("frontend: %s", rec.Body.String())
			}
		}
	}
	if inst := r.m.pool.runningInstance("alpha"); inst == nil || inst.proxy != "" || r.m.pool.runningInstance("other") != nil {
		t.Fatal("query launch options were not discarded")
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.RemoteAddr, req.Host = ingressAddr, "browser.example"
		h.ServeHTTP(w, req)
	}))
	t.Cleanup(front.Close)
	for _, origin := range []string{"https://evil.example", "devtools://devtools", ""} {
		headers := http.Header{}
		if origin != "" {
			headers.Set("Origin", origin)
			headers.Set("Sec-Fetch-Site", "cross-site")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(front.URL, "http")+"/fingerprint/alpha/devtools/browser/GUID123", &websocket.DialOptions{HTTPHeader: headers})
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		switch {
		case origin == "https://evil.example":
			if err == nil {
				_ = c.CloseNow()
				t.Error("page socket accepted")
			}
		case err != nil:
			t.Errorf("origin %q: %v", origin, err)
		default:
			if _, msg, readErr := c.Read(ctx); readErr != nil || !strings.Contains(string(msg), "CDP.greeting") {
				t.Errorf("greeting: %q %v", msg, readErr)
			}
			_ = c.CloseNow()
		}
		cancel()
	}
	// The proxy logs its disconnect after the client closes; a later test swaps the
	// global logger, so wait for every proxied session to finish first.
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.m.pool.mu.Lock()
		n := r.m.pool.conns["alpha"]
		r.m.pool.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d proxied sessions still open", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWireSeedsBindsBeforeServing(t *testing.T) {
	r := trustedRemote(t, false)
	pool := r.m.pool
	body := fmt.Sprintf("agent sha256:%x seed=worker\n", sha256.Sum256([]byte(testAgentToken)))
	if err := os.WriteFile(r.cfg.principalsFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.principals.reload(r.cfg.principalsFile); err != nil {
		t.Fatal(err)
	}
	wireSeeds(t.Context(), serveConfig{dataDir: t.TempDir(), port: 9222}, pool, r)
	t.Cleanup(r.mcp.children.close)
	if key, err := pool.seedKeyFor("worker"); err != nil || key != "worker" {
		t.Fatalf("bound seed refused before any reload: %q %v", key, err)
	}
	busy := *pool.busyCheck.Load()
	if busy("worker") {
		t.Fatal("idle seed reported busy")
	}
	pool.handover.front = "worker"
	if busy("worker") {
		t.Fatal("the front seed is pinned with no viewer open")
	}
	r.viewers.Add(1)
	if !busy("worker") {
		t.Fatal("an open viewer must pin the seed in front")
	}
	r.viewers.Add(-1)
	if _, err := r.mintTicket(ticket{principal: "agent", seed: "worker", kind: ticketViewer, expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if !busy("worker") {
		t.Fatal("a live ticket must pin its seed")
	}
	if r.raiseSeed == nil || pool.afterLaunch == nil {
		t.Fatal("handover hooks not wired")
	}
	if err := os.WriteFile(r.cfg.principalsFile, fmt.Appendf(nil, "agent sha256:%x\n", sha256.Sum256([]byte(testAgentToken))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.principals.reload(r.cfg.principalsFile); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.seedKeyFor("worker"); err == nil {
		t.Fatal("unbound seed still admitted after reload")
	}
}
