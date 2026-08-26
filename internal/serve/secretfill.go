package serve

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// The CDP half of daemon-owned secrets: sentinel substitution, the mandatory
// pre-flight target check, the credential-field refusal, and the derived
// post-type verification. The store it reads lives in secrets.go.
//
// Two properties decide the shape of everything here:
//
//   - It runs OUTSIDE the --humanize gate. Interception used to be
//     `if h.enabled && h.handleClientFrame(...)`; a secret path behind that gate
//     would type `{{cuttle:GH_PASS}}` literally into a live password field the
//     moment someone passed --humanize=false, which is the exact fail-open this
//     feature exists to kill.
//   - It fails CLOSED. An unknown name, a stale value, an embedded sentinel or a
//     target the probe cannot vouch for is a hard CDP error with nothing typed -
//     never a literal fallback (Playwright's substitution returns the NAME as the
//     value on a miss, with no error and no log).
const (
	// secretFillBudget is the ceiling on ONE substituted fill, end to end: world
	// setup, pre-flight probe, typing, and the post-type probe all draw down the
	// same deadline - the world build included, which is the part that silently
	// escaped it once already. It has to stay under the driver's ~5s action timeout, or the
	// driver gives up mid-fill and retries the credential into the field twice -
	// and per-step timeouts do not add up to that guarantee, they only bound each
	// step in isolation (worst case 2 worldTimeouts + 2 queryTimeouts + the type
	// is ~7.5s, which is how a budget silently stops being one).
	secretFillBudget = 4500 * time.Millisecond
	// secretProbeTimeout bounds each of the two probes inside that budget, and
	// secretWorldTimeout bounds building the isolated world they need. Both are
	// well under their unbudgeted equivalents (queryTimeout, worldTimeout): a page
	// that cannot answer this fast was going to blow the whole fill anyway, and
	// the point is that the SUM stays under the driver's action timeout.
	secretProbeTimeout = 700 * time.Millisecond
	secretWorldTimeout = 600 * time.Millisecond
	// secretTypeBudget caps the typing itself; the remaining-time arithmetic below
	// can only shrink it further.
	secretTypeBudget = 2500 * time.Millisecond
	// secretMaxRunes caps the keystroke head for a secret. insertTextMaxRunes (20)
	// paces at ~2.8s, which overruns secretTypeBudget on its own; the remainder
	// rides one insertText exactly as it does for an ordinary fill.
	secretMaxRunes = 12
)

// inputNeedle is the humanizer's own cheap prefilter, kept for its byte-level
// entry point. The SECRETS path deliberately has none - see decodeClientFrame.
var inputNeedle = []byte("Input.")

// methodSendMessageToTarget is the pre-flat-session transport: one CDP command
// carrying another as a JSON STRING. Nothing downstream inspects that payload,
// so a fill tunneled through it reached Chrome untouched - including a sentinel,
// which then typed as its own literal text.
const methodSendMessageToTarget = "Target.sendMessageToTarget"

type sentinelKind int

const (
	sentinelNone sentinelKind = iota
	sentinelWhole
	sentinelEmbedded
)

// parseSentinel classifies typed text. A sentinel is only a sentinel when it is
// the ENTIRE value: `Bearer {{cuttle:TOKEN}}` matches no name, and letting it
// fall through would type that literal into a live field.
func parseSentinel(text string) (string, sentinelKind) {
	if !strings.Contains(text, sentinelPrefix) {
		return "", sentinelNone
	}
	name := strings.TrimSuffix(strings.TrimPrefix(text, sentinelPrefix), sentinelSuffix)
	if strings.HasPrefix(text, sentinelPrefix) && strings.HasSuffix(text, sentinelSuffix) &&
		len(text) > len(sentinelPrefix)+len(sentinelSuffix) && validSecretName(name) {
		return name, sentinelWhole
	}
	return "", sentinelEmbedded
}

