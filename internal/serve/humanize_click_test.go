package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// clickHarness drives a real proxyCDPWebsocket against a scriptable fake browser
// and records every Input.dispatchMouseEvent the browser receives. Tests script
// the two in-page queries the click path issues: probeFn answers the settle
// gate's probeExpr at press time ({x,y,w,h,tattr,tval}); pollFn answers
// verifyToggle's togglePollExpr after release ({present,attr,val}). A nil
// response value makes query() fail open. call is the 0-indexed occurrence.
type clickHarness struct {
	probeFn func(call int) map[string]any
	pollFn  func(call int) map[string]any

	mu         sync.Mutex
	mouse      []map[string]any
	probeCalls int
	pollCalls  int
}

func (h *clickHarness) probeResp(call int) map[string]any {
	if h.probeFn != nil {
		return h.probeFn(call)
	}
	// Default: a stable, non-toggle element - the gate settles on the first pair.
	return map[string]any{"x": 100.0, "y": 200.0, "w": 50.0, "h": 40.0, "tattr": "", "tval": ""}
}

func (h *clickHarness) pollResp(call int) map[string]any {
	if h.pollFn != nil {
		return h.pollFn(call)
	}
	return map[string]any{"present": false}
}

func (h *clickHarness) mouseOfType(typ string) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, e := range h.mouse {
		if e["type"] == typ {
			out = append(out, e)
		}
	}
	return out
}

func (h *clickHarness) counts() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.probeCalls, h.pollCalls
}

// run wires a fake browser + the real proxy + a client, sends the driver's
// press then release at the fixed target (125,220), and blocks until the browser
// stops receiving frames for quietFor - so async post-release work (the toggle
// poll and any re-click) has completed before the caller inspects the recording.
func (h *clickHarness) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	activity := make(chan struct{}, 512)
	browser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		for {
			_, data, rerr := conn.Read(context.Background())
			if rerr != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			result := map[string]any{}
			switch m["method"] {
			case "Runtime.evaluate":
				params, _ := m["params"].(map[string]any)
				expr, _ := params["expression"].(string)
				var val map[string]any
				if strings.Contains(expr, "tattr") { // probeExpr (settle gate)
					h.mu.Lock()
					call := h.probeCalls
					h.probeCalls++
					h.mu.Unlock()
					val = h.probeResp(call)
				} else { // togglePollExpr (post-click verify)
					h.mu.Lock()
					call := h.pollCalls
					h.pollCalls++
					h.mu.Unlock()
					val = h.pollResp(call)
				}
				if val != nil {
					result = map[string]any{"result": map[string]any{"value": val}}
				}
			case "Input.dispatchMouseEvent":
				if p, _ := m["params"].(map[string]any); p != nil {
					h.mu.Lock()
					h.mouse = append(h.mouse, p)
					h.mu.Unlock()
				}
			}
			ack, _ := json.Marshal(map[string]any{"id": m["id"], "result": result})
			_ = conn.Write(context.Background(), websocket.MessageText, ack)
			select {
			case activity <- struct{}{}:
			default:
			}
		}
	}))
	defer browser.Close()
	target := "ws" + strings.TrimPrefix(browser.URL, "http")

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientWS, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		proxyCDPWebsocket(context.Background(), clientWS, target, "test", "", "", true, "")
	}))
	defer proxy.Close()

	cl, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http"), nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer cl.Close(websocket.StatusNormalClosure, "")
	// Drain the driver-facing acks so the proxy's clientSend never blocks.
	go func() {
		for {
			if _, _, rerr := cl.Read(ctx); rerr != nil {
				return
			}
		}
	}()

	press := `{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":125,"y":220,"button":"left","clickCount":1}}`
	release := `{"id":2,"method":"Input.dispatchMouseEvent","params":{"type":"mouseReleased","x":125,"y":220,"button":"left","clickCount":1}}`
	if werr := cl.Write(ctx, websocket.MessageText, []byte(press)); werr != nil {
		t.Fatalf("client write press: %v", werr)
	}
	if werr := cl.Write(ctx, websocket.MessageText, []byte(release)); werr != nil {
		t.Fatalf("client write release: %v", werr)
	}

	// Wait for quiescence: no browser frame for quietFor, bounded by a deadline.
	// quietFor exceeds every inter-frame gap the click path can produce (the
	// longest is the settle-gate backoff, well under 500ms).
	quietFor := 500 * time.Millisecond
	deadline := time.After(8 * time.Second)
	timer := time.NewTimer(quietFor)
	defer timer.Stop()
	for {
		select {
		case <-activity:
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(quietFor)
		case <-timer.C:
			return
		case <-deadline:
			return
		}
	}
}

