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
	"strconv"
	"strings"
	"time"
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
	grabTimeout = 30 * time.Second
	// tabCreateTimeout bounds the scratch-tab create, which runs detached from the
	// caller so a cancelled request cannot orphan a tab.
	tabCreateTimeout = 5 * time.Second
	grabBodyLimit    = 8 << 20
	// ioReadChunk is passed explicitly on every IO.read: leaving size unset
	// truncates large documents. 1MB rather than the protocol's small default
	// because the read is sequential - at 32KB an 8MB grab is 256 round-trips.
	ioReadChunk = 1 << 20
)

var (
	// errNoPage is shared by grab and capture: both need a signed-in page, and
	// naming it after one of them mislabels the other's failures.
	errNoPage        = errors.New("no page to work with")
	errGrabFailed    = errors.New("grab failed")
	errCaptureFailed = errors.New("capture failed")
)

// handleGrab fetches one URL through the running browser and returns the bytes.
func (m *multiplexer) handleGrab(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	_, inst := m.runningSeedInstance(w, r)
	if inst == nil {
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, secretBodyLimit)).Decode(&body); err != nil || body.URL == "" {
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
	id, pageURL := findPage(ctx, port, func(u string) bool {
		return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
	})
	if id == "" {
		return "", "", fmt.Errorf("%w: no tab is on an http(s) page, so there is no signed-in context to read from - navigate a tab first (`cuttle open <url>`)", errNoPage)
	}
	return id, pageURL, nil
}

// findPage returns the first page target whose URL the caller accepts, with that
// URL. Shared by the two callers that ask the same question of /json/list with
// different ideas of which pages count.
func findPage(ctx context.Context, port int, want func(pageURL string) bool) (string, string) {
	var pages []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if err := fetchCDP(ctx, port, "/json/list", &pages); err != nil {
		return "", ""
	}
	for _, t := range pages {
		if t.Type == targetPage && t.ID != "" && want(t.URL) {
			return t.ID, t.URL
		}
	}
	return "", ""
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
	res, err := c.callInWorld(ctx, sid, pageFetchJS, target, false)
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

// reapMarkedTab closes a scratch tab whose create reply never arrived in time.
// Best effort by definition: it runs after a call that already failed, so a
// browser that has stopped answering simply keeps the tab.
func (c *cdpConn) reapMarkedTab(ctx context.Context, marker string) {
	reapCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tabCreateTimeout)
	defer cancel()
	res, err := c.call(reapCtx, "", "Target.getTargets", map[string]any{})
	if err != nil {
		return
	}
	infos, _ := res["targetInfos"].([]any)
	for _, t := range infos {
		info, _ := t.(map[string]any)
		if asString(info[cdpURL]) != marker {
			continue
		}
		id := asString(info[keyTargetID])
		logWarn("grab: reaping a scratch tab whose create reply arrived too late (target=%s)", id)
		_, _ = c.call(reapCtx, "", "Target.closeTarget", map[string]any{keyTargetID: id})
		return
	}
}

