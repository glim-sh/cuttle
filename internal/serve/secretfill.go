package serve

import (
	"bytes"
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
	// secretTypeBudget is the wall-clock ceiling on typing a secret, cut down from
	// insertTextBudget to leave room for the two probes. The whole sequence -
	// world setup (worldTimeout) + pre-flight + type + post-type - must stay under
	// the driver's ~5s action timeout, or the driver retries the fill and the
	// credential lands twice.
	secretTypeBudget = 2500 * time.Millisecond
	// secretMaxRunes caps the keystroke head for a secret. insertTextMaxRunes (20)
	// paces at ~2.8s, which overruns secretTypeBudget on its own; the remainder
	// rides one insertText exactly as it does for an ordinary fill.
	secretMaxRunes = 12
)

// Prefilter needles. handleSecretFrame runs on every client frame, so it decodes
// nothing until one of these hits: the two Input methods that can place a whole
// value, plus the sentinel bytes themselves (which is how a Runtime frame with a
// sentinel in its script text is caught without decoding every Runtime command -
// Playwright emits one for nearly every action).
var (
	insertTextNeedle  = []byte(`"` + methodInsertText + `"`)
	compositionNeedle = []byte(`"` + methodIMEComposition + `"`)
	sentinelNeedle    = []byte(sentinelPrefix)
)

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

// handleSecretFrame is the client->browser hook for everything secret-related.
// It returns the frame to forward and whether it has fully handled the command
// (answered the driver itself), in which case the caller must NOT forward.
func (h *humanizer) handleSecretFrame(data []byte) ([]byte, bool) {
	if h.secrets == nil {
		return data, false
	}
	if !bytes.Contains(data, insertTextNeedle) && !bytes.Contains(data, compositionNeedle) &&
		!bytes.Contains(data, sentinelNeedle) {
		return data, false
	}
	msg, ok := decodeCDP(data)
	if !ok {
		return data, false
	}
	params, _ := msg[cdpParams].(map[string]any)
	if params == nil {
		return data, false
	}
	sid := asString(msg[cdpSessionID])
	switch asString(msg[cdpMethod]) {
	case methodInsertText:
		return h.secretInsertText(msg, params, sid, data)
	case methodIMEComposition:
		// The composition path bypasses the humanizer's counting and the field's
		// own maxlength, and it is not a path a substituted value should ride.
		return h.refuseSentinelOn(msg, sid, data, asString(params[cdpText]),
			"a secret cannot be typed through an IME composition - fill the field instead")
	case "Runtime.evaluate":
		return h.refuseSentinelOn(msg, sid, data, asString(params["expression"]),
			"a cuttle secret can only be typed, not evaluated - fill the field with the sentinel instead")
	case "Runtime.callFunctionOn":
		// Script text only. Playwright's own fill sends the value as a bare
		// `arguments[].value` in a callFunctionOn BEFORE the insertText, so refusing
		// on the raw frame bytes would hard-error the primary flow on frame one.
		return h.refuseSentinelOn(msg, sid, data, asString(params["functionDeclaration"]),
			"a cuttle secret can only be typed, not evaluated - fill the field with the sentinel instead")
	default:
		return data, false
	}
}