// TestHumanizePressesAtTarget: the humanized press must land at the driver's
// target coordinates - no off-centre re-aim, whose extra 12-sample micro-move
// used to dismiss blur-sensitive widgets - and the settle gate must run its
// in-page stability query before the button goes down.
func TestHumanizePressesAtTarget(t *testing.T) {
	h := &clickHarness{}
	h.run(t)

	if probes, _ := h.counts(); probes == 0 {
		t.Fatal("no probeExpr issued - settle gate never ran")
	}
	presses := h.mouseOfType("mousePressed")
	if len(presses) != 1 {
		t.Fatalf("browser received %d presses, want exactly 1 (no spurious re-click)", len(presses))
	}
	if px, _ := presses[0]["x"].(float64); px != 125 {
		t.Fatalf("press x=%v, want the driver's target 125 (no off-centre re-aim)", presses[0]["x"])
	}
	if py, _ := presses[0]["y"].(float64); py != 220 {
		t.Fatalf("press y=%v, want the driver's target 220", presses[0]["y"])
	}
	if presses[0]["clickCount"].(float64) != 1 || presses[0]["button"] != "left" {
		t.Fatalf("press lost button/clickCount: %v", presses[0])
	}
}

// TestHumanizeSettleGateRetriesWhileMoving: while the target's box keeps moving
// the gate re-samples (backoff) instead of pressing immediately, then presses
// once the box comes to rest.
func TestHumanizeSettleGateRetriesWhileMoving(t *testing.T) {
	h := &clickHarness{
		// x drifts for the first samples, then holds at 110 from call 2 on, so the
		// gate sees motion on the first pairs and stability only after retrying.
		probeFn: func(call int) map[string]any {
			shift := min(call, 2)
			return map[string]any{
				"x": 100.0 + float64(shift)*5, "y": 200.0, "w": 50.0, "h": 40.0,
				"tattr": "", "tval": "",
			}
		},
	}
	h.run(t)

	if probes, _ := h.counts(); probes < 3 {
		t.Fatalf("settle gate issued %d probes, want >=3 (it must re-sample a moving target)", probes)
	}
	if n := len(h.mouseOfType("mousePressed")); n != 1 {
		t.Fatalf("browser received %d presses, want 1 (press once the target settles)", n)
	}
}

// TestHumanizeToggleReclickWhenSwallowed: a widget whose aria-expanded never
// flips after the click (the humanized click was swallowed) gets exactly one
// tight deterministic re-click.
func TestHumanizeToggleReclickWhenSwallowed(t *testing.T) {
	h := &clickHarness{
		probeFn: func(int) map[string]any {
			return map[string]any{"x": 100.0, "y": 200.0, "w": 50.0, "h": 40.0, "tattr": "aria-expanded", "tval": "false"}
		},
		pollFn: func(int) map[string]any { // state never changes -> swallowed click
			return map[string]any{"present": true, "attr": "aria-expanded", "val": "false"}
		},
	}
	h.run(t)

	if _, polls := h.counts(); polls == 0 {
		t.Fatal("verifyToggle never polled a toggle element")
	}
	if n := len(h.mouseOfType("mousePressed")); n != 2 {
		t.Fatalf("browser received %d presses, want 2 (driver click + one re-click)", n)
	}
	if n := len(h.mouseOfType("mouseReleased")); n != 2 {
		t.Fatalf("browser received %d releases, want 2 (driver click + one re-click)", n)
	}
}

// TestHumanizeNoReclickWhenToggleFlips: when aria-expanded flips after the click,
// the click registered - no re-click is issued.
func TestHumanizeNoReclickWhenToggleFlips(t *testing.T) {
	h := &clickHarness{
		probeFn: func(int) map[string]any {
			return map[string]any{"x": 100.0, "y": 200.0, "w": 50.0, "h": 40.0, "tattr": "aria-expanded", "tval": "false"}
		},
		pollFn: func(int) map[string]any { // flipped on the first poll
			return map[string]any{"present": true, "attr": "aria-expanded", "val": "true"}
		},
	}
	h.run(t)

	if n := len(h.mouseOfType("mousePressed")); n != 1 {
		t.Fatalf("browser received %d presses, want 1 (a click that flipped the state is not retried)", n)
	}
}

// TestHumanizeNoReclickWhenOverlayOpens: when the click point stops resolving to
// the toggle element (an overlay opened over it), the click took effect - no
// re-click.
func TestHumanizeNoReclickWhenOverlayOpens(t *testing.T) {
	h := &clickHarness{
		probeFn: func(int) map[string]any {
			return map[string]any{"x": 100.0, "y": 200.0, "w": 50.0, "h": 40.0, "tattr": "aria-expanded", "tval": "false"}
		},
		pollFn: func(int) map[string]any { return map[string]any{"present": false} }, // overlay now covers the point
	}
	h.run(t)

	if n := len(h.mouseOfType("mousePressed")); n != 1 {
		t.Fatalf("browser received %d presses, want 1 (overlay over the point means the click landed)", n)
	}
}
