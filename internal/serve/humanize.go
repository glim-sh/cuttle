package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

const (
	scrollChunkPx  = 32.0 // nominal pixels per emitted wheel notch
	scrollMinSteps = 3
	scrollMaxSteps = 40
	scrollDtMs     = 22.0 // median gap between notches at cruise
	scrollDtSigma  = 0.30
)

const (
	prePressMs     = 55.0 // median dwell after the cursor settles, before pressing
	prePressSigma  = 0.40
	clickHoldMs    = 80.0 // median button DOWN->UP hold of a click
	clickHoldSigma = 0.35

	queryTimeout = 2 * time.Second // bound on one probe round-trip
	// worldTimeout bounds the two setup calls that build a session's isolated
	// world. They are not the gate itself, and they are sequential
	// (createIsolatedWorld needs getFrameTree's frameId), so at queryTimeout they
	// could add 4s to the first click of a session - against awaitStable's promise
	// that a click is never blocked, only delayed. Both are cheap browser-side
	// lookups: a browser that misses this deadline was going to stall the probe too.
	worldTimeout = 500 * time.Millisecond

	// Settle gate: wait for the target under the click point to stop moving
	// before pressing, so a click never lands on an element still animating into
	// place. gateSampleGap separates the two rect samples; on motion the gate
	// re-checks after a growing backoff, up to gateMaxRetries, then fails open.
	gateSampleGap  = 40 * time.Millisecond
	gateMaxRetries = 3

	// Post-click toggle verify + settle window: poll the pressed element's aria
	// toggle state (every togglePollGap) until it flips, for up to
	// togglePollBudget. verifyToggle returns the instant it flips - so a click that
	// works only waits the real open latency - and only a state that NEVER moves in
	// the whole window is treated as swallowed and re-clicked. The budget is
	// deliberately generous so a slow-but-working open is never re-clicked shut;
	// it doubles as the settle-before-ack hold, and only bites clicks that fail.
	togglePollBudget = 600 * time.Millisecond
	togglePollGap    = 35 * time.Millisecond
	tightHoldMs      = 30.0
	tightHoldSpread  = 10.0
)

// clickTarget is what the settle gate saw under the click point: the aria toggle
// state the post-click verify re-checks, plus enough identity to report what the
// click actually landed on.
type clickTarget struct {
	toggleAttr, toggleVal string
	desc                  string // tag#id.class of the hit element
	modal                 bool   // hit element sits inside a dialog or modal
	shifted               bool   // a different element took the point while settling
}

// gateBackoff returns the delay before settle-gate re-check number attempt
// (0-indexed), mirroring Playwright/cloakbrowser actionability's growing wait.
func gateBackoff(attempt int) time.Duration {
	schedule := []time.Duration{60 * time.Millisecond, 150 * time.Millisecond, 300 * time.Millisecond}
	if attempt >= len(schedule) {
		return schedule[len(schedule)-1]
	}
	return schedule[attempt]
}

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
	cdpResult    = "result"
	cdpText      = "text"
	cdpURL       = "url"
	targetPage   = "page"
)

const (
	methodMouse      = "Input.dispatchMouseEvent"
	methodKey        = "Input.dispatchKeyEvent"
	methodInsertText = "Input.insertText"
	// methodIMEComposition places text into the focused field as an uncommitted
	// IME composition - a whole value in ONE call, live in .value and submitted
	// with the form, with no insertText or dispatchKeyEvent frame ever crossing
	// the wire. See handleComposition.
	methodIMEComposition = "Input.imeSetComposition"
)

// isolatedWorldName labels the private execution context the probes evaluate in,
// so they never touch the page's main world. See query.
const isolatedWorldName = "cuttle_probe"

const (
	shiftModifier   = 8  // CDP modifier bit for Shift (Alt=1, Ctrl=2, Meta=4)
	vkShift         = 16 // windowsVirtualKeyCode for Shift
	keyLocationLeft = 1  // KeyboardEvent.DOM_KEY_LOCATION_LEFT

	// insertTextMaxRunes caps how many runes are typed as real keystrokes. The
	// budget is the driver's action timeout (playwright-cli defaults to 5000ms),
	// not the value's length: measured pacing is ~140ms/rune, so the old 80 ran
	// ~10.6s and anything over ~38 runes timed the driver out mid-word and got
	// retried into the field twice. 20 runes is ~2.8s, leaving room for the
	// log-normal tail. The remainder rides one insertText (see handleInsertText).
	insertTextMaxRunes = 20
)

// insertTextBudget bounds the rewrite in wall-clock terms; it must stay UNDER the
// driver's action timeout or the error it raises arrives after the driver has
// already given up. The humanizer runs on the reader goroutine, so an overlong
// type stalls every other command from that driver, not just the field.
const insertTextBudget = 4500 * time.Millisecond

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

// scrollEvent is one emitted wheel notch: a delta and the delay to wait BEFORE
// dispatching it.
type scrollEvent struct {
	dx, dy float64
	dt     time.Duration
}

