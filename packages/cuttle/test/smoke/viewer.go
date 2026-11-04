package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	viewerPrefix         = "/viewer/"
	viewerSocketPath     = "api/websockets"
	viewerConnectTimeout = 30 * time.Second
	viewerPathHeader     = "X-Smoke-Path"
	// The viewer as the shown container's own browser reaches it: plain HTTP
	// on loopback, the path cuttle up and the ssh and k8s tunnels give a person.
	viewerLoopbackURL = "http://127.0.0.1:6080/"
	// A phone in portrait.
	phoneWidth, phoneHeight = 390, 844
	keymapRestoreWait       = 15 * time.Second
)

var (
	errViewerTimeout = errors.New("viewer did not stream")
	errViewerError   = errors.New("viewer check failed")
)

// viewerChecks loads the shipped Selkies viewer in a real Chrome through two
// TLS reverse proxies, one at the root and one that serves it under a path
// prefix and strips that prefix from every request, websocket included, as a
// path-routing ingress does. Selkies refuses to run outside a secure context
// (it decodes with WebCodecs), and the browser reaches these proxies by a
// non-loopback name, so both are HTTPS. Each must carry the stream's websocket
// inside the page's own path and deliver frames. With CUTTLE_VIEWER_CDP_URL
// set, the browser the viewer shows also loads it over plain loopback HTTP,
// and a person's takeover is then played through the viewer against that
// browser (see viewerTakeoverChecks).
func viewerChecks(ctx context.Context, cuttleURL, viewerURL, viewerCDPURL, browserHost, runID string) []checkResult {
	upstream, err := url.Parse(viewerURL)
	if err != nil {
		return []checkResult{viewerFailure("viewer-proxy", "parse CUTTLE_VIEWER_URL: %v", err)}
	}
	rootProxy, err := startViewerProxy(ctx, upstream, browserHost, "")
	if err != nil {
		return []checkResult{viewerFailure("viewer-proxy", "start root proxy: %v", err)}
	}
	defer rootProxy.Close()
	prefixProxy, err := startViewerProxy(ctx, upstream, browserHost, viewerPrefix)
	if err != nil {
		return []checkResult{viewerFailure("viewer-proxy", "start prefix proxy: %v", err)}
	}
	defer prefixProxy.Close()

	client, err := dialSeed(ctx, cuttleURL, "viewer-"+runID)
	if err != nil {
		return []checkResult{viewerFailure("viewer-browser", "%v", err)}
	}
	defer func() { _ = client.conn.CloseNow() }()

	// Browser-wide, so it is in place before the tab's first request; the seed
	// is this run's own throwaway.
	certCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err = client.send(certCtx, "Security.setIgnoreCertificateErrors", map[string]any{"ignore": true}, ""); err != nil {
		return []checkResult{viewerFailure("viewer-browser", "allow local test certificate: %v", err)}
	}

	results := make([]checkResult, 0, 7)
	results = append(results,
		viewerStreamCheck(ctx, client, rootProxy.pageURL, rootProxy, "viewer-root-websocket"),
		viewerStreamCheck(ctx, client, prefixProxy.pageURL, prefixProxy, "viewer-prefixed-websocket"))
	if viewerCDPURL == "" {
		fmt.Println("  takeover checks skipped: CUTTLE_VIEWER_CDP_URL is unset")
		return results
	}
	shown, err := dialSeed(ctx, viewerCDPURL, "")
	if err != nil {
		return append(results, viewerFailure("viewer-takeover", "attach to the shown browser: %v", err))
	}
	defer func() { _ = shown.conn.CloseNow() }()
	results = append(results, viewerStreamCheck(ctx, shown, viewerLoopbackURL, nil, "viewer-loopback-http"))
	return append(results, viewerTakeoverChecks(ctx, client, rootProxy, shown)...)
}

// viewerProxy is a TLS reverse proxy in front of the viewer that records the
// path of each websocket upgrade the viewer accepted.
type viewerProxy struct {
	*httptest.Server

	// pageURL is the viewer page as the containerized browser reaches it.
	pageURL string
	prefix  string

	mu      sync.Mutex
	sockets []string // request paths of accepted upgrades, before any strip
}

func (p *viewerProxy) socketPaths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.sockets...)
}

func (p *viewerProxy) reset() {
	p.mu.Lock()
	p.sockets = nil
	p.mu.Unlock()
}

