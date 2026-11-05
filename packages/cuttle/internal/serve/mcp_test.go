package serve

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeBrowser stands in for Chrome: a page list the fake upstream mutates, and
// fixed answers for what the aggregator reads over CDP.
type fakeBrowser struct {
	mu        sync.Mutex
	list      []pageInfo
	next      int
	fields    []string // live password field values
	value     string
	activated []string
	reversed  bool // Target.getTargets need not list pages in Playwright's order
}

func (b *fakeBrowser) pages(context.Context) ([]pageInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := slices.Clone(b.list)
	if b.reversed {
		slices.Reverse(out)
	}
	return out, nil
}

func (b *fakeBrowser) add(u, opener string) pageInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	p := pageInfo{ID: "T" + strconv.Itoa(b.next), URL: u, Title: "page " + u, Opener: opener}
	b.list = append(b.list, p)
	return p
}

func (b *fakeBrowser) passwords(context.Context) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.fields)
}

func (b *fakeBrowser) cookies(context.Context) ([]originAuth, bool) {
	return []originAuth{{Domain: ".example.com", Cookies: 2}}, true
}

func (b *fakeBrowser) capture(_ context.Context, _ pageInfo, _ string, _ bool) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return []byte(b.value), nil
}

func (b *fakeBrowser) activate(_ context.Context, id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.activated = append(b.activated, id)
}

// fakeUpstream is an in-process Playwright MCP: session-bound, with the tab
// model of the real one (a shared list, a current tab per session).
type fakeUpstream struct {
	browser *fakeBrowser
	server  *mcp.Server

	mu         sync.Mutex
	current    map[string]string // session id -> page id
	calls      map[string]int
	failClosed bool
	onClosed   func() // runs as failClosed fires, under f.mu
	snapshot   string
	hidden     map[string]bool // pages the upstream lost track of

	gets       atomic.Int32
	violations atomic.Int32
}

func newFakeUpstream(t *testing.T, b *fakeBrowser) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{browser: b, current: map[string]string{}, calls: map[string]int{}}
	f.server = mcp.NewServer(&mcp.Implementation{Name: "Playwright", Version: "test"}, &mcp.ServerOptions{
		InitializedHandler: func(_ context.Context, req *mcp.InitializedRequest) {
			// Playwright MCP answers a roots capability with a roots/list request
			// on a stream the aggregator never opens.
			//nolint:staticcheck // checking that the deprecated capability is NOT sent
			if caps := req.Session.InitializeParams().Capabilities; caps != nil && caps.RootsV2 != nil {
				f.violations.Add(1)
			}
		},
	})
	obj := map[string]any{"type": "object"}
	add := func(name string, readOnly bool, h mcp.ToolHandler) {
		f.server.AddTool(&mcp.Tool{
			Name: name, Description: name + " upstream", InputSchema: obj,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly},
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			f.mu.Lock()
			f.calls[name]++
			f.mu.Unlock()
			return h(ctx, req)
		})
	}
	add("browser_tabs", false, f.tabs)
	add("browser_navigate", false, f.navigate)
	add("browser_snapshot", true, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: f.snapshot}}}, nil
	})
	add("browser_take_screenshot", true, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: []byte{1, 2, 3}, MIMEType: "image/png"}}}, nil
	})
	add("browser_resize", false, f.navigate)
	add("browser_run_code_unsafe", false, f.navigate)
	add("browser_wait_for", true, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if tok := req.Params.GetProgressToken(); tok != nil {
			for i := range 3 {
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: tok, Progress: float64(i + 1), Total: 3})
				time.Sleep(20 * time.Millisecond) // a long call: progress arrives while it is still running
			}
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "waited"}}}, nil
	})
	return f
}

func (f *fakeUpstream) handler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return f.server }, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			f.gets.Add(1)
			http.Error(w, "the standalone stream is never opened", http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasPrefix(r.Host, "localhost:") {
			f.violations.Add(1)
			http.Error(w, "Host not allowed", http.StatusForbidden)
			return
		}
		if v := r.Header.Get("Mcp-Protocol-Version"); v != "" && v != upstreamProtocol {
			f.violations.Add(1)
			http.Error(w, "unsupported protocol version", http.StatusBadRequest)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (f *fakeUpstream) navigations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls["browser_navigate"]
}

// visible is the upstream's tab list: creation order, minus what it lost.
func (f *fakeUpstream) visible() []pageInfo {
	f.browser.mu.Lock()
	defer f.browser.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(f.browser.list), func(p pageInfo) bool { return f.hidden[p.ID] })
}

