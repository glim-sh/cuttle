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

	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/fingerprint"
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
	proxyCDPWebsocket(r.Context(), clientWS, target, label, user, pass)
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
func proxyCDPWebsocket(ctx context.Context, clientWS *websocket.Conn, target, label, user, pass string) {
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

	var wg sync.WaitGroup
	wg.Go(func() {
		defer cancel()
		for {
			typ, data, err := clientWS.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageText && inject {
				data = rewriteFetchEnable(data)
			}
			if err := cdpSend(typ, data); err != nil {
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
		if typ == websocket.MessageText {
			data = stampSWContext(data)
		}
		if err := clientWS.Write(ctx, typ, data); err != nil {
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
