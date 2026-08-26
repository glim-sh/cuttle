package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/glim-sh/cuttle/internal/fingerprint"
)

// injectedIDBase is the CDP command-id floor for our transparent proxy-auth
// commands - far above any id a real client uses, so their responses are
// recognizable and swallowed rather than forwarded.
const injectedIDBase = 2_000_000_000

// injectedIDPrefilter is the cheap byte guard for injected-command responses.
// injectedIDBase is 2_000_000_000, so every id we send serializes as "id":2000...
// Gating swallowInjected on len(injectedIDs) alone is a STATE gate, not a content
// one: one outstanding id then sends every frame of the session through a full
// JSON decode (measured 6.7us + 5.6KB/frame vs 330ns + 0 allocs with this).
// swallowInjected re-checks exact membership, so the prefilter needs no precision.
var injectedIDPrefilter = []byte(`"id":2000`)

// methodAttachedToTarget is the frame the per-page pins (focus emulation, locale)
// and the service_worker stamp all key off: it is the only place a new session id
// is announced.
const methodAttachedToTarget = "Target.attachedToTarget"

// attachedToTargetBytes is the prefilter form of methodAttachedToTarget.
var attachedToTargetBytes = []byte(`"` + methodAttachedToTarget + `"`)

// Prefilters for the frames that retire a session's cached isolated world.
// executionContextCreated is deliberately NOT matched: it retires nothing and is
// one of the most frequent frames on the wire, so matching it would pay a full
// decode per frame.
var (
	frameNavigatedBytes = []byte(`"Page.frameNavigated"`)
	execDestroyedBytes  = []byte(`"Runtime.executionContextDestroyed"`)
	execClearedBytes    = []byte(`"Runtime.executionContextsCleared"`)
)

// methodSetLocaleOverride pins ICU/Intl for a session; the fork's
// --fingerprint-locale moves navigator.language but not ICU's default.
const methodSetLocaleOverride = "Emulation.setLocaleOverride"

// methodSetFocusEmulation keeps a non-foreground tab rendering. Chrome runs no
// compositor frames for a hidden tab, so requestAnimationFrame never fires there
// - and Playwright's "stable" actionability check is a bare rAF loop with no
// timeout, so click/hover/check/selectOption/scrollIntoViewIfNeeded hang until
// the whole action times out ("waiting for element to be visible, enabled and
// stable", never the matching "element is ..."). Playwright itself sends this per
// main frame, but its CDP-attach path skips it whenever the client passes
// noDefaults (@playwright/cli does) and the page is in the default context -
// which is every page behind an attach. So the daemon pins it. Measured against
// this image, driving a background tab the way @playwright/cli does: rAF never
// fired and click() hit its 10s timeout without the pin; with it, rAF in 1-4ms
// and click() in 565-649ms. It also holds for every tab at once, unlike
// bringToFront, which is exclusive and would yank the VNC view out from the user.
// It also restores document.hasFocus()==true, which detectors read directly.
const methodSetFocusEmulation = "Emulation.setFocusEmulationEnabled"

// synthBrowserContextID is stamped onto default-context service_worker targets
// (see stampSWContext). Any truthy value works: playwright looks it up, misses,
// and falls back to its default context; it never resolves to a real id.
const synthBrowserContextID = "0000000000000000000000000000CA5E"

const wsReadLimit = -1 // disable coder/websocket's default message size cap

func (m *multiplexer) handleWSSeed(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedOrigin(w, r) {
		return
	}
	seed := r.PathValue("seed")
	path := r.PathValue("path")

	cp, err := m.pool.getOrLaunch(r.Context(), connectRequest{seed: seed})
	if err != nil {
		writeLaunchError(w, err)
		return
	}
	m.serveWS(w, r, cp, seed, "CDP seed="+seed+" ["+path+"]", path)
}

func (m *multiplexer) handleWSDefault(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedOrigin(w, r) {
		return
	}
	path := r.PathValue("path")

	cp, err := m.pool.getOrLaunch(r.Context(), connectRequest{})
	if err != nil {
		writeLaunchError(w, err)
		return
	}
	// The pool decides what an unseeded connection means - the reserved seed in
	// session mode, the operator's --fingerprint default in pool mode - so ask it
	// rather than assuming. Assuming reservedSeed here refcounted a key no
	// instance was stored under, which left the idle reaper free to kill a
	// browser with a live client on it.
	seedKey, lerr := m.pool.seedKeyFor("")
	if lerr != nil {
		writeLaunchError(w, lerr)
		return
	}
	m.serveWS(w, r, cp, seedKey, "CDP default ["+path+"]", path)
}