// decodeClientFrame decodes every client command, once, for both client-side
// hooks.
//
// There is deliberately NO byte prefilter here, and that is the whole point: a
// byte test cannot be sound against JSON, because JSON can spell any character
// as \uXXXX. A red-team pass walked straight through three successive needles -
// `"Input.i` missed `Input.\u0069nsertText`, dropping the quote still missed
// `Input\u002einsertText`, and `{{cuttle:` missed `{{cuttle\u003aGH_PASS}}` -
// while Chrome, which parses rather than scans, acted on every one of them. Each
// miss was a silent fail-open: no target check, and a sentinel typed into a live
// field as its own literal text.
//
// The cost is one decodeCDP per client command: measured 1.9us, 1.9KB and 36
// allocations on a representative mouse frame. That is a driver's own command
// rate - a click is about three frames, a twenty-character type about forty, so
// ~76us against a type that takes seconds - not the thousands-per-second
// browser->client stream the other prefilters in this package guard. It is also
// paid once for both client-side hooks now rather than twice. At that price,
// correctness is not a trade.
func (h *humanizer) decodeClientFrame(data []byte) (map[string]any, bool) {
	msg, ok := decodeCDP(data)
	if !ok {
		return nil, false
	}
	return msg, true
}

// handleSecretFrame is the byte-level entry point, kept for tests and for any
// caller that has not decoded yet.
func (h *humanizer) handleSecretFrame(data []byte) ([]byte, bool) {
	msg, ok := h.decodeClientFrame(data)
	if !ok {
		return nil, false
	}
	return h.handleSecretMsg(msg)
}

// handleSecretMsg is the client->browser hook for everything secret-related. It
// returns a REWRITTEN frame to forward (nil when the original should go as-is)
// and whether it has fully handled the command, in which case the caller must
// not forward at all.
func (h *humanizer) handleSecretMsg(msg map[string]any) ([]byte, bool) {
	if h.secrets == nil {
		return nil, false
	}
	params, _ := msg[cdpParams].(map[string]any)
	if params == nil {
		return nil, false
	}
	sid := asString(msg[cdpSessionID])
	switch asString(msg[cdpMethod]) {
	case methodInsertText:
		return h.secretInsertText(msg, params, sid)
	case methodIMEComposition:
		// The composition path bypasses the humanizer's counting and the field's
		// own maxlength, and it is not a path a substituted value should ride.
		return h.refuseSentinelOn(msg, sid, asString(params[cdpText]),
			"a secret cannot be typed through an IME composition - fill the field instead")
	case methodSendMessageToTarget:
		return h.refuseTunneled(msg, sid, asString(params["message"]))
	case "Runtime.evaluate":
		return h.refuseSentinelOn(msg, sid, asString(params["expression"]),
			"a cuttle secret can only be typed, not evaluated - fill the field with the sentinel instead")
	case "Runtime.callFunctionOn":
		// Script text only. Playwright's own fill sends the value as a bare
		// `arguments[].value` in a callFunctionOn BEFORE the insertText, so refusing
		// on the raw frame bytes would hard-error the primary flow on frame one.
		return h.refuseSentinelOn(msg, sid, asString(params["functionDeclaration"]),
			"a cuttle secret can only be typed, not evaluated - fill the field with the sentinel instead")
	default:
		return nil, false
	}
}

// refuseTunneled refuses a command carried inside another one. cuttle drives
// flat sessions, so nothing downstream decodes that nested payload: a fill
// tunneled this way reached Chrome with no target check and no substitution,
// which for a sentinel meant typing `{{cuttle:NAME}}` as literal text into a
// live field. Only value-placing payloads are refused; anything else tunnels on.
func (h *humanizer) refuseTunneled(msg map[string]any, sid, nested string) ([]byte, bool) {
	// The nested payload is a JSON string, so it is decoded rather than scanned -
	// the same reason decodeClientFrame has no prefilter.
	var inner map[string]any
	if json.Unmarshal([]byte(nested), &inner) != nil {
		return nil, false
	}
	method := asString(inner[cdpMethod])
	carriesSentinel := strings.Contains(rawText(inner), sentinelPrefix)
	if !carriesSentinel && !strings.HasPrefix(method, "Input.") {
		return nil, false
	}
	id, hasID := clientID(msg)
	if !hasID {
		logWarn("secrets: dropped a %s carrying an input command with no id to answer", methodSendMessageToTarget)
		return nil, true
	}
	h.answerError(id, sid, "cuttle: re-attach with flat sessions (flatten: true) - an input command tunneled"+
		" through Target.sendMessageToTarget is not inspected, so cuttle cannot vouch for the field it lands in"+
		" or substitute a secret into it. Nothing was typed.")
	return nil, true
}