func (f *fakeUpstream) list(sid string) string {
	pages := f.visible()
	var b strings.Builder
	b.WriteString("### Result\n")
	for i, p := range pages {
		cur := ""
		if f.current[sid] == p.ID || f.current[sid] == "" && i == 0 {
			cur = "(current) "
		}
		fmt.Fprintf(&b, "- %d: %s[%s](%s)\n", i, cur, p.Title, p.URL)
	}
	return b.String()
}

func (f *fakeUpstream) tabs(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Action string   `json:"action"`
		Index  *float64 `json:"index"`
		URL    string   `json:"url"`
	}
	_ = json.Unmarshal(req.Params.Arguments, &in)
	sid := req.Session.ID()
	f.mu.Lock()
	defer f.mu.Unlock()
	switch in.Action {
	case "new":
		u := in.URL
		if u == "" || strings.HasPrefix(u, "download:") {
			u = "about:blank"
		}
		f.current[sid] = f.browser.add(u, "").ID
		if strings.HasPrefix(in.URL, "download:") {
			return errorResult("### Error\nError: browserBackend.callTool: Download is starting"), nil
		}
	case "select", "close":
		pages := f.visible()
		i := 0
		if in.Index != nil {
			i = int(*in.Index)
		}
		if i >= len(pages) {
			return errorResult("no tab at that index"), nil
		}
		if in.Action == "select" {
			f.current[sid] = pages[i].ID
		} else {
			f.browser.mu.Lock()
			f.browser.list = slices.DeleteFunc(f.browser.list, func(p pageInfo) bool { return p.ID == pages[i].ID })
			f.browser.mu.Unlock()
		}
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: f.list(sid)}}}, nil
}

func (f *fakeUpstream) navigate(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(req.Params.Arguments, &in)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failClosed {
		f.failClosed = false
		if f.onClosed != nil {
			f.onClosed()
		}
		return errorResult("Error: " + msgTargetClosed), nil
	}
	id := f.current[req.Session.ID()]
	f.browser.mu.Lock()
	for i := range f.browser.list {
		if f.browser.list[i].ID == id {
			f.browser.list[i].URL = in.URL
		}
	}
	f.browser.mu.Unlock()
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "navigated to " + in.URL}}}, nil
}

// bearerTransport adds an Authorization header, the way the egress gateway does.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(req)
}

type mcpHarness struct {
	r        *remote
	browser  *fakeBrowser
	upstream *fakeUpstream
	srv      *httptest.Server
	seed     string
}

const (
	testPublicURL   = "https://browser.example"
	testAgentToken  = "agent-bearer"
	testOtherToken  = "other-bearer"
	testViewerAddr  = "http://127.0.0.1:1"
	testOwnerPageID = "OWNER"
)

func newMCPHarness(t *testing.T) *mcpHarness {
	t.Helper()
	return newMCPHarnessViewer(t, testViewerAddr)
}

// newMCPHarnessViewer is newMCPHarness with the viewer proxy pointed at viewer.
func newMCPHarnessViewer(t *testing.T, viewer string) *mcpHarness {
	t.Helper()
	b := &fakeBrowser{list: []pageInfo{{ID: testOwnerPageID, URL: "https://owner.example/", Title: "owner"}}}
	up := newFakeUpstream(t, b)
	upSrv := httptest.NewTestServer(t, up.handler())
	file := filepath.Join(t.TempDir(), "principals")
	body := fmt.Sprintf("agent sha256:%x\nother sha256:%x\n", sha256.Sum256([]byte(testAgentToken)), sha256.Sum256([]byte(testOtherToken)))
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := newTestPool(t, serveConfig{mode: modeSession}, (&fakeLauncher{port: 5100}).toLauncher())
	m := &multiplexer{pool: pool}
	r, err := newRemote(m, remoteConfig{listen: ":0", publicURL: testPublicURL, principalsFile: file, viewerUpstream: viewer},
		"http://127.0.0.1:8931/mcp", "localhost:8931", b)
	if err != nil {
		t.Fatal(err)
	}
	r.mcp.client = &http.Client{Transport: upstreamTransport{host: "localhost:8931", base: upSrv.Client().Transport}}
	t.Cleanup(r.close)
	seed, _ := pool.seedKeyFor("")
	t.Cleanup(func() { pool.secrets.dropSeed(seed) })
	return &mcpHarness{r: r, browser: b, upstream: up, srv: httptest.NewTestServer(t, r.handler()), seed: seed}
}