func (m *multiplexer) serveWS(w http.ResponseWriter, r *http.Request, cp *chromeInstance, seedKey, label, path string) {
	// Origin already enforced by rejectUntrustedOrigin.
	clientWS, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		logError("%s: accept failed: %v", label, err)
		return
	}

	m.pool.connect(seedKey)
	defer m.pool.disconnect(seedKey)

	target := "ws://127.0.0.1:" + strconv.Itoa(cp.cdpPort) + "/devtools/" + path
	_, user, pass := fingerprint.SplitProxyAuth(cp.proxy)
	proxyCDPWebsocket(r.Context(), clientWS, target, label, cdpSessionOpts{
		user: user, pass: pass, humanize: m.humanize,
		keepAlive: cp, locale: cp.locale,
		allowContexts: m.allowContexts,
		secrets:       m.pool.secrets, seed: seedKey,
	})
}

// cdpSessionOpts is the per-seed configuration a proxied CDP session needs.
// Grouped rather than passed positionally: user/pass/locale are all bare strings
// and transposing them is silent (wrong credentials, wrong identity).
type cdpSessionOpts struct {
	user, pass    string // proxy credentials; user == "" means the seed has no proxy auth
	humanize      bool
	keepAlive     *chromeInstance // owns the tab that holds this browser open
	locale        string          // seed locale; pins ICU/Intl per page session (see pinPage)
	allowContexts bool            // see blockContextCreation
	// secrets is the pool-wide secret store and seed is this connection's bucket
	// in it. Unlike locale, the store is keyed per seed, so the key has to travel
	// with it - the humanizer cannot derive it from the connection.
	secrets *secretStore
	seed    string
}