// startViewerProxy serves the viewer to browserHost, at the root or under
// prefix, which it strips from every path before forwarding.
func startViewerProxy(ctx context.Context, upstream *url.URL, browserHost, prefix string) (*viewerProxy, error) {
	// The browser under test runs in Docker and must reach this host listener.
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", "0.0.0.0:0") // #nosec G102 -- test proxy for the local container
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	p := &viewerProxy{prefix: prefix}
	// Keeps the browser's Host header, which Selkies' same-origin websocket
	// check compares the Origin against.
	proxy := httputil.NewSingleHostReverseProxy(upstream) // #nosec G704 -- local smoke-test upstream
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode != http.StatusSwitchingProtocols {
			return nil
		}
		p.mu.Lock()
		p.sockets = append(p.sockets, resp.Request.Header.Get(viewerPathHeader))
		p.mu.Unlock()
		return nil
	}
	var handler http.Handler = proxy
	if prefix != "" {
		handler = http.StripPrefix(strings.TrimSuffix(prefix, "/"), proxy)
	}
	p.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.Header.Set(viewerPathHeader, req.URL.Path)
		handler.ServeHTTP(w, req)
	}))
	p.Listener = listener
	p.Config.ReadHeaderTimeout = 5 * time.Second
	p.Config.ErrorLog = log.New(io.Discard, "", 0)
	p.StartTLS()

	base, err := url.Parse(p.URL)
	if err != nil {
		p.Close()
		return nil, fmt.Errorf("parse proxy URL: %w", err)
	}
	base.Host = net.JoinHostPort(browserHost, base.Port())
	p.pageURL = base.String() + "/"
	if prefix != "" {
		p.pageURL = base.String() + prefix
	}
	return p, nil
}

func viewerStreamCheck(ctx context.Context, client *cdpClient, pageURL string, proxy *viewerProxy, name string) checkResult {
	ctx, cancel := context.WithTimeout(ctx, viewerConnectTimeout+30*time.Second)
	defer cancel()

	_, frames, closeTab, err := openViewer(ctx, client, pageURL, proxy)
	defer closeTab()
	if err != nil {
		return viewerFailure(name, "%v", err)
	}
	if proxy == nil {
		return checkResult{name, statusPass, fmt.Sprintf("%s video chunks=%d", pageURL, frames)}
	}
	return checkResult{name, statusPass, fmt.Sprintf("socket=%s video chunks=%d",
		strings.Join(proxy.socketPaths(), ","), frames)}
}

// openViewer opens the viewer page in a new tab and waits until it has
// decoded video - and, behind a proxy, until its stream websocket sat inside
// the page's own path. It returns the tab's session and the video chunks the
// page had decoded; the returned close func is always safe to call.
func openViewer(ctx context.Context, client *cdpClient, pageURL string, proxy *viewerProxy) (string, int, func(), error) {
	// Opened at the page itself, not navigated there: a navigation away from
	// about:blank swaps renderer processes, and Chrome can drop a command sent
	// during that swap without ever answering it.
	targetID, sessionID, err := client.openTab(ctx, pageURL)
	closeTab := func() { client.closeTarget(targetID) }
	if err != nil {
		return "", 0, closeTab, fmt.Errorf("open viewer tab: %w", err)
	}
	var wantSocket string
	if proxy != nil {
		proxy.reset()
		wantSocket = "/" + viewerSocketPath
		if proxy.prefix != "" {
			wantSocket = proxy.prefix + viewerSocketPath
		}
	}
	var page viewerPage
	deadline := time.Now().Add(viewerConnectTimeout)
	for time.Now().Before(deadline) {
		raw, err := client.evaluate(ctx, sessionID, viewerPageJS)
		if err != nil {
			return sessionID, 0, closeTab, fmt.Errorf("read viewer page: %w", err)
		}
		if err = json.Unmarshal([]byte(raw), &page); err != nil {
			return sessionID, 0, closeTab, fmt.Errorf("decode viewer page: %w", err)
		}
		// Selkies writes a fatal error (an insecure context, a codec it cannot
		// decode) into the page instead of streaming.
		if strings.HasPrefix(page.Text, "Error:") {
			return sessionID, 0, closeTab, fmt.Errorf("%w: the page shows %q", errViewerError, strings.SplitN(page.Text, "\n", 2)[0])
		}
		socketOK := proxy == nil
		if proxy != nil {
			sockets := proxy.socketPaths()
			for _, s := range sockets {
				if s != wantSocket {
					return sessionID, 0, closeTab, fmt.Errorf("%w: socket at %q, want %q", errViewerError, s, wantSocket)
				}
			}
			socketOK = len(sockets) > 0
		}
		if socketOK && page.Chunks > 0 {
			return sessionID, page.Chunks, closeTab, nil
		}
		pause(ctx, 200*time.Millisecond)
	}
	return sessionID, 0, closeTab, fmt.Errorf("%w within %s (page %q)",
		errViewerTimeout, viewerConnectTimeout, strings.SplitN(page.Text, "\n", 2)[0])
}

