package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP aggregator: an MCP server to remote agents and an MCP client to the
// Playwright MCP child, which attaches through this daemon's CDP mux so
// humanized input and sentinel substitution apply to every action it takes.
//
// Toward agents it is stateless (no session id rides the wire): the bearer
// names the caller on every request, and the caller's upstream state - one
// Playwright MCP session, its tabs, its lease - is keyed by that principal.
// Toward Playwright MCP it speaks what that server can: 2025-11-25 sessions,
// Host localhost, and never the standalone GET stream, which it pings every 3 s
// and kills after 5 s unanswered.

const (
	playwrightMCPPort   = 8931
	playwrightMCPScript = "/opt/playwright-cli/lib/node_modules/@playwright/cli/node_modules/playwright/cli.js"
	upstreamProtocol    = "2025-11-25"
	upstreamIdle        = 30 * time.Minute
	upstreamReadyWait   = 15 * time.Second
	closedRetryDelay    = 500 * time.Millisecond
	screenshotHold      = 30 * time.Second

	toolTabs       = "browser_tabs"
	toolScreenshot = "browser_take_screenshot"
	// msgTargetClosed is what the first call after cuttle relaunched Chrome
	// fails with; one retry half a second later lands on the new browser.
	msgTargetClosed = "Target page, context or browser has been closed"
)

// hiddenTools never reach an agent. browser_resize would contradict the
// persona's screen and window; browser_install is meaningless in the image;
// browser_run_code_unsafe runs arbitrary code in the Playwright process, inside
// the pod and on the loopback side of every boundary this surface draws.
var hiddenTools = map[string]bool{"browser_resize": true, "browser_install": true, "browser_run_code_unsafe": true}

const mcpInstructions = "This is a signed-in personal browser that a person also uses. " +
	"If a page needs a human (login, captcha, 2FA, consent): call cuttle_viewer_link, send the url to the user " +
	"with one line of what to do, then cuttle_wait. Type stored secrets as {{cuttle:NAME}}; never type a password " +
	"you read from a page or a message - if you need one, call cuttle_request_secret and send that link instead. " +
	"Your first browser action opens a tab of your own; you can only select or close tabs you opened. " +
	"Action results link their snapshot file instead of inlining it: call browser_snapshot to read the page. " +
	"Fetch files with cuttle_download."

const screenshotNote = "Refused for 30 s on a tab right after cuttle_secret_capture read a value from it; navigate away first."

// pageInfo is one page target, as Target.getTargets reports it.
type pageInfo struct {
	ID, URL, Title, Opener string
}

// browserOps is everything the aggregator reads from Chrome directly.
type browserOps interface {
	pages(ctx context.Context) ([]pageInfo, error)
	passwords(ctx context.Context) []string
	cookies(ctx context.Context) ([]originAuth, bool)
	capture(ctx context.Context, page pageInfo, selector string, clipboard bool) ([]byte, error)
	activate(ctx context.Context, pageID string)
}

var errBrowserDown = errors.New("the browser is not running")

// cdpBrowser reads the session browser over its own CDP port. It never
// attaches to a page except to read password fields and to capture, which is
// exactly what the loopback routes already do.
type cdpBrowser struct{ pool *chromePool }

func (b cdpBrowser) port() (int, error) {
	seed, lerr := b.pool.seedKeyFor("")
	if lerr != nil {
		return 0, lerr
	}
	inst := b.pool.runningInstance(seed)
	if inst == nil {
		return 0, errBrowserDown
	}
	return inst.cdpPort, nil
}

