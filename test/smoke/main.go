// Command smoke is the cuttle smoke harness - neutral and self-contained.
//
// It drives a running cuttle over CDP and introspects each seed's browser
// directly - no third-party sites, no network targets, no local server. It
// checks:
//
//  1. per-seed fingerprint isolation - each fingerprint seed gets its own
//     coherent identity, so an in-page canvas readback differs across seeds.
//  2. stealth coherence - navigator.webdriver is falsy, the UA/platform agree
//     (a Windows UA must not pair with a non-Windows platform), and the WebGL
//     UNMASKED_RENDERER reads as a real GPU via ANGLE (not SwiftShader/llvmpipe/
//     Mesa software rendering, the classic automation tell the fork masks).
//  3. connection stability under cold-cycle load - fresh seeds are launched in a
//     loop; every cycle must connect and probe without error.
//
// Run:  go run ./test/smoke   (from the repo root)
// Green = distinct per-seed canvas, coherent stealth signals, no failures.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	defaultCuttleURL  = "http://127.0.0.1:9222"
	defaultColdCycles = 3
	connectTimeout    = 90 * time.Second
)

const (
	statusPass          = "pass"
	statusFail          = "fail"
	nameCanvasIsolation = "canvas-isolation"
)

// One self-contained expression: build a canvas (farbling is fingerprint-seeded,
// so it differs per seed) and read the stealth signals, returned as JSON.
const probeJS = `
(() => {
  let canvas = "missing";
  try {
    const c = document.createElement("canvas");
    c.width = 200; c.height = 40;
    const ctx = c.getContext("2d");
    ctx.textBaseline = "top";
    ctx.font = "16px Arial";
    ctx.fillStyle = "#f60"; ctx.fillRect(0, 0, 200, 40);
    ctx.fillStyle = "#069"; ctx.fillText("cuttle-smoke", 2, 2);
    canvas = c.toDataURL();
  } catch (e) { canvas = "canvas-error:" + e.message; }
  let webglVendor = "missing", webglRenderer = "missing";
  try {
    const gc = document.createElement("canvas");
    const gl = gc.getContext("webgl") || gc.getContext("experimental-webgl");
    const dbg = gl.getExtension("WEBGL_debug_renderer_info");
    webglVendor = gl.getParameter(dbg.UNMASKED_VENDOR_WEBGL);
    webglRenderer = gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL);
  } catch (e) { webglRenderer = "webgl-error:" + e.message; }
  return JSON.stringify({
    webdriver: navigator.webdriver,
    ua: navigator.userAgent,
    platform: navigator.platform,
    hardwareConcurrency: navigator.hardwareConcurrency,
    canvas,
    webglVendor,
    webglRenderer,
  });
})()
`

// A raw software-renderer string in the WebGL UNMASKED_RENDERER is the classic
// automation tell the fork exists to hide: it spoofs a real desktop GPU (via
// ANGLE) on top of whatever renders underneath, so any of these substrings means
// the spoof is not engaging - a real stealth regression, not container noise.
var softwareGLMarkers = []string{"swiftshader", "llvmpipe", "mesa", "software"}

var (
	errNoWSURL     = errors.New("multiplexer did not return a webSocketDebuggerUrl")
	errCDPCommand  = errors.New("cdp command failed")
	errEmptyResult = errors.New("cdp command returned no result")
)

type checkResult struct {
	name   string
	status string
	detail string
}

type probeInfo struct {
	Webdriver           any    `json:"webdriver"`
	UA                  string `json:"ua"`
	Platform            string `json:"platform"`
	HardwareConcurrency int    `json:"hardwareConcurrency"`
	Canvas              string `json:"canvas"`
	WebglVendor         string `json:"webglVendor"`
	WebglRenderer       string `json:"webglRenderer"`
}

func main() {
	os.Exit(run(context.Background()))
}

