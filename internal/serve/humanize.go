package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Behavioral humanization rewrites a driver's instant CDP Input events into
// human-like sequences before they reach Chrome. It is the dynamic half of
// stealth: a flawless fingerprint still fails behavioral checks (mouse-trajectory
// analysis, keystroke dynamics, isTrusted/cadence) when input teleports. Because
// the rewrite emits real Input.dispatch* commands over the same CDP session, the
// resulting events keep isTrusted=true with no JS stack - the property naive
// in-page humanizers cannot achieve.
//
// The statistics here deliberately beat the common uniform-jitter approach:
// movement time follows Fitts' law (scales with distance AND target size - the
// one pointing law behavioral detectors actually test), per-sample intervals are
// log-normal (right-skewed, like real motion, not flat-topped uniform), Bezier
// control points are randomized (no templated 25/75 arc), and positional noise is
// low-frequency correlated tremor rather than per-sample white noise (whose flat
// spectrum is itself a tell).

const (
	// Fitts' law movement time: MT = fittsA + fittsB*log2(dist/fittsW + 1).
	fittsA = 0.075 // s, intercept (base reaction/settle)
	fittsB = 0.105 // s/bit, slope
	// fittsW is the nominal effective target width. The Input layer carries no
	// element size, so we assume a small target - the conservative (slower) end,
	// which reads as more deliberate rather than flick-fast.
	fittsW = 18.0

	moveDurSigma  = 0.22 // log-normal sigma on total movement time
	moveStepMs    = 12.0 // target ms between emitted samples (real devices ~8-16ms)
	moveMinSteps  = 12
	moveMaxSteps  = 120
	moveDtSigma   = 0.28 // log-normal sigma on per-step dt (skews intervals right)
	ctrlPerpFrac  = 0.09 // gaussian sigma of perpendicular control-point offset / dist
	tremorPx      = 1.4  // peak correlated-tremor amplitude
	overshootProb = 0.14
)

// mouseEvent is one emitted cursor sample: an absolute position and the delay to
// wait BEFORE dispatching it (so the caller paces the sequence in real time).
type mouseEvent struct {
	x, y float64
	dt   time.Duration
}

// planMouseMove builds a humanized cursor trajectory from (fromX,fromY) to
// (toX,toY). The final event lands exactly on the target; intermediate samples
// ride a randomized cubic Bezier with correlated tremor, and an occasional
// overshoot-then-correct. rng makes it deterministic under test and unique per
// connection in production.
func planMouseMove(rng *rand.Rand, fromX, fromY, toX, toY float64) []mouseEvent {
	dx, dy := toX-fromX, toY-fromY
	dist := math.Hypot(dx, dy)
	if dist < 1 {
		return []mouseEvent{{x: toX, y: toY, dt: jitterDur(rng, 8, 4)}}
	}

	id := math.Log2(dist/fittsW + 1)
	mt := clampF((fittsA+fittsB*id)*logNormal(rng, moveDurSigma), 0.04, 2.0)
	steps := clampI(int(math.Round(mt*1000/moveStepMs)), moveMinSteps, moveMaxSteps)

	// Randomized control points: anchors jittered along the path and offset along
	// its normal by a gaussian, so the arc is never the fixed shape a classifier
	// could template.
	nx, ny := -dy/dist, dx/dist
	t1 := 0.2 + rng.Float64()*0.2
	t2 := 0.6 + rng.Float64()*0.2
	o1 := rng.NormFloat64() * ctrlPerpFrac * dist
	o2 := rng.NormFloat64() * ctrlPerpFrac * dist
	p1x, p1y := fromX+dx*t1+nx*o1, fromY+dy*t1+ny*o1
	p2x, p2y := fromX+dx*t2+nx*o2, fromY+dy*t2+ny*o2

	// Correlated tremor: two low-frequency sinusoids with random phase, enveloped
	// to zero at both ends. Smooth, human wander - not per-sample white noise.
	f1, f2 := 1.0+rng.Float64()*2, 2.0+rng.Float64()*3
	ph1, ph2 := rng.Float64()*2*math.Pi, rng.Float64()*2*math.Pi
	a1 := tremorPx * (0.5 + rng.Float64())
	a2 := tremorPx * (0.3 + rng.Float64()*0.5)
	skew := 0.85 + rng.Float64()*0.3

	stepMs := mt * 1000 / float64(steps)
	events := make([]mouseEvent, 0, steps+1)
	for i := 1; i <= steps; i++ {
		p := float64(i) / float64(steps)
		e := easeInOut(p, skew)
		bx, by := cubicBezier(e, fromX, fromY, p1x, p1y, p2x, p2y, toX, toY)
		env := math.Sin(math.Pi * p)
		tx := env * (a1*math.Sin(2*math.Pi*f1*p+ph1) + a2*math.Sin(2*math.Pi*f2*p+ph2))
		ty := env * (a1*math.Cos(2*math.Pi*f1*p+ph1) + a2*math.Cos(2*math.Pi*f2*p+ph2)) * 0.7
		dt := time.Duration(stepMs * logNormal(rng, moveDtSigma) * float64(time.Millisecond))
		events = append(events, mouseEvent{x: bx + tx, y: by + ty, dt: dt})
	}
	// Pin the last sample to the exact target so tremor never leaves the cursor
	// a pixel off where the driver asked to click.
	events[len(events)-1] = mouseEvent{x: toX, y: toY, dt: events[len(events)-1].dt}

	// Occasional overshoot then correction: land a few px past, settle back.
	if rng.Float64() < overshootProb {
		ux, uy := dx/dist, dy/dist
		over := 3 + rng.Float64()*5
		events[len(events)-1] = mouseEvent{x: toX + ux*over, y: toY + uy*over, dt: events[len(events)-1].dt}
		events = append(events, mouseEvent{x: toX, y: toY, dt: jitterDur(rng, 45, 20)})
	}
	return events
}