func (b cdpBrowser) pages(ctx context.Context) ([]pageInfo, error) {
	port, err := b.port()
	if err != nil {
		inst, lerr := b.pool.getOrLaunch(ctx, connectRequest{})
		if lerr != nil {
			return nil, err
		}
		port = inst.cdpPort
	}
	conn, err := dialBrowser(ctx, port)
	if err != nil {
		return nil, err
	}
	defer conn.close()
	raw, err := conn.callRaw(ctx, "", "Target.getTargets", map[string]any{})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Result struct {
			TargetInfos []struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
				Title    string `json:"title"`
				URL      string `json:"url"`
				OpenerID string `json:"openerId"`
			} `json:"targetInfos"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%w: %w", errCDPCall, err)
	}
	out := []pageInfo{}
	for _, t := range parsed.Result.TargetInfos {
		if t.Type == targetPage {
			out = append(out, pageInfo{ID: t.TargetID, URL: t.URL, Title: t.Title, Opener: t.OpenerID})
		}
	}
	return out, nil
}

func (b cdpBrowser) passwords(ctx context.Context) []string {
	port, err := b.port()
	if err != nil {
		return nil
	}
	return passwordFieldValues(ctx, port)
}

func (b cdpBrowser) cookies(ctx context.Context) ([]originAuth, bool) {
	port, err := b.port()
	if err != nil {
		return nil, false
	}
	return browserCookieDomains(ctx, port)
}

func (b cdpBrowser) capture(ctx context.Context, page pageInfo, selector string, clipboard bool) ([]byte, error) {
	port, err := b.port()
	if err != nil {
		return nil, err
	}
	if clipboard {
		return captureClipboardIn(ctx, port, page.ID, page.URL)
	}
	return captureSelectorIn(ctx, port, page.ID, selector)
}

func (b cdpBrowser) activate(ctx context.Context, pageID string) {
	port, err := b.port()
	if err != nil {
		return
	}
	conn, err := dialBrowser(ctx, port)
	if err != nil {
		return
	}
	defer conn.close()
	_, _ = conn.call(ctx, "", "Target.activateTarget", map[string]any{keyTargetID: pageID})
}

// relay maps one upstream progress token back to the agent call it belongs to.
// It keeps that call's context: a stateless server can only send a
// notification on the stream of the request it is made in context of.
type relay struct {
	ctx     context.Context //nolint:containedctx // see above
	session *mcp.ServerSession
	token   any
}

type capturedTab struct {
	url string
	at  time.Time
}

// agentSession is one principal's state. callMu serializes its upstream calls:
// the tab bookkeeping reads the browser before and after a call and cannot
// interleave with another of the same principal's calls.
type agentSession struct {
	principal string
	server    *mcp.Server
	callMu    sync.Mutex

	mu         sync.Mutex
	cs         *mcp.ClientSession
	loaded     bool
	upstream   []string // pass-through tool names currently registered
	owned      map[string]bool
	current    string
	leaseToken string
	captured   map[string]capturedTab
	idle       *time.Timer
}

type aggregator struct {
	r        *remote
	endpoint string
	browser  browserOps
	client   *http.Client
	h        http.Handler

	mu     sync.Mutex
	agents map[string]*agentSession
	// tabMu serializes tab creation across principals, so the before/after
	// diff of the target list names exactly one tab.
	tabMu   sync.Mutex
	relays  sync.Map
	nextTok atomic.Uint64
}

// upstreamTransport rewrites what Playwright MCP refuses: any Host but
// localhost:<port>, and any protocol version but 2025-11-25.
type upstreamTransport struct {
	host string
	base http.RoundTripper
}

func (t upstreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Host = t.host
	if req.Header.Get("Mcp-Protocol-Version") != "" {
		req.Header.Set("Mcp-Protocol-Version", upstreamProtocol)
	}
	return t.base.RoundTrip(req) //nolint:wrapcheck // a transport passes errors through
}

func newAggregator(r *remote, endpoint string, browser browserOps) *aggregator {
	host := "localhost"
	if u, err := url.Parse(endpoint); err == nil && u.Port() != "" {
		host += ":" + u.Port()
	}
	a := &aggregator{
		r: r, endpoint: endpoint, browser: browser, agents: map[string]*agentSession{},
		client: &http.Client{Transport: upstreamTransport{host: host, base: &http.Transport{Proxy: nil}}},
	}
	a.h = mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return a.serverFor(req.Context(), principalOf(req.Context()))
	}, &mcp.StreamableHTTPOptions{
		Stateless: true, DisableLocalhostProtection: true, PropagateRequestCancellation: true,
	})
	return a
}

func (a *aggregator) handler() http.Handler { return a.h }

func (a *aggregator) seed() string {
	seed, _ := a.r.m.pool.seedKeyFor("")
	return seed
}

func (a *aggregator) close() {
	a.mu.Lock()
	agents := a.agents
	a.agents = map[string]*agentSession{}
	a.mu.Unlock()
	for _, s := range agents {
		s.mu.Lock()
		if s.idle != nil {
			s.idle.Stop()
		}
		cs := s.cs
		s.cs = nil
		s.mu.Unlock()
		if cs != nil {
			_ = cs.Close()
		}
	}
}

// serverFor returns the principal's MCP server, its tool list loaded from the
// upstream when it can be.
func (a *aggregator) serverFor(ctx context.Context, principal string) *mcp.Server {
	a.mu.Lock()
	s := a.agents[principal]
	if s == nil {
		s = &agentSession{principal: principal, owned: map[string]bool{}, captured: map[string]capturedTab{}}
		s.server = mcp.NewServer(&mcp.Implementation{Name: "cuttle", Version: "1"}, &mcp.ServerOptions{
			Instructions: mcpInstructions,
			Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		})
		s.server.AddReceivingMiddleware(a.maskMiddleware)
		a.addCuttleTools(s)
		a.agents[principal] = s
	}
	a.mu.Unlock()
	a.loadTools(ctx, s)
	return s.server
}

// loadTools registers the upstream's tools once. The child may still be
// starting when the first agent connects, so it is waited for briefly; a miss
// leaves only cuttle's own tools and is retried on the next request.
func (a *aggregator) loadTools(ctx context.Context, s *agentSession) {
	s.mu.Lock()
	loaded := s.loaded
	s.mu.Unlock()
	if loaded {
		return
	}
	s.callMu.Lock()
	defer s.callMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, upstreamReadyWait)
	defer cancel()
	for {
		err := a.relistLocked(ctx, s)
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			logWarn("remote: the Playwright MCP upstream is not answering, %s gets cuttle's own tools only for now: %v", s.principal, err)
			return
		case <-time.After(closedRetryDelay):
		}
	}
}

// relistLocked re-reads the upstream tool list and re-registers it; callMu held.
func (a *aggregator) relistLocked(ctx context.Context, s *agentSession) error {
	cs, err := a.session(ctx, s)
	if err != nil {
		return err
	}
	var tools []*mcp.Tool
	for t, err := range cs.Tools(ctx, nil) {
		if err != nil {
			a.reset(s)
			return fmt.Errorf("list upstream tools: %w", err)
		}
		tools = append(tools, t)
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if hiddenTools[t.Name] || strings.HasPrefix(t.Name, "cuttle_") {
			continue
		}
		tool := *t
		if tool.Name == toolScreenshot {
			tool.Description = strings.TrimSpace(tool.Description + " " + screenshotNote)
		}
		readOnly := t.Annotations != nil && t.Annotations.ReadOnlyHint
		s.server.AddTool(&tool, a.passthrough(s, t.Name, readOnly))
		names = append(names, t.Name)
	}
	s.mu.Lock()
	gone := []string{}
	for _, n := range s.upstream {
		if !slices.Contains(names, n) {
			gone = append(gone, n)
		}
	}
	s.upstream, s.loaded = names, true
	s.mu.Unlock()
	s.server.RemoveTools(gone...)
	return nil
}

// session returns the principal's upstream session, opening it when needed.
func (a *aggregator) session(ctx context.Context, s *agentSession) (*mcp.ClientSession, error) {
	s.mu.Lock()
	cs := s.cs
	s.mu.Unlock()
	if cs != nil {
		return cs, nil
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "cuttle", Version: "1"}, &mcp.ClientOptions{
		// No roots: the SDK advertises them by default, and Playwright MCP then
		// asks for them over the GET stream this client never opens, stalling the
		// session's first tool call for about a minute [measured].
		Capabilities:                &mcp.ClientCapabilities{},
		ProgressNotificationHandler: a.relayProgress,
		ToolListChangedHandler: func(ctx context.Context, _ *mcp.ToolListChangedRequest) {
			// Off the notification's goroutine: it can arrive during a call that
			// holds callMu.
			go func() {
				s.callMu.Lock()
				defer s.callMu.Unlock()
				if err := a.relistLocked(context.WithoutCancel(ctx), s); err != nil {
					logWarn("remote: re-listing upstream tools for %s failed: %v", s.principal, err)
				}
			}()
		},
	})
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: a.endpoint, HTTPClient: a.client, DisableStandaloneSSE: true, MaxRetries: -1,
	}, &mcp.ClientSessionOptions{ProtocolVersion: upstreamProtocol})
	if err != nil {
		return nil, fmt.Errorf("connect to the Playwright MCP upstream: %w", err)
	}
	s.mu.Lock()
	s.cs = cs
	s.mu.Unlock()
	return cs, nil
}

// reset forgets the upstream session: the next call opens a fresh one, whose
// current tab is the browser's first, so the principal's own tab is reopened.
func (a *aggregator) reset(s *agentSession) {
	s.mu.Lock()
	cs := s.cs
	s.cs, s.current = nil, ""
	s.mu.Unlock()
	if cs != nil {
		go func() { _ = cs.Close() }()
	}
}

func (a *aggregator) relayProgress(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
	v, ok := a.relays.Load(fmt.Sprint(req.Params.ProgressToken))
	if !ok {
		return
	}
	rl, _ := v.(relay)
	_ = rl.session.NotifyProgress(rl.ctx, &mcp.ProgressNotificationParams{
		ProgressToken: rl.token, Progress: req.Params.Progress, Total: req.Params.Total, Message: req.Params.Message,
	})
}

// touch restarts the idle clock: an upstream session nobody used for 30
// minutes is closed, and reopened on the next call.
func (a *aggregator) touch(s *agentSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idle == nil {
		s.idle = time.AfterFunc(upstreamIdle, func() { a.expireIdle(s) })
		return
	}
	s.idle.Reset(upstreamIdle)
}

func (a *aggregator) expireIdle(s *agentSession) {
	if !s.callMu.TryLock() {
		a.touch(s)
		return
	}
	defer s.callMu.Unlock()
	s.mu.Lock()
	idle := s.cs != nil
	s.mu.Unlock()
	if idle {
		logInfo("remote: closing the idle upstream session of %s", s.principal)
		a.reset(s)
	}
}

func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

func resultText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func targetClosed(res *mcp.CallToolResult, err error) bool {
	if err != nil {
		return strings.Contains(err.Error(), msgTargetClosed)
	}
	return res != nil && res.IsError && strings.Contains(resultText(res), msgTargetClosed)
}

func rawArgs(req *mcp.CallToolRequest) any {
	if req == nil || req.Params == nil || len(bytes.TrimSpace(req.Params.Arguments)) == 0 || string(req.Params.Arguments) == "null" {
		return map[string]any{} // Playwright MCP refuses null arguments
	}
	return req.Params.Arguments
}

// passthrough forwards one upstream tool, inside cuttle's rules: the lease for
// a driving call, the principal's own tab and the screenshot hold. Masking is
// the server middleware's, so it covers cuttle's own tools too.
func (a *aggregator) passthrough(s *agentSession, name string, readOnly bool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		s.callMu.Lock()
		defer s.callMu.Unlock()
		a.touch(s)
		if !readOnly {
			if refused := a.holdLease(s); refused != nil {
				return refused, nil
			}
		}
		res, err := a.forward(ctx, s, name, req)
		if err != nil {
			return errorResult("cuttle: the browser automation call failed: " + err.Error()), nil
		}
		return res, nil
	}
}

func (a *aggregator) forward(ctx context.Context, s *agentSession, name string, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if name == toolTabs {
		return a.tabs(ctx, s, req)
	}
	if err := a.ensureOwnTab(ctx, s); err != nil {
		return errorResult(err.Error()), nil
	}
	if name == toolScreenshot {
		if msg := a.screenshotRefusal(ctx, s); msg != "" {
			return errorResult(msg), nil
		}
	}
	return a.call(ctx, s, name, rawArgs(req), req)
}

// call makes one upstream tool call, relaying progress and recovering from the
// two ways the upstream session dies under it.
func (a *aggregator) call(ctx context.Context, s *agentSession, name string, args any, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	params := &mcp.CallToolParams{Name: name, Arguments: args}
	if req != nil && req.Params != nil {
		if tok := req.Params.GetProgressToken(); tok != nil {
			up := "cuttle-" + strconv.FormatUint(a.nextTok.Add(1), 10)
			a.relays.Store(up, relay{ctx: ctx, session: req.Session, token: tok})
			defer a.relays.Delete(up)
			params.SetProgressToken(up)
		}
	}
	for attempt := 0; ; attempt++ {
		cs, err := a.session(ctx, s)
		if err != nil {
			return nil, err
		}
		res, err := cs.CallTool(ctx, params)
		if err != nil && (errors.Is(err, mcp.ErrSessionMissing) || errors.Is(err, mcp.ErrConnectionClosed)) {
			a.reset(s)
			return errorResult("cuttle: the browser automation session behind this agent restarted, so element refs and" +
				" the tab selection from before are gone and nothing was done. Take a new browser_snapshot and act on that."), nil
		}
		if attempt == 0 && targetClosed(res, err) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err() //nolint:wrapcheck // the caller's own cancellation
			case <-time.After(closedRetryDelay):
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return res, nil
	}
}

// holdLease takes or renews the driving lease as mcp:<principal>, or returns
// the refusal to send instead.
func (a *aggregator) holdLease(s *agentSession) *mcp.CallToolResult {
	leases := a.r.m.pool.leases
	seed, owner := a.seed(), "mcp:"+s.principal
	s.mu.Lock()
	token := s.leaseToken
	s.mu.Unlock()
	got, err := leases.acquire(seed, owner, token)
	if errors.Is(err, errLeaseLost) {
		got, err = leases.acquire(seed, owner, "")
	}
	if err != nil {
		v := leases.leaseView(got, false)
		return errorResult(fmt.Sprintf("cuttle: the browser is being driven by %s (for %vs), so this call was refused."+
			" Read-only tools still work; retry when it is free.", got.owner, v["age_seconds"]))
	}
	s.mu.Lock()
	s.leaseToken = got.token
	s.mu.Unlock()
	return nil
}

// adopt brings the principal's view of its tabs up to date: closed tabs are
// dropped, and a page one of its tabs opened (a popup, a target=_blank link)
// becomes its own.
func (s *agentSession) adopt(pages []pageInfo) {
	live := map[string]bool{}
	for _, p := range pages {
		live[p.ID] = true
	}
	for id := range s.owned {
		if !live[id] {
			delete(s.owned, id)
		}
	}
	for grew := true; grew; {
		grew = false
		for _, p := range pages {
			if !s.owned[p.ID] && p.Opener != "" && s.owned[p.Opener] {
				s.owned[p.ID], grew = true, true
			}
		}
	}
	if !live[s.current] {
		s.current = ""
	}
}

// ensureOwnTab makes the upstream session's current tab one this principal
// opened, opening one when it has none: no agent acts on the person's tab.
func (a *aggregator) ensureOwnTab(ctx context.Context, s *agentSession) error {
	pages, err := a.browser.pages(ctx)
	if err != nil {
		return fmt.Errorf("cuttle: the browser is not reachable: %w", err)
	}
	s.mu.Lock()
	s.adopt(pages)
	has := s.current != ""
	s.mu.Unlock()
	if has {
		return nil
	}
	res, err := a.openTab(ctx, s, map[string]any{"action": "new"}, nil)
	if err != nil {
		return fmt.Errorf("cuttle: could not open a tab for %s: %w", s.principal, err)
	}
	if res.IsError {
		return fmt.Errorf("cuttle: could not open a tab for %s: %s", s.principal, strings.TrimSpace(resultText(res))) //nolint:err113 // upstream text
	}
	s.mu.Lock()
	has = s.current != ""
	s.mu.Unlock()
	if !has {
		return fmt.Errorf("cuttle: opened a tab for %s but could not tell which one it is - retry", s.principal) //nolint:err113 // one-off
	}
	return nil
}

// openTab runs browser_tabs new and records the tab it made as the principal's.
func (a *aggregator) openTab(ctx context.Context, s *agentSession, args any, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	a.tabMu.Lock()
	defer a.tabMu.Unlock()
	before, err := a.browser.pages(ctx)
	if err != nil {
		return nil, err
	}
	res, err := a.call(ctx, s, toolTabs, args, req)
	if err != nil || res.IsError {
		return res, err
	}
	after, err := a.browser.pages(ctx)
	if err != nil {
		return res, nil // the tab exists; it is attributed on the next call
	}
	known := map[string]bool{}
	for _, p := range before {
		known[p.ID] = true
	}
	want := ""
	for _, e := range parseTabList(resultText(res)) {
		if e.current {
			want = e.url
		}
	}
	chosen := ""
	for _, p := range after {
		if known[p.ID] {
			continue
		}
		if chosen == "" || p.URL == want {
			chosen = p.ID
		}
	}
	s.mu.Lock()
	if chosen != "" {
		s.owned[chosen] = true
	}
	s.current = chosen
	s.mu.Unlock()
	return res, nil
}

type tabEntry struct {
	index   int
	current bool
	title   string
	url     string
}

var tabLine = regexp.MustCompile(`(?m)^- (\d+): (\(current\) )?\[(.*)\]\(([^()\s]*)\)\s*$`)

// parseTabList reads browser_tabs' "- N: (current) [title](url)" lines.
func parseTabList(text string) []tabEntry {
	out := []tabEntry{}
	seen := map[int]bool{}
	for _, m := range tabLine.FindAllStringSubmatch(text, -1) {
		i, err := strconv.Atoi(m[1])
		if err != nil || seen[i] {
			continue // the action results repeat the list under "Open tabs"
		}
		seen[i] = true
		out = append(out, tabEntry{index: i, current: m[2] != "", title: m[3], url: m[4]})
	}
	return out
}

// tabs applies tab ownership to browser_tabs: list passes, new is recorded,
// select and close only reach the principal's own tabs.
func (a *aggregator) tabs(ctx context.Context, s *agentSession, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := rawArgs(req)
	var in struct {
		Action string   `json:"action"`
		Index  *float64 `json:"index"`
	}
	if raw, ok := args.(json.RawMessage); ok {
		_ = json.Unmarshal(raw, &in)
	}
	switch in.Action {
	case "new":
		return a.openTab(ctx, s, args, req)
	case "select", "close":
		target, refusal, err := a.ownedTab(ctx, s, in.Index)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return errorResult(refusal), nil
		}
		res, err := a.call(ctx, s, toolTabs, args, req)
		if err != nil || res.IsError || target == "" {
			return res, err
		}
		s.mu.Lock()
		if in.Action == "select" {
			s.current = target
		} else {
			delete(s.owned, target)
			if s.current == target {
				s.current = ""
			}
		}
		s.mu.Unlock()
		return res, nil
	default:
		return a.call(ctx, s, toolTabs, args, req)
	}
}

// ownedTab resolves the tab a select or close names and decides whether this
// principal may touch it. Playwright addresses tabs by index and Chrome by
// target id; the two meet only on the URL, so a tab passes only when every page
// at that URL is the principal's - an ambiguous match is refused.
func (a *aggregator) ownedTab(ctx context.Context, s *agentSession, index *float64) (string, string, error) {
	pages, err := a.browser.pages(ctx)
	if err != nil {
		return "", "", err
	}
	s.mu.Lock()
	s.adopt(pages)
	current := s.current
	owned := maps.Clone(s.owned)
	s.mu.Unlock()
	if index == nil {
		if current == "" {
			return "", "cuttle: no tab of yours is selected - open one with browser_tabs new, or select one you opened.", nil
		}
		return current, "", nil
	}
	res, err := a.call(ctx, s, toolTabs, map[string]any{"action": "list"}, nil)
	if err != nil {
		return "", "", err
	}
	var entry *tabEntry
	for _, e := range parseTabList(resultText(res)) {
		if float64(e.index) == *index {
			entry = &e
		}
	}
	if entry == nil {
		return "", "", nil // the upstream answers an index it does not have
	}
	target := ""
	for _, p := range pages {
		if p.URL != entry.url {
			continue
		}
		if !owned[p.ID] {
			return "", fmt.Sprintf("cuttle: tab %d (%s) was not opened by %s - an agent may only select or close tabs it opened"+
				" itself; the others belong to the person or to another agent. If it is yours and another tab shows the same"+
				" address, navigate one of them first.", entry.index, entry.url, s.principal), nil
		}
		target = p.ID
	}
	if target == "" {
		return "", fmt.Sprintf("cuttle: tab %d (%s) is not a tab %s opened.", entry.index, entry.url, s.principal), nil
	}
	return target, "", nil
}

// screenshotRefusal holds screenshots of a tab a value was just captured from,
// while it still shows the page the value came from: an image cannot be masked.
func (a *aggregator) screenshotRefusal(ctx context.Context, s *agentSession) string {
	s.mu.Lock()
	c, ok := s.captured[s.current]
	current := s.current
	s.mu.Unlock()
	left := screenshotHold - time.Since(c.at)
	if !ok || left <= 0 {
		return ""
	}
	pages, err := a.browser.pages(ctx)
	if err == nil {
		for _, p := range pages {
			if p.ID == current && p.URL != c.url {
				return ""
			}
		}
	}
	return fmt.Sprintf("cuttle: a secret was captured from this tab %ds ago, so screenshots of it are refused for another %ds"+
		" - an image cannot be masked. Navigate away first, or use browser_snapshot, whose text is masked.",
		int(time.Since(c.at).Seconds()), int(left.Seconds())+1)
}

// mask replaces every held secret and every live password field value in the
// result's text with its sentinel. Image content cannot be masked; see
// screenshotRefusal.
func (a *aggregator) mask(ctx context.Context, res *mcp.CallToolResult) {
	if res == nil {
		return
	}
	hasText := res.StructuredContent != nil
	for _, c := range res.Content {
		switch c.(type) {
		case *mcp.TextContent, *mcp.EmbeddedResource:
			hasText = true
		}
	}
	if !hasText {
		return
	}
	pairs := a.r.m.pool.secrets.heldPairs(a.seed())
	for _, v := range a.browser.passwords(ctx) {
		pairs = append(pairs, exactPairs(v, passwordPlaceholder)...)
	}
	if len(pairs) == 0 {
		return
	}
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			v.Text = maskExact(v.Text, pairs)
		case *mcp.EmbeddedResource:
			if v.Resource != nil {
				v.Resource.Text = maskExact(v.Resource.Text, pairs)
			}
		}
	}
	if res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			res.StructuredContent = json.RawMessage(maskExact(string(b), jsonPairs(pairs)))
		}
	}
}

// maskMiddleware masks every tools/call result on its way to the agent.
func (a *aggregator) maskMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if r, ok := res.(*mcp.CallToolResult); ok && err == nil {
			a.mask(ctx, r)
		}
		return res, err
	}
}

// jsonPairs re-spells each pair as it appears inside a JSON string.
func jsonPairs(pairs [][]string) [][]string {
	out := make([][]string, 0, len(pairs))
	for _, p := range pairs {
		v, err1 := json.Marshal(p[0])
		r, err2 := json.Marshal(p[1])
		if err1 == nil && err2 == nil {
			out = append(out, []string{strings.Trim(string(v), `"`), strings.Trim(string(r), `"`)})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The Playwright MCP child
// ---------------------------------------------------------------------------

// runPlaywrightMCP supervises `playwright mcp`, restarting it with backoff like
// the pool restarts Chrome. Its working directory is the driver's download dir:
// Playwright MCP confines file access to it, so an agent can upload only what
// the browser downloaded, never the profile around it.
func runPlaywrightMCP(ctx context.Context, cdpPort int, dir string) {
	backoff := time.Second
	for {
		start := time.Now()
		//nolint:gosec // fixed argv; the only variables are our own port and dir
		cmd := exec.CommandContext(ctx, "node", playwrightMCPScript, "mcp",
			"--host", "127.0.0.1", "--port", strconv.Itoa(playwrightMCPPort),
			"--cdp-endpoint", "http://127.0.0.1:"+strconv.Itoa(cdpPort),
			"--shared-browser-context", "--output-dir", dir)
		cmd.Dir = dir
		out := &lineLogger{prefix: "playwright-mcp: "}
		cmd.Stdout, cmd.Stderr = out, out
		cmd.WaitDelay = 5 * time.Second
		logInfo("remote: starting the Playwright MCP upstream on 127.0.0.1:%d", playwrightMCPPort)
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		logWarn("remote: the Playwright MCP upstream exited (%v); restarting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, launchBackoffMax)
	}
}

// lineLogger turns a child's output into daemon log lines, so it is masked and
// teed like every other line.
type lineLogger struct {
	prefix string
	buf    []byte
}

func (l *lineLogger) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimSpace(string(l.buf[:i])); line != "" {
			logInfo("%s%s", l.prefix, line)
		}
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > 4096 {
		logInfo("%s%s", l.prefix, l.buf)
		l.buf = l.buf[:0]
	}
	return len(p), nil
}