// Selkies 2.2.1 counts the video chunks its page received in a global; a
// Selkies bump must check it still does.
const viewerPageJS = `JSON.stringify({
  text: document.body ? document.body.innerText.slice(0, 200) : "",
  chunks: window.videoChunksReceived || 0,
})`

type viewerPage struct {
	Text   string `json:"text"`
	Chunks int    `json:"chunks"`
}

func viewerFailure(name, format string, args ...any) checkResult {
	return checkResult{name, statusFail, fmt.Sprintf(format, args...)}
}

func pause(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// --- Takeover: a person drives the shown browser through the viewer. ------

// viewerTyped is typed through the viewer, once: plain and shifted ASCII, then
// Cyrillic and accented Latin, none of which the persona keymap has, so Selkies
// binds each to a spare keycode on its first use - the keystroke a viewer that
// races the remote browser's keymap reload drops.
const viewerTyped = "Hi, cuttle! привіт жüéß"

// Installed in a blank tab of the browser the viewer shows: a full-screen
// textarea, so a click anywhere on the stream focuses it, and a recorder of
// every key and pointer event the page receives.
const takeoverPageJS = `(() => {
  document.title = "cuttle viewer smoke";
  document.body.style.margin = "0";
  document.body.innerHTML = '<textarea id="t" style="position:fixed;inset:0;width:100vw;height:100vh;border:0;font-size:40px"></textarea>';
  window.__rec = { keys: [], mouse: [] };
  for (const type of ["keydown", "keyup"]) {
    addEventListener(type, (e) => window.__rec.keys.push({ type, key: e.key, trusted: e.isTrusted }), true);
  }
  addEventListener("mousedown", (e) => window.__rec.mouse.push(e.isTrusted), true);
  document.getElementById("t").focus();
  return "";
})()`

// What a page can read of the screen, the window and the keyboard layout.
const takeoverStateJS = `(async () => {
  const layout = {};
  for (const [code, key] of await navigator.keyboard.getLayoutMap()) layout[code] = key;
  return JSON.stringify({
    screen: [screen.width, screen.height, screen.availWidth, screen.availHeight],
    window: [innerWidth, innerHeight, outerWidth, outerHeight],
    layout,
    value: document.getElementById("t")?.value ?? "",
    keys: window.__rec?.keys ?? [],
    mouse: window.__rec?.mouse ?? [],
  });
})()`

type takeoverKey struct {
	Type    string `json:"type"`
	Key     string `json:"key"`
	Trusted bool   `json:"trusted"`
}

type takeoverState struct {
	Screen []int             `json:"screen"`
	Window []int             `json:"window"`
	Layout map[string]string `json:"layout"`
	Value  string            `json:"value"`
	Keys   []takeoverKey     `json:"keys"`
	Mouse  []bool            `json:"mouse"`
}

// viewerTakeoverChecks plays a person's takeover from a phone-sized viewer:
// click into the shown page, type, paste, leave. The shown page must receive
// exactly that, as trusted events with no lock-key taps, while its screen and
// window stay the persona's, and once the viewer is gone its keyboard layout
// must read as it did before.
func viewerTakeoverChecks(ctx context.Context, viewer *cdpClient, proxy *viewerProxy, shown *cdpClient) []checkResult {
	const name = "viewer-takeover"
	ctx, cancel := context.WithTimeout(ctx, viewerConnectTimeout+2*time.Minute)
	defer cancel()

	// getLayoutMap() needs a secure context, which about:blank is not; the
	// shown container's own daemon is a loopback origin that serves the
	// browser a cheap page with no side effect.
	pageID, page, err := shown.openTab(ctx, "http://127.0.0.1:9222/healthz")
	defer shown.closeTarget(pageID)
	if err != nil {
		return []checkResult{viewerFailure(name, "open the shown page: %v", err)}
	}
	// The tab is attached before its page commits, and a recorder installed
	// into the blank document before it would be wiped by the commit.
	if err = waitForEval(ctx, shown, page, `String(location.pathname === "/healthz" && document.readyState === "complete")`); err != nil {
		return []checkResult{viewerFailure(name, "load the shown page: %v", err)}
	}
	if _, err = shown.evaluate(ctx, page, takeoverPageJS); err != nil {
		return []checkResult{viewerFailure(name, "install the recorder: %v", err)}
	}
	if _, err = shown.send(ctx, "Page.bringToFront", nil, page); err != nil {
		return []checkResult{viewerFailure(name, "show the page: %v", err)}
	}
	before, err := readTakeoverState(ctx, shown, page)
	if err != nil {
		return []checkResult{viewerFailure(name, "%v", err)}
	}

	during, err := driveTakeover(ctx, viewer, proxy, shown, page)
	if err != nil {
		return []checkResult{viewerFailure(name, "%v", err)}
	}
	return []checkResult{
		takeoverScreenResult(before, during),
		takeoverInputResult(during),
		takeoverLockKeysResult(during),
		takeoverKeymapResult(ctx, shown, page, before, during),
	}
}

func readTakeoverState(ctx context.Context, client *cdpClient, sessionID string) (takeoverState, error) {
	raw, err := client.evaluate(ctx, sessionID, takeoverStateJS)
	if err != nil {
		return takeoverState{}, fmt.Errorf("read the shown page: %w", err)
	}
	var state takeoverState
	if err = json.Unmarshal([]byte(raw), &state); err != nil {
		return takeoverState{}, fmt.Errorf("decode the shown page's state: %w", err)
	}
	return state, nil
}

// driveTakeover opens the viewer at phone size, clicks into the shown page,
// types viewerTyped, pastes a line from the local clipboard with Ctrl+V and
// reads the shown page back while the viewer is still connected. The viewer
// tab is closed on return, which ends the takeover.
func driveTakeover(ctx context.Context, viewer *cdpClient, proxy *viewerProxy, shown *cdpClient, page string) (takeoverState, error) {
	origin := strings.TrimSuffix(proxy.pageURL, "/")
	grant := map[string]any{"origin": origin, "permissions": []string{"clipboardReadWrite", "clipboardSanitizedWrite"}}
	if _, err := viewer.send(ctx, "Browser.grantPermissions", grant, ""); err != nil {
		return takeoverState{}, fmt.Errorf("grant the viewer clipboard access: %w", err)
	}
	session, _, closeTab, err := openViewer(ctx, viewer, proxy.pageURL, proxy)
	defer closeTab()
	if err != nil {
		return takeoverState{}, err
	}
	// Selkies must letterbox a phone-sized page, never resize the display.
	phone := map[string]any{"width": phoneWidth, "height": phoneHeight, "deviceScaleFactor": 3, "mobile": true}
	if _, err = viewer.send(ctx, "Emulation.setDeviceMetricsOverride", phone, session); err != nil {
		return takeoverState{}, fmt.Errorf("size the viewer like a phone: %w", err)
	}
	if _, err = viewer.send(ctx, "Page.bringToFront", nil, session); err != nil {
		return takeoverState{}, fmt.Errorf("bring the viewer to front: %w", err)
	}
	pause(ctx, 2*time.Second) // the client lays its canvas out again for the new size

	// The stream is letterboxed in the middle of the page.
	center, err := viewer.evaluate(ctx, session, `JSON.stringify([innerWidth / 2, innerHeight / 2])`)
	if err != nil {
		return takeoverState{}, fmt.Errorf("locate the stream: %w", err)
	}
	var xy [2]float64
	if err = json.Unmarshal([]byte(center), &xy); err != nil {
		return takeoverState{}, fmt.Errorf("decode the stream's position: %w", err)
	}
	if err = viewerClick(ctx, viewer, session, xy[0], xy[1]); err != nil {
		return takeoverState{}, err
	}
	pause(ctx, 500*time.Millisecond)
	for _, r := range viewerTyped {
		if err = viewerType(ctx, viewer, session, r); err != nil {
			return takeoverState{}, err
		}
	}

	pasted := fmt.Sprintf(" pasted-%d", time.Now().UnixMilli()%1e5)
	if _, err = viewer.evaluate(ctx, session, fmt.Sprintf(`navigator.clipboard.writeText(%q).then(() => "")`, pasted)); err != nil {
		return takeoverState{}, fmt.Errorf("fill the local clipboard: %w", err)
	}
	ctrl := viewerKey{key: "Control", code: "ControlLeft", vk: 17}
	if err = viewerChord(ctx, viewer, session, ctrl, cdpModCtrl, viewerKey{key: "v", code: "KeyV", vk: 86}); err != nil {
		return takeoverState{}, err
	}

	want := viewerTyped + pasted
	deadline := time.Now().Add(10 * time.Second)
	for {
		state, err := readTakeoverState(ctx, shown, page)
		if err != nil {
			return takeoverState{}, err
		}
		if state.Value == want {
			return state, nil
		}
		if time.Now().After(deadline) {
			return state, fmt.Errorf("%w: the shown page holds %q, want %q", errViewerError, state.Value, want)
		}
		pause(ctx, 200*time.Millisecond)
	}
}

// CDP Input modifier bits.
const (
	cdpModCtrl  = 2
	cdpModShift = 8
)

type viewerKey struct {
	key, code, text string
	vk              int
	shift           bool
}

// asciiKeys maps the punctuation in viewerTyped to the US key that types it.
var asciiKeys = map[rune]viewerKey{
	' ': {key: " ", code: "Space", vk: 32},
	',': {key: ",", code: "Comma", vk: 188},
	'!': {key: "!", code: "Digit1", vk: 49, shift: true},
}

func keyFor(r rune) viewerKey {
	s := string(r)
	switch {
	case r >= 'a' && r <= 'z':
		return viewerKey{key: s, code: "Key" + strings.ToUpper(s), text: s, vk: int(r - 'a' + 'A')}
	case r >= 'A' && r <= 'Z':
		return viewerKey{key: s, code: "Key" + s, text: s, vk: int(r), shift: true}
	}
	if k, ok := asciiKeys[r]; ok {
		k.text = k.key
		return k
	}
	// A character no US key types, on the key a Ukrainian, German or French
	// layout types it with.
	return viewerKey{key: s, code: layoutCodes[r], text: s}
}

var layoutCodes = map[rune]string{
	'п': "KeyG", 'р': "KeyH", 'и': "KeyB", 'в': "KeyD", 'і': "KeyS", 'т': "KeyN", 'ж': "Semicolon",
	'ü': "BracketLeft", 'é': "Digit2", 'ß': "Minus",
}

// viewerType presses the key for r in the viewer, holding Shift around it
// when a person would.
func viewerType(ctx context.Context, client *cdpClient, session string, r rune) error {
	k := keyFor(r)
	if k.shift {
		shift := viewerKey{key: "Shift", code: "ShiftLeft", vk: 16}
		return viewerChord(ctx, client, session, shift, cdpModShift, k)
	}
	return viewerPress(ctx, client, session, k, 0)
}

// viewerChord holds the modifier key mod around k.
func viewerChord(ctx context.Context, client *cdpClient, session string, mod viewerKey, modifiers int, k viewerKey) error {
	if err := dispatchKey(ctx, client, session, "rawKeyDown", mod, modifiers); err != nil {
		return err
	}
	if modifiers&cdpModCtrl != 0 {
		k.text = "" // a Ctrl chord types nothing
	}
	if err := viewerPress(ctx, client, session, k, modifiers); err != nil {
		return err
	}
	return dispatchKey(ctx, client, session, "keyUp", mod, 0)
}

func viewerPress(ctx context.Context, client *cdpClient, session string, k viewerKey, modifiers int) error {
	downType := "rawKeyDown"
	if k.text != "" {
		downType = "keyDown"
	}
	if err := dispatchKey(ctx, client, session, downType, k, modifiers); err != nil {
		return err
	}
	k.text = ""
	if err := dispatchKey(ctx, client, session, "keyUp", k, modifiers); err != nil {
		return err
	}
	pause(ctx, 60*time.Millisecond) // a fast typist, and room for a spare-keycode rebind
	return nil
}

func dispatchKey(ctx context.Context, client *cdpClient, session, typ string, k viewerKey, modifiers int) error {
	params := map[string]any{
		"type": typ, "key": k.key, "code": k.code, "windowsVirtualKeyCode": k.vk, "modifiers": modifiers,
	}
	if k.text != "" {
		params["text"], params["unmodifiedText"] = k.text, k.text
	}
	if _, err := client.send(ctx, "Input.dispatchKeyEvent", params, session); err != nil {
		return fmt.Errorf("dispatch %s %q: %w", typ, k.key, err)
	}
	return nil
}

func viewerClick(ctx context.Context, client *cdpClient, session string, x, y float64) error {
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		params := map[string]any{"type": typ, "x": x, "y": y}
		if typ != "mouseMoved" {
			params["button"], params["clickCount"] = "left", 1
		}
		if _, err := client.send(ctx, "Input.dispatchMouseEvent", params, session); err != nil {
			return fmt.Errorf("dispatch %s: %w", typ, err)
		}
	}
	return nil
}

