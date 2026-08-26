package serve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// `cuttle grab <url>` pulls bytes out of the authenticated browser to the host.
// It is the same boundary crossing downloads already makes - the browser holds
// the session, the caller is outside the container - for the case that has no
// download button: an API response an agent needs while signed in.
//
// The mechanism is picked by ORIGIN, because the obvious single mechanism fails
// on the dominant case:
//
//   - Same-origin (or a blob: URL the page made): fetch it in the page's own
//     isolated world with credentials. Simple, no tab, no navigation.
//   - Cross-origin: an isolated world gets NO CORS exemption - it is not an
//     extension content script - so `fetch` there returns an opaque, unreadable
//     response. A scratch tab navigated to the URL does work: it sends the
//     browser's cookies for that origin and the body comes back over
//     Network.getResponseBody. Cookie auth only: it carries no Authorization
//     header, and the verb's own error says so.
const (
	grabTimeout   = 30 * time.Second
	grabBodyLimit = 8 << 20
	// ioReadChunk is passed explicitly on every IO.read: leaving size unset
	// truncates large documents.
	ioReadChunk = 32768
)

var (
	errNoGrabTarget  = errors.New("no page to grab from")
	errGrabFailed    = errors.New("grab failed")
	errCaptureFailed = errors.New("capture failed")
)

// handleGrab fetches one URL through the running browser and returns the bytes.
func (m *multiplexer) handleGrab(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	inst := m.runningSeedInstance(w, r)
	if inst == nil {
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil || body.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{keyError: "a url is required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), grabTimeout)
	defer cancel()

	data, err := grabURL(ctx, inst.cdpPort, body.URL)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{keyError: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

// grabURL resolves the page to grab from, picks the mechanism by origin, and
// returns the bytes.
func grabURL(ctx context.Context, port int, target string) ([]byte, error) {
	pageID, pageURL, err := activePage(ctx, port)
	if err != nil {
		return nil, err
	}
	conn, err := dialBrowser(ctx, port)
	if err != nil {
		return nil, err
	}
	defer conn.close()

	sid, err := conn.attach(ctx, pageID)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(target, "blob:") || sameOrigin(target, pageURL) {
		return conn.grabInPage(ctx, sid, target)
	}
	return conn.grabInScratchTab(ctx, target)
}

// sameOrigin compares scheme+host+port, the unit the browser's own CORS check
// uses. A URL that will not parse is treated as cross-origin: the scratch-tab
// path handles more shapes, so guessing wrong there costs a tab, not a failure.
func sameOrigin(a, b string) bool {
	ua, erra := url.Parse(a)
	ub, errb := url.Parse(b)
	if erra != nil || errb != nil {
		return false
	}
	return ua.Scheme == ub.Scheme && ua.Host == ub.Host && ua.Host != ""
}

// activePage picks the page a grab runs against: the first ordinary http(s)
// page. It ERRORS rather than guessing when there is none - a browser showing
// only chrome:// surfaces has no authenticated context to grab from, and
// silently using one would produce a confusing empty result.
func activePage(ctx context.Context, port int) (string, string, error) {
	var pages []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if err := fetchCDP(ctx, port, "/json/list", &pages); err != nil {
		return "", "", fmt.Errorf("%w: could not list the browser's tabs", errNoGrabTarget)
	}
	for _, t := range pages {
		if t.Type != targetPage || t.ID == "" {
			continue
		}
		if strings.HasPrefix(t.URL, "http://") || strings.HasPrefix(t.URL, "https://") {
			return t.ID, t.URL, nil
		}
	}
	return "", "", fmt.Errorf("%w: no tab is on an http(s) page, so there is no signed-in context to grab from - navigate a tab first (cuttle open <url>)", errNoGrabTarget)
}

// ---------------------------------------------------------------------------
// A small raw CDP connection
// ---------------------------------------------------------------------------

// cdpConn is one websocket to the browser endpoint with flat sessions. chromedp
// is deliberately not used here: its session management creates targets of its
// own, and a grab must not open tabs the user did not ask for beyond the single
// scratch tab it closes itself.
type cdpConn struct {
	ws     *websocket.Conn
	nextID int64
}

func dialBrowser(ctx context.Context, port int) (*cdpConn, error) {
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := fetchCDP(ctx, port, "/json/version", &v); err != nil || v.WebSocketDebuggerURL == "" {
		return nil, fmt.Errorf("%w: the browser's CDP endpoint did not answer", errGrabFailed)
	}
	ws, resp, err := websocket.Dial(ctx, v.WebSocketDebuggerURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errGrabFailed, err)
	}
	ws.SetReadLimit(wsReadLimit)
	return &cdpConn{ws: ws}, nil
}

func (c *cdpConn) close() { _ = c.ws.Close(websocket.StatusNormalClosure, "") }

// send issues one command and returns its id, so the caller can recognize the
// reply among the events streaming past.
func (c *cdpConn) send(ctx context.Context, sid, method string, params map[string]any) (int64, error) {
	c.nextID++
	cmd := map[string]any{cdpID: c.nextID, cdpMethod: method}
	if params != nil {
		cmd[cdpParams] = params
	}
	if sid != "" {
		cmd[cdpSessionID] = sid
	}
	b, err := json.Marshal(cmd)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errGrabFailed, err)
	}
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		return 0, fmt.Errorf("%w: %w", errGrabFailed, err)
	}
	return c.nextID, nil
}

