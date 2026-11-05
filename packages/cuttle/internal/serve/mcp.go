package serve

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
// the random Host passed to it as --allowed-hosts, and never the standalone GET
// stream, which it pings every 3 s and kills after 5 s unanswered.

const (
	playwrightMCPPort   = 8931
	playwrightMCPScript = "/opt/playwright-cli/lib/node_modules/@playwright/cli/node_modules/playwright/cli.js"
	upstreamProtocol    = "2025-11-25"
	upstreamIdle        = 30 * time.Minute
	upstreamReadyWait   = 15 * time.Second
	upstreamRetryAfter  = 30 * time.Second
	closedRetryDelay    = 500 * time.Millisecond
	screenshotHold      = 30 * time.Second

	toolTabs       = "browser_tabs"
	tabAction      = "action"
	tabSelect      = "select"
	tabNew         = "new"
	tabClose       = "close"
	tabList        = "list"
	tabIndex       = "index"
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

const screenshotNote = "Refused for 30 s on a tab right after cuttle_secret_capture read a value from it."

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
// Every call is bounded (browserCallTimeout, or the read's own timeout): a
// wedged Chrome must not park a principal's call lock, or the global tab lock,
// forever.
type cdpBrowser struct{ pool *chromePool }

const browserCallTimeout = 5 * time.Second

func (b cdpBrowser) port() (int, error) {
	inst := b.pool.runningInstance(reservedSeed)
	if inst == nil {
		return 0, errBrowserDown
	}
	return inst.cdpPort, nil
}

func (b cdpBrowser) pages(ctx context.Context) ([]pageInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, browserCallTimeout)
	defer cancel()
	port, err := b.port()
	if err != nil {
		inst, lerr := b.pool.getOrLaunch(ctx, connectRequest{})
		if lerr != nil {
			return nil, lerr
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
	ctx, cancel := context.WithTimeout(ctx, browserCallTimeout)
	defer cancel()
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
	ctx, cancel := context.WithTimeout(ctx, browserCallTimeout)
	defer cancel()
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

type capturedTab struct{ at time.Time }

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
	lastMiss   time.Time // when loading the upstream tools last failed
	relisting  atomic.Bool
	upstream   []string // pass-through tool names currently registered
	owned      map[string]bool
	current    string
	leaseToken string
	captured   map[string]capturedTab
	idle       *time.Timer
	// revoked is set once the principal's line is gone: a call already past the
	// bearer check must not take the lease or reopen an upstream session.
	revoked bool
	// takenOver is when a takeover evicted this principal's lease; it does not
	// drive again until a lease lifetime has passed.
	takenOver time.Time
	// seen numbers each page by the snapshot it first appeared in, which is
	// the order Playwright lists tabs in; pages first seen together tie.
	seen  map[string]int
	batch int
}

var errRevoked = errors.New("cuttle: this principal was revoked")

func (s *agentSession) isRevoked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revoked
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

// upstreamTransport sends the one Host the child accepts and the one protocol
// version it speaks (2025-11-25). The Host is a random name passed to the child
// as --allowed-hosts: a page in the browser can reach 127.0.0.1:8931 too, and
// it cannot send a Host it does not know.
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

func newAggregator(r *remote, endpoint, host string, browser browserOps) *aggregator {
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

// revoke ends the upstream sessions of principals no longer listed and frees
// a lease one of them holds.
func (a *aggregator) revoke(live map[string]bool) {
	a.mu.Lock()
	var gone []*agentSession
	for name, s := range a.agents {
		if !live[name] {
			gone = append(gone, s)
			delete(a.agents, name)
		}
	}
	a.mu.Unlock()
	for _, s := range gone {
		s.mu.Lock()
		s.revoked = true
		if s.idle != nil {
			s.idle.Stop()
		}
		token := s.leaseToken
		s.mu.Unlock()
		a.r.m.pool.leases.release(reservedSeed, token)
		a.reset(s)
		logInfo("remote: %s is no longer a principal; its links, viewer sessions, files and browser session are ended", s.principal)
	}
}

func (a *aggregator) close() {
	a.mu.Lock()
	agents := a.agents
	a.agents = map[string]*agentSession{}
	a.mu.Unlock()
	for _, s := range agents {
		s.mu.Lock()
		s.revoked = true
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
	// A request admitted just before a revoke must not bring the session back.
	if !a.r.principals.names()[principal] {
		return nil
	}
	a.mu.Lock()
	s := a.agents[principal]
	if s == nil {
		s = &agentSession{principal: principal, owned: map[string]bool{}, seen: map[string]int{}, captured: map[string]capturedTab{}}
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
// leaves only cuttle's own tools, and for upstreamRetryAfter every later
// request tries once without waiting, so a down child does not stall each one.
func (a *aggregator) loadTools(ctx context.Context, s *agentSession) {
	s.mu.Lock()
	loaded, recent := s.loaded, time.Since(s.lastMiss) < upstreamRetryAfter
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
		if recent {
			// One try, no wait: the SDK asks for the server twice per request.
			s.mu.Lock()
			s.lastMiss = time.Now()
			s.mu.Unlock()
			return
		}
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.lastMiss = time.Now()
			s.mu.Unlock()
			if !recent {
				logWarn("remote: the Playwright MCP upstream is not answering, %s gets cuttle's own tools only for now: %v", s.principal, err)
			}
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
	cs, revoked := s.cs, s.revoked
	s.mu.Unlock()
	if revoked {
		return nil, errRevoked
	}
	if cs != nil {
		return cs, nil
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "cuttle", Version: "1"}, &mcp.ClientOptions{
		// No roots: the SDK advertises them by default, and Playwright MCP then
		// asks for them over the GET stream this client never opens, stalling the
		// session's first tool call for about a minute.
		Capabilities:                &mcp.ClientCapabilities{},
		ProgressNotificationHandler: a.relayProgress,
		ToolListChangedHandler: func(ctx context.Context, _ *mcp.ToolListChangedRequest) {
			// Off the notification's goroutine: it can arrive during a call that
			// holds callMu. One re-list at a time; a burst coalesces into it.
			if !s.relisting.CompareAndSwap(false, true) {
				return
			}
			go func() {
				defer s.relisting.Store(false)
				s.callMu.Lock()
				defer s.callMu.Unlock()
				rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upstreamReadyWait)
				defer cancel()
				if err := a.relistLocked(rctx, s); err != nil {
					logWarn("remote: re-listing upstream tools for %s failed: %v", s.principal, err)
				}
			}()
		},
	})
	// The SDK detaches the session from this context; the bound is only for the
	// handshake, so a hung child cannot park the principal's call lock.
	cctx, cancel := context.WithTimeout(ctx, upstreamReadyWait)
	defer cancel()
	cs, err := client.Connect(cctx, &mcp.StreamableClientTransport{
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
// current tab is the browser's first, so ensureOwnTab reselects (or reopens)
// the principal's own tab before anything acts.
func (a *aggregator) reset(s *agentSession) {
	s.mu.Lock()
	cs := s.cs
	// A fresh upstream session may list the tabs it finds in any order.
	s.cs, s.current, s.seen = nil, "", map[string]int{}
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
		ProgressToken: rl.token, Progress: req.Params.Progress, Total: req.Params.Total,
		Message: maskExact(req.Params.Message, a.r.m.pool.secrets.heldPairs(reservedSeed)),
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
		if s.isRevoked() {
			return errorResult(errRevoked.Error()), nil
		}
		a.touch(s)
		// A read-only call that must first open or select the principal's tab
		// changes the visible tab, so it drives too.
		s.mu.Lock()
		needsTab := s.current == "" && name != toolTabs
		s.mu.Unlock()
		if name == toolTabs {
			readOnly = tabsAction(req) == tabList
		}
		if !readOnly || needsTab {
			if err := a.holdLease(s); err != nil {
				return errorResult(err.Error()), nil
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
		if msg := a.screenshotRefusal(s); msg != "" {
			return errorResult(msg), nil
		}
	}
	res, err := a.call(ctx, s, name, rawArgs(req), req)
	if !targetClosed(res, err) {
		return res, err
	}
	// Chrome was relaunched under the call, or the tab closed: the upstream's
	// current tab is now some other page. Only a relaunch, which takes every tab
	// with it, is retried, in a fresh own tab; an action that closed its own tab
	// may have done its work, and replaying it on another tab would act twice.
	select {
	case <-ctx.Done():
		return nil, ctx.Err() //nolint:wrapcheck // the caller's own cancellation
	case <-time.After(closedRetryDelay):
	}
	if pages, perr := a.browser.pages(ctx); perr == nil {
		s.mu.Lock()
		s.adopt(pages)
		survived := len(s.owned) > 0
		s.mu.Unlock()
		if survived {
			return res, err
		}
	}
	if err := a.ensureOwnTab(ctx, s); err != nil {
		return errorResult(err.Error()), nil
	}
	return a.call(ctx, s, name, rawArgs(req), req)
}

// call makes one upstream tool call, relaying progress, and re-initializes
// when the upstream lost the session.
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
	cs, err := a.session(ctx, s)
	if err != nil {
		return nil, err
	}
	res, err := cs.CallTool(ctx, params)
	if err != nil && (errors.Is(err, mcp.ErrSessionMissing) || errors.Is(err, mcp.ErrConnectionClosed)) {
		a.reset(s)
		return errorResult("cuttle: the browser automation session behind this agent restarted, so element refs and" +
			" the tab selection from before are gone and this call may not have completed. Take a new browser_snapshot and act on that."), nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return res, nil
}

// holdLease takes or renews the driving lease as mcp:<principal>, or says why
// it cannot. A lease lost to a takeover is not taken back at once: the person
// who took the browser over gets it for a lease lifetime first.
func (a *aggregator) holdLease(s *agentSession) error {
	leases := a.r.m.pool.leases
	seed, owner := reservedSeed, "mcp:"+s.principal
	s.mu.Lock()
	token, takenOver := s.leaseToken, s.takenOver
	s.mu.Unlock()
	if wait := leaseTTL - time.Since(takenOver); wait > 0 {
		return fmt.Errorf("cuttle: the browser was taken over from you %ds ago, so driving calls are refused for another %ds."+ //nolint:err113 // refusal text
			" Read-only tools still work", int(time.Since(takenOver).Seconds()), int(wait.Seconds())+1)
	}
	got, err := leases.acquire(seed, owner, token)
	if errors.Is(err, errLeaseLost) && got.forced {
		s.mu.Lock()
		s.leaseToken, s.takenOver = "", time.Now()
		s.mu.Unlock()
		return fmt.Errorf("cuttle: %s took the browser over, so this call was refused and driving calls are refused for %ds."+ //nolint:err113 // refusal text
			" Read-only tools still work", got.owner, int(leaseTTL.Seconds()))
	}
	if errors.Is(err, errLeaseLost) {
		got, err = leases.acquire(seed, owner, "")
	}
	if err != nil {
		return fmt.Errorf("cuttle: the browser is being driven by %s (for %ds), so this call was refused."+ //nolint:err113 // refusal text
			" Read-only tools still work; retry when it is free", got.owner, int(time.Since(got.acquired).Seconds()))
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
	for id := range s.seen {
		if !live[id] {
			delete(s.seen, id)
		}
	}
	fresh := false
	for _, p := range pages {
		if _, ok := s.seen[p.ID]; !ok {
			if !fresh {
				s.batch, fresh = s.batch+1, true
			}
			s.seen[p.ID] = s.batch
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
	for id := range s.captured {
		if !live[id] {
			delete(s.captured, id)
		}
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
	has, owns := s.current != "", len(s.owned) > 0
	s.mu.Unlock()
	if has {
		return nil
	}
	// A fresh upstream session (after a reset) starts on the first tab: go back
	// to a tab this principal already has rather than leaking a new one.
	if owns && a.reselect(ctx, s, pages) {
		return nil
	}
	res, err := a.openTab(ctx, s, map[string]any{tabAction: tabNew}, nil)
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

// reselect selects one of the principal's own tabs in a fresh upstream session.
func (a *aggregator) reselect(ctx context.Context, s *agentSession, pages []pageInfo) bool {
	res, err := a.call(ctx, s, toolTabs, map[string]any{tabAction: tabList}, nil)
	if err != nil || res.IsError {
		return false
	}
	list := parseTabList(resultText(res))
	for _, e := range list {
		s.mu.Lock()
		target, mine, exact := s.resolveTab(pages, list, e)
		s.mu.Unlock()
		if !mine || !exact {
			continue
		}
		sel, err := a.call(ctx, s, toolTabs, map[string]any{tabAction: tabSelect, tabIndex: e.index}, nil)
		if err != nil || sel.IsError || currentURL(sel) != e.url {
			return false
		}
		s.mu.Lock()
		s.current = target
		s.mu.Unlock()
		return true
	}
	return false
}

// resolveTab names the page behind one browser_tabs entry and says whether it
// is the principal's; s.mu held. Playwright addresses tabs by index and Chrome
// by target id. The upstream's current tab is the principal's current one;
// any other entry is matched among the pages at its URL by the order they
// appeared in, which is the order Playwright lists them in. When the two lists
// disagree, or pages that appeared together share the URL, every candidate
// must be the principal's. It returns the page, whether it is the
// principal's, and whether it is exactly that page rather than one of several.
func (s *agentSession) resolveTab(pages []pageInfo, list []tabEntry, e tabEntry) (string, bool, bool) {
	if e.current && s.current != "" {
		for _, p := range pages {
			if p.ID == s.current && p.URL == e.url {
				return s.current, true, true
			}
		}
	}
	var cands []pageInfo
	for _, p := range pages {
		if p.URL == e.url {
			cands = append(cands, p)
		}
	}
	pos, n := 0, 0
	for _, o := range list {
		if o.url == e.url {
			n++
			if o.index < e.index {
				pos++
			}
		}
	}
	slices.SortStableFunc(cands, func(x, y pageInfo) int { return cmp.Compare(s.seen[x.ID], s.seen[y.ID]) })
	run := cands
	if n == len(cands) && len(list) == len(pages) {
		lo, hi := pos, pos+1
		for lo > 0 && s.seen[cands[lo-1].ID] == s.seen[cands[pos].ID] {
			lo--
		}
		for hi < len(cands) && s.seen[cands[hi].ID] == s.seen[cands[pos].ID] {
			hi++
		}
		run = cands[lo:hi]
	}
	if len(run) == 0 {
		return "", false, false
	}
	for _, p := range run {
		if !s.owned[p.ID] {
			return "", false, false
		}
	}
	return run[0].ID, true, len(run) == 1
}

func currentURL(res *mcp.CallToolResult) string {
	for _, e := range parseTabList(resultText(res)) {
		if e.current {
			return e.url
		}
	}
	return ""
}

// openTab runs browser_tabs new and records the tab it made as the principal's.
// A navigation that fails (a download, a network error, a timeout) still
// leaves the new tab open and current upstream, so it is recorded too.
func (a *aggregator) openTab(ctx context.Context, s *agentSession, args any, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	a.tabMu.Lock()
	defer a.tabMu.Unlock()
	before, err := a.browser.pages(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.adopt(before)
	s.mu.Unlock()
	res, callErr := a.call(ctx, s, toolTabs, args, req)
	after, err := a.browser.pages(ctx)
	if err != nil {
		return res, callErr
	}
	known := map[string]bool{}
	for _, p := range before {
		known[p.ID] = true
	}
	var fresh []pageInfo
	for _, p := range after {
		if !known[p.ID] && p.Opener == "" {
			fresh = append(fresh, p)
		}
	}
	// Another tab that opened meanwhile could be the person's: with more than
	// one new page, only the one at the URL the result names is taken.
	chosen := ""
	if len(fresh) == 1 {
		chosen = fresh[0].ID
	} else if want := currentURL(res); want != "" {
		for _, p := range fresh {
			if p.URL == want {
				if chosen != "" {
					chosen = ""
					break
				}
				chosen = p.ID
			}
		}
	}
	s.mu.Lock()
	if chosen != "" {
		s.owned[chosen] = true
	}
	s.adopt(after)
	s.current = chosen
	if s.cs == nil || callErr != nil {
		s.current = "" // the session restarted, or the upstream's current tab is unknown
	}
	s.mu.Unlock()
	return res, callErr
}

type tabEntry struct {
	index   int
	current bool
	url     string
}

var tabLine = regexp.MustCompile(`(?m)^- (\d+): (\(current\) )?\[.*\]\((\S*)\)\s*$`)

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
		out = append(out, tabEntry{index: i, current: m[2] != "", url: m[3]})
	}
	return out
}

// tabsAction reads browser_tabs' action the way tabs does.
func tabsAction(req *mcp.CallToolRequest) string {
	var in struct {
		Action string `json:"action"`
	}
	if raw, ok := rawArgs(req).(json.RawMessage); ok {
		_ = json.Unmarshal(raw, &in)
	}
	return in.Action
}

// tabs applies tab ownership to browser_tabs: list passes, new is recorded,
// select and close only reach the principal's own tabs.
func (a *aggregator) tabs(ctx context.Context, s *agentSession, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Action string   `json:"action"`
		Index  *float64 `json:"index"`
		URL    string   `json:"url"`
	}
	if raw, ok := rawArgs(req).(json.RawMessage); ok {
		_ = json.Unmarshal(raw, &in)
	}
	// Forward what was checked, never the raw arguments: Go matches JSON keys
	// case-insensitively and keeps the last duplicate, JavaScript does neither,
	// so {"action":"select","ACTION":"list"} would read as list here and as
	// select upstream.
	args := map[string]any{tabAction: in.Action}
	if in.Index != nil {
		args[tabIndex] = *in.Index
	}
	if in.URL != "" {
		args["url"] = in.URL
	}
	switch in.Action {
	case tabNew:
		return a.openTab(ctx, s, args, req)
	case tabSelect, tabClose:
		pick, refusal, err := a.ownedTab(ctx, s, in.Action, in.Index)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return errorResult(refusal), nil
		}
		res, err := a.call(ctx, s, toolTabs, args, req)
		if err != nil || res.IsError {
			return res, err
		}
		if in.Action == tabSelect {
			s.mu.Lock()
			// The index was resolved a moment ago; if the tabs shifted since, the
			// upstream selected some other page, and the next call reopens ours.
			s.current = pick.target
			if pick.url != "" && currentURL(res) != pick.url {
				s.current = ""
			}
			s.mu.Unlock()
			return res, nil
		}
		s.mu.Lock()
		if pick.current || s.current == pick.target {
			s.current = ""
		}
		s.mu.Unlock()
		if pages, err := a.browser.pages(ctx); err == nil {
			s.mu.Lock()
			s.adopt(pages)
			s.mu.Unlock()
		}
		return res, nil
	default:
		return a.call(ctx, s, toolTabs, args, req)
	}
}

// tabPick is the tab a select or close names: its page, its URL in the
// upstream's list, and whether it is the upstream's current tab.
type tabPick struct {
	target, url string
	current     bool
}

// ownedTab resolves the tab a select or close names and decides whether this
// principal may touch it (see resolveTab); a doubtful match is refused.
func (a *aggregator) ownedTab(ctx context.Context, s *agentSession, action string, index *float64) (tabPick, string, error) {
	pages, err := a.browser.pages(ctx)
	if err != nil {
		return tabPick{}, "", err
	}
	s.mu.Lock()
	s.adopt(pages)
	current := s.current
	s.mu.Unlock()
	if index == nil {
		if current == "" {
			return tabPick{}, "cuttle: no tab of yours is selected - open one with browser_tabs new, or select one you opened.", nil
		}
		return tabPick{target: current, current: true}, "", nil
	}
	res, err := a.call(ctx, s, toolTabs, map[string]any{tabAction: tabList}, nil)
	if err != nil {
		return tabPick{}, "", err
	}
	list := parseTabList(resultText(res))
	var entry *tabEntry
	for _, e := range list {
		if float64(e.index) == *index {
			entry = &e
		}
	}
	if entry == nil {
		return tabPick{}, fmt.Sprintf("cuttle: there is no tab %v in browser_tabs list.", *index), nil
	}
	s.mu.Lock()
	target, mine, exact := s.resolveTab(pages, list, *entry)
	s.mu.Unlock()
	if !mine {
		return tabPick{}, fmt.Sprintf("cuttle: tab %d (%s) was not opened by %s - an agent may only select or close tabs it opened"+
			" itself; the others belong to the person or to another agent. If it is yours and another tab shows the same"+
			" address, navigate one of them first.", entry.index, entry.url, s.principal), nil
	}
	if !exact && action == tabSelect {
		return tabPick{}, fmt.Sprintf("cuttle: tab %d (%s) is one of several tabs of yours at that address that cuttle cannot"+
			" tell apart - navigate one of them elsewhere first.", entry.index, entry.url), nil
	}
	return tabPick{target: target, url: entry.url, current: entry.current}, "", nil
}

// screenshotRefusal holds screenshots of a tab a value was just captured from
// for the whole hold: an image cannot be masked, and a URL change (a fragment,
// history.replaceState) does not mean the value left the screen.
func (a *aggregator) screenshotRefusal(s *agentSession) string {
	s.mu.Lock()
	c, ok := s.captured[s.current]
	s.mu.Unlock()
	left := screenshotHold - time.Since(c.at)
	if !ok || left <= 0 {
		return ""
	}
	return fmt.Sprintf("cuttle: a secret was captured from this tab %ds ago, so screenshots of it are refused for another %ds"+
		" - an image cannot be masked. Use browser_snapshot, whose text is masked, or wait.",
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
	pairs := a.r.m.pool.secrets.heldPairs(reservedSeed)
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
func runPlaywrightMCP(ctx context.Context, cdpPort int, dir, allowedHost string) {
	go pruneArtifacts(ctx, dir)
	backoff := time.Second
	for {
		start := time.Now()
		//nolint:gosec // fixed argv; the only variables are our own port and dir
		cmd := exec.CommandContext(ctx, "node", playwrightMCPScript, "mcp",
			"--host", "127.0.0.1", "--port", strconv.Itoa(playwrightMCPPort),
			"--cdp-endpoint", "http://127.0.0.1:"+strconv.Itoa(cdpPort),
			"--shared-browser-context", "--output-dir", dir, "--allowed-hosts", allowedHost)
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

const (
	artifactMaxAge     = time.Hour
	artifactPruneEvery = 10 * time.Minute
)

// pruneArtifacts deletes the snapshots and screenshots Playwright MCP writes
// on every action once they are an hour old; on a durable profile they would
// otherwise fill the volume.
func pruneArtifacts(ctx context.Context, dir string) {
	tick := time.NewTicker(artifactPruneEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !autoArtifact.MatchString(e.Name()) {
				continue
			}
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > artifactMaxAge {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
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
