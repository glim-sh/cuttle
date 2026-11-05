package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	tabNew         = "new"
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
	"Your first browser action opens a tab of your own; tabs are shared, so close what you opened. After cuttle_wait, a handoff or any pause, take a browser_snapshot before acting. " +
	"Action results link their snapshot file instead of inlining it: call browser_snapshot to read the page. " +
	"Fetch files with cuttle_download."

const screenshotNote = "Refused for 30 s after any cuttle_secret_capture; the hold covers all screenshots."

// pageInfo is one page target, as Target.getTargets reports it.
type pageInfo struct {
	ID, URL, Title, Opener string
}

// browserOps is everything the aggregator reads from Chrome directly.
type browserOps interface {
	pages(ctx context.Context) ([]pageInfo, error)
	passwords(ctx context.Context) []string
	cookies(ctx context.Context) ([]originAuth, bool)
}

var errBrowserDown = errors.New("the browser is not running")

// cdpBrowser reads the session browser over its own CDP port. It never
// attaches to a page except to read password fields and to capture, which is
// exactly what the loopback routes already do.
// Every call is bounded (browserCallTimeout, or the read's own timeout): a
// wedged Chrome must not park a principal's call lock forever.
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
	inst := b.pool.runningInstance(reservedSeed)
	if inst == nil {
		return nil
	}
	return passwordFieldValues(ctx, inst.cdpPort, &inst.passwords)
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

// relay maps one upstream progress token back to the agent call it belongs to.
// It keeps that call's context: a stateless server can only send a
// notification on the stream of the request it is made in context of.
type relay struct {
	ctx     context.Context //nolint:containedctx // see above
	session *mcp.ServerSession
	token   any
}

// agentSession is one principal's state. callMu serializes its upstream calls:
// calls cannot interleave with another of the same principal's calls.
type agentSession struct {
	principal string
	server    *mcp.Server
	callMu    sync.Mutex

	mu         sync.Mutex
	instance   *chromeInstance
	fresh      bool
	cs         *mcp.ClientSession
	loaded     bool
	lastMiss   time.Time // when loading the upstream tools last failed
	relisting  atomic.Bool
	upstream   []string // pass-through tool names currently registered
	leaseToken string
	idle       *time.Timer
	// revoked is set once the principal's line is gone: a call already past the
	// bearer check must not take the lease or reopen an upstream session.
	revoked bool
	// takenOver is when a takeover evicted this principal's lease; it does not
	// drive again until a lease lifetime has passed.
	takenOver time.Time
}

var errRevoked = errors.New("cuttle: this principal was revoked")

func (s *agentSession) isRevoked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revoked
}

type aggregator struct {
	captureMu  sync.Mutex
	capturedAt time.Time
	r          *remote
	endpoint   string
	browser    browserOps
	client     *http.Client
	h          http.Handler

	mu      sync.Mutex
	agents  map[string]*agentSession
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
		s = &agentSession{principal: principal}
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
	if s.revoked {
		s.mu.Unlock()
		_ = cs.Close()
		return nil, errRevoked
	}
	s.cs, s.fresh = cs, true
	s.mu.Unlock()
	return cs, nil
}