func run(ctx context.Context) int {
	cuttleURL := getenv("CUTTLE_URL", defaultCuttleURL)
	coldCycles := getenvInt("COLD_CYCLES", defaultColdCycles)
	runID := strconv.FormatInt(time.Now().Unix(), 16)

	fmt.Println("cuttle smoke harness")
	fmt.Printf("  CUTTLE_URL  = %s\n", cuttleURL)
	fmt.Printf("  cold cycles = %d\n", coldCycles)

	var results []checkResult
	var canvases []string

	fmt.Println("\n== stealth coherence + connection stability (cold cycles) ==")
	for i := 1; i <= coldCycles; i++ {
		seed := fmt.Sprintf("smoke-%s-%d", runID, i)
		res, canvas := coldCycle(ctx, cuttleURL, seed, i)
		results = append(results, res)
		if canvas != "" {
			canvases = append(canvases, canvas)
		}
	}

	fmt.Println("\n== per-seed fingerprint isolation ==")
	results = append(results, canvasIsolation(canvases))

	passed := 0
	for _, r := range results {
		if r.status == statusPass {
			passed++
		}
		fmt.Printf("  [%s] %s - %s\n", strings.ToUpper(r.status), r.name, r.detail)
	}

	fmt.Println("\n================ SUMMARY ================")
	fmt.Printf("  cases  %d/%d pass\n", passed, len(results))
	fmt.Println("========================================")

	if passed == len(results) {
		fmt.Println("\nGREEN: per-seed isolation, coherent stealth, no failures.")
		return 0
	}
	fmt.Println("\nNOT GREEN: see failures above.")
	return 1
}

// coldCycle launches a fresh seed, probes it, and grades the stealth signals. A
// connection or probe failure IS the stability signal, so it is caught and
// recorded as a fail rather than aborting the run. The seed's canvas is returned
// (empty on failure) for the cross-seed isolation check.
func coldCycle(ctx context.Context, cuttleURL, seed string, cycle int) (checkResult, string) {
	name := fmt.Sprintf("cold-cycle-%d", cycle)

	cycleCtx, cancel := context.WithTimeout(ctx, connectTimeout+30*time.Second)
	defer cancel()

	info, err := probeSeed(cycleCtx, cuttleURL, seed)
	if err != nil {
		return checkResult{name, statusFail, fmt.Sprintf("probe failed: %v", err)}, ""
	}

	var problems []string
	if isTruthy(info.Webdriver) {
		problems = append(problems, fmt.Sprintf("webdriver=%v", info.Webdriver))
	}
	uaWindows := strings.Contains(strings.ToLower(info.UA), "windows")
	platformWin := strings.HasPrefix(strings.ToLower(info.Platform), "win")
	if uaWindows != platformWin {
		problems = append(problems, fmt.Sprintf(
			"incoherent ua/platform (ua=%q platform=%q)", truncate(info.UA, 40), info.Platform,
		))
	}
	if !strings.HasPrefix(info.Canvas, "data:image") {
		problems = append(problems, "canvas="+truncate(info.Canvas, 24))
	}

	// The load-bearing one: the WebGL renderer must read as a real GPU via ANGLE,
	// not a software renderer.
	renderer := info.WebglRenderer
	lower := strings.ToLower(renderer)
	switch {
	case renderer == "" || renderer == "missing":
		problems = append(problems, "webgl-renderer-missing")
	case containsAny(lower, softwareGLMarkers):
		problems = append(problems, fmt.Sprintf("software webgl renderer=%q", truncate(renderer, 60)))
	case !strings.Contains(lower, "angle"):
		problems = append(problems, fmt.Sprintf("webgl renderer not via ANGLE=%q", truncate(renderer, 60)))
	}

	if len(problems) > 0 {
		return checkResult{name, statusFail, strings.Join(problems, "; ")}, info.Canvas
	}
	detail := fmt.Sprintf("webdriver=%v platform=%s canvas=ok webgl=%q seed=%s",
		info.Webdriver, info.Platform, truncate(renderer, 48), seed)
	return checkResult{name, statusPass, detail}, info.Canvas
}

func canvasIsolation(canvases []string) checkResult {
	values := make([]string, 0, len(canvases))
	distinct := map[string]struct{}{}
	for _, v := range canvases {
		if strings.HasPrefix(v, "data:image") {
			values = append(values, v)
			distinct[v] = struct{}{}
		}
	}
	if len(values) < 2 {
		return checkResult{nameCanvasIsolation, statusFail, fmt.Sprintf("need >=2 canvas readbacks, got %d", len(values))}
	}
	if len(distinct) < 2 {
		return checkResult{nameCanvasIsolation, statusFail, fmt.Sprintf(
			"all %d seeds produced an identical canvas (no per-seed farbling)", len(values),
		)}
	}
	return checkResult{nameCanvasIsolation, statusPass, fmt.Sprintf(
		"%d distinct canvas fingerprints across %d seeds", len(distinct), len(values),
	)}
}