// await reads frames until fn reports it has what it wanted. fn sees every
// decoded frame - replies and events alike - which is what lets one wait watch
// for a response event and its command's reply at the same time.
func (c *cdpConn) await(ctx context.Context, fn func(msg map[string]any) bool) error {
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return fmt.Errorf("%w: %w", errGrabFailed, err)
		}
		msg, ok := decodeCDP(data)
		if !ok {
			continue
		}
		if fn(msg) {
			return nil
		}
	}
}

// call sends a command and returns its result object.
func (c *cdpConn) call(ctx context.Context, sid, method string, params map[string]any) (map[string]any, error) {
	id, err := c.send(ctx, sid, method, params)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	var cdpErr string
	awaitErr := c.await(ctx, func(msg map[string]any) bool {
		if got, ok := asInt(msg[cdpID]); !ok || got != id {
			return false
		}
		if e, ok := msg["error"].(map[string]any); ok {
			cdpErr = asString(e["message"])
			return true
		}
		result, _ = msg[cdpResult].(map[string]any)
		return true
	})
	if awaitErr != nil {
		return nil, awaitErr
	}
	if cdpErr != "" {
		return nil, fmt.Errorf("%w: %s: %s", errGrabFailed, method, cdpErr)
	}
	return result, nil
}

func (c *cdpConn) attach(ctx context.Context, targetID string) (string, error) {
	res, err := c.call(ctx, "", "Target.attachToTarget", map[string]any{
		"targetId": targetID, "flatten": true,
	})
	if err != nil {
		return "", err
	}
	sid := asString(res["sessionId"])
	if sid == "" {
		return "", fmt.Errorf("%w: could not attach to the tab", errGrabFailed)
	}
	return sid, nil
}

// ---------------------------------------------------------------------------
// The two mechanisms
// ---------------------------------------------------------------------------

// pageFetchJS fetches a URL from the page's own context, with its cookies, and
// returns the response as a Blob. The URL rides as a callFunctionOn ARGUMENT
// rather than being formatted into the script text, which is what keeps escaping
// bugs out of an evaluated string. A Blob rather than text because text() would
// corrupt any binary body (a PDF, an image, a zip) - the bytes come back through
// IO.read instead, which is exact for both.
const pageFetchJS = `async function(u){` +
	`const r=await fetch(u,{credentials:'include'});` +
	`if(r.type==='opaque')throw new Error('the response was opaque - the page cannot read a cross-origin response without CORS');` +
	`if(!r.ok)throw new Error('HTTP '+r.status);` +
	`return await r.blob();}`

// grabInPage runs the fetch inside the page's isolated world and streams the
// resulting Blob out with IO.resolveBlob - no download, no file, no temp path.
// Used for a same-origin URL and for a blob: URL, which only the page that
// created it can read at all.
func (c *cdpConn) grabInPage(ctx context.Context, sid, target string) ([]byte, error) {
	worldID, err := c.isolatedWorld(ctx, sid)
	if err != nil {
		return nil, err
	}
	res, err := c.call(ctx, sid, "Runtime.callFunctionOn", map[string]any{
		"functionDeclaration": pageFetchJS,
		"executionContextId":  worldID,
		"arguments":           []any{map[string]any{keyValue: target}},
		"awaitPromise":        true,
	})
	if err != nil {
		return nil, err
	}
	if exc, ok := res["exceptionDetails"].(map[string]any); ok {
		return nil, fmt.Errorf("%w: the page could not fetch it: %s", errGrabFailed, exceptionText(exc))
	}
	result, _ := res[cdpResult].(map[string]any)
	objectID := asString(result["objectId"])
	if objectID == "" {
		return nil, fmt.Errorf("%w: the page returned nothing to read", errGrabFailed)
	}
	defer func() {
		_, _ = c.call(ctx, sid, "Runtime.releaseObject", map[string]any{"objectId": objectID})
	}()

	resolved, err := c.call(ctx, sid, "IO.resolveBlob", map[string]any{"objectId": objectID})
	if err != nil {
		return nil, err
	}
	uuid := asString(resolved["uuid"])
	if uuid == "" {
		return nil, fmt.Errorf("%w: the response could not be resolved to a readable stream", errGrabFailed)
	}
	return c.readStream(ctx, sid, "blob:"+uuid)
}