// reset forgets the upstream session; its replacement opens a fresh tab.
func (a *aggregator) reset(s *agentSession) {
	s.mu.Lock()
	cs := s.cs
	s.cs = nil
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

// decodeArgs reads a call's arguments into v; none at all leave v as it is.
func decodeArgs(req *mcp.CallToolRequest, v any) error {
	raw, ok := rawArgs(req).(json.RawMessage)
	if !ok {
		return nil
	}
	return json.Unmarshal(raw, v) //nolint:wrapcheck // the agent's own arguments
}

// passthrough forwards one upstream tool, inside cuttle's rules: the lease for
// a driving call and the screenshot hold. Masking is
// the server middleware's, so it covers cuttle's own tools too.
func (a *aggregator) passthrough(s *agentSession, name string, readOnly bool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		s.callMu.Lock()
		defer s.callMu.Unlock()
		if s.isRevoked() {
			return errorResult(errRevoked.Error()), nil
		}
		a.touch(s)
		ro := readOnly
		if name == toolTabs {
			var in struct {
				Action string `json:"action"`
			}
			_ = decodeArgs(req, &in)
			ro = in.Action == tabList
		}
		if !ro {
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
	if name == toolScreenshot {
		if msg := a.screenshotRefusal(s); msg != "" {
			return errorResult(msg), nil
		}
	}
	if name == toolType || name == toolFillForm {
		if msg := a.sentinelRefusal(ctx, s, name, req); msg != "" {
			return errorResult(msg), nil
		}
	}
	do := func() (*mcp.CallToolResult, error) { return a.call(ctx, s, name, rawArgs(req), req) }
	if name == toolFind {
		do = func() (*mcp.CallToolResult, error) { return a.find(ctx, s, req) }
	}
	if name == toolTabs {
		do = func() (*mcp.CallToolResult, error) { return a.tabs(ctx, s, req) }
	}
	res, err := do()
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
	s.mu.Lock()
	before := s.instance
	s.mu.Unlock()
	after, launchErr := a.r.m.pool.getOrLaunch(ctx, connectRequest{})
	if launchErr != nil || before == nil || before == after {
		return res, err
	}
	a.reset(s)
	return do()
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
	s.mu.Lock()
	fresh := s.fresh
	s.mu.Unlock()
	if fresh {
		if leaseErr := a.holdLease(s); leaseErr != nil {
			return nil, leaseErr
		}
		inst, launchErr := a.r.m.pool.getOrLaunch(ctx, connectRequest{})
		if launchErr != nil {
			return nil, launchErr
		}
		s.mu.Lock()
		s.instance = inst
		s.mu.Unlock()
		opened, openErr := cs.CallTool(ctx, &mcp.CallToolParams{Name: toolTabs, Arguments: map[string]any{tabAction: tabNew}})
		if openErr != nil {
			a.reset(s)
			return nil, fmt.Errorf("open session tab: %w", openErr)
		}
		if opened.IsError {
			return opened, nil
		}
		s.mu.Lock()
		s.fresh = false
		s.mu.Unlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revoked {
		return errRevoked
	}
	leases := a.r.m.pool.leases
	seed, owner := reservedSeed, "mcp:"+s.principal
	token, takenOver := s.leaseToken, s.takenOver
	if wait := leaseTTL - time.Since(takenOver); wait > 0 {
		return fmt.Errorf("cuttle: the browser was taken over from you %ds ago, so driving calls are refused for another %ds."+ //nolint:err113 // refusal text
			" Read-only tools still work", int(time.Since(takenOver).Seconds()), int(wait.Seconds())+1)
	}
	got, err := leases.acquire(seed, owner, token)
	if errors.Is(err, errLeaseLost) && got.forced {
		s.leaseToken, s.takenOver = "", time.Now()
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
	s.leaseToken = got.token
	return nil
}

func (a *aggregator) tabs(ctx context.Context, s *agentSession, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Action string   `json:"action"`
		Index  *float64 `json:"index"`
		URL    string   `json:"url"`
	}
	_ = decodeArgs(req, &in)
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
	return a.call(ctx, s, toolTabs, args, req)
}

// screenshotRefusal covers every tab: shared tabs can expose a captured value
// to another session, and images cannot be masked.
func (a *aggregator) screenshotRefusal(_ *agentSession) string {
	a.captureMu.Lock()
	at := a.capturedAt
	a.captureMu.Unlock()
	left := screenshotHold - time.Since(at)
	if at.IsZero() || left <= 0 {
		return ""
	}
	return fmt.Sprintf("cuttle: a secret was captured %ds ago, so all screenshots are refused for another %ds"+
		" - an image cannot be masked. Use browser_snapshot, whose text is masked, or wait.",
		int(time.Since(at).Seconds()), int(left.Seconds())+1)
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
	pairs := a.maskPairs(ctx)
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

// maskPairs is every held secret and live password field value with its
// sentinel. They are read once per tools/call: a handler that masks what it
// works on (browser_find) and the middleware after it share one read of every
// page's password fields.
func (a *aggregator) maskPairs(ctx context.Context) [][]string {
	read := func() [][]string {
		pairs := a.r.m.pool.secrets.heldPairs(reservedSeed)
		for _, v := range a.browser.passwords(ctx) {
			pairs = append(pairs, exactPairs(v, passwordPlaceholder)...)
		}
		return pairs
	}
	c, ok := ctx.Value(maskPairsKey{}).(*maskPairsOnce)
	if !ok {
		return read()
	}
	c.once.Do(func() { c.pairs = read() })
	return c.pairs
}

type (
	maskPairsKey  struct{}
	maskPairsOnce struct {
		once  sync.Once
		pairs [][]string
	}
)

// maskMiddleware masks every tools/call result on its way to the agent.
func (a *aggregator) maskMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		ctx = context.WithValue(ctx, maskPairsKey{}, &maskPairsOnce{})
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
			out = append(out, []string{string(v[1 : len(v)-1]), string(r[1 : len(r)-1])})
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
			"--no-webmcp", "--shared-browser-context", "--output-dir", dir, "--allowed-hosts", allowedHost)
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