// refuseSentinelOn answers a hard CDP error when script text (or a composition)
// carries a sentinel in any form, and forwards untouched when it does not.
func (h *humanizer) refuseSentinelOn(msg map[string]any, sid string, data []byte, text, why string) ([]byte, bool) {
	if !strings.Contains(text, sentinelPrefix) {
		return data, false
	}
	id, hasID := asInt(msg[cdpID])
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
func (h *humanizer) secretInsertText(msg, params map[string]any, sid string, data []byte) ([]byte, bool) {
	text := asString(params[cdpText])
	if text == "" {
		return data, false
	}
	name, kind := parseSentinel(text)
	id, hasID := asInt(msg[cdpID])

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
		return h.checkLiteralFill(sid, data, id, hasID)
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
func (h *humanizer) checkLiteralFill(sid string, data []byte, id int64, hasID bool) ([]byte, bool) {
	tgt, probed := h.preflight(sid)
	// Fail OPEN when the probe cannot run: this path is on every fill, and
	// refusing every one of them on a page with no isolated world would break far
	// more than it protects. A SENTINEL fill in the same state fails closed (see
	// substitute) - that is the one case where an unverified target is not
	// acceptable.
	if !probed || !tgt.credential() {
		return data, false
	}
	if h.secrets.takeLiteral(h.seed) {
		logInfo("secrets: literal fill allowed into %s by an armed cuttle secret allow-literal (seed=%s)",
			tgt.describe(), h.seed)
		return data, false
	}
	if !hasID {
		return data, false
	}
	h.answerError(id, sid, fmt.Sprintf(
		"cuttle: type {{cuttle:NAME}} instead, or run `cuttle secret allow-literal` - refusing a literal"+
			" typed into %s. Register one with `cuttle secret set NAME --stdin`. Nothing was typed.", tgt.describe(),
	))
	return nil, true
}

// substitute resolves a sentinel and puts the real value into the field. The
// three failure answers are deliberately distinct: unknown, expired-with-a-recipe
// and expired-without-one need three different fixes, and one generic error would
// leave an agent guessing which.
func (h *humanizer) substitute(msg, params map[string]any, sid string, id int64, name string) ([]byte, bool) {
	val, source, status := h.secrets.take(h.seed, name)
	switch status {
	case secretUnknown:
		h.answerError(id, sid, fmt.Sprintf(
			"cuttle: unknown secret %q - run `cuttle secret set %s --stdin` first.%s Nothing was typed.",
			name, name, registeredNames(h.secrets.names(h.seed)),
		))
		return nil, true
	case secretStale:
		if source == sourceExec {
			h.answerError(id, sid, fmt.Sprintf(
				"cuttle: run `cuttle secret refresh %s` - its value expired and the daemon holds no copy."+
					" The recipe is on the host; refresh re-runs it. Nothing was typed.", name,
			))
			return nil, true
		}
		h.answerError(id, sid, fmt.Sprintf(
			"cuttle: run `cuttle secret set %s --stdin` again - its value expired and there is no --exec"+
				" recipe to re-run. Nothing was typed.", name,
		))
		return nil, true
	case secretLive:
	}
	defer clear(val)

	tgt, probed := h.preflight(sid)
	if !probed {
		h.answerError(id, sid, fmt.Sprintf(
			"cuttle: refusing to type secret %s - the target could not be inspected, so cuttle cannot tell"+
				" what the value would land in. Nothing was typed.", name,
		))
		return nil, true
	}
	if why := tgt.refuse(len([]rune(string(val)))); why != "" {
		h.answerError(id, sid, fmt.Sprintf(
			"cuttle: refusing to type secret %s - %s. Nothing was typed.", name, why,
		))
		return nil, true
	}
	if prev := h.secrets.noteOrigin(h.seed, name, tgt.origin); prev != "" && prev != tgt.origin {
		logWarn("secrets: %s was first used on %s and is now being typed on %s", name, prev, tgt.origin)
	}

	value := string(val)
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

	runes := []rune(value)
	head, tail := runes, []rune(nil)
	if len(runes) > secretMaxRunes {
		head, tail = runes[:secretMaxRunes], runes[secretMaxRunes:]
	}
	// Typos are suppressed: emitTypo corrects with a blind Backspace, which on a
	// segmented auto-advancing OTP field lands in the NEXT box.
	budget := secretTypeBudget
	if h.secretBudget > 0 {
		budget = h.secretBudget
	}
	done, abandoned := h.typeHumanized(sid, head, budget, true)
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
	if why := h.verifyTyped(sid, name, tgt, len(runes)); why != "" {
		h.answerError(id, sid, why)
		return nil, true
	}
	_ = h.clientSend(websocket.MessageText, okResponse(id, sid))
	return nil, true
}

// verifyTyped reads back DERIVED properties only - a length, an identity, never
// the value or a prefix of it - and reports rather than repairs. A repair retype
// is actively dangerous: on a segmented OTP input focus auto-advances per
// character, and on auto-submit the page has already navigated, so the "fix"
// fires a live credential into the next field or the post-submit page.
func (h *humanizer) verifyTyped(sid, name string, before fillTarget, want int) string {
	after, ok := h.probe(sid, verifyProbeJS)
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

// answerError answers one client command with a CDP error. Everything cuttle
// writes goes through the mask on its way out - these messages are built from
// names and lengths, never values, so this is the belt to that braces.
func (h *humanizer) answerError(id int64, sid, message string) {
	_ = h.clientSend(websocket.MessageText, errResponse(id, sid, maskWith(h.secrets, message)))
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
	disabled, readOnly, editable, suggested   bool
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
	`var sug=false;try{sug=!!(e.matches&&e.matches(':autofill')&&!e.value);}catch(x){}` +
	`var at=function(n){try{return((e.getAttribute&&e.getAttribute(n))||'').toLowerCase();}catch(x){return'';}};` +
	`return{ok:true,token:g.n,tag:tag,type:at('type'),disabled:!!e.disabled,readonly:!!e.readOnly,` +
	`editable:(tag==='input'||tag==='textarea'||e.isContentEditable===true),maxLength:ml,` +
	`autocomplete:at('autocomplete'),inputmode:at('inputmode'),suggested:sug,origin:o};})()`

// verifyProbeJS reads back only derived properties: whether focus is still on the
// stamped element, and how many characters it holds.
const verifyProbeJS = `(function(){` + activeElementJS +
	`var g=window.__cuttleFill||{};var n=-1;try{if(e&&typeof e.value==='string')n=e.value.length;}catch(x){}` +
	`return{ok:true,same:!!(e&&g.el===e),token:g.n||0,length:n,origin:o};})()`

// preflight runs the mandatory target check. ok=false means the probe could not
// run at all (no isolated world, a dead session, a cross-origin focused frame) -
// which is a different thing from a target it refuses.
func (h *humanizer) preflight(sid string) (fillTarget, bool) {
	val, ok := h.probe(sid, preflightProbeJS)
	if !ok || !asBool(val["ok"]) {
		return fillTarget{}, false
	}
	return fillTarget{
		tag: asString(val["tag"]), typ: asString(val["type"]),
		autocomplete: asString(val["autocomplete"]), inputMode: asString(val["inputmode"]),
		origin:   asString(val["origin"]),
		disabled: asBool(val["disabled"]), readOnly: asBool(val["readonly"]),
		editable: asBool(val["editable"]), suggested: asBool(val["suggested"]),
		maxLength: int(asFloat(val["maxLength"])), token: int64(asFloat(val["token"])),
	}, true
}

// probe evaluates one of the constant expressions above in the session's
// isolated world. No value is ever formatted into an expression - the probes
// take no arguments at all, which is what keeps a secret out of script text.
func (h *humanizer) probe(sid, expr string) (map[string]any, bool) {
	val, ok := h.query(sid, expr)
	if !ok || val == nil {
		return nil, false
	}
	return val, true
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}