// freeLease lets the driving lease lapse the way an idle one expires, so a
// second principal can drive.
func (h *mcpHarness) freeLease() {
	leases := h.r.m.pool.leases
	leases.mu.Lock()
	defer leases.mu.Unlock()
	l := leases.held[h.seed]
	l.expires = time.Time{}
	leases.held[h.seed] = l
}

func (h *mcpHarness) client(token string) *http.Client {
	return &http.Client{Transport: bearerTransport{token: token, base: h.srv.Client().Transport}}
}

func (h *mcpHarness) connect(t *testing.T, token string, progress func(*mcp.ProgressNotificationParams)) *mcp.ClientSession {
	t.Helper()
	opts := &mcp.ClientOptions{}
	if progress != nil {
		opts.ProgressNotificationHandler = func(_ context.Context, req *mcp.ProgressNotificationClientRequest) { progress(req.Params) }
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "test"}, opts)
	cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: testPublicURL + "/mcp", HTTPClient: h.client(token), DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func structured[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	var out T
	b, err := json.Marshal(res.StructuredContent)
	if err != nil || json.Unmarshal(b, &out) != nil {
		t.Fatalf("no structured content in %q", resultText(res))
	}
	if strings.TrimSpace(resultText(res)) != strings.TrimSpace(string(b)) {
		t.Fatalf("text %q does not mirror structured content %s", resultText(res), b)
	}
	return out
}

func TestAggregatorToolList(t *testing.T) {
	h := newMCPHarness(t)
	cs := h.connect(t, testAgentToken, nil)
	var names []string
	for tool, err := range cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
		if tool.Name == toolScreenshot && !strings.Contains(tool.Description, "Refused for 30 s") {
			t.Error("the screenshot tool does not say it can be held after a capture")
		}
	}
	for _, want := range []string{
		"browser_navigate", "browser_snapshot", "browser_tabs", "cuttle_status", "cuttle_viewer_link",
		"cuttle_wait", "cuttle_secret_capture", "cuttle_request_secret", "cuttle_download",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("tool %s missing from %v", want, names)
		}
	}
	for _, hidden := range []string{"browser_resize", "browser_run_code_unsafe"} {
		if slices.Contains(names, hidden) {
			t.Errorf("hidden tool %s is listed", hidden)
		}
		if res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: hidden, Arguments: map[string]any{}}); err == nil && !res.IsError {
			t.Errorf("hidden tool %s ran", hidden)
		}
	}
	if h.upstream.gets.Load() != 0 {
		t.Error("the aggregator opened the upstream's standalone GET stream")
	}
	if h.upstream.violations.Load() != 0 {
		t.Error("the aggregator sent a Host or protocol version the upstream refuses")
	}
}

func TestAggregatorOpensOwnTabAndMasks(t *testing.T) {
	h := newMCPHarness(t)
	h.r.m.pool.secrets.put(h.seed, "API_KEY", []byte("held-secret-value"), sourceStdin, time.Minute)
	h.browser.fields = []string{"typed-password"}
	h.upstream.snapshot = "token held-secret-value and field typed-password shown"
	cs := h.connect(t, testAgentToken, nil)

	res := callTool(t, cs, "browser_navigate", map[string]any{"url": "https://site.example/"})
	if res.IsError {
		t.Fatalf("navigate: %s", resultText(res))
	}
	pages, _ := h.browser.pages(t.Context())
	if len(pages) != 2 || pages[0].URL != "https://owner.example/" || pages[1].URL != "https://site.example/" {
		t.Fatalf("the navigation did not land in a tab of the agent's own: %+v", pages)
	}
	if l, held := h.r.m.pool.leases.status(h.seed); !held || l.owner != "mcp:agent" {
		t.Fatalf("a driving call must take the lease as mcp:agent, got %+v", l)
	}

	text := resultText(callTool(t, cs, "browser_snapshot", nil))
	if strings.Contains(text, "held-secret-value") || strings.Contains(text, "typed-password") {
		t.Fatalf("a secret reached the agent: %q", text)
	}
	if !strings.Contains(text, "{{cuttle:API_KEY}}") || !strings.Contains(text, passwordPlaceholder) {
		t.Fatalf("masking did not leave sentinels: %q", text)
	}

	shot := callTool(t, cs, toolScreenshot, map[string]any{"scale": "css"})
	if len(shot.Content) != 1 {
		t.Fatalf("screenshot content %+v", shot.Content)
	}
	if img, ok := shot.Content[0].(*mcp.ImageContent); !ok || string(img.Data) != "\x01\x02\x03" {
		t.Fatal("image content did not pass through")
	}
}