func cubicBezier(t, x0, y0, x1, y1, x2, y2, x3, y3 float64) (float64, float64) {
	u := 1 - t
	c0, c1, c2, c3 := u*u*u, 3*u*u*t, 3*u*t*t, t*t*t
	return c0*x0 + c1*x1 + c2*x2 + c3*x3, c0*y0 + c1*y1 + c2*y2 + c3*y3
}

// easeInOut is a smoothstep with an asymmetry knob: skew!=1 tilts the balance of
// acceleration vs deceleration so the velocity profile is not perfectly mirrored.
func easeInOut(p, skew float64) float64 {
	pe := math.Pow(p, skew)
	return pe * pe * (3 - 2*pe)
}

// logNormal returns a positive multiplier with median 1 and the given sigma, for
// right-skewed jitter on durations and intervals.
func logNormal(rng *rand.Rand, sigma float64) float64 {
	return math.Exp(rng.NormFloat64() * sigma)
}

// jitterDur returns meanMs +/- spreadMs (uniform), floored at 1ms.
func jitterDur(rng *rand.Rand, meanMs, spreadMs float64) time.Duration {
	ms := meanMs + (rng.Float64()-0.5)*2*spreadMs
	if ms < 1 {
		ms = 1
	}
	return time.Duration(ms * float64(time.Millisecond))
}

func clampF(v, lo, hi float64) float64 {
	return math.Min(math.Max(v, lo), hi)
}

func clampI(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// humanizeIDBase is the id floor for humanizer-injected Input commands. It sits
// above injectedIDBase (proxy-auth's range) so the two never collide, and far
// above any real client id, so their browser responses are recognizable and
// swallowed instead of leaking to the driver.
const humanizeIDBase = 3_000_000_000

// humanizer rewrites a driver's Input.dispatchMouseEvent commands into human
// motion. It lives for one CDP connection. Cursor state is touched only by the
// client->browser goroutine (all Input flows one way); the injected-id set is
// shared with the browser->client loop, which swallows the injected commands'
// responses.
type humanizer struct {
	ctx        context.Context //nolint:containedctx // bounds paced injection to the connection
	enabled    bool
	rng        *rand.Rand
	cdpSend    func(websocket.MessageType, []byte) error
	clientSend func(websocket.MessageType, []byte) error

	curX, curY float64 // last cursor position (client->browser goroutine only)

	mu       sync.Mutex
	nextID   int64
	pending  map[int64]struct{}
	inFlight atomic.Int64 // count of pending injected ids; a cheap steady-state gate
}

func newHumanizer(ctx context.Context, enabled bool, cdpSend, clientSend func(websocket.MessageType, []byte) error) *humanizer {
	return &humanizer{
		ctx:        ctx,
		enabled:    enabled,
		rng:        rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())), //nolint:gosec // motion jitter, not security-sensitive
		cdpSend:    cdpSend,
		clientSend: clientSend,
		nextID:     humanizeIDBase,
		pending:    map[int64]struct{}{},
	}
}

