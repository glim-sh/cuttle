package serve

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
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
		{"no CDP here", http.MethodGet, "/json/version", bearer, 404},
		{"no secret API here", http.MethodGet, "/secret", bearer, 404},
		{"no root", http.MethodGet, "/", bearer, 404},
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
		cs := h.connect(t, testAgentToken, nil)
		callTool(t, cs, "browser_navigate", map[string]any{"url": "https://login.example/"})
		link := structured[linkOut](t, callTool(t, cs, "cuttle_viewer_link", map[string]any{"reason": "solve the <b>captcha</b>", "ttl_minutes": 10}))
		h.browser.mu.Lock()
		activated := slices.Clone(h.browser.activated)
		h.browser.mu.Unlock()
		if len(activated) != 1 || activated[0] == testOwnerPageID {
			t.Fatalf("minting a viewer link must bring the agent's own tab forward, activated %v", activated)
		}
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
		if code != http.StatusSeeOther || resp.Header.Get("Location") != "/v/" {
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
		if code, _, _ := h.do(t, http.MethodGet, "/v/", withCookie, nil); code == 401 {
			t.Fatal("the viewer session did not open /v/")
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

// fakeViewer is a viewer upstream on loopback: a page and an echoing socket.
func fakeViewer(t *testing.T, seen chan<- *http.Request) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(context.Background())
		if r.URL.Path != "/websockify" {
			_, _ = io.WriteString(w, "viewer page "+r.URL.Path)
			return
		}
		c, err := websocket.Accept(w, r, nil)
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
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestViewerProxy(t *testing.T) {
	seen := make(chan *http.Request, 16)
	viewer := fakeViewer(t, seen)
	h := newMCPHarness(t)
	vu, _ := url.Parse(viewer.URL)
	h.r.viewer = vu
	front := httptest.NewServer(h.r.handler())
	t.Cleanup(front.Close)
	h.r.startView(tokenHash("view-token"), "agent", time.Hour)
	cookie := viewerCookie + "=view-token"

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, front.URL+"/v/vnc.html?x=1", nil)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Authorization", "Bearer "+testAgentToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "viewer page /vnc.html" {
		t.Fatalf("proxied page %q", b)
	}
	if up := <-seen; up.Header.Get("Cookie") != "" || up.Header.Get("Authorization") != "" || up.URL.RawQuery != "x=1" {
		t.Fatalf("the viewer upstream saw %v %q", up.Header, up.URL.RawQuery)
	}

	wsURL := "ws" + strings.TrimPrefix(front.URL, "http") + "/v/websockify"
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
	for len(seen) > 0 {
		<-seen
	}

	// At session expiry the daemon closes the socket it relays.
	h.r.startView(tokenHash("short-token"), "agent", 300*time.Millisecond)
	short, err := dial(testPublicURL, viewerCookie+"=short-token")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = short.CloseNow() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, _, err := short.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("the socket outlived its viewer session: %v", err)
	}
}

func TestRevokeEndsWhatAPrincipalMinted(t *testing.T) {
	h := newMCPHarness(t)
	cs := h.connect(t, testOtherToken, nil)
	link := structured[linkOut](t, callTool(t, cs, "cuttle_viewer_link", map[string]any{"reason": "x"}))
	h.r.startView(tokenHash("other-view"), "other", time.Hour)
	h.r.startView(tokenHash("agent-view"), "agent", time.Hour)
	file := h.r.mintFile("other", t.TempDir(), "f")
	h.r.revoke(map[string]bool{"agent": true})
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