func TestAggregatorTabOwnership(t *testing.T) {
	h := newMCPHarness(t)
	agent := h.connect(t, testAgentToken, nil)
	other := h.connect(t, testOtherToken, nil)
	callTool(t, agent, "browser_navigate", map[string]any{"url": "https://agent.example/"})
	h.freeLease()
	callTool(t, other, "browser_navigate", map[string]any{"url": "https://other.example/"})

	for _, tc := range []struct {
		name    string
		cs      *mcp.ClientSession
		args    map[string]any
		refused bool
	}{
		{"person's tab", agent, map[string]any{"action": "select", "index": 0}, true},
		{"other agent's tab", agent, map[string]any{"action": "select", "index": 2}, true},
		{"own tab", agent, map[string]any{"action": "select", "index": 1}, false},
		{"close person's tab", other, map[string]any{"action": "close", "index": 0}, true},
		{"no such tab", agent, map[string]any{"action": "close", "index": 9}, true},
		{"list", other, map[string]any{"action": "list"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.freeLease()
			res := callTool(t, tc.cs, toolTabs, tc.args)
			if res.IsError != tc.refused {
				t.Fatalf("refused=%v, want %v: %s", res.IsError, tc.refused, resultText(res))
			}
		})
	}
	pages, _ := h.browser.pages(t.Context())
	if len(pages) != 3 || pages[0].ID != testOwnerPageID {
		t.Fatalf("a refused close reached the browser: %+v", pages)
	}

	// Go reads JSON keys case-insensitively, JavaScript does not: what is checked
	// must be what is sent, or this selects the person's tab upstream.
	h.freeLease()
	callTool(t, agent, toolTabs, map[string]any{"action": "select", "index": 0, "ACTION": "list"})
	h.freeLease()
	callTool(t, agent, "browser_navigate", map[string]any{"url": "https://agent.example/again"})
	if now, _ := h.browser.pages(t.Context()); now[0].URL != "https://owner.example/" {
		t.Fatalf("a smuggled select drove the person's tab: %+v", now)
	}

	// A popup a principal's tab opened is the principal's too.
	pop := h.browser.add("https://popup.example/", pages[1].ID)
	h.freeLease()
	if res := callTool(t, agent, toolTabs, map[string]any{"action": "select", "index": 3}); res.IsError {
		t.Fatalf("selecting a popup of an own tab: %s", resultText(res))
	}
	status := structured[statusOut](t, callTool(t, agent, "cuttle_status", nil))
	owners := map[string]string{}
	for _, tab := range status.Tabs {
		owners[tab.URL] = tab.Owner
	}
	if owners["https://owner.example/"] != "person" || owners["https://agent.example/again"] != "agent" ||
		owners["https://other.example/"] != "other" || owners[pop.URL] != "agent" {
		t.Fatalf("status owners %v", owners)
	}
}