// rawText is every string a tunneled command could hide a sentinel in: its own
// params, at one level. Enough to decide whether to refuse - the refusal is not
// trying to parse someone else's payload for them.
func rawText(inner map[string]any) string {
	params, _ := inner[cdpParams].(map[string]any)
	var b strings.Builder
	for _, v := range params {
		if s, ok := v.(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

// refuseSentinelOn answers a hard CDP error when script text (or a composition)
// carries a sentinel in any form, and forwards untouched when it does not.
func (h *humanizer) refuseSentinelOn(msg map[string]any, sid, text, why string) ([]byte, bool) {
	if !strings.Contains(text, sentinelPrefix) {
		return nil, false
	}
	id, hasID := clientID(msg)
	if !hasID {
		logWarn("secrets: dropped a %s frame carrying a sentinel with no id to answer", asString(msg[cdpMethod]))
		return nil, true
	}
	h.answerError(id, sid, "cuttle: "+why+". Nothing was typed.")
	return nil, true
}

// secretInsertText is the primary path: every fill a driver performs. It carries
// three responsibilities - substitute a sentinel, refuse a literal credential,
// and vouch for the target before either.
func (h *humanizer) secretInsertText(msg, params map[string]any, sid string) ([]byte, bool) {
	text := asString(params[cdpText])
	if text == "" {
		return nil, false
	}
	name, kind := parseSentinel(text)
	id, hasID := clientID(msg)

	if kind == sentinelEmbedded {
		if !hasID {
			logWarn("secrets: dropped an Input.insertText carrying an embedded sentinel with no id to answer")
			return nil, true
		}
		h.answerError(id, sid, "cuttle: type {{cuttle:NAME}} as the WHOLE value - a sentinel inside other text"+
			" matches no secret and would be typed literally. Nothing was typed.")
		return nil, true
	}

	if kind == sentinelNone {
		// The commit of a composition this connection already typed out is not a
		// fresh fill: the value is in the field, and handleInsertText answers it
		// without typing again. Judging it here would answer "Nothing was typed"
		// about a value that WAS typed, and burn an armed allow-literal token on a
		// fill that already happened.
		if text == h.composed {
			return nil, false
		}
		return h.checkLiteralFill(sid, id, hasID)
	}
	if !hasID {
		logWarn("secrets: dropped a sentinel fill with no id to answer (nothing is waiting on a reply)")
		return nil, true
	}
	return h.substitute(msg, params, sid, id, name)
}

// checkLiteralFill refuses a literal typed into a credential field - the one
// path drivers overwhelmingly use for credentials, and the moment a whole task
// was once lost to a driver typing the string "HC_PASS" into a live login form.
// It is a tripwire, not an airtight control: a literal typed per-character, set
// through a .value setter, composed or pasted is NOT refused (SKILL.md says so).
func (h *humanizer) checkLiteralFill(sid string, id any, hasID bool) ([]byte, bool) {
	// An ordinary fill has no fill-wide deadline to draw on, so it gets one of its
	// own covering the same two steps.
	tgt, probed := h.preflight(sid, time.Now().Add(secretWorldTimeout+secretProbeTimeout))
	// Fail OPEN when the probe cannot run: this path is on every fill, and
	// refusing every one of them on a page with no isolated world would break far
	// more than it protects. A SENTINEL fill in the same state fails closed (see
	// substitute) - that is the one case where an unverified target is not
	// acceptable.
	if !probed || !tgt.credential() {
		return nil, false
	}
	if h.secrets.takeLiteral(h.seed) {
		logInfo("secrets: literal fill allowed into %s by an armed cuttle secret allow-literal (seed=%s)",
			tgt.describe(), h.seed)
		return nil, false
	}
	if !hasID {
		// No id means nothing is waiting on a reply, so there is no way to say no.
		// Refusing silently would strand the driver; this is the one fill that gets
		// through the refusal, and it is logged.
		logWarn("secrets: a literal fill into %s had no id to refuse with; forwarded (seed=%s)", tgt.describe(), h.seed)
		return nil, false
	}
	h.answerError(id, sid, fmt.Sprintf(
		"cuttle: type {{cuttle:NAME}} instead, or run `cuttle secret allow-literal` - refusing a literal"+
			" typed into %s. Register one with `cuttle secret set NAME --stdin`. Nothing was typed.", tgt.describe(),
	))
	return nil, true
}

// substitute resolves a sentinel and hands the value to the fill. The three
// failure answers are deliberately distinct: unknown, expired-with-a-recipe and
// expired-without-one need three different fixes, and one generic error would
// leave an agent guessing which.
func (h *humanizer) substitute(msg, params map[string]any, sid string, id any, name string) ([]byte, bool) {
	val, source, status := h.secrets.take(h.seed, name)
	switch status {
	case secretUnknown:
		h.answerError(id, sid, fmt.Sprintf(
			"cuttle: unknown secret %q - run `cuttle secret set %s --stdin` first.%s Nothing was typed.",
			name, name, registeredNames(h.secrets.names(h.seed)),
		))
		return nil, true
	case secretStale:
		h.answerError(id, sid, staleSecretError(name, source))
		return nil, true
	case secretLive:
	}
	defer clear(val)
	return h.fillWithSecret(msg, params, sid, id, name, val)
}

// staleSecretError says how to get a value back, which depends entirely on where
// the last one came from: only an --exec recipe can be re-run unattended.
func staleSecretError(name, source string) string {
	switch source {
	case sourceExec:
		return fmt.Sprintf("cuttle: run `cuttle secret refresh %s` - its value expired and the daemon holds no copy."+
			" The recipe is on the host; refresh re-runs it. Nothing was typed.", name)
	case sourcePrompt:
		return fmt.Sprintf("cuttle: run `cuttle secret prompt %s` again - its value expired, and a value a human"+
			" typed in has no recipe to re-run. Nothing was typed.", name)
	default:
		return fmt.Sprintf("cuttle: run `cuttle secret set %s --stdin` again - its value expired and there is no"+
			" --exec recipe to re-run. Nothing was typed.", name)
	}
}

// fillWithSecret vouches for the target and puts the value in it. Every step
// draws down one shared deadline (secretFillBudget), because the hazard is not a
// slow step - it is the SUM overrunning the driver's action timeout, after which
// the driver retries and the credential lands twice.
func (h *humanizer) fillWithSecret(msg, params map[string]any, sid string, id any, name string, val []byte) ([]byte, bool) {
	deadline := time.Now().Add(secretFillBudget)

	// One string and one rune slice, and no more: every copy of a secret is an
	// un-zeroable one the GC decides the lifetime of, so the raw-mode branch below
	// reuses this string rather than making its own.
	value := string(val)
	runes := []rune(value)

	tgt, probed := h.preflight(sid, deadline)
	if !probed {
		h.answerError(id, sid, fmt.Sprintf(
			"cuttle: refusing to type secret %s - the target could not be inspected, so cuttle cannot tell"+
				" what the value would land in. Nothing was typed.", name,
		))
		return nil, true
	}
	if why := tgt.refuse(len(runes)); why != "" {
		h.answerError(id, sid, fmt.Sprintf(
			"cuttle: refusing to type secret %s - %s. Nothing was typed.", name, why,
		))
		return nil, true
	}
	if prev := h.secrets.noteOrigin(h.seed, name, tgt.origin); prev != "" && prev != tgt.origin {
		logWarn("secrets: %s was first used on %s and is now being typed on %s", name, prev, tgt.origin)
	}

	if !h.enabled {
		// No keystroke path runs in this mode, so the frame itself carries the
		// substituted value under the driver's own id and the browser answers the
		// driver directly. Post-type verification does not apply: a user who turned
		// humanization off asked for the raw path.
		params[cdpText] = value
		out, err := json.Marshal(msg)
		if err != nil {
			h.answerError(id, sid, "cuttle: substituting the secret failed. Nothing was typed.")
			return nil, true
		}
		return out, false
	}

	head, tail := runes, []rune(nil)
	if len(runes) > secretMaxRunes {
		head, tail = runes[:secretMaxRunes], runes[secretMaxRunes:]
	}
	// Typos are suppressed: emitTypo corrects with a blind Backspace, which on a
	// segmented auto-advancing OTP field lands in the NEXT box. The typing budget
	// is whatever is left after reserving the post-type probe's share.
	typing := budgetFor(deadline.Add(-secretProbeTimeout), h.secretTypingBudget())
	done, abandoned := h.typeHumanized(sid, head, typing, true)
	if abandoned {
		h.answerError(id, sid, fmt.Sprintf(
			"cuttle: typing secret %s stopped after %d of %d characters - the field holds a partial value;"+
				" clear it before retrying.", name, done, len(runes),
		))
		return nil, true
	}
	if len(tail) > 0 {
		h.inject(sid, methodInsertText, map[string]any{cdpText: string(tail)})
	}
	if why := h.verifyTyped(sid, name, tgt, len(runes), deadline); why != "" {
		h.answerError(id, sid, why)
		return nil, true
	}
	_ = h.clientSend(websocket.MessageText, okResponse(id, sid))
	return nil, true
}

// budgetFor is the time left before deadline, capped at want. A step that is
// already out of time gets a floor rather than a negative deadline: the call
// then fails fast on its own terms instead of being skipped silently.
func budgetFor(deadline time.Time, want time.Duration) time.Duration {
	left := time.Until(deadline)
	switch {
	case left <= 0:
		return time.Millisecond
	case left < want:
		return left
	default:
		return want
	}
}

// secretTypingBudget is the typing ceiling, overridable by tests.
func (h *humanizer) secretTypingBudget() time.Duration {
	if h.secretBudget > 0 {
		return h.secretBudget
	}
	return secretTypeBudget
}

// verifyTyped reads back DERIVED properties only - a length, an identity, never
// the value or a prefix of it - and reports rather than repairs. A repair retype
// is actively dangerous: on a segmented OTP input focus auto-advances per
// character, and on auto-submit the page has already navigated, so the "fix"
// fires a live credential into the next field or the post-submit page.
func (h *humanizer) verifyTyped(sid, name string, before fillTarget, want int, deadline time.Time) string {
	after, ok := h.probe(sid, verifyProbeJS, deadline)
	if !ok {
		return "" // fail open: an unverifiable type is not a failed one
	}
	if !asBool(after["same"]) || int64(asFloat(after["token"])) != before.token ||
		asString(after["origin"]) != before.origin {
		// The OTP auto-advance / auto-submit case, and it is normal.
		logInfo("secrets: %s typed; focus left the field before it could be verified (seed=%s)", name, h.seed)
		return ""
	}
	got := int(asFloat(after["length"]))
	if got < 0 || got == want {
		return ""
	}
	return fmt.Sprintf("cuttle: typed %d characters of secret %s but the field holds %d"+
		" - re-read the field before continuing; cuttle did not retype it.", want, name, got)
}

func registeredNames(names []string) string {
	if len(names) == 0 {
		return " No secrets are registered for this session."
	}
	return " Registered: " + strings.Join(names, ", ") + "."
}

// fillTarget is the shape of the element a fill is about to land in - never any
// of its content. CDP reports success for an insertText that inserted nothing at
// all, and on a disabled target it inserts into WHATEVER WAS FOCUSED INSTEAD, so
// the driver's "ok" is not evidence the value went anywhere it meant.
type fillTarget struct {
	tag, typ, autocomplete, inputMode, origin string
	disabled, readOnly, editable              bool
	maxLength                                 int
	token                                     int64
}

// credential reports whether the target is a field a credential goes in: a
// password box, or the OTP shapes (a TOTP field is type=text, never
// type=password, so type alone would miss the headline case).
func (t fillTarget) credential() bool {
	return t.typ == "password" ||
		strings.Contains(t.autocomplete, "one-time-code") ||
		t.inputMode == "numeric"
}

// refuse reports why a secret must not be typed into this target, or "".
func (t fillTarget) refuse(runes int) string {
	switch {
	case t.disabled:
		return "the focused element is disabled, so the value would land in whatever was focused before it"
	case t.readOnly:
		return "the focused element is readonly - the page still receives every character, but the field keeps none"
	case !t.editable:
		return "the focused element (" + t.describe() + ") is not an editable field"
	case t.maxLength >= 0 && t.maxLength < runes:
		return fmt.Sprintf("the field's maxlength (%d) is shorter than the value (%d characters), which would truncate it silently", t.maxLength, runes)
	default:
		return ""
	}
}

func (t fillTarget) describe() string {
	d := "<" + t.tag
	if t.typ != "" {
		d += " type=" + t.typ
	}
	if t.autocomplete != "" {
		d += " autocomplete=" + t.autocomplete
	}
	return d + ">"
}

// activeElementJS resolves the element a fill will actually reach, walking into
// a same-origin iframe: a login form inside one otherwise reports tag IFRAME and
// dodges every check below it. A cross-document hop that throws is reported as
// unavailable, and the caller decides (fail-open for a literal, closed for a
// sentinel).
const activeElementJS = `var d=document,e=d.activeElement;` +
	`try{if(e&&e.tagName==='IFRAME'&&e.contentDocument&&e.contentDocument.activeElement){d=e.contentDocument;e=d.activeElement;}}catch(x){return null;}` +
	`var o='';try{o=location.origin;}catch(x){}`

// preflightProbeJS reports the SHAPE of the focused element and the page origin,
// never any value, and stamps the element in the isolated world so the post-type
// check can prove it is looking at the same node rather than a focus-advanced
// sibling. The stamp is an isolated-world reference, not a DOM attribute, so the
// page cannot see it.
const preflightProbeJS = `(function(){` + activeElementJS +
	`if(!e||e===d.body||e===d.documentElement)return{ok:false,origin:o};` +
	`var g=window.__cuttleFill||(window.__cuttleFill={n:0});g.n++;g.el=e;` +
	`var tag=(e.tagName||'').toLowerCase();` +
	`var ml=-1;try{if(typeof e.maxLength==='number')ml=e.maxLength;}catch(x){}if(ml<0||ml>1000000)ml=-1;` +
	`var at=function(n){try{return((e.getAttribute&&e.getAttribute(n))||'').toLowerCase();}catch(x){return'';}};` +
	`return{ok:true,token:g.n,tag:tag,type:at('type'),disabled:!!e.disabled,readonly:!!e.readOnly,` +
	`editable:(tag==='input'||tag==='textarea'||e.isContentEditable===true),maxLength:ml,` +
	`autocomplete:at('autocomplete'),inputmode:at('inputmode'),origin:o};})()`

// verifyProbeJS reads back only derived properties: whether focus is still on the
// stamped element, and how many characters it holds.
const verifyProbeJS = `(function(){` + activeElementJS +
	`var g=window.__cuttleFill||{};var n=-1;try{if(e&&typeof e.value==='string')n=e.value.length;}catch(x){}` +
	`return{ok:true,same:!!(e&&g.el===e),token:g.n||0,length:n,origin:o};})()`

// preflight runs the mandatory target check. ok=false means the probe could not
// run at all (no isolated world, a dead session, a cross-origin focused frame) -
// which is a different thing from a target it refuses.
func (h *humanizer) preflight(sid string, deadline time.Time) (fillTarget, bool) {
	val, ok := h.probe(sid, preflightProbeJS, deadline)
	if !ok || !asBool(val["ok"]) {
		return fillTarget{}, false
	}
	return fillTarget{
		tag: asString(val["tag"]), typ: asString(val["type"]),
		autocomplete: asString(val["autocomplete"]), inputMode: asString(val["inputmode"]),
		origin:   asString(val["origin"]),
		disabled: asBool(val["disabled"]), readOnly: asBool(val["readonly"]),
		editable:  asBool(val["editable"]),
		maxLength: int(asFloat(val["maxLength"])), token: int64(asFloat(val["token"])),
	}, true
}

// probe evaluates one of the constant expressions above, and ONLY in the
// session's isolated world. query falls back to the page's main world when no
// isolated one can be built - right for the settle gate, wrong here twice over:
// the pre-flight stamps a marker the page would then be able to read and
// fingerprint, and a sentinel fill would treat that unverifiable read as a
// verified target instead of refusing. No isolated world means no probe.
//
// No value is ever formatted into an expression either - the probes take no
// arguments at all, which is what keeps a secret out of script text.
func (h *humanizer) probe(sid, expr string, deadline time.Time) (map[string]any, bool) {
	// The world build draws on the SAME deadline as the evaluate. Leaving it on
	// its own clock is what made the budget stop being one: two setup calls at
	// worldTimeout each sat outside the fill's ceiling entirely.
	if h.isolatedWorldWithin(sid, budgetFor(deadline, secretWorldTimeout)) == 0 {
		return nil, false
	}
	val, ok := h.queryWithin(sid, expr, budgetFor(deadline, secretProbeTimeout))
	if !ok || val == nil {
		return nil, false
	}
	return val, true
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}