// probeSeed resolves the seed's browser WebSocket (which launches the seed),
// opens a raw CDP connection, creates a scratch tab, evaluates the probe, and
// returns the parsed signals.
func probeSeed(ctx context.Context, cuttleURL, seed string) (*probeInfo, error) {
	wsURL, err := browserWSForSeed(ctx, cuttleURL, seed)
	if err != nil {
		return nil, err
	}

	dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dialing CDP websocket: %w", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(-1)

	client := &cdpClient{conn: conn}

	created, err := client.send(ctx, "Target.createTarget", map[string]any{"url": "about:blank"}, "")
	if err != nil {
		return nil, err
	}
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err = json.Unmarshal(created, &target); err != nil {
		return nil, fmt.Errorf("decoding createTarget: %w", err)
	}

	attached, err := client.send(ctx, "Target.attachToTarget",
		map[string]any{"targetId": target.TargetID, "flatten": true}, "")
	if err != nil {
		return nil, err
	}
	var session struct {
		SessionID string `json:"sessionId"`
	}
	if err = json.Unmarshal(attached, &session); err != nil {
		return nil, fmt.Errorf("decoding attachToTarget: %w", err)
	}

	evaluated, err := client.send(ctx, "Runtime.evaluate", map[string]any{
		"expression": probeJS, "returnByValue": true, "awaitPromise": true,
	}, session.SessionID)
	if err != nil {
		return nil, err
	}

	if _, err = client.send(ctx, "Target.closeTarget", map[string]any{"targetId": target.TargetID}, ""); err != nil {
		return nil, err
	}

	var eval struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err = json.Unmarshal(evaluated, &eval); err != nil {
		return nil, fmt.Errorf("decoding evaluate result: %w", err)
	}
	var info probeInfo
	if err = json.Unmarshal([]byte(eval.Result.Value), &info); err != nil {
		return nil, fmt.Errorf("decoding probe payload: %w", err)
	}
	return &info, nil
}

// cdpClient is a minimal CDP client: send a command, return its result (matching
// on the request id and skipping unrelated events).
type cdpClient struct {
	conn *websocket.Conn
	id   int
}

func (c *cdpClient) send(ctx context.Context, method string, params map[string]any, sessionID string) (json.RawMessage, error) {
	c.id++
	mid := c.id
	if params == nil {
		params = map[string]any{}
	}
	req := struct {
		ID        int            `json:"id"`
		Method    string         `json:"method"`
		Params    map[string]any `json:"params"`
		SessionID string         `json:"sessionId,omitempty"`
	}{ID: mid, Method: method, Params: params, SessionID: sessionID}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encoding %s: %w", method, err)
	}
	if err := c.conn.Write(ctx, websocket.MessageText, payload); err != nil {
		return nil, fmt.Errorf("sending %s: %w", method, err)
	}

	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading %s response: %w", method, err)
		}
		var resp struct {
			ID     int             `json:"id"`
			Error  json.RawMessage `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("decoding %s frame: %w", method, err)
		}
		if resp.ID != mid {
			continue
		}
		if len(resp.Error) > 0 {
			return nil, fmt.Errorf("%w: %s: %s", errCDPCommand, method, string(resp.Error))
		}
		if len(resp.Result) == 0 {
			return nil, fmt.Errorf("%w: %s", errEmptyResult, method)
		}
		return resp.Result, nil
	}
}

// browserWSForSeed asks cuttle for the seed's browser CDP WebSocket, which also
// launches the seed. The multiplexer rewrites webSocketDebuggerUrl to its own
// host, so the returned URL is correct behind a port-forward.
func browserWSForSeed(ctx context.Context, cuttleURL, seed string) (string, error) {
	base, err := url.Parse(cuttleURL)
	if err != nil {
		return "", fmt.Errorf("parsing CUTTLE_URL %q: %w", cuttleURL, err)
	}
	base.Path = "/json/version"
	base.RawQuery = url.Values{"fingerprint": {seed}}.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base.String(), nil)
	if err != nil {
		return "", fmt.Errorf("building /json/version request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("reaching cuttle: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading /json/version: %w", err)
	}
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("decoding /json/version: %w", err)
	}
	if v.WebSocketDebuggerURL == "" {
		return "", errNoWSURL
	}
	return v.WebSocketDebuggerURL, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func isTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	default:
		return true
	}
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