// Ownership follows the page, not its URL: with the person's tab and the
// agent's at the same address, the agent reaches only its own, even when
// Chrome lists pages in another order than Playwright.
func TestAggregatorTabOwnershipSameURL(t *testing.T) {
	h := newMCPHarness(t)
	h.browser.list[0].URL = "about:blank"
	h.browser.reversed = true
	cs := h.connect(t, testAgentToken, nil)
	tabs := func(args map[string]any) *mcp.CallToolResult {
		t.Helper()
		h.freeLease()
		return callTool(t, cs, toolTabs, args)
	}
	if res := tabs(map[string]any{"action": "new"}); res.IsError {
		t.Fatalf("new: %s", resultText(res))
	}
	for _, action := range []string{"select", "close"} {
		if res := tabs(map[string]any{"action": action, "index": 0}); !res.IsError {
			t.Fatalf("%s reached the person's tab at the same URL", action)
		}
	}
	if res := tabs(map[string]any{"action": "new"}); res.IsError {
		t.Fatalf("second new: %s", resultText(res))
	}
	first := h.upstream.visible()[1].ID
	if res := tabs(map[string]any{"action": "select", "index": 1}); res.IsError {
		t.Fatalf("selecting an own tab that shares its URL with the person's: %s", resultText(res))
	}
	if res := tabs(map[string]any{"action": "close", "index": 1}); res.IsError {
		t.Fatalf("closing an own tab that shares its URL with the person's: %s", resultText(res))
	}
	left := h.upstream.visible()
	if len(left) != 2 || left[0].ID != testOwnerPageID || left[1].ID == first {
		t.Fatalf("the wrong tab closed: %+v", left)
	}
	owners := map[string]string{}
	for _, tab := range structured[statusOut](t, callTool(t, cs, "cuttle_status", nil)).Tabs {
		owners[tab.Title] = tab.Owner
	}
	if owners["owner"] != "person" || owners["page about:blank"] != "agent" {
		t.Fatalf("status owners %v", owners)
	}
}

// A tab whose navigation became a download stays the agent's, and when the
// upstream's list and Chrome's disagree, the person's tab is never handed over.
func TestAggregatorDownloadTabStaysOwn(t *testing.T) {
	h := newMCPHarness(t)
	h.browser.list[0].URL = "about:blank"
	cs := h.connect(t, testAgentToken, nil)
	if res := callTool(t, cs, toolTabs, map[string]any{"action": "new", "url": "download:file"}); !res.IsError {
		t.Fatal("the fake upstream should fail the navigation")
	}
	pages, _ := h.browser.pages(t.Context())
	status := structured[statusOut](t, callTool(t, cs, "cuttle_status", nil))
	if len(pages) != 2 || len(status.Tabs) != 2 || status.Tabs[1].Owner != "agent" {
		t.Fatalf("the download tab is not the agent's: %+v", status.Tabs)
	}

	h.upstream.mu.Lock()
	h.upstream.hidden = map[string]bool{pages[1].ID: true}
	h.upstream.mu.Unlock()
	if res := callTool(t, cs, toolTabs, map[string]any{"action": "close", "index": 0}); !res.IsError {
		t.Fatal("with the lists out of step, a close reached the person's tab")
	}

	h.upstream.mu.Lock()
	h.upstream.hidden = nil
	h.upstream.mu.Unlock()
	if res := callTool(t, cs, toolTabs, map[string]any{"action": "close", "index": 1}); res.IsError {
		t.Fatalf("closing the download tab: %s", resultText(res))
	}
	if now, _ := h.browser.pages(t.Context()); len(now) != 1 || now[0].ID != testOwnerPageID {
		t.Fatalf("pages after the close: %+v", now)
	}
}

func TestAggregatorLeaseRefusal(t *testing.T) {
	h := newMCPHarness(t)
	cs := h.connect(t, testAgentToken, nil)
	fresh := h.connect(t, testOtherToken, nil)
	callTool(t, cs, "browser_navigate", map[string]any{"url": "https://own.example/"})
	h.freeLease()
	if _, err := h.r.m.pool.leases.acquire(h.seed, "cuttle pw (host)", ""); err != nil {
		t.Fatal(err)
	}
	if res := callTool(t, fresh, "browser_snapshot", nil); !res.IsError {
		t.Fatal("a read-only call that has to open a tab first changes the visible tab and needs the lease")
	}
	calls := h.upstream.navigations()
	res := callTool(t, cs, "browser_navigate", map[string]any{"url": "https://site.example/"})
	if !res.IsError || !strings.Contains(resultText(res), "cuttle pw (host)") {
		t.Fatalf("a driving call while another client holds the lease: %s", resultText(res))
	}
	if h.upstream.navigations() != calls {
		t.Fatal("a refused call reached the browser")
	}
	if res := callTool(t, cs, "browser_snapshot", nil); res.IsError {
		t.Fatalf("a read-only call on an own tab must pass the lease: %s", resultText(res))
	}
	if res := callTool(t, fresh, "cuttle_secret_capture", map[string]any{"name": "X", "selector": "#x"}); !res.IsError {
		t.Fatal("capture drives the page and must need the lease too")
	}
}