// proxyCDPWebsocket pipes CDP frames between the client and the seed's Chrome.
//
// When the seed runs behind a credential-stripped --proxy-server (the forks
// reject inline creds), it transparently answers proxy 407s: the client's own
// Fetch.enable is rewritten to also handleAuthRequests, and the resulting
// Fetch.authRequired events are intercepted here and answered with the stored
// credentials over CDP - never surfaced to the client. This rides the client's
// OWN Fetch session, so it works for HTTPS CONNECT and does not conflict with
// the client's own request interception.
func proxyCDPWebsocket(ctx context.Context, clientWS *websocket.Conn, target, label string, opts cdpSessionOpts) {
	user, pass, humanize, keepAlive := opts.user, opts.pass, opts.humanize, opts.keepAlive
	allowContexts := opts.allowContexts
	inject := user != ""

	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	cdpWS, dialResp, err := websocket.Dial(dialCtx, target, nil)
	dialCancel()
	if dialResp != nil && dialResp.Body != nil {
		_ = dialResp.Body.Close()
	}
	if err != nil {
		logError("%s error: %v", label, err)
		_ = clientWS.Close(websocket.StatusInternalError, "cdp dial failed")
		return
	}
	logInfo("%s: connected to %s", label, target)
	clientWS.SetReadLimit(wsReadLimit)
	cdpWS.SetReadLimit(wsReadLimit)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var cdpMu sync.Mutex
	cdpSend := func(typ websocket.MessageType, data []byte) error {
		cdpMu.Lock()
		defer cdpMu.Unlock()
		return cdpWS.Write(ctx, typ, data)
	}
	// clientSend serializes writes to the client: the reader goroutine below may
	// answer a blocked command while the main loop is forwarding a Chrome frame.
	var clientMu sync.Mutex
	clientSend := func(typ websocket.MessageType, data []byte) error {
		clientMu.Lock()
		defer clientMu.Unlock()
		return clientWS.Write(ctx, typ, data)
	}

	h := newHumanizer(ctx, humanize, opts.secrets, opts.seed, cdpSend, clientSend)

	// preprocessClient applies the client->browser guardrails to one frame:
	// blockContextCreation answers and drops it; the humanizer may replace an
	// Input.* command with a motion sequence it answers itself; proxy-auth rewrites
	// Fetch.enable. done=true means the frame was fully handled - do not forward.
	preprocessClient := func(typ websocket.MessageType, data []byte) ([]byte, bool) {
		if typ != websocket.MessageText {
			return data, false
		}
		if allowContexts {
			data = stripContextIdentityOverrides(data)
		} else if blocked, resp := blockContextCreation(data); blocked {
			_ = clientSend(websocket.MessageText, resp)
			return nil, true
		}
		// Browser.close from one client would take the seed down for every other
		// client and the viewer. Translate it into "detach this client", which is
		// what connectOverCDP means by it anyway. `cuttle down` is unaffected: it
		// signals the process, not CDP.
		if blocked, resp := blockBrowserTeardown(data); blocked {
			if resp != nil {
				_ = clientSend(websocket.MessageText, resp)
			}
			cancel()
			return nil, true
		}
		// One tab always holds this browser open, so a teardown that closes every
		// page cannot exit Chrome out from under the viewer and the other clients.
		// It is the session's own tab, so a driver lists and drives it like any
		// other - and its close is HONORED, once a replacement exists to take over
		// the job. Refusing outright (the old behavior) answered with a success
		// that never produced Target.targetDestroyed, which hangs any driver that
		// waits for the target to die; Playwright's page.close() does, forever.
		if keepAlive != nil && bytes.Contains(data, []byte("Target.closeTarget")) &&
			closeTargetID(data) == keepAlive.keepAliveID() {
			if !keepAlive.replaceKeepAlive(ctx, closeTargetID(data)) {
				// No replacement: refusing is the lesser evil - a hung close beats
				// a browser that exits under everyone using it.
				logWarn("%s: could not open a replacement for the keep-alive tab; refusing its close", label)
				if resp := keepAliveCloseResponse(data); resp != nil {
					_ = clientSend(websocket.MessageText, resp)
					return nil, true
				}
			}
		}
		// Secrets run OUTSIDE the humanize gate below: --humanize=false is a
		// supported mode, and behind the gate it would type `{{cuttle:NAME}}`
		// literally into a live password field. The frame it returns may carry a
		// substituted value (that is this mode's emission path), so it replaces
		// data rather than only being inspected.
		out, handled := h.handleSecretFrame(data)
		if handled {
			return nil, true
		}
		data = out
		if h.enabled && h.handleClientFrame(data) {
			return nil, true
		}
		if inject {
			data = rewriteFetchEnable(data)
		}
		return data, false
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		defer cancel()
		for {
			typ, data, err := clientWS.Read(ctx)
			if err != nil {
				return
			}
			out, done := preprocessClient(typ, data)
			if done {
				continue
			}
			if err := cdpSend(typ, out); err != nil {
				return
			}
		}
	})

	// id -> method, so a failed injected command can name itself in the log.
	injectedIDs := map[int64]string{}
	nextInjected := int64(injectedIDBase)

	// sendInjected issues one proxy-owned command under the next injected id and
	// registers that id so the browser's reply is swallowed rather than forwarded
	// to a driver that never sent it. Registration follows a successful send, like
	// the proxy-auth path below and for the same reason: an id registered for a
	// command that never went out is never answered, so it would pin
	// len(injectedIDs) > 0 - and the per-frame prefilter scan it gates - for the
	// life of the connection.
	sendInjected := func(method, sid string, params map[string]any) {
		cmd := dispatchCmd(nextInjected, method, sid, params)
		if cmd == nil || cdpSend(websocket.MessageText, cmd) != nil {
			return
		}
		injectedIDs[nextInjected] = method
		nextInjected++
	}

	// pinPage applies the per-page DevTools overrides to one session ("" for a
	// client that dialed a page endpoint and drives its target directly):
	//
	//   - focus emulation, so a non-foreground tab keeps compositing and a
	//     driver's actionability wait cannot hang on it (methodSetFocusEmulation).
	//   - ICU/Intl locale. The fork's --fingerprint-locale moves navigator.language
	//     but NOT ICU's default, so Intl.DateTimeFormat().resolvedOptions().locale
	//     keeps reporting en-US - a mismatch no real browser has and one CreepJS
	//     surfaces directly. There is no launch flag for it (--lang is inert
	//     headless).
	//
	// Both last as long as the session that set them, which is this proxied
	// connection, and re-apply on the next attach. A page that navigates keeps
	// them (they survive a renderer swap).
	pinPage := func(sid string) {
		sendInjected(methodSetFocusEmulation, sid, map[string]any{"enabled": true})
		if opts.locale != "" {
			sendInjected(methodSetLocaleOverride, sid, map[string]any{keyLocale: opts.locale})
		}
	}

	// A client that dials a PAGE endpoint (/devtools/page/<id>) drives that target
	// directly and never sees Target.attachedToTarget, so the per-attach pin below
	// never fires for it. Pin this session up front, BEFORE the client's reader
	// goroutine starts, so a driver that pipelines its first command cannot beat
	// the pin to Chrome; browser-endpoint clients get theirs per attached page.
	if strings.Contains(target, "/devtools/page/") {
		pinPage("")
	}

	for {
		typ, data, err := cdpWS.Read(ctx)
		if err != nil {
			break
		}
		// Drop the browser's reply to any command the proxy itself sent - the driver
		// never issued those ids and would fault on an unknown response. Every check
		// in this loop prefilters with bytes.Contains before decoding: in steady
		// state a CDP session streams thousands of frames none of them care about.
		if typ == websocket.MessageText && len(injectedIDs) > 0 &&
			bytes.Contains(data, injectedIDPrefilter) && swallowInjected(data, injectedIDs) {
			continue
		}
		// One scan for the frame both the per-page pins and the service_worker stamp
		// key off, reused by each below rather than rescanned - the needle is long
		// and frames are uncapped (wsReadLimit), so a second pass is not free.
		isAttach := typ == websocket.MessageText && bytes.Contains(data, attachedToTargetBytes)
		if isAttach {
			if psid := attachedPageSession(data); psid != "" {
				pinPage(psid)
			}
		}
		if typ == websocket.MessageText &&
			(bytes.Contains(data, frameNavigatedBytes) ||
				bytes.Contains(data, execDestroyedBytes) ||
				bytes.Contains(data, execClearedBytes)) {
			h.invalidateWorld(data)
		}
		if inject && typ == websocket.MessageText &&
			bytes.Contains(data, []byte(`"Fetch.authRequired"`)) {
			handled, cmd := handleProxyAuth(data, injectedIDs, nextInjected, user, pass)
			if cmd != nil {
				nextInjected++
				_ = cdpSend(websocket.MessageText, cmd)
			}
			if handled {
				continue
			}
		}
		// Swallow responses to the humanizer's injected Input commands so the
		// driver never sees ids it did not send. Near-free in steady state.
		if typ == websocket.MessageText && h.maybeSwallow(data) {
			continue
		}
		if isAttach {
			data = stampSWContext(data)
		}
		if err := clientSend(typ, data); err != nil {
			break
		}
	}

	cancel()
	_ = cdpWS.Close(websocket.StatusNormalClosure, "")
	_ = clientWS.Close(websocket.StatusNormalClosure, "")
	wg.Wait()
	logInfo("%s: disconnected", label)
}