// grabInScratchTab navigates a tab of its own to the URL and reads the response
// body off the wire. This is the cross-origin path: it sends the browser's
// cookies for that origin, and it carries no Authorization header - a token-auth
// API is out of reach this way, which the error says rather than returning an
// empty body.
func (c *cdpConn) grabInScratchTab(ctx context.Context, target string) ([]byte, error) {
	// The create runs detached from the caller's context: the frame is already on
	// the wire, so a client that disconnects (or a deadline that lands) between
	// the write and the reply would leave a tab nobody knows the id of, and
	// closing the socket does not reap it.
	//
	// The tab is opened on a MARKED url rather than a bare about:blank, because
	// the detached context bounds how long the reply is waited for, not whether
	// the tab exists: a browser that answers slower than tabCreateTimeout has
	// already opened one, and the id needed to close it is in the reply nobody is
	// waiting for any more. The marker is what lets that tab be found and reaped
	// without touching a blank tab the user opened themselves.
	marker := fmt.Sprintf("about:blank#cuttle-grab-%d", c.nextID+1)
	createCtx, cancelCreate := context.WithTimeout(context.WithoutCancel(ctx), tabCreateTimeout)
	created, err := c.call(createCtx, "", "Target.createTarget", map[string]any{cdpURL: marker})
	cancelCreate()
	if err != nil {
		c.reapMarkedTab(ctx, marker)
		return nil, err
	}
	targetID := asString(created[keyTargetID])
	if targetID == "" {
		return nil, fmt.Errorf("%w: the browser opened no tab to read through", errGrabFailed)
	}
	defer func() {
		// The tab is ours; it never outlives the grab, even on failure.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_, _ = c.call(closeCtx, "", "Target.closeTarget", map[string]any{keyTargetID: targetID})
	}()

	sid, err := c.attach(ctx, targetID)
	if err != nil {
		return nil, err
	}
	// The buffer caps are the ONLY ceiling on this path: unlike the same-origin
	// read, the body arrives whole from Network.getResponseBody, so a huge
	// response would otherwise be held in the browser and then again here.
	if _, nerr := c.call(ctx, sid, "Network.enable", map[string]any{
		"maxResourceBufferSize": grabBodyLimit,
		"maxTotalBufferSize":    grabBodyLimit,
	}); nerr != nil {
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
	var declared float64
	awaitErr := c.await(ctx, func(msg map[string]any, _ []byte) bool {
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
				declared = contentLength(resp)
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
	// Checked BEFORE the body is asked for: the buffer caps above mean Chrome has
	// already dropped an oversized body, so getResponseBody would fail with
	// "no data found" and the caller would get a protocol error where a size
	// error belongs.
	if declared > grabBodyLimit {
		return nil, grabTooLarge(int(declared))
	}

	res, err := c.call(ctx, sid, "Network.getResponseBody", map[string]any{"requestId": requestID})
	if err != nil {
		if strings.Contains(err.Error(), "No resource with given identifier") {
			// What a download looks like from here: the response never became a
			// document, so there is no body to ask the tab for.
			return nil, fmt.Errorf("%w - the browser turned that URL into a download rather than a page,"+
				" so it has no readable body; pull it with `cuttle downloads --latest --wait 30s`", err)
		}
		return nil, err
	}
	body := asString(res["body"])
	if asBool(res["base64Encoded"]) {
		decoded, derr := base64.StdEncoding.DecodeString(body)
		if derr != nil {
			return nil, fmt.Errorf("%w: the response body did not decode", errGrabFailed)
		}
		return checkGrabSize(decoded)
	}
	return checkGrabSize([]byte(body))
}

// checkGrabSize applies the same ceiling both grab paths answer to, so the limit
// is a property of the verb rather than of one of its two mechanisms.
func checkGrabSize(body []byte) ([]byte, error) {
	if len(body) > grabBodyLimit {
		return nil, grabTooLarge(len(body))
	}
	return body, nil
}

func grabTooLarge(size int) error {
	return fmt.Errorf("%w: the response is %d bytes, over the %d-byte limit - download it in the browser and pull it with `cuttle downloads --latest --wait 30s` instead",
		errGrabFailed, size, grabBodyLimit)
}

// contentLength reads a response's declared size, header casing being the
// protocol's business rather than ours.
func contentLength(resp map[string]any) float64 {
	headers, _ := resp["headers"].(map[string]any)
	for k, v := range headers {
		if strings.EqualFold(k, "content-length") {
			n, err := strconv.ParseFloat(strings.TrimSpace(asString(v)), 64)
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
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
	res, err := conn.callInWorld(ctx, sid, captureSelectorJS, selector, true)
	if err != nil {
		return nil, err
	}
	if exc, ok := res["exceptionDetails"].(map[string]any); ok {
		return nil, fmt.Errorf("%w: %s", errCaptureFailed, exceptionText(exc))
	}
	result, _ := res[cdpResult].(map[string]any)
	value, _ := result[cdpValue].(map[string]any)
	if value == nil {
		return nil, fmt.Errorf("%w: the page returned nothing", errCaptureFailed)
	}
	if !asBool(value["ok"]) {
		return nil, fmt.Errorf("%w: %s", errCaptureFailed, asString(value["why"]))
	}
	return []byte(asString(value[cdpValue])), nil
}

// ---------------------------------------------------------------------------
// capture --from-clipboard
// ---------------------------------------------------------------------------

// clipboardReadJS reads the system clipboard from the page. There is no
// CDP-native clipboard read - the protocol's only clipboard identifiers are
// permission enums - so this is the mechanism, and it works from an isolated
// world: the clipboard's preconditions check a secure context, document focus
// and permission, never which world is asking.
const clipboardReadJS = `async function(){return await navigator.clipboard.readText();}`

// captureClipboard reads the browser's clipboard through the active page.
//
// Two things have to be arranged first, and both are one call:
//
//   - Permission, via Browser.setPermission and NOT Browser.grantPermissions:
//     grantPermissions DENIES every other permission type as a side effect,
//     which would silently break geolocation and notifications for the whole
//     context. (It is also absent from the pinned protocol; it is deprecated.)
//   - Focus. document.hasFocus() is the first hard gate the clipboard checks, and
//     a container browser nobody is looking at fails it. cuttle's focus emulation
//     satisfies it, but that pin lives on the CDP session that set it, so this
//     session sets its own rather than assuming a driver is attached.
func captureClipboard(ctx context.Context, port int) ([]byte, error) {
	pageID, pageURL, err := activePage(ctx, port)
	if err != nil {
		return nil, err
	}
	conn, err := dialBrowser(ctx, port)
	if err != nil {
		return nil, err
	}
	defer conn.close()

	origin := originOfURL(pageURL)
	if _, perr := conn.call(ctx, "", "Browser.setPermission", map[string]any{
		"origin":     origin,
		"permission": map[string]any{"name": "clipboard-read"},
		"setting":    "granted",
	}); perr != nil {
		return nil, perr
	}
	sid, err := conn.attach(ctx, pageID)
	if err != nil {
		return nil, err
	}
	if _, ferr := conn.call(ctx, sid, "Emulation.setFocusEmulationEnabled", map[string]any{cdpEnabled: true}); ferr != nil {
		return nil, ferr
	}
	res, err := conn.callInWorld(ctx, sid, clipboardReadJS, "", true)
	if err != nil {
		return nil, err
	}
	if exc, ok := res["exceptionDetails"].(map[string]any); ok {
		return nil, fmt.Errorf("%w: the page could not read the clipboard: %s"+
			" (it needs an https page - reading from an http:// or about:blank tab is refused by the browser itself)",
			errCaptureFailed, exceptionText(exc))
	}
	result, _ := res[cdpResult].(map[string]any)
	text := asString(result[cdpValue])
	if text == "" {
		return nil, fmt.Errorf("%w: the clipboard is empty", errCaptureFailed)
	}
	return []byte(text), nil
}

// originOfURL is scheme://host, the scope a permission grant is addressed to.
func originOfURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