func TestAggregatorTakeoverIsNotTakenBack(t *testing.T) {
	h := newMCPHarness(t)
	cs := h.connect(t, testAgentToken, nil)
	callTool(t, cs, "browser_navigate", map[string]any{"url": "https://own.example/"})
	h.r.m.pool.leases.forceRelease(h.seed, "cuttle pw")
	for range 2 {
		res := callTool(t, cs, "browser_navigate", map[string]any{"url": "https://again.example/"})
		if !res.IsError || !strings.Contains(resultText(res), "browser over") && !strings.Contains(resultText(res), "taken over") {
			t.Fatalf("a driving call right after a takeover: %s", resultText(res))
		}
	}
	if l, held := h.r.m.pool.leases.status(h.seed); held {
		t.Fatalf("the agent took the lease back from the takeover: %+v", l.owner)
	}
	if res := callTool(t, cs, "browser_snapshot", nil); res.IsError {
		t.Fatalf("read-only calls still pass after a takeover: %s", resultText(res))
	}
	if res := callTool(t, cs, toolTabs, map[string]any{"action": "list"}); res.IsError {
		t.Fatalf("listing tabs is read-only and still passes after a takeover: %s", resultText(res))
	}
	if res := callTool(t, cs, toolTabs, map[string]any{"action": "select", "index": 1}); !res.IsError {
		t.Fatal("selecting a tab drives and is refused after a takeover")
	}
}

func TestAggregatorProgressAndRecovery(t *testing.T) {
	h := newMCPHarness(t)
	var got atomic.Int32
	cs := h.connect(t, testAgentToken, func(p *mcp.ProgressNotificationParams) {
		if p.ProgressToken == "agent-token" {
			got.Add(1)
		}
	})
	params := &mcp.CallToolParams{Name: "browser_wait_for", Arguments: map[string]any{}}
	params.SetProgressToken("agent-token")
	if _, err := cs.CallTool(t.Context(), params); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); got.Load() < 2 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	// The last one can race the result, which is the client's to order.
	if got.Load() < 2 {
		t.Fatalf("relayed %d progress notifications under the agent's own token, want at least 2", got.Load())
	}

	// The action closed the agent's own tab while another own tab lives: it is
	// not replayed there, since it may have done its work.
	callTool(t, cs, "browser_navigate", map[string]any{"url": "https://first.example/"})
	h.browser.add("https://second.example/", "")
	h.r.mcp.mu.Lock()
	s := h.r.mcp.agents["agent"]
	h.r.mcp.mu.Unlock()
	s.mu.Lock()
	for _, p := range h.browser.list {
		s.owned[p.ID] = p.ID != testOwnerPageID
	}
	s.mu.Unlock()
	navigated := h.upstream.navigations()
	h.upstream.mu.Lock()
	h.upstream.failClosed = true
	h.upstream.mu.Unlock()
	if res := callTool(t, cs, "browser_navigate", map[string]any{"url": "https://a.example/"}); !res.IsError {
		t.Fatal("a call whose tab closed under it was replayed on another own tab")
	}
	if n := h.upstream.navigations() - navigated; n != 1 {
		t.Fatalf("navigate reached the upstream %d times, want 1", n)
	}

	// Chrome relaunched under the upstream, taking every tab with it: one retry
	// lands in a fresh own tab.
	navigated = h.upstream.navigations()
	h.upstream.mu.Lock()
	h.upstream.failClosed = true
	h.upstream.onClosed = func() {
		h.browser.mu.Lock()
		h.browser.list = []pageInfo{{ID: "RELAUNCHED", URL: "https://owner.example/", Title: "owner"}}
		h.browser.mu.Unlock()
	}
	h.upstream.mu.Unlock()
	if res := callTool(t, cs, "browser_navigate", map[string]any{"url": "https://a.example/"}); res.IsError {
		t.Fatalf("the closed-target retry did not recover: %s", resultText(res))
	}
	if n := h.upstream.navigations() - navigated; n != 2 {
		t.Fatalf("navigate reached the upstream %d times, want 2", n)
	}

	// The upstream forgot the session: the agent is told, then the next call
	// works on a fresh session and a tab of its own.
	for ss := range h.upstream.server.Sessions() {
		_ = ss.Close()
	}
	res := callTool(t, cs, "browser_navigate", map[string]any{"url": "https://b.example/"})
	if !res.IsError || !strings.Contains(resultText(res), "restarted") {
		t.Fatalf("after the upstream lost the session: %s", resultText(res))
	}
	before, _ := h.browser.pages(t.Context())
	if res := callTool(t, cs, "browser_navigate", map[string]any{"url": "https://c.example/"}); res.IsError {
		t.Fatalf("after re-initializing: %s", resultText(res))
	}
	after, _ := h.browser.pages(t.Context())
	if len(after) != len(before) || after[0].URL != "https://owner.example/" || after[1].URL != "https://c.example/" {
		t.Fatalf("a fresh upstream session must go back to the agent's own tab, not open another: %+v", after)
	}
}