// stampSWContext works around Chrome 148 reporting a site's service_worker
// target under the default browser context with an EMPTY browserContextId.
// playwright-core's connectOverCDP asserts that field is truthy in its
// Target.attachedToTarget handler, and the uncaught throw kills the client
// process (repro: any page that registers a service worker). Stamping a
// synthetic id makes the assert pass; playwright then falls back to its default
// context and handles the SW normally. The browser and page stay fully
// authentic - nothing in navigator is patched. Only service_worker
// attachedToTarget frames with a missing id are touched.
// Callers gate this on the frame already being a Target.attachedToTarget.
func stampSWContext(data []byte) []byte {
	if !bytes.Contains(data, []byte(`"service_worker"`)) {
		return data
	}
	msg, ok := decodeCDP(data)
	if !ok {
		return data
	}
	if asString(msg["method"]) != methodAttachedToTarget {
		return data
	}
	params, _ := msg["params"].(map[string]any)
	targetInfo, _ := params["targetInfo"].(map[string]any)
	if targetInfo == nil || asString(targetInfo["type"]) != "service_worker" {
		return data
	}
	if bcid, ok := targetInfo["browserContextId"]; ok && asString(bcid) != "" {
		return data
	}
	targetInfo["browserContextId"] = synthBrowserContextID
	out, err := json.Marshal(msg)
	if err != nil {
		return data
	}
	return out
}