// The shown page's screen and window are the persona's, whatever size the
// viewer is: a phone-sized viewer must not resize them.
func takeoverScreenResult(before, during takeoverState) checkResult {
	const name = "viewer-screen"
	detail := fmt.Sprintf("screen=%v window=%v with a %dx%d viewer", during.Screen, during.Window, phoneWidth, phoneHeight)
	if !slices.Equal(before.Screen, during.Screen) || !slices.Equal(before.Window, during.Window) {
		return viewerFailure(name, "before the viewer screen=%v window=%v, during it %s", before.Screen, before.Window, detail)
	}
	return checkResult{name, statusPass, detail}
}

// Typed and pasted text landed (driveTakeover waited for it), through trusted
// events only.
func takeoverInputResult(during takeoverState) checkResult {
	const name = "viewer-input"
	untrusted := 0
	for _, k := range during.Keys {
		if !k.Trusted {
			untrusted++
		}
	}
	for _, trusted := range during.Mouse {
		if !trusted {
			untrusted++
		}
	}
	detail := fmt.Sprintf("value=%q key events=%d mousedowns=%d untrusted=%d",
		during.Value, len(during.Keys), len(during.Mouse), untrusted)
	if untrusted > 0 || len(during.Mouse) == 0 {
		return viewerFailure(name, "%s", detail)
	}
	return checkResult{name, statusPass, detail}
}