// handleClientFrame intercepts a client->browser frame. It returns true when it
// has fully handled the command (emitted a humanized sequence and answered the
// driver) so the caller must NOT forward the original; false to forward as-is.
func (h *humanizer) handleClientFrame(data []byte) bool {
	if !bytes.Contains(data, []byte("Input.dispatchMouseEvent")) {
		return false
	}
	msg, ok := decodeCDP(data)
	if !ok {
		return false
	}
	if asString(msg["method"]) != "Input.dispatchMouseEvent" {
		return false
	}
	params, _ := msg["params"].(map[string]any)
	if params == nil {
		return false
	}
	x, y := asFloat(params["x"]), asFloat(params["y"])
	sid := asString(msg["sessionId"])
	buttons, modifiers := asFloat(params["buttons"]), asFloat(params["modifiers"])

	switch asString(params["type"]) {
	case "mouseMoved":
		// Replace the instant teleport with a curved, paced trajectory, then
		// answer the driver's command ourselves - the browser never sees the
		// original single move, only our sequence.
		h.emitMove(h.curX, h.curY, x, y, sid, buttons, modifiers)
		h.curX, h.curY = x, y
		id, _ := asInt(msg["id"])
		_ = h.clientSend(websocket.MessageText, okResponse(id, sid))
		return true
	case "mousePressed", "mouseReleased":
		// A press/release whose coordinates jumped from the cursor means the
		// driver skipped a move (it clicks by coordinate). Move there humanly
		// first, then forward the driver's OWN press/release so button/clickCount
		// semantics are preserved exactly.
		if math.Hypot(x-h.curX, y-h.curY) > 2 {
			h.emitMove(h.curX, h.curY, x, y, sid, buttons, modifiers)
		}
		h.curX, h.curY = x, y
		return false
	default:
		return false
	}
}

// emitMove dispatches a paced, humanized cursor trajectory to the browser.
func (h *humanizer) emitMove(fromX, fromY, toX, toY float64, sid string, buttons, modifiers float64) {
	for _, e := range planMouseMove(h.rng, fromX, fromY, toX, toY) {
		if !h.sleep(e.dt) {
			return
		}
		id := h.allocID()
		if err := h.cdpSend(websocket.MessageText, moveCmd(id, sid, e.x, e.y, buttons, modifiers)); err != nil {
			h.releaseID(id)
			return
		}
	}
}

// maybeSwallow reports whether data is a response to one of our injected Input
// commands, consuming it. It skips the JSON decode entirely in steady state (no
// injection in flight), so it adds ~nothing to the thousands of frames a session
// streams.
func (h *humanizer) maybeSwallow(data []byte) bool {
	if h.inFlight.Load() == 0 {
		return false
	}
	msg, ok := decodeCDP(data)
	if !ok {
		return false
	}
	id, ok := asInt(msg["id"])
	if !ok || id < humanizeIDBase {
		return false
	}
	h.mu.Lock()
	_, injected := h.pending[id]
	if injected {
		delete(h.pending, id)
	}
	h.mu.Unlock()
	if injected {
		h.inFlight.Add(-1)
	}
	return injected
}

func (h *humanizer) allocID() int64 {
	h.mu.Lock()
	id := h.nextID
	h.nextID++
	h.pending[id] = struct{}{}
	h.mu.Unlock()
	h.inFlight.Add(1)
	return id
}

func (h *humanizer) releaseID(id int64) {
	h.mu.Lock()
	_, ok := h.pending[id]
	if ok {
		delete(h.pending, id)
	}
	h.mu.Unlock()
	if ok {
		h.inFlight.Add(-1)
	}
}

// sleep waits d, returning false if the connection is torn down first.
func (h *humanizer) sleep(d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-h.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func moveCmd(id int64, sid string, x, y, buttons, modifiers float64) []byte {
	params := map[string]any{"type": "mouseMoved", "x": x, "y": y}
	if buttons != 0 {
		params["buttons"] = buttons
	}
	if modifiers != 0 {
		params["modifiers"] = modifiers
	}
	cmd := map[string]any{"id": id, "method": "Input.dispatchMouseEvent", "params": params}
	if sid != "" {
		cmd["sessionId"] = sid
	}
	b, _ := json.Marshal(cmd)
	return b
}

func okResponse(id int64, sid string) []byte {
	resp := map[string]any{"id": id, "result": map[string]any{}}
	if sid != "" {
		resp["sessionId"] = sid
	}
	b, _ := json.Marshal(resp)
	return b
}

// asFloat reads a numeric CDP field. decodeCDP preserves numbers as json.Number
// (for id fidelity), so a plain float64 assertion would silently yield 0 - which
// would collapse every move to the origin.
func asFloat(v any) float64 {
	if n, ok := v.(json.Number); ok {
		f, _ := n.Float64()
		return f
	}
	f, _ := v.(float64)
	return f
}