// exceptionText pulls the readable half out of a CDP exceptionDetails.
func exceptionText(exc map[string]any) string {
	if e, ok := exc["exception"].(map[string]any); ok {
		if d := asString(e["description"]); d != "" {
			return d
		}
	}
	return asString(exc["text"])
}

// readStream drains an IO stream. Two details are measured, not assumed: the
// per-chunk base64Encoded flag really does vary (false for a text blob, true for
// a binary one), so each chunk is decoded on its own - concatenating the base64
// text first would corrupt the result - and size is always passed explicitly,
// because leaving it unset truncates large documents.
func (c *cdpConn) readStream(ctx context.Context, sid, handle string) ([]byte, error) {
	defer func() {
		_, _ = c.call(ctx, sid, "IO.close", map[string]any{"handle": handle})
	}()
	var out []byte
	for {
		chunk, err := c.call(ctx, sid, "IO.read", map[string]any{"handle": handle, "size": ioReadChunk})
		if err != nil {
			return nil, err
		}
		data := asString(chunk["data"])
		if asBool(chunk["base64Encoded"]) {
			decoded, derr := base64.StdEncoding.DecodeString(data)
			if derr != nil {
				return nil, fmt.Errorf("%w: a chunk of the response did not decode", errGrabFailed)
			}
			out = append(out, decoded...)
		} else {
			out = append(out, data...)
		}
		if len(out) > grabBodyLimit {
			return nil, fmt.Errorf("%w: the response is larger than %d bytes - download it in the browser and pull it with `cuttle downloads` instead",
				errGrabFailed, grabBodyLimit)
		}
		if asBool(chunk["eof"]) {
			return out, nil
		}
	}
}

func (c *cdpConn) isolatedWorld(ctx context.Context, sid string) (int64, error) {
	tree, err := c.call(ctx, sid, "Page.getFrameTree", map[string]any{})
	if err != nil {
		return 0, err
	}
	frameTree, _ := tree["frameTree"].(map[string]any)
	frame, _ := frameTree["frame"].(map[string]any)
	frameID := asString(frame["id"])
	if frameID == "" {
		return 0, fmt.Errorf("%w: the tab has no main frame", errGrabFailed)
	}
	// Created fresh at point of use rather than cached: numeric context ids are
	// recycled across navigations, so a cached one can silently address a
	// different document.
	world, err := c.call(ctx, sid, "Page.createIsolatedWorld", map[string]any{
		"frameId": frameID, "worldName": "cuttle_grab",
	})
	if err != nil {
		return 0, err
	}
	id, ok := asInt(world["executionContextId"])
	if !ok || id == 0 {
		return 0, fmt.Errorf("%w: no isolated world to fetch from", errGrabFailed)
	}
	return id, nil
}