// No CapsLock or NumLock event may reach the page while a person types: a
// lock tap around keys is a behavioural fingerprint any page can read.
func takeoverLockKeysResult(during takeoverState) checkResult {
	const name = "viewer-lock-keys"
	var locks []string
	for _, k := range during.Keys {
		if k.Key == "CapsLock" || k.Key == "NumLock" {
			locks = append(locks, k.Type+":"+k.Key)
		}
	}
	if len(locks) > 0 {
		return viewerFailure(name, "the shown page received %v", locks)
	}
	return checkResult{name, statusPass, fmt.Sprintf("no lock-key events among %d key events", len(during.Keys))}
}

// Selkies binds spare keycodes for the letters the persona keymap lacks, which
// getLayoutMap() reports; once the viewer leaves, the entrypoint's disconnect
// hook must have put the persona keymap back.
func takeoverKeymapResult(ctx context.Context, shown *cdpClient, page string, before, during takeoverState) checkResult {
	const name = "viewer-keymap-restore"
	changed := layoutDiff(before.Layout, during.Layout)
	if changed == 0 {
		// Nothing to restore means the disconnect hook went unexercised.
		return viewerFailure(name, "the layout never changed while typing characters the persona lacks")
	}
	deadline := time.Now().Add(keymapRestoreWait)
	for {
		after, err := readTakeoverState(ctx, shown, page)
		if err != nil {
			return viewerFailure(name, "%v", err)
		}
		if maps.Equal(after.Layout, before.Layout) {
			return checkResult{name, statusPass, fmt.Sprintf("layout entries before=%d after=%d; %d differed during the takeover",
				len(before.Layout), len(after.Layout), changed)}
		}
		if time.Now().After(deadline) {
			return viewerFailure(name, "%d layout entries still differ %s after the viewer left",
				layoutDiff(before.Layout, after.Layout), keymapRestoreWait)
		}
		pause(ctx, 250*time.Millisecond)
	}
}

func layoutDiff(a, b map[string]string) int {
	n := 0
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			n++
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			n++
		}
	}
	return n
}

// waitForEval polls a string expression until it yields "true".
func waitForEval(ctx context.Context, client *cdpClient, session, expression string) error {
	deadline := time.Now().Add(viewerConnectTimeout)
	for {
		got, err := client.evaluate(ctx, session, expression)
		if err == nil && got == "true" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %s still not true after %s (last error: %w)", errViewerTimeout, expression, viewerConnectTimeout, err)
		}
		pause(ctx, 100*time.Millisecond)
	}
}