func TestAggregatorRelistsChangedTools(t *testing.T) {
	h := newMCPHarness(t)
	cs := h.connect(t, testAgentToken, nil)
	h.upstream.server.AddTool(&mcp.Tool{Name: "browser_new_thing", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
		})
	h.upstream.server.RemoveTools("browser_wait_for")
	h.r.mcp.mu.Lock()
	s := h.r.mcp.agents["agent"]
	h.r.mcp.mu.Unlock()
	s.callMu.Lock()
	err := h.r.mcp.relistLocked(t.Context(), s)
	s.callMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tool, err := range cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	if !slices.Contains(names, "browser_new_thing") || slices.Contains(names, "browser_wait_for") {
		t.Fatalf("tool list after the upstream changed: %v", names)
	}
}

func TestCaptureBindsOriginAndHoldsScreenshots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newMCPHarness(t)
		h.browser.value = "captured-value-1"
		cs := h.connect(t, testAgentToken, nil)
		callTool(t, cs, "browser_navigate", map[string]any{"url": "https://login.example:443/form"})
		out := structured[captureOut](t, callTool(t, cs, "cuttle_secret_capture", map[string]any{"name": "OTP", "selector": "#code"}))
		if out.Sentinel != "{{cuttle:OTP}}" || out.Length != 16 || out.BoundOrigin != "https://login.example" {
			t.Fatalf("capture answer %+v", out)
		}
		info := h.r.m.pool.secrets.list(h.seed)
		if len(info) != 1 || info[0].BoundOrigin != "https://login.example" || info[0].Source != sourceCapture {
			t.Fatalf("stored %+v", info)
		}
		h.upstream.mu.Lock()
		h.upstream.snapshot = "code captured-value-1"
		h.upstream.mu.Unlock()
		if text := resultText(callTool(t, cs, "browser_snapshot", nil)); strings.Contains(text, "captured-value-1") {
			t.Fatal("a captured value must join the masking set at once")
		}
		if res := callTool(t, cs, toolScreenshot, map[string]any{"scale": "css"}); !res.IsError {
			t.Fatal("a screenshot right after a capture must be refused")
		}
		time.Sleep(screenshotHold)
		if res := callTool(t, cs, toolScreenshot, map[string]any{"scale": "css"}); res.IsError {
			t.Fatalf("the hold must lift after 30 s: %s", resultText(res))
		}
	})
}

// A URL change (a fragment, history.replaceState) does not take the value off
// the screen, so the hold does not lift on one.
func TestScreenshotHoldSurvivesNavigation(t *testing.T) {
	h := newMCPHarness(t)
	h.browser.value = "captured-value-2"
	cs := h.connect(t, testAgentToken, nil)
	callTool(t, cs, "browser_navigate", map[string]any{"url": "https://login.example/"})
	callTool(t, cs, "cuttle_secret_capture", map[string]any{"name": "OTP", "from_clipboard": true})
	callTool(t, cs, "browser_navigate", map[string]any{"url": "https://login.example/#done"})
	if res := callTool(t, cs, toolScreenshot, map[string]any{"scale": "css"}); !res.IsError {
		t.Fatal("a URL change lifted the screenshot hold")
	}
}

func TestCuttleWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newMCPHarness(t)
		var progress atomic.Int32
		cs := h.connect(t, testAgentToken, func(*mcp.ProgressNotificationParams) { progress.Add(1) })
		wait := func(until string, minutes int) waitOut {
			params := &mcp.CallToolParams{Name: "cuttle_wait", Arguments: map[string]any{"until": until, "timeout_minutes": minutes}}
			params.SetProgressToken("w")
			res, err := cs.CallTool(t.Context(), params)
			if err != nil {
				t.Fatal(err)
			}
			return structured[waitOut](t, res)
		}
		if out := wait("url:https://owner.example/*", 1); !out.Met || out.URL != "https://owner.example/" {
			t.Fatalf("an already-true condition: %+v", out)
		}
		if out := wait("gone:https://owner.*", 1); out.Met || out.WaitedS != 60 {
			t.Fatalf("a timeout must answer met:false after the timeout: %+v", out)
		}
		if n := progress.Load(); n < 3 {
			t.Fatalf("%d progress notifications in a minute, want one per 15 s", n)
		}
		go func() {
			time.Sleep(90 * time.Second)
			h.r.m.pool.secrets.putBound(h.seed, "PIN_CODE", []byte("123456789"), sourceHumanForm, time.Hour, "https://a.example")
		}()
		if out := wait("secret:PIN_CODE", 5); !out.Met || out.WaitedS < 90 || out.WaitedS > 92 {
			t.Fatalf("secret wait: %+v", out)
		}
		if res := callTool(t, cs, "cuttle_wait", map[string]any{"until": "cookie:x"}); !res.IsError {
			t.Fatal("an unknown condition must be refused")
		}
	})
}

func TestCompileGlob(t *testing.T) {
	for _, tc := range []struct {
		glob, s string
		want    bool
	}{
		{"https://*.example/*", "https://a.example/login", true},
		{"https://example.com/", "https://example.com/", true},
		{"https://example.com/", "https://example.com/x", false},
		{"*callback?code=*", "https://a/callback?code=1", true},
		{"https://a.example/?x", "https://a.example/?x", true},
		{"https://a.example/?x", "https://a.example/ax", false},
		{"(", "(", true},
	} {
		if got := compileGlob(tc.glob).MatchString(tc.s); got != tc.want {
			t.Errorf("compileGlob(%q) on %q = %v", tc.glob, tc.s, got)
		}
	}
}

func TestDownloadHandles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newMCPHarness(t)
		dir := driverDownloadsDir(h.r.m.pool.dataDir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		h.r.m.pool.secrets.put(h.seed, "API_KEY", []byte("held-secret-value"), sourceStdin, 2*time.Hour)
		if err := os.WriteFile(filepath.Join(dir, "report.csv"), []byte("a,b\nheld-secret-value,2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		if err := os.WriteFile(filepath.Join(dir, "page-2026-11-04T22-33-59-657Z.yml"), []byte("snapshot"), 0o600); err != nil {
			t.Fatal(err)
		}
		cs := h.connect(t, testAgentToken, nil)
		out := structured[downloadOut](t, callTool(t, cs, "cuttle_download", map[string]any{"name": "latest"}))
		if out.Name != "report.csv" || out.Size != 24 || !strings.HasPrefix(out.URL, testPublicURL+"/files/") {
			t.Fatalf("latest download %+v", out)
		}
		if strings.Contains(out.Text, "held-secret-value") || !strings.Contains(out.Text, "{{cuttle:API_KEY}}") {
			t.Fatalf("inlined text must be masked: %q", out.Text)
		}
		for _, name := range []string{"../secret", "page-2026-11-04T22-33-59-657Z.yml"} {
			if res := callTool(t, cs, "cuttle_download", map[string]any{"name": name}); !res.IsError {
				t.Fatalf("%s must not be served as a download", name)
			}
		}
		u, _ := url.Parse(out.URL)
		get := func(token string) (int, string) {
			resp, err := h.client(token).Get(testPublicURL + u.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(body)
		}
		if code, body := get(testAgentToken); code != http.StatusOK || body != "a,b\nheld-secret-value,2\n" {
			t.Fatalf("own handle: %d %q", code, body)
		}
		for _, token := range []string{testOtherToken, ""} {
			if code, _ := get(token); code == http.StatusOK {
				t.Fatalf("a handle opened for token %q", token)
			}
		}
		time.Sleep(fileHandleTTL)
		if code, _ := get(testAgentToken); code != http.StatusNotFound {
			t.Fatalf("an expired handle answered %d", code)
		}
	})
}