// swallowInjected reports whether data is the browser's response to a command
// the proxy originated (proxy-auth, the per-page focus/locale pins), and consumes it. The client
// never sent those ids, so forwarding the reply can fault a strict driver.
func swallowInjected(data []byte, injectedIDs map[int64]string) bool {
	msg, ok := decodeCDP(data)
	if !ok {
		return false
	}
	mid, ok := asInt(msg["id"])
	if !ok {
		return false
	}
	method, ours := injectedIDs[mid]
	if !ours {
		return false
	}
	delete(injectedIDs, mid)
	if e, hasErr := msg["error"]; hasErr {
		// Emulation.setLocaleOverride is claimed per DevTools session but applied per
		// renderer PROCESS, so every session after the first to touch a given
		// renderer is refused - while still inheriting the locale the first one set.
		// Expected and benign, so it stays out of the log; see
		// docs/2608-18-improvements-issues-research for the real fix.
		if method == methodSetLocaleOverride && strings.Contains(errText(e), "Another locale override") {
			return true
		}
		logWarn("injected CDP command failed (%s): %v", method, e)
	}
	return true
}

// attachedPageSession returns the session id a Target.attachedToTarget frame
// announces for a PAGE target, or "" for anything else. Page targets only: the
// per-page pins are meaningless on workers/service_workers, and an error reply
// there would just be noise.
func attachedPageSession(data []byte) string {
	msg, ok := decodeCDP(data)
	if !ok || asString(msg[cdpMethod]) != methodAttachedToTarget {
		return ""
	}
	params, _ := msg[cdpParams].(map[string]any)
	sessionID := asString(params["sessionId"])
	targetInfo, _ := params["targetInfo"].(map[string]any)
	if sessionID == "" || targetInfo == nil || asString(targetInfo["type"]) != "page" {
		return ""
	}
	return sessionID
}

// blockContextCreation enforces the one-identity-per-seed contract at the
// protocol level. A client that calls Target.createBrowserContext gets a fresh,
// separate browser context - a second identity behind the same seed's
// fingerprint/proxy - which silently defeats the "attach, never create a
// context" guardrail the briefing states. Instead of trusting prose, the proxy
// rejects the command and answers the client with a CDP error echoing the
// original id/sessionId, so a driver that reflexively opens a context (e.g.
// Playwright's newContext) sees a clean failure rather than an orphaned identity.
// A new SEED is the supported way to get a separate identity.
//
// --allow-context-creation lifts this for drivers that open a context
// unconditionally and cannot be told not to (Playwright's new_context is not
// optional in some scraping stacks). The fingerprint and geoip survive it for
// free - they are Chrome launch flags, so every context in the process inherits
// them - but the proxy does NOT, which is why the allowed path still runs
// stripContextIdentityOverrides. What the opt-out does give up is the guarantee
// that a driver cannot hold two SEPARATE cookie jars behind one seed.
func blockContextCreation(data []byte) (bool, []byte) {
	if !bytes.Contains(data, []byte("Target.createBrowserContext")) {
		return false, nil
	}
	msg, ok := decodeCDP(data)
	if !ok {
		return false, nil
	}
	if asString(msg["method"]) != "Target.createBrowserContext" {
		return false, nil
	}
	resp := map[string]any{
		"error": map[string]any{
			"code": -32000,
			"message": "Target.createBrowserContext is blocked by cuttle: one identity per seed - " +
				"attach to the existing default context, or start a new seed for a separate identity",
		},
	}
	if id, ok := msg["id"]; ok {
		resp["id"] = id
	}
	if sid := asString(msg["sessionId"]); sid != "" {
		resp["sessionId"] = sid
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return false, nil
	}
	return true, out
}

// browserTeardownMethods end the whole browser process, not one client's session.
var browserTeardownMethods = map[string]struct{}{
	"Browser.close": {}, "Browser.crash": {}, "Browser.crashGpuProcess": {},
}

// blockBrowserTeardown answers a process-ending Browser.* command with success
// and lets the caller drop that client instead of killing the seed.
func blockBrowserTeardown(data []byte) (bool, []byte) {
	if !bytes.Contains(data, []byte(`"Browser.c`)) {
		return false, nil
	}
	msg, ok := decodeCDP(data)
	if !ok {
		return false, nil
	}
	if _, teardown := browserTeardownMethods[asString(msg[cdpMethod])]; !teardown {
		return false, nil
	}
	resp := map[string]any{cdpResult: map[string]any{}}
	if id, ok := msg[cdpID]; ok {
		resp[cdpID] = id
	}
	if sid := asString(msg[cdpSessionID]); sid != "" {
		resp[cdpSessionID] = sid
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return true, nil
	}
	return true, out
}

