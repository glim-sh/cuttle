package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/glim-sh/cuttle/internal/fingerprint"
)

// injectedIDBase is the CDP command-id floor for our transparent proxy-auth
// commands - far above any id a real client uses, so their responses are
// recognizable and swallowed rather than forwarded.
const injectedIDBase = 2_000_000_000

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
	_, user, pass := fingerprint.SplitProxyAuth(cp.proxy)
	m.serveWS(w, r, cp, seed, "CDP seed="+seed+" ["+path+"]", path, user, pass)
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
	_, user, pass := fingerprint.SplitProxyAuth(cp.proxy)
	m.serveWS(w, r, cp, reservedSeed, "CDP default ["+path+"]", path, user, pass)
}

func (m *multiplexer) serveWS(w http.ResponseWriter, r *http.Request, cp *chromeInstance, seedKey, label, path, user, pass string) {
	// Origin already enforced by rejectUntrustedOrigin.
	clientWS, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		logError("%s: accept failed: %v", label, err)
		return
	}

	m.pool.connect(seedKey)
	defer m.pool.disconnect(seedKey)

	target := "ws://127.0.0.1:" + strconv.Itoa(cp.cdpPort) + "/devtools/" + path
	proxyCDPWebsocket(r.Context(), clientWS, target, label, user, pass, m.humanize, cp.keepAliveID)
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
func proxyCDPWebsocket(ctx context.Context, clientWS *websocket.Conn, target, label, user, pass string, humanize bool, keepAliveID string) {
	inject := user != ""
	var keepAliveBytes []byte
	if keepAliveID != "" {
		keepAliveBytes = []byte(keepAliveID)
	}

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

	h := newHumanizer(ctx, humanize, cdpSend, clientSend)

	// preprocessClient applies the client->browser guardrails to one frame:
	// blockContextCreation answers and drops it; the humanizer may replace an
	// Input.* command with a motion sequence it answers itself; proxy-auth rewrites
	// Fetch.enable. done=true means the frame was fully handled - do not forward.
	preprocessClient := func(typ websocket.MessageType, data []byte) ([]byte, bool) {
		if typ != websocket.MessageText {
			return data, false
		}
		if blocked, resp := blockContextCreation(data); blocked {
			_ = clientSend(websocket.MessageText, resp)
			return nil, true
		}
		// The daemon owns an immortal keep-alive tab so a teardown that closes the
		// last page can't exit Chrome. The tab is hidden from drivers, so this
		// close-refusal is only a backstop for a driver that learned its id anyway.
		if keepAliveID != "" && bytes.Contains(data, []byte("Target.closeTarget")) &&
			closeTargetID(data) == keepAliveID {
			if resp := keepAliveCloseResponse(data); resp != nil {
				_ = clientSend(websocket.MessageText, resp)
				return nil, true
			}
		}
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

	injectedIDs := map[int64]struct{}{}
	nextInjected := int64(injectedIDBase)
	for {
		typ, data, err := cdpWS.Read(ctx)
		if err != nil {
			break
		}
		// Prefilter before the full JSON decode: handleProxyAuth only acts on a
		// response to one of our injected commands (which exist only while
		// injectedIDs is non-empty) or a Fetch.authRequired event. In steady
		// state a CDP session streams thousands of other frames; skip decoding
		// them, matching the bytes.Contains guard the sibling patches use.
		if inject && typ == websocket.MessageText &&
			(len(injectedIDs) > 0 || bytes.Contains(data, []byte(`"Fetch.authRequired"`))) {
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
		if h.enabled && typ == websocket.MessageText && h.maybeSwallow(data) {
			continue
		}
		// Hide the daemon-owned keep-alive tab from the driver: drop its lifecycle
		// events and strip it from getTargets results. Gated on the id bytes so only
		// the rare frame that mentions the tab pays the decode.
		if keepAliveBytes != nil && typ == websocket.MessageText && bytes.Contains(data, keepAliveBytes) {
			out, drop := hideKeepAlive(data, keepAliveID)
			if drop {
				continue
			}
			data = out
		}
		if typ == websocket.MessageText {
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
func stampSWContext(data []byte) []byte {
	if !bytes.Contains(data, []byte(`"Target.attachedToTarget"`)) ||
		!bytes.Contains(data, []byte(`"service_worker"`)) {
		return data
	}
	msg, ok := decodeCDP(data)
	if !ok {
		return data
	}
	if asString(msg["method"]) != "Target.attachedToTarget" {
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

// blockContextCreation enforces the one-identity-per-seed contract at the
// protocol level. A client that calls Target.createBrowserContext gets a fresh,
// separate browser context - a second identity behind the same seed's
// fingerprint/proxy - which silently defeats the "attach, never create a
// context" guardrail the briefing states. Instead of trusting prose, the proxy
// rejects the command and answers the client with a CDP error echoing the
// original id/sessionId, so a driver that reflexively opens a context (e.g.
// Playwright's newContext) sees a clean failure rather than an orphaned identity.
// A new SEED is the supported way to get a separate identity.
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
func handleProxyAuth(data []byte, injectedIDs map[int64]struct{}, cmdID int64, user, pass string) (bool, []byte) {
	msg, ok := decodeCDP(data)
	if !ok {
		return false, nil
	}
	if mid, ok := asInt(msg["id"]); ok {
		if _, ours := injectedIDs[mid]; ours {
			delete(injectedIDs, mid)
			if _, hasErr := msg["error"]; hasErr {
				logWarn("proxy-auth: continueWithAuth failed: %v", msg["error"])
			}
			return true, nil
		}
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
	injectedIDs[cmdID] = struct{}{}
	out, err := json.Marshal(cmd)
	if err != nil {
		return true, nil
	}
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