// grabInScratchTab navigates a tab of its own to the URL and reads the response
// body off the wire. This is the cross-origin path: it sends the browser's
// cookies for that origin, and it carries no Authorization header - a token-auth
// API is out of reach this way, which the error says rather than returning an
// empty body.
func (c *cdpConn) grabInScratchTab(ctx context.Context, target string) ([]byte, error) {
	created, err := c.call(ctx, "", "Target.createTarget", map[string]any{cdpURL: "about:blank"})
	if err != nil {
		return nil, err
	}
	targetID := asString(created["targetId"])
	defer func() {
		// The tab is ours; it never outlives the grab, even on failure.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_, _ = c.call(closeCtx, "", "Target.closeTarget", map[string]any{"targetId": targetID})
	}()

	sid, err := c.attach(ctx, targetID)
	if err != nil {
		return nil, err
	}
	if _, nerr := c.call(ctx, sid, "Network.enable", map[string]any{}); nerr != nil {
		return nil, nerr
	}
	navID, err := c.send(ctx, sid, "Page.navigate", map[string]any{cdpURL: target})
	if err != nil {
		return nil, err
	}

	// One wait watches for three things at once: the request id of the response
	// for our URL, that request finishing (the body is only readable then), and a
	// navigation that failed outright.
	requestID, netErr := "", ""
	finished := false
	awaitErr := c.await(ctx, func(msg map[string]any) bool {
		params, _ := msg[cdpParams].(map[string]any)
		if id, ok := asInt(msg[cdpID]); ok && id == navID {
			if e, isErr := msg["error"].(map[string]any); isErr {
				netErr = asString(e["message"])
				return true
			}
			if res, _ := msg[cdpResult].(map[string]any); res != nil {
				if text := asString(res["errorText"]); text != "" {
					netErr = text
					return true
				}
			}
			return false
		}
		switch asString(msg[cdpMethod]) {
		case "Network.responseReceived":
			resp, _ := params["response"].(map[string]any)
			if requestID == "" && sameURL(asString(resp[cdpURL]), target) {
				requestID = asString(params["requestId"])
			}
		case "Network.loadingFinished", "Network.loadingFailed":
			if requestID != "" && asString(params["requestId"]) == requestID {
				finished = true
				return true
			}
		}
		return false
	})
	if awaitErr != nil {
		return nil, awaitErr
	}
	if netErr != "" {
		return nil, fmt.Errorf("%w: the browser could not load it: %s", errGrabFailed, netErr)
	}
	if !finished || requestID == "" {
		return nil, fmt.Errorf("%w: no response for that URL", errGrabFailed)
	}

	res, err := c.call(ctx, sid, "Network.getResponseBody", map[string]any{"requestId": requestID})
	if err != nil {
		return nil, fmt.Errorf("%w - a response the browser turned into a download has no body to read;"+
			" pull it with `cuttle downloads` instead", err)
	}
	body := asString(res["body"])
	if asBool(res["base64Encoded"]) {
		decoded, derr := base64.StdEncoding.DecodeString(body)
		if derr != nil {
			return nil, fmt.Errorf("%w: the response body did not decode", errGrabFailed)
		}
		return decoded, nil
	}
	return []byte(body), nil
}

// sameURL compares two URLs ignoring a trailing slash difference, which is the
// one rewrite a navigation routinely performs on the URL it was given.
func sameURL(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

// ---------------------------------------------------------------------------
// capture --selector
// ---------------------------------------------------------------------------

// captureSelectorJS reads one element's value. The selector rides as an
// ARGUMENT, never formatted into the script text - the probe elsewhere in this
// package builds its expression with Sprintf and must not be copied here, where
// a selector is caller input.
//
// The empty case is split deliberately: a password Chrome autofilled but nobody
// has touched lives in a suggested value the page's own scripts cannot read, so
// reporting it as an empty capture would be wrong twice over.
const captureSelectorJS = `function(sel){` +
	`var el;try{el=document.querySelector(sel);}catch(e){return{ok:false,why:'that is not a valid CSS selector'};}` +
	`if(!el)return{ok:false,why:'no element matches that selector - note this does not pierce shadow DOM, and a cross-origin iframe is a separate target'};` +
	`var v=(typeof el.value==='string')?el.value:(el.textContent||'');` +
	`if(!v){var af=false;try{af=!!(el.matches&&el.matches(':autofill'));}catch(e){}` +
	`if(af)return{ok:false,why:'the browser autofilled this field but nobody has interacted with it, so its value is hidden from scripts until a real user gesture - click the field in the viewer first'};` +
	`return{ok:false,why:'that element holds no text'};}` +
	`return{ok:true,value:v};}`

// captureSelector reads one element's value out of the active page.
func captureSelector(ctx context.Context, port int, selector string) ([]byte, error) {
	pageID, _, err := activePage(ctx, port)
	if err != nil {
		return nil, err
	}
	conn, err := dialBrowser(ctx, port)
	if err != nil {
		return nil, err
	}
	defer conn.close()

	sid, err := conn.attach(ctx, pageID)
	if err != nil {
		return nil, err
	}
	worldID, err := conn.isolatedWorld(ctx, sid)
	if err != nil {
		return nil, err
	}
	res, err := conn.call(ctx, sid, "Runtime.callFunctionOn", map[string]any{
		"functionDeclaration": captureSelectorJS,
		"executionContextId":  worldID,
		"arguments":           []any{map[string]any{keyValue: selector}},
		"returnByValue":       true,
	})
	if err != nil {
		return nil, err
	}
	if exc, ok := res["exceptionDetails"].(map[string]any); ok {
		return nil, fmt.Errorf("%w: %s", errCaptureFailed, exceptionText(exc))
	}
	result, _ := res[cdpResult].(map[string]any)
	value, _ := result[keyValue].(map[string]any)
	if value == nil {
		return nil, fmt.Errorf("%w: the page returned nothing", errCaptureFailed)
	}
	if !asBool(value["ok"]) {
		return nil, fmt.Errorf("%w: %s", errCaptureFailed, asString(value["why"]))
	}
	return []byte(asString(value[keyValue])), nil
}