// errText pulls the message out of a CDP error object for classification.
func errText(e any) string {
	m, _ := e.(map[string]any)
	if m == nil {
		return ""
	}
	return asString(m["message"])
}

// contextIdentityParams are Target.createBrowserContext params that would move a
// created context off the seed's identity.
var contextIdentityParams = []string{"proxyServer", "proxyBypassList"}

// stripContextIdentityOverrides drops the params above from a createBrowserContext
// command. --allow-context-creation is about letting a driver HAVE a context, not
// about letting it choose a different identity: a context created with its own
// proxyServer egresses somewhere else while still presenting the seed's
// fingerprint, timezone and WebRTC IP - incoherent on its face - and the
// proxy-auth injector would then answer that context's 407 with the SEED's stored
// credentials, handing them to whatever host the driver named.
func stripContextIdentityOverrides(data []byte) []byte {
	if !bytes.Contains(data, []byte("Target.createBrowserContext")) {
		return data
	}
	msg, ok := decodeCDP(data)
	if !ok || asString(msg["method"]) != "Target.createBrowserContext" {
		return data
	}
	params, _ := msg["params"].(map[string]any)
	stripped := false
	for _, k := range contextIdentityParams {
		if _, present := params[k]; present {
			delete(params, k)
			stripped = true
		}
	}
	if !stripped {
		return data
	}
	out, err := json.Marshal(msg)
	if err != nil {
		return data
	}
	logWarn("dropped %v from Target.createBrowserContext: a context cannot pick its own identity", contextIdentityParams)
	return out
}

// rewriteFetchEnable adds handleAuthRequests to a client's Fetch.enable so
// Chrome surfaces proxy 407s as Fetch.authRequired on the client's own session.
func rewriteFetchEnable(data []byte) []byte {
	if !bytes.Contains(data, []byte(`"Fetch.enable"`)) {
		return data
	}
	msg, ok := decodeCDP(data)
	if !ok {
		return data
	}
	if asString(msg["method"]) != "Fetch.enable" {
		return data
	}
	params, _ := msg["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
		msg["params"] = params
	}
	if v, ok := params["handleAuthRequests"].(bool); ok && v {
		return data
	}
	params["handleAuthRequests"] = true
	out, err := json.Marshal(msg)
	if err != nil {
		return data
	}
	return out
}

// handleProxyAuth inspects a Chrome->client frame. It returns (swallow, command):
// a response to one of our injected commands swallows it; a Fetch.authRequired
// yields a continueWithAuth command to send and is swallowed (the client never
// asked for auth handling); anything else is forwarded untouched.
func handleProxyAuth(data []byte, injectedIDs map[int64]string, cmdID int64, user, pass string) (bool, []byte) {
	msg, ok := decodeCDP(data)
	if !ok {
		return false, nil
	}
	if asString(msg["method"]) != "Fetch.authRequired" {
		return false, nil
	}
	params, _ := msg["params"].(map[string]any)
	challenge, _ := params["authChallenge"].(map[string]any)
	var response map[string]any
	if asString(challenge["source"]) == "Proxy" {
		response = map[string]any{"response": "ProvideCredentials", "username": user, "password": pass}
	} else {
		response = map[string]any{"response": "Default"}
	}
	cmd := map[string]any{
		"id":     cmdID,
		"method": "Fetch.continueWithAuth",
		"params": map[string]any{
			"requestId":             params["requestId"],
			"authChallengeResponse": response,
		},
	}
	if sid := asString(msg["sessionId"]); sid != "" {
		cmd["sessionId"] = sid
	}
	out, err := json.Marshal(cmd)
	if err != nil {
		return true, nil
	}
	// Register only once the command is known to be sendable: an id registered for
	// a command that never goes out is never answered, so it would pin
	// len(injectedIDs) > 0 for the life of the connection.
	injectedIDs[cmdID] = "Fetch.continueWithAuth"
	return true, out
}

// decodeCDP unmarshals a CDP frame with number fidelity preserved (json.Number)
// so large command ids survive a re-marshal.
func decodeCDP(data []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var msg map[string]any
	if err := dec.Decode(&msg); err != nil {
		return nil, false
	}
	return msg, true
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asInt(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return i, true
}
