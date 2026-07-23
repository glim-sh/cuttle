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

const (
	interKeyBaseMs   = 85.0 // median gap between keystrokes
	keyDtSigma       = 0.35 // log-normal sigma on inter-key gap (skews right)
	keyHoldBaseMs    = 24.0 // median key DOWN->UP hold
	keyHoldSigma     = 0.30
	keyPauseProb     = 0.03 // chance a gap is a longer "thinking" pause instead
	keyPauseMeanMs   = 520.0
	keyPauseSpreadMs = 320.0

	typoProb            = 0.015 // per printable-letter chance of a corrected typo
	typoNoticeMs        = 180.0 // pause after the wrong key, before backspacing
	typoNoticeSpreadMs  = 90.0
	typoCorrectMs       = 90.0 // pause after backspacing, before the right key
	typoCorrectSpreadMs = 45.0
)

// cdpKeyUp is the CDP key-event type dispatched to release a key.
const cdpKeyUp = "keyUp"

// CDP frame field names, hoisted so each literal appears once (and to satisfy
// goconst) across the humanizer's decode paths and command builders.
const (
	cdpID        = "id"
	cdpMethod    = "method"
	cdpParams    = "params"
	cdpSessionID = "sessionId"
	cdpType      = "type"
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
// driver) so the caller must NOT forward the original; false to forward as-is
// (possibly after pacing it in real time).
func (h *humanizer) handleClientFrame(data []byte) bool {
	if !bytes.Contains(data, []byte("Input.dispatch")) {
		return false
	}
	msg, ok := decodeCDP(data)
	if !ok {
		return false
	}
	params, _ := msg[cdpParams].(map[string]any)
	if params == nil {
		return false
	}
	sid := asString(msg[cdpSessionID])
	switch asString(msg[cdpMethod]) {
	case "Input.dispatchMouseEvent":
		return h.handleMouse(msg, params, sid)
	case "Input.dispatchKeyEvent":
		return h.handleKey(params, sid)
	default:
		return false
	}
}

func (h *humanizer) handleMouse(msg, params map[string]any, sid string) bool {
	x, y := asFloat(params["x"]), asFloat(params["y"])
	buttons, modifiers := asFloat(params["buttons"]), asFloat(params["modifiers"])
	switch asString(params[cdpType]) {
	case "mouseMoved":
		// Replace the instant teleport with a curved, paced trajectory, then
		// answer the driver's command ourselves - the browser never sees the
		// original single move, only our sequence.
		h.emitMove(h.curX, h.curY, x, y, sid, buttons, modifiers)
		h.curX, h.curY = x, y
		id, _ := asInt(msg[cdpID])
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

// handleKey paces a driver key event with human timing and, occasionally,
// injects a QWERTY-adjacent typo the driver never asked for and corrects it. It
// always returns false: the driver's OWN key event is forwarded (keeping its
// exact keycodes/text/isTrusted), just delayed - only the extra typo keystrokes
// are synthesized. keystroke-dynamics detectors histogram the DOWN->UP hold and
// key-to-key gaps; both are log-normal here, not the flat uniform a naive
// humanizer emits.
func (h *humanizer) handleKey(params map[string]any, sid string) bool {
	switch asString(params[cdpType]) {
	case "keyDown", "rawKeyDown":
		if text := asString(params["text"]); isTypoable(text) && h.rng.Float64() < typoProb {
			h.emitTypo(text, sid)
		}
		h.sleep(h.interKeyDelay())
	case cdpKeyUp:
		h.sleep(h.keyHold())
	}
	return false
}

// emitTypo types a wrong (adjacent) key, pauses as if noticing, backspaces it,
// and pauses again before the real key is forwarded. Net text is unchanged - the
// injected char is deleted by the injected Backspace, all self-contained.
func (h *humanizer) emitTypo(text, sid string) {
	wrong := adjacentKey(h.rng, text)
	if wrong == "" {
		return
	}
	h.injectKeyEvent(sid, keyEventParams("keyDown", wrong, wrong, "", 0))
	h.injectKeyEvent(sid, keyEventParams(cdpKeyUp, "", wrong, "", 0))
	if !h.sleep(jitterDur(h.rng, typoNoticeMs, typoNoticeSpreadMs)) {
		return
	}
	for _, typ := range []string{"rawKeyDown", cdpKeyUp} {
		h.injectKeyEvent(sid, keyEventParams(typ, "", "Backspace", "Backspace", 8))
	}
	h.sleep(jitterDur(h.rng, typoCorrectMs, typoCorrectSpreadMs))
}

// keyEventParams builds an Input.dispatchKeyEvent params map. A key with text
// produces a character; code + a virtual-key code are needed for named keys
// (e.g. Backspace) to register as the real key rather than inert text.
func keyEventParams(typ, text, key, code string, vk int) map[string]any {
	p := map[string]any{cdpType: typ}
	if text != "" {
		p["text"] = text
	}
	if key != "" {
		p["key"] = key
	}
	if code != "" {
		p["code"] = code
	}
	if vk != 0 {
		p["windowsVirtualKeyCode"] = vk
		p["nativeVirtualKeyCode"] = vk
	}
	return p
}

func (h *humanizer) injectKeyEvent(sid string, params map[string]any) {
	id := h.allocID()
	if err := h.cdpSend(websocket.MessageText, keyCmd(id, sid, params)); err != nil {
		h.releaseID(id)
	}
}

func (h *humanizer) interKeyDelay() time.Duration {
	if h.rng.Float64() < keyPauseProb {
		return jitterDur(h.rng, keyPauseMeanMs, keyPauseSpreadMs)
	}
	return time.Duration(interKeyBaseMs * logNormal(h.rng, keyDtSigma) * float64(time.Millisecond))
}

func (h *humanizer) keyHold() time.Duration {
	return time.Duration(keyHoldBaseMs * logNormal(h.rng, keyHoldSigma) * float64(time.Millisecond))
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
	id, ok := asInt(msg[cdpID])
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
	params := map[string]any{cdpType: "mouseMoved", "x": x, "y": y}
	if buttons != 0 {
		params["buttons"] = buttons
	}
	if modifiers != 0 {
		params["modifiers"] = modifiers
	}
	cmd := map[string]any{cdpID: id, cdpMethod: "Input.dispatchMouseEvent", cdpParams: params}
	if sid != "" {
		cmd[cdpSessionID] = sid
	}
	b, _ := json.Marshal(cmd)
	return b
}

func okResponse(id int64, sid string) []byte {
	resp := map[string]any{cdpID: id, "result": map[string]any{}}
	if sid != "" {
		resp[cdpSessionID] = sid
	}
	b, _ := json.Marshal(resp)
	return b
}

func keyCmd(id int64, sid string, params map[string]any) []byte {
	cmd := map[string]any{cdpID: id, cdpMethod: "Input.dispatchKeyEvent", cdpParams: params}
	if sid != "" {
		cmd[cdpSessionID] = sid
	}
	b, _ := json.Marshal(cmd)
	return b
}

// qwertyNeighbors maps a lowercase letter to the keys physically adjacent on a
// QWERTY keyboard - the pool a realistic slip lands in.
var qwertyNeighbors = map[rune]string{
	'q': "wa", 'w': "qeas", 'e': "wrsd", 'r': "etdf", 't': "ryfg",
	'y': "tugh", 'u': "yijh", 'i': "uojk", 'o': "ipkl", 'p': "ol",
	'a': "qwsz", 's': "awedxz", 'd': "serfcx", 'f': "drtgvc", 'g': "ftyhbv",
	'h': "gyujnb", 'j': "huiknm", 'k': "jiolm", 'l': "kop",
	'z': "asx", 'x': "zsdc", 'c': "xdfv", 'v': "cfgb", 'b': "vghn",
	'n': "bhjm", 'm': "njk",
}

// isTypoable reports whether text is a single ASCII letter - the only chars we
// risk fumbling (digits/symbols/control keys are left exact).
func isTypoable(text string) bool {
	if len(text) != 1 {
		return false
	}
	c := text[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// adjacentKey returns a QWERTY neighbor of the given single letter, preserving
// case, or "" if it has none.
func adjacentKey(rng *rand.Rand, text string) string {
	r := rune(text[0])
	lower := r
	upper := false
	if r >= 'A' && r <= 'Z' {
		lower = r + ('a' - 'A')
		upper = true
	}
	pool := qwertyNeighbors[lower]
	if pool == "" {
		return ""
	}
	c := rune(pool[rng.IntN(len(pool))])
	if upper {
		c -= 'a' - 'A'
	}
	return string(c)
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