// planScroll decomposes a single (deltaX,deltaY) wheel command into a burst of
// smaller notches under a trapezoidal velocity profile (ease in, cruise, ease
// out), paced slower at the ends. The notches sum EXACTLY to the requested delta
// so the final scroll position is unchanged - the last notch absorbs rounding.
func planScroll(rng *rand.Rand, deltaX, deltaY float64) []scrollEvent {
	mag := math.Hypot(deltaX, deltaY)
	if mag < scrollChunkPx {
		return []scrollEvent{{dx: deltaX, dy: deltaY, dt: jitterDur(rng, 12, 6)}}
	}
	steps := clampI(int(math.Round(mag/scrollChunkPx)), scrollMinSteps, scrollMaxSteps)

	weights := make([]float64, steps)
	var sum float64
	for i := range steps {
		p := (float64(i) + 0.5) / float64(steps)
		weights[i] = scrollEnvelope(p) * (0.85 + rng.Float64()*0.3)
		sum += weights[i]
	}

	events := make([]scrollEvent, steps)
	var accX, accY float64
	for i := range steps {
		frac := weights[i] / sum
		dx, dy := deltaX*frac, deltaY*frac
		accX, accY = accX+dx, accY+dy
		// Ends scroll slower (bigger gaps) than the cruise middle.
		p := (float64(i) + 0.5) / float64(steps)
		dtMs := scrollDtMs / scrollEnvelope(p) * logNormal(rng, scrollDtSigma)
		events[i] = scrollEvent{dx: dx, dy: dy, dt: time.Duration(dtMs * float64(time.Millisecond))}
	}
	events[steps-1].dx += deltaX - accX
	events[steps-1].dy += deltaY - accY
	return events
}

// scrollEnvelope is a trapezoid in [0,1]: ramps up over the first fifth, cruises
// at 1, ramps down over the last fifth (min 0.4 so the ends never stall).
func scrollEnvelope(p float64) float64 {
	switch {
	case p < 0.2:
		return 0.4 + 3*p
	case p > 0.8:
		return 0.4 + 3*(1-p)
	default:
		return 1.0
	}
}

// humanizeIDBase is the id floor for humanizer-injected Input commands: far above
// any real client id (drivers count up from 1) so their browser responses are
// recognizable and swallowed instead of leaking to the driver, yet well under
// math.MaxInt32. Chrome parses a CDP message id as a 32-bit int and rejects
// anything larger with "Message must have integer 'id' property" - so this base
// (and the whole per-connection range above it) MUST stay below 2^31. It sits
// below injectedIDBase (proxy-auth's 2e9 range, also under 2^31): the two ranges
// [1e9,2e9) and [2e9,2^31) never overlap, and the humanizer resets nextID per
// connection so its 1e9-wide range is never exhausted.
const humanizeIDBase = 1_000_000_000

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
	typeBudget time.Duration // 0 = insertTextBudget; raised in tests

	// secrets is the pool-wide store; seed keys this connection's bucket in it.
	// The secrets path runs whether or not humanization is enabled, so neither
	// field may be gated on `enabled` (see secretfill.go).
	secrets *secretStore
	seed    string
	// secretBudget overrides secretTypeBudget; tests shorten it so a paced type
	// stays well inside `go test` wall-clock.
	secretBudget time.Duration

	// cursor + last-click state, touched only by the client->browser goroutine.
	curX, curY               float64
	lastClickX, lastClickY   float64 // humanized point a press landed on, reused by its release
	lastPressCX, lastPressCY float64 // the driver's ORIGINAL press coords, to tell a click from a drag
	haveClick                bool

	// toggle state (aria-expanded/-pressed/-checked) of the element under the last
	// press, captured by the settle gate and re-checked after release so a click a
	// widget silently swallowed can be retried. Empty attr = no toggle to verify.
	pressToggleAttr string
	pressToggleVal  string

	// composed holds the text of the composition handleComposition just typed
	// out, so the driver's insertText commit of that same value is answered
	// rather than typed a second time. Same goroutine as the cursor state above.
	composed string

	mu       sync.Mutex
	nextID   int64
	pending  map[int64]struct{}
	waiters  map[int64]chan []byte // ids whose response is awaited (queries) not just swallowed
	worlds   map[string]int64      // session id -> isolated-world context for the probes
	inFlight atomic.Int64          // count of pending injected ids; a cheap steady-state gate
}

func newHumanizer(ctx context.Context, enabled bool, secrets *secretStore, seed string, cdpSend, clientSend func(websocket.MessageType, []byte) error) *humanizer {
	return &humanizer{
		ctx:        ctx,
		enabled:    enabled,
		secrets:    secrets,
		seed:       seed,
		rng:        rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())), //nolint:gosec // motion jitter, not security-sensitive
		cdpSend:    cdpSend,
		clientSend: clientSend,
		typeBudget: insertTextBudget,
		nextID:     humanizeIDBase,
		pending:    map[int64]struct{}{},
		waiters:    map[int64]chan []byte{},
		worlds:     map[string]int64{},
	}
}

// handleClientFrame intercepts a client->browser frame. It returns true when it
// has fully handled the command (emitted a humanized sequence and answered the
// driver) so the caller must NOT forward the original; false to forward as-is
// (possibly after pacing it in real time).
func (h *humanizer) handleClientFrame(data []byte) bool {
	// One scan covering all three handled methods; the switch below re-checks the
	// exact name, so the prefilter needs no precision - only cheapness.
	if !bytes.Contains(data, []byte("Input.")) {
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
	case methodMouse:
		return h.handleMouse(msg, params, sid)
	case methodKey:
		return h.handleKey(params, sid)
	case methodInsertText:
		return h.handleInsertText(msg, params, sid)
	case methodIMEComposition:
		return h.handleComposition(msg, params, sid)
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
	case "mousePressed":
		// Settle gate (fail-open): wait for whatever is under the target point to
		// stop moving before we commit, so a click never lands on an element still
		// animating into place (e.g. an option in a just-opened dropdown). It also
		// captures the element's aria toggle state for the post-release verify.
		tgt := h.awaitStable(sid, x, y)
		h.pressToggleAttr, h.pressToggleVal = tgt.toggleAttr, tgt.toggleVal
		// The gate already hit-tests the point; saying what it found turns a click
		// that silently hits an overlay into one line naming the culprit.
		// Only a shifted target is an anomaly; clicking inside a modal is routine
		// (consent banners, pickers), so it rides the message rather than firing one.
		if tgt.shifted && tgt.desc != "" {
			where := ""
			if tgt.modal {
				where = " (inside a modal/dialog)"
			}
			logWarn("humanize: click at (%.0f,%.0f) landed on %s%s - a different element took the point while settling",
				x, y, tgt.desc, where)
		}
		// Press at the driver's target - NO off-centre re-aim. The old off-centre
		// pick fired a second, 12-sample micro-move between the approach and the
		// press; blur/dismiss-sensitive widgets (Material/CDK selects, menus) read
		// that stray motion as a mouse-leave and never open, or open then close.
		// Pressing where the approach landed keeps the click one coherent gesture.
		// A press with no preceding move still gets an approach so the cursor is
		// really on the target before the button goes down.
		if math.Hypot(x-h.curX, y-h.curY) > 2 {
			h.emitMove(h.curX, h.curY, x, y, sid, buttons, modifiers)
		}
		h.curX, h.curY = x, y
		h.lastPressCX, h.lastPressCY = x, y
		h.lastClickX, h.lastClickY = x, y
		h.haveClick = true
		h.sleep(h.prePressDwell())
		params["x"], params["y"] = x, y
		return h.forwardRewritten(msg)
	case "mouseReleased":
		// Release at the same point as its press (a click), holding the button a
		// human moment first. A release whose coords differ from the press is a
		// drag - forward it verbatim rather than snap it to the press point.
		px, py := x, y
		isClick := h.haveClick && math.Hypot(x-h.lastPressCX, y-h.lastPressCY) < 2
		isLeftClick := isClick && asString(params["button"]) == "left"
		if isClick {
			px, py = h.lastClickX, h.lastClickY
		}
		h.haveClick = false
		h.curX, h.curY = px, py
		h.sleep(h.clickHold())
		params["x"], params["y"] = px, py
		// Settle-before-ack: a left click on an element that exposes a toggle state
		// (aria-expanded/-pressed/-checked) is the race-prone case - the widget
		// opens a beat after the click, so a driver that snapshots the instant its
		// click() returns reads the OLD state, thinks nothing happened, and re-clicks
		// (which toggles it shut). Here we dispatch the release ourselves (injected,
		// swallowed), then WITHHOLD the driver's ack until verifyToggle sees the
		// state settle (or re-issues one tight click if it never does), and only then
		// answer the driver - so its next read reflects the reacted DOM. Every other
		// release forwards immediately under the driver's id, unchanged.
		if isLeftClick && h.pressToggleAttr != "" {
			h.inject(sid, methodMouse, params)
			h.verifyToggle(sid, px, py, modifiers)
			id, _ := asInt(msg[cdpID])
			_ = h.clientSend(websocket.MessageText, okResponse(id, sid))
			return true
		}
		return h.forwardRewritten(msg)
	case "mouseWheel":
		// Replace one big wheel event with a paced burst of smaller notches that
		// sum to the exact requested delta, then answer the driver ourselves.
		h.emitScroll(x, y, asFloat(params["deltaX"]), asFloat(params["deltaY"]), sid, modifiers)
		id, _ := asInt(msg[cdpID])
		_ = h.clientSend(websocket.MessageText, okResponse(id, sid))
		return true
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
		if text := asString(params[cdpText]); isTypoable(text) && h.rng.Float64() < typoProb {
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
//
// The wrong key goes out through the same full key shape as a real one
// (charKeyParams: code, virtual-key code, and a held Shift for a capital). A
// bare {key,text} pair would land a keydown with code "", keyCode 0 and
// shiftKey false on an uppercase letter - the exact anomaly typeChar holds Shift
// to avoid, injected right next to the character it is imitating.
func (h *humanizer) emitTypo(text, sid string) {
	wrong := adjacentKey(h.rng, text)
	if wrong == "" {
		return
	}
	if k, ok := charKeys[[]rune(wrong)[0]]; ok {
		h.typeKey(sid, k)
	} else {
		h.inject(sid, methodKey, keyEventParams("keyDown", wrong, wrong, "", 0))
		h.inject(sid, methodKey, keyEventParams(cdpKeyUp, "", wrong, "", 0))
	}
	if !h.sleep(jitterDur(h.rng, typoNoticeMs, typoNoticeSpreadMs)) {
		return
	}
	for _, typ := range []string{"rawKeyDown", cdpKeyUp} {
		h.inject(sid, methodKey, keyEventParams(typ, "", "Backspace", "Backspace", 8))
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

// inject fires one proxy-owned CDP command under an injected id, so the browser's
// reply is swallowed rather than forwarded to a driver that never sent it.
func (h *humanizer) inject(sid, method string, params map[string]any) {
	id := h.allocID()
	if err := h.cdpSend(websocket.MessageText, dispatchCmd(id, method, sid, params)); err != nil {
		h.releaseID(id)
	}
}

// handleInsertText replaces a driver's Input.insertText with real keystrokes.
// Both drivers' fill() lands here: Playwright's injected fill selects the field's
// text and commits the whole value through insertText, which Chrome routes down
// the IME path - so the field ends up populated by a single input event with ZERO
// keydown/keyup. That absence is exactly what a keystroke-dynamics detector reads
// as "no human typed here", and it is the one input path the humanizer never saw.
//
// Typing it out costs nothing in correctness: the client has ALREADY selected the
// old text, so the first keystroke replaces it just as the commit would have (no
// select-all of our own is needed). Characters with no US-layout keycode - emoji,
// CJK, accents, newlines - keep their runs on insertText, the same carve-out
// Playwright's own keyboard.type makes.
func (h *humanizer) handleInsertText(msg, params map[string]any, sid string) bool {
	// Decided before any keystroke goes out: once we have typed, forwarding the
	// original too would type the value twice.
	id, hasID := asInt(msg[cdpID])
	if !hasID {
		return false // nothing is waiting on a reply; let the original through
	}
	text := asString(params[cdpText])
	if text == "" {
		return false // nothing to type: forward as-is
	}
	// A composition we already typed out (handleComposition) is committed by the
	// driver with an insertText carrying the same value. Typing it again would
	// double the field, so the commit is answered without touching the browser.
	// Consumed either way: only the composition's OWN commit can match.
	if composed := h.composed; composed != "" {
		h.composed = ""
		if composed == text {
			_ = h.clientSend(websocket.MessageText, okResponse(id, sid))
			return true
		}
	}
	if !hasTypeable(text) {
		// All emoji/CJK: re-injecting the identical command under our own id would
		// only cost a round-trip and hide the browser's real answer.
		return false
	}

	// The client has already selected the field's existing text, so the head's
	// first keystroke replaces it and the tail appends at the caret.
	return h.typeValueAndAck(id, sid, text)
}

// handleComposition rewrites a driver's Input.imeSetComposition into real
// keystrokes. It is the humanizer's blind spot: one call places a whole value,
// the value is live in .value and the form submits it, yet no insertText and no
// dispatchKeyEvent ever crosses the wire - so it was never humanized and never
// counted, fill()'s zero-keystroke tell in a new costume. (The composition path
// also ignores the field's own maxlength, which the insertText path respects.)
// Playwright never emits it, so rewriting costs no supported flow today.
//
// A GENUINE composition is left alone: one carrying a replacement range is
// mid-edit, and one with no US-layout-typeable rune is the CJK case the IME path
// exists for. Both forward unchanged and are logged, because a value placed that
// way is still a value the humanizer did not see.
func (h *humanizer) handleComposition(msg, params map[string]any, sid string) bool {
	text := asString(params[cdpText])
	if text == "" {
		return false // clearing or cancelling a composition places no value
	}
	id, hasID := asInt(msg[cdpID])
	if !hasID {
		return false // nothing is waiting on a reply; let the original through
	}
	_, hasStart := params["replacementStart"]
	_, hasEnd := params["replacementEnd"]
	if hasStart || hasEnd || !hasTypeable(text) {
		logWarn("humanize: forwarding %s verbatim (%d runes) - a composition with a replacement range or no typeable rune is a real IME edit, but its text reaches the field unhumanized",
			methodIMEComposition, len([]rune(text)))
		return false
	}
	// The commit that follows carries this same text; remember it so the value is
	// typed once, not twice.
	h.composed = text
	return h.typeValueAndAck(id, sid, text)
}

// typeValueAndAck types text as humanized keystrokes and answers the driver's
// command itself - the original frame must never also reach the browser, or the
// value lands twice. Runes past insertTextMaxRunes ride one insertText so a long
// value cannot blow the driver's action timeout. Always returns true (handled).
func (h *humanizer) typeValueAndAck(id int64, sid, text string) bool {
	runes := []rune(text)
	head, tail := runes, []rune(nil)
	if len(runes) > insertTextMaxRunes {
		head, tail = runes[:insertTextMaxRunes], runes[insertTextMaxRunes:]
	}

	done, abandoned := h.typeHumanized(sid, head, 0, false)
	if abandoned {
		// Never ack a partial type: the driver would advance believing the field
		// holds the whole value.
		h.answerError(id, sid, fmt.Sprintf(
			"cuttle: humanized typing stopped after %d of %d characters (budget %s) - the field holds a partial value; re-read it before continuing",
			done, len(runes), h.typingBudget(),
		))
		return true
	}
	if len(tail) > 0 {
		h.inject(sid, methodInsertText, map[string]any{cdpText: string(tail)})
	}

	_ = h.clientSend(websocket.MessageText, okResponse(id, sid))
	return true
}

// typeHumanized types runes as real keystrokes, batching each run of characters
// with no US-layout keycode onto one insertText (the same carve-out Playwright's
// keyboard.type makes). It returns how many runes landed and whether the budget
// or a torn-down connection cut it short. noTypo suppresses the injected-typo
// behaviour, which a secret must never take (see substitute).
func (h *humanizer) typeHumanized(sid string, runes []rune, budget time.Duration, noTypo bool) (int, bool) {
	var untypeable []rune
	flush := func() {
		if len(untypeable) > 0 {
			h.inject(sid, methodInsertText, map[string]any{cdpText: string(untypeable)})
			untypeable = untypeable[:0]
		}
	}
	if budget <= 0 {
		budget = h.typingBudget()
	}
	deadline := time.Now().Add(budget)
	done := 0
	abandoned := false
	for _, r := range runes {
		if time.Now().After(deadline) {
			abandoned = true
			break
		}
		k, ok := charKeys[r]
		if !ok {
			untypeable = append(untypeable, r)
			done++
			continue
		}
		flush()
		if !h.typeChar(sid, k, noTypo) {
			abandoned = true // connection torn down mid-word
			break
		}
		done++
	}
	flush()
	return done, abandoned
}

// typingBudget is the wall-clock ceiling on one rewritten value; tests raise it.
func (h *humanizer) typingBudget() time.Duration {
	if h.typeBudget <= 0 {
		return insertTextBudget
	}
	return h.typeBudget
}

// hasTypeable reports whether any rune of text has a US-layout keycode.
func hasTypeable(text string) bool {
	for _, r := range text {
		if _, ok := charKeys[r]; ok {
			return true
		}
	}
	return false
}

// typeChar paces one character: the gap before it, an occasional corrected typo,
// then the keystroke itself. Returns false when the connection went away.
func (h *humanizer) typeChar(sid string, k charKey, noTypo bool) bool {
	if !h.sleep(h.interKeyDelay()) {
		return false
	}
	if ch := string(k.char); h.shouldTypo(ch, noTypo) {
		h.emitTypo(ch, sid)
	}
	return h.typeKey(sid, k)
}

// shouldTypo decides whether this character gets fumbled and corrected. A secret
// never does: emitTypo corrects with a blind Backspace, and on a segmented
// auto-advancing OTP field the wrong character advances focus, so the Backspace
// lands in the NEXT box.
func (h *humanizer) shouldTypo(ch string, noTypo bool) bool {
	return !noTypo && isTypoable(ch) && h.rng.Float64() < typoProb
}

// typeKey emits one character's keystroke, holding Shift around it when the
// character needs one. Playwright does NOT press Shift for capitals - it sends a
// bare key with modifiers 0, leaving event.shiftKey false on an uppercase letter,
// an anomaly no real keyboard produces. Pressing it is both more correct and more
// human. The release always goes out, even when the hold was cut short, so a
// teardown mid-keystroke never leaves a key logically down.
func (h *humanizer) typeKey(sid string, k charKey) bool {
	if k.shift {
		h.inject(sid, methodKey, shiftKeyParams("rawKeyDown"))
	}
	h.inject(sid, methodKey, charKeyParams("keyDown", k))
	held := h.sleep(h.keyHold())
	h.inject(sid, methodKey, charKeyParams(cdpKeyUp, k))
	if k.shift {
		h.inject(sid, methodKey, shiftKeyParams(cdpKeyUp))
	}
	return held
}

// charKeyParams builds the Input.dispatchKeyEvent params for one printable
// character - the same shape a driver's own pressSequentially would send, so an
// injected keystroke is indistinguishable from a driver-issued one.
func charKeyParams(typ string, k charKey) map[string]any {
	p := keyEventParams(typ, "", string(k.char), k.code, k.vk)
	if k.shift {
		p["modifiers"] = shiftModifier
	}
	if typ != cdpKeyUp {
		p["text"] = string(k.char)
		p["unmodifiedText"] = string(k.base)
	}
	return p
}

func shiftKeyParams(typ string) map[string]any {
	p := keyEventParams(typ, "", "Shift", "ShiftLeft", vkShift)
	p["location"] = keyLocationLeft
	if typ != cdpKeyUp {
		p["modifiers"] = shiftModifier
	}
	return p
}

// charKey is one produced character and the CDP identity a real keystroke of it
// carries: the physical key (code, virtual-key code), the character that key
// produces unshifted, and whether Shift is held to reach this one.
type charKey struct {
	code       string
	vk         int
	char, base rune
	shift      bool
}

// charKeys indexes the US layout by produced character. Anything absent has no
// keycode to synthesize and stays on the insertText path.
var charKeys = buildCharKeys()

func buildCharKeys() map[rune]charKey {
	m := map[rune]charKey{}
	// shifted is 0 for a key that produces nothing extra with Shift held.
	add := func(code string, vk int, base, shifted rune) {
		m[base] = charKey{code: code, vk: vk, char: base, base: base}
		if shifted != 0 {
			m[shifted] = charKey{code: code, vk: vk, char: shifted, base: base, shift: true}
		}
	}
	for i := range 26 {
		add("Key"+string(rune('A'+i)), 65+i, rune('a'+i), rune('A'+i))
	}
	const digitShifts = ")!@#$%^&*("
	for i := range 10 {
		add("Digit"+string(rune('0'+i)), 48+i, rune('0'+i), rune(digitShifts[i]))
	}
	for _, k := range []struct {
		code          string
		vk            int
		base, shifted rune
	}{
		{"Space", 32, ' ', 0},
		{"Semicolon", 186, ';', ':'},
		{"Equal", 187, '=', '+'},
		{"Comma", 188, ',', '<'},
		{"Minus", 189, '-', '_'},
		{"Period", 190, '.', '>'},
		{"Slash", 191, '/', '?'},
		{"Backquote", 192, '`', '~'},
		{"BracketLeft", 219, '[', '{'},
		{"Backslash", 220, '\\', '|'},
		{"BracketRight", 221, ']', '}'},
		{"Quote", 222, '\'', '"'},
	} {
		add(k.code, k.vk, k.base, k.shifted)
	}
	return m
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

// emitScroll dispatches a paced wheel burst summing to the requested delta.
func (h *humanizer) emitScroll(x, y, deltaX, deltaY float64, sid string, modifiers float64) {
	for _, e := range planScroll(h.rng, deltaX, deltaY) {
		if !h.sleep(e.dt) {
			return
		}
		id := h.allocID()
		if err := h.cdpSend(websocket.MessageText, wheelCmd(id, sid, x, y, e.dx, e.dy, modifiers)); err != nil {
			h.releaseID(id)
			return
		}
	}
}

// awaitStable is the fail-open settle gate. It waits until whatever element is
// under (cx,cy) stops moving before the caller presses, so a click never lands
// on a target still animating into place (e.g. an option in a dropdown that just
// opened). It samples the element's box twice; on motion it re-checks after a
// growing backoff up to gateMaxRetries, then presses anyway. ANY doubt - no
// element, a failed/slow round-trip, the connection closing - returns
// immediately, so a click is never blocked, only delayed until the layout rests.
//
// The gate never inspects WHAT is under the point (the proxy has no target
// selector); it only confirms the point has come to rest. Its final sample also
// carries the element's aria toggle attribute/value, which the post-click verify
// in mouseReleased uses to detect a click the widget silently swallowed.
func (h *humanizer) awaitStable(sid string, cx, cy float64) clickTarget {
	prev, ok := h.query(sid, probeExpr(cx, cy))
	if !ok || prev == nil {
		return clickTarget{} // fail-open: nothing to click or cannot inspect
	}
	first := asString(prev["desc"])
	settle := func(p map[string]any) clickTarget {
		t := targetOf(p)
		t.shifted = first != "" && t.desc != "" && first != t.desc
		return t
	}
	for attempt := 0; ; attempt++ {
		if !h.sleep(gateSampleGap) {
			return settle(prev)
		}
		cur, ok := h.query(sid, probeExpr(cx, cy))
		if !ok || cur == nil {
			return settle(prev)
		}
		if probeRectsMatch(prev, cur) || attempt >= gateMaxRetries {
			return settle(cur) // settled, or out of budget - press anyway
		}
		prev = cur
		if !h.sleep(gateBackoff(attempt)) {
			return settle(prev)
		}
	}
}

// verifyToggle is the fail-safe retry. When the pressed element advertised an
// aria toggle state (aria-expanded/-pressed/-checked) that a real click flips,
// it polls that state for up to togglePollBudget. If the state flips, or the
// point stops resolving to that toggle element (an overlay opened over it), the
// click registered - nothing to do. If the state never moves across the whole
// window, the humanized click was swallowed: re-issue ONE tight, deterministic
// click (no curve, minimal hold) at the same point. Because a click that worked
// changes the state, a working click never reaches the re-issue - only a
// swallowed one does, so this cannot double-toggle a widget that opened.
func (h *humanizer) verifyToggle(sid string, x, y, modifiers float64) {
	if h.pressToggleAttr == "" {
		return // element exposed no toggle state - nothing to verify
	}
	for waited := time.Duration(0); waited < togglePollBudget; waited += togglePollGap {
		if !h.sleep(togglePollGap) {
			return
		}
		res, ok := h.query(sid, togglePollExpr(x, y))
		if !ok || res == nil {
			return // fail-open: cannot confirm, do not risk a stray click
		}
		if present, _ := res["present"].(bool); !present {
			return // point no longer over the toggle element (overlay opened) - it took effect
		}
		if asString(res["attr"]) != h.pressToggleAttr || asString(res["val"]) != h.pressToggleVal {
			return // state flipped - the click registered
		}
	}
	h.emitTightClick(sid, x, y, modifiers)
}

// emitTightClick dispatches a single crisp down/up at (x,y) - no approach curve,
// a short hold - as injected commands the browser->client loop swallows. Used
// only by verifyToggle to recover a click a widget swallowed.
func (h *humanizer) emitTightClick(sid string, x, y, modifiers float64) {
	h.inject(sid, methodMouse, tightClickParams("mousePressed", x, y, 1, modifiers))
	h.sleep(jitterDur(h.rng, tightHoldMs, tightHoldSpread))
	h.inject(sid, methodMouse, tightClickParams("mouseReleased", x, y, 0, modifiers))
}

// tightClickParams builds a left-button press/release with the given held-button
// bitmask (1 while down, 0 on release) and clickCount 1.
func tightClickParams(typ string, x, y, buttons, modifiers float64) map[string]any {
	p := map[string]any{cdpType: typ, "x": x, "y": y, "button": "left", "clickCount": 1, "buttons": buttons}
	if modifiers != 0 {
		p["modifiers"] = modifiers
	}
	return p
}

// toggleOf extracts the aria toggle attribute name and value a probe carried, or
// two empty strings when the element exposed none.
func targetOf(probe map[string]any) clickTarget {
	modal, _ := probe["modal"].(bool)
	return clickTarget{
		toggleAttr: asString(probe["tattr"]),
		toggleVal:  asString(probe["tval"]),
		desc:       asString(probe["desc"]),
		modal:      modal,
	}
}

// probeRectsMatch reports whether two probes put the element's box in the same
// place within a 1px tolerance - i.e. it has stopped moving.
func probeRectsMatch(a, b map[string]any) bool {
	return math.Abs(asFloat(a["x"])-asFloat(b["x"])) <= 1 &&
		math.Abs(asFloat(a["y"])-asFloat(b["y"])) <= 1 &&
		math.Abs(asFloat(a["w"])-asFloat(b["w"])) <= 1 &&
		math.Abs(asFloat(a["h"])-asFloat(b["h"])) <= 1
}

// call sends one CDP command under an injected id and waits for its response,
// returning the raw frame. Unlike the fire-and-swallow injections, it registers a
// waiter so the browser->client loop hands the response back here. Bounded by
// queryTimeout and the connection ctx; a miss returns ok=false so the caller
// falls back.
func (h *humanizer) call(sid, method string, params map[string]any) ([]byte, bool) {
	return h.callWithin(sid, method, params, queryTimeout)
}

// callWithin is call with an explicit deadline, for commands that are setup
// rather than the probe itself.
func (h *humanizer) callWithin(sid, method string, params map[string]any, timeout time.Duration) ([]byte, bool) {
	id := h.allocID()
	ch := make(chan []byte, 1)
	h.mu.Lock()
	h.waiters[id] = ch
	h.mu.Unlock()
	// Drop only the waiter here; do NOT releaseID. On timeout / ctx-cancel the id
	// stays pending so its (still in-flight) response is recognized and swallowed by
	// maybeSwallow instead of leaking to the driver with an id it never sent.
	// A CDP command always replies unless the connection dies, so this reconciles.
	defer func() {
		h.mu.Lock()
		delete(h.waiters, id)
		h.mu.Unlock()
	}()

	if err := h.cdpSend(websocket.MessageText, dispatchCmd(id, method, sid, params)); err != nil {
		h.releaseID(id) // the command never left; no response will ever reconcile it
		return nil, false
	}

	select {
	case <-h.ctx.Done():
		return nil, false
	case <-time.After(timeout):
		return nil, false
	case data := <-ch:
		return data, true
	}
}

// query evaluates expr against the page and returns the JSON object it produced.
// It runs in the session's ISOLATED world, not the page's main world: the settle
// gate and the post-click toggle poll fire up to ~17 evaluates per click, and a
// main-world evaluate is observable by the page - it can trap elementFromPoint or
// getBoundingClientRect and read the probe as automation. An isolated world sees
// the same DOM, so the probe expressions are unchanged. If the world cannot be
// built the probe still runs in the main world rather than dropping the gate.
func (h *humanizer) query(sid, expr string) (map[string]any, bool) {
	val, ok, stale := h.evaluate(sid, expr)
	if !stale {
		return val, ok
	}
	// The isolated world went with the document the page just navigated away from.
	// evaluate has dropped it; rebuild and retry ONCE, so the first click after a
	// navigation still gets its settle gate and toggle capture instead of failing
	// open - navigate-then-click is exactly what those exist for.
	val, ok, _ = h.evaluate(sid, expr)
	return val, ok
}

// evaluate runs one probe. stale reports that the evaluate failed because the
// session's cached isolated world no longer exists (and has now been dropped),
// which is the one failure worth retrying.
func (h *humanizer) evaluate(sid, expr string) (map[string]any, bool, bool) {
	params := map[string]any{"expression": expr, "returnByValue": true}
	ctxID := h.isolatedWorld(sid)
	if ctxID != 0 {
		params["contextId"] = ctxID
	}
	data, sent := h.call(sid, "Runtime.evaluate", params)
	if !sent {
		return nil, false, false
	}

	var resp struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			Result struct {
				Value json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"result"`
	}
	if json.Unmarshal(data, &resp) != nil {
		return nil, false, false
	}
	if len(resp.Error) > 0 {
		// Only a probe that actually carried a contextId can have been rejected for
		// a stale one; without it the error is a different class (detached target,
		// dead session) and dropping the cache would just buy two more round-trips
		// on the next probe.
		if ctxID == 0 {
			return nil, false, false
		}
		h.mu.Lock()
		delete(h.worlds, sid)
		h.mu.Unlock()
		return nil, false, true
	}
	if len(resp.Result.Result.Value) == 0 {
		return nil, false, false
	}
	var val map[string]any
	if json.Unmarshal(resp.Result.Result.Value, &val) != nil {
		return nil, false, false
	}
	return val, true, false
}

// invalidateWorld drops a session's cached isolated world when its document is
// replaced. Without this the cache is only dropped on an evaluate ERROR, and a
// bfcached context stays valid across a navigation - so evaluate keeps
// succeeding against the document the page already left, silently degrading the
// settle gate and the toggle verify to no-ops for the rest of the connection.
func (h *humanizer) invalidateWorld(data []byte) {
	msg, ok := decodeCDP(data)
	if !ok {
		return
	}
	sid := asString(msg[cdpSessionID])
	params, _ := msg[cdpParams].(map[string]any)
	switch asString(msg[cdpMethod]) {
	case "Page.frameNavigated":
		frame, _ := params["frame"].(map[string]any)
		if frame == nil || asString(frame["parentId"]) != "" {
			return // a subframe navigation leaves the main world alone
		}
	case "Runtime.executionContextsCleared":
	case "Runtime.executionContextDestroyed":
		id, idOK := asInt(params["executionContextId"])
		if !idOK {
			return
		}
		h.mu.Lock()
		if cur, cached := h.worlds[sid]; cached && cur == id {
			delete(h.worlds, sid)
		}
		h.mu.Unlock()
		return
	default:
		return
	}
	h.mu.Lock()
	delete(h.worlds, sid)
	h.mu.Unlock()
}

// isolatedWorld returns the execution-context id of this session's private world,
// creating it on first use. The result is cached, INCLUDING a 0 "unavailable":
// a session that cannot host one (no Page domain, a target that is not a page)
// would otherwise pay two failed round-trips on every one of the ~17 probes a
// click fires. The cost of that choice is that such a session keeps probing the
// main world for the rest of the connection, so the downgrade is logged.
func (h *humanizer) isolatedWorld(sid string) int64 {
	h.mu.Lock()
	ctxID, cached := h.worlds[sid]
	h.mu.Unlock()
	if cached {
		return ctxID
	}

	ctxID = h.createWorld(sid)
	if ctxID == 0 {
		logWarn("humanize: no isolated world for session %q; probes fall back to the page's main world", sid)
	}
	h.mu.Lock()
	h.worlds[sid] = ctxID
	h.mu.Unlock()
	return ctxID
}

// createWorld builds the isolated world for a session, returning 0 if any step
// fails (no Page domain, a detached target, a dead connection).
func (h *humanizer) createWorld(sid string) int64 {
	data, ok := h.callWithin(sid, "Page.getFrameTree", map[string]any{}, worldTimeout)
	if !ok {
		return 0
	}
	var tree struct {
		Result struct {
			FrameTree struct {
				Frame struct {
					ID string `json:"id"`
				} `json:"frame"`
			} `json:"frameTree"`
		} `json:"result"`
	}
	if json.Unmarshal(data, &tree) != nil || tree.Result.FrameTree.Frame.ID == "" {
		return 0
	}

	data, ok = h.callWithin(sid, "Page.createIsolatedWorld", map[string]any{
		"frameId":   tree.Result.FrameTree.Frame.ID,
		"worldName": isolatedWorldName,
	}, worldTimeout)
	if !ok {
		return 0
	}
	var world struct {
		Result struct {
			ExecutionContextID int64 `json:"executionContextId"`
		} `json:"result"`
	}
	if json.Unmarshal(data, &world) != nil {
		return 0
	}
	return world.Result.ExecutionContextID
}

// toggleAttrsJS is the ordered list of aria attributes a click flips, shared by
// the probe (captured at press) and the poll (re-checked after release). The
// value is read from the element at the point or its nearest ancestor that
// carries one, so a click on an inner label still finds the host's state.
const toggleAttrsJS = `['aria-expanded','aria-pressed','aria-checked']`

// probeExpr resolves the element at (cx,cy) and returns its box plus the aria
// toggle attribute/value it (or its nearest ancestor) exposes. The box drives the
// settle gate's stability check; the toggle fields seed the post-click verify.
// null when nothing is under the point.
func probeExpr(cx, cy float64) string {
	return fmt.Sprintf(`(function(cx,cy){var e=document.elementFromPoint(cx,cy);if(!e)return null;`+
		`var r=e.getBoundingClientRect();var a='',v='',n=%s,node=e;`+
		`while(node&&node.getAttribute){for(var i=0;i<n.length;i++){if(node.hasAttribute(n[i])){a=n[i];v=node.getAttribute(n[i]);break;}}`+
		`if(a)break;node=node.parentElement;}`+
		`var d=e.tagName.toLowerCase();if(e.id)d+='#'+e.id;`+
		`var k=(typeof e.className==='string'&&e.className.trim())?e.className.trim().split(/\s+/).slice(0,2).join('.'):'';`+
		`if(k)d+='.'+k;`+
		`var m=!!(e.closest&&e.closest('[role=dialog],[role=alertdialog],dialog[open],[aria-modal="true"]'));`+
		`return{x:r.left,y:r.top,w:r.width,h:r.height,tattr:a,tval:v,desc:d,modal:m};})(%g,%g)`, toggleAttrsJS, cx, cy)
}

// togglePollExpr re-reads the aria toggle state at (cx,cy) after a click. present
// is false when the point no longer resolves to a toggle-bearing element (an
// overlay opened over it) - which the verify treats as "the click took effect".
func togglePollExpr(cx, cy float64) string {
	return fmt.Sprintf(`(function(cx,cy){var e=document.elementFromPoint(cx,cy);if(!e)return{present:false};`+
		`var n=%s,node=e;while(node&&node.getAttribute){for(var i=0;i<n.length;i++){if(node.hasAttribute(n[i]))`+
		`return{present:true,attr:n[i],val:node.getAttribute(n[i])};}node=node.parentElement;}`+
		`return{present:false};})(%g,%g)`, toggleAttrsJS, cx, cy)
}

// forwardRewritten marshals a coordinate-rewritten Input command and sends it to
// the browser under the driver's ORIGINAL id, so the browser's real response
// flows back to the driver. Returns true (handled); false on a marshal failure so
// the caller forwards the untouched original.
func (h *humanizer) forwardRewritten(msg map[string]any) bool {
	b, err := json.Marshal(msg)
	if err != nil {
		return false
	}
	_ = h.cdpSend(websocket.MessageText, b)
	return true
}

func (h *humanizer) prePressDwell() time.Duration {
	return time.Duration(prePressMs * logNormal(h.rng, prePressSigma) * float64(time.Millisecond))
}

func (h *humanizer) clickHold() time.Duration {
	return time.Duration(clickHoldMs * logNormal(h.rng, clickHoldSigma) * float64(time.Millisecond))
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
	w := h.waiters[id]
	h.mu.Unlock()
	if injected {
		h.inFlight.Add(-1)
	}
	// A query awaits this response: hand it over (buffered, never blocks) as well
	// as swallowing it, so the driver never sees the injected id.
	if w != nil {
		w <- data
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

// dispatchCmd marshals one CDP command with the given id and session, shared by
// every injected command the proxy sends.
func dispatchCmd(id int64, method, sid string, params map[string]any) []byte {
	cmd := map[string]any{cdpID: id, cdpMethod: method, cdpParams: params}
	if sid != "" {
		cmd[cdpSessionID] = sid
	}
	b, _ := json.Marshal(cmd)
	return b
}

func moveCmd(id int64, sid string, x, y, buttons, modifiers float64) []byte {
	params := map[string]any{cdpType: "mouseMoved", "x": x, "y": y}
	if buttons != 0 {
		params["buttons"] = buttons
	}
	if modifiers != 0 {
		params["modifiers"] = modifiers
	}
	return dispatchCmd(id, methodMouse, sid, params)
}

func wheelCmd(id int64, sid string, x, y, dx, dy, modifiers float64) []byte {
	params := map[string]any{cdpType: "mouseWheel", "x": x, "y": y, "deltaX": dx, "deltaY": dy}
	if modifiers != 0 {
		params["modifiers"] = modifiers
	}
	return dispatchCmd(id, methodMouse, sid, params)
}

func okResponse(id int64, sid string) []byte {
	resp := map[string]any{cdpID: id, cdpResult: map[string]any{}}
	if sid != "" {
		resp[cdpSessionID] = sid
	}
	b, _ := json.Marshal(resp)
	return b
}

// errResponse answers a client command with a CDP error under its own id, so a
// driver sees a clean failure rather than a success it cannot act on.
func errResponse(id int64, sid, message string) []byte {
	resp := map[string]any{
		cdpID:   id,
		"error": map[string]any{"code": -32000, "message": message},
	}
	if sid != "" {
		resp[cdpSessionID] = sid
	}
	b, _ := json.Marshal(resp)
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
