package serve

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// The CDP half of daemon-owned secrets: sentinel substitution and the pre-flight
// target check that runs before a credential is placed. The store it reads lives
// in secrets.go.
//
// Two properties decide the shape of everything here:
//
//   - It runs OUTSIDE the --humanize gate. Interception used to be
//     `if h.enabled && h.handleClientMsg(...)`; a secret path behind that gate
//     would type `{{cuttle:GH_PASS}}` literally into a live password field the
//     moment someone passed --humanize=false, which is the exact fail-open this
//     feature exists to kill.
//   - It fails CLOSED. An unknown name, a stale value, an embedded sentinel or a
//     target the probe cannot vouch for is a hard CDP error with nothing typed -
//     never a literal fallback (Playwright's substitution returns the NAME as the
//     value on a miss, with no error and no log).
const (
	// secretFillBudget is the ceiling on ONE substituted fill, end to end: world
	// setup, pre-flight probe and typing all draw down the same deadline - the
	// world build included, which is the part that silently escaped it once
	// already. It has to stay under the driver's ~5s action timeout, or the driver
	// gives up mid-fill and retries the credential into the field twice - and
	// per-step timeouts do not add up to that guarantee, they only bound each step
	// in isolation.
	secretFillBudget = 4500 * time.Millisecond
	// secretProbeTimeout bounds the pre-flight probe inside that budget, and
	// secretWorldTimeout bounds building the isolated world it needs. Both are
	// well under their unbudgeted equivalents (queryTimeout, worldTimeout): a page
	// that cannot answer this fast was going to blow the whole fill anyway, and
	// the point is that the SUM stays under the driver's action timeout.
	secretProbeTimeout = 700 * time.Millisecond
	secretWorldTimeout = 600 * time.Millisecond
	// secretCommitTimeout bounds the await on the insertText carrying the value's
	// tail. Its own name because it is not a probe: it is the commit the success
	// answer is allowed to speak for.
	secretCommitTimeout = 700 * time.Millisecond
	// secretTypeBudget caps the typing itself; the remaining-time arithmetic below
	// can only shrink it further.
	secretTypeBudget = 2500 * time.Millisecond
	// secretMaxRunes caps the keystroke head for a secret. insertTextMaxRunes (20)
	// paces at ~2.8s, which overruns secretTypeBudget on its own; the remainder
	// rides one insertText exactly as it does for an ordinary fill.
	secretMaxRunes = 12
)

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
		h.noteSentinelArgument(params)
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
	// the same reason the proxy decodes rather than scans (see wsproxy).
	var inner map[string]any
	if json.Unmarshal([]byte(nested), &inner) != nil {
		return nil, false
	}
	method := asString(inner[cdpMethod])
	carriesSentinel := strings.Contains(rawText(inner), sentinelPrefix)
	// Only the methods that can PLACE A VALUE. Refusing every tunneled Input.*
	// answered a mouse move or a scroll with a message about fields and secrets
	// that says "Nothing was typed" - and it is the first frame such a client
	// sends, so a non-flat-session driver died on a mouse move.
	placesValue := method == methodInsertText || method == methodIMEComposition
	if !carriesSentinel && !placesValue {
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

// noteSentinelArgument logs a sentinel riding in callFunctionOn's arguments.
//
// It cannot be refused: Playwright's own fill sends the fill value as a bare
// argument in exactly this shape BEFORE the Input.insertText that cuttle does
// substitute, so refusing would break the primary supported flow on its first
// frame. But the same shape is reachable directly - `page.$eval(sel, (el, v) =>
// el.value = v, '{{cuttle:X}}')` sets the LITERAL sentinel into a live field
// with no substitution and no error. That is a documented gap in SKILL.md rather
// than a silent one, and this line is what makes it visible after the fact.
func (h *humanizer) noteSentinelArgument(params map[string]any) {
	args, _ := params["arguments"].([]any)
	for _, a := range args {
		arg, _ := a.(map[string]any)
		if s, ok := arg[cdpValue].(string); ok && strings.Contains(s, sentinelPrefix) {
			logWarn("secrets: a sentinel rode in a Runtime.callFunctionOn argument (seed=%s)."+
				" cuttle substitutes only on the fill path, so if this was not a driver's own fill,"+
				" the literal text was set into the page", h.seed)
			return
		}
	}
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
	logWarn("secrets: refused a sentinel carried in %s (seed=%s)", asString(msg[cdpMethod]), h.seed)
	h.answerError(id, sid, "cuttle: "+why+". Nothing was typed.")
	return nil, true
}

// secretInsertText is the primary path: every fill a driver performs. A fill that
// carries no sentinel is forwarded untouched; one that does is substituted, after
// the target has been vouched for.
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
		logWarn("secrets: refused a fill whose sentinel was embedded in other text (seed=%s)", h.seed)
		h.answerError(id, sid, "cuttle: type {{cuttle:NAME}} as the WHOLE value - a sentinel inside other text"+
			" matches no secret and would be typed literally. Nothing was typed.")
		return nil, true
	}

	if kind == sentinelNone {
		// Forwarded untouched, and deliberately unprobed. cuttle used to refuse a
		// literal typed into a credential field here, which put an isolated-world
		// probe in front of EVERY fill in the session to decide whether the field
		// counted. It is gone: the refusal was invisible on the default driver
		// (playwright's fill retries a protocol error into a bare timeout), never
		// covered the per-character path two of three drivers use, and its field
		// predicate fired on ordinary zip and date inputs. cuttle does not judge
		// what an agent types into a field it never named a secret for.
		return nil, false
	}
	if !hasID {
		logWarn("secrets: dropped a sentinel fill with no id to answer (nothing is waiting on a reply)")
		return nil, true
	}
	return h.substitute(msg, params, sid, id, name)
}

// substitute resolves a sentinel and hands the value to the fill. The three
// failure answers are deliberately distinct: unknown, expired-with-a-recipe and
// expired-without-one need three different fixes, and one generic error would
// leave an agent guessing which.
func (h *humanizer) substitute(msg, params map[string]any, sid string, id any, name string) ([]byte, bool) {
	val, source, status := h.secrets.take(h.seed, name)
	// Every refusal below is logged as well as answered. The CDP error does not
	// always reach the agent: playwright's fill retries a protocol error until its
	// own timeout and reports only that timeout, dropping this text entirely - and
	// `cuttle logs` is where SKILL.md sends an agent whose fill just failed. An
	// expired secret is the likeliest failure this feature produces (the default
	// TTL is 15 minutes), so it is the one that most needs to reach that log.
	switch status {
	case secretUnknown:
		logWarn("secrets: refused %s - no secret of that name is registered (seed=%s)", name, h.seed)
		// Both verbs, because the daemon cannot tell which one applies: it holds no
		// recipes (those live in the host's config), so after a restart a name with
		// a perfectly good --exec recipe looks exactly like a name that never
		// existed. Naming only `set --stdin` sends the agent looking for a raw value
		// that, for a TOTP, does not exist anywhere.
		h.answerError(id, sid, fmt.Sprintf(
			"cuttle: unknown secret %q - run `cuttle secret set %s --stdin` first, or"+
				" `cuttle secret refresh %s` if it has an --exec recipe on the host.%s Nothing was typed.",
			name, name, name, registeredNames(h.secrets.names(h.seed)),
		))
		return nil, true
	case secretStale:
		logWarn("secrets: refused %s - its value expired (source=%s, seed=%s)", name, source, h.seed)
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
		why := "the target could not be inspected, so cuttle cannot tell what the value would land in"
		if tgt.nothingFocused {
			why = "nothing is focused - note that a disabled or readonly field silently refuses focus," +
				" so a fill aimed at one lands here"
		}
		logWarn("secrets: refused %s - %s (seed=%s)", name, why, h.seed)
		h.answerError(id, sid, fmt.Sprintf(
			"cuttle: click or focus the field first, then fill it - refusing to type secret %s because %s."+
				" Nothing was typed.", name, why,
		))
		return nil, true
	}
	if why := tgt.refuse(len(runes)); why != "" {
		logWarn("secrets: refused %s into %s - %s (seed=%s)", name, tgt.describe(), why, h.seed)
		h.answerError(id, sid, fmt.Sprintf(
			"cuttle: refusing to type secret %s - %s. Nothing was typed.", name, why,
		))
		return nil, true
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
		h.noteFilled(name, tgt)
		return out, false
	}

	head, tail := runes, []rune(nil)
	if len(runes) > secretMaxRunes {
		head, tail = runes[:secretMaxRunes], runes[secretMaxRunes:]
	}
	// Typos are suppressed: emitTypo corrects with a blind Backspace, which on a
	// segmented auto-advancing OTP field lands in the NEXT box.
	//
	// The typing gets ALL the time left in the fill's budget. It used to hand back
	// secretProbeTimeout for a post-type probe to spend, and that probe is gone -
	// so the keystroke head was being squeezed by 700ms it no longer owed, which
	// is how a slow pre-flight could push an ordinary 13-character secret into an
	// abandoned type. Measured about 1 fill in 78 before this line was corrected.
	typing := budgetFor(deadline, h.secretTypingBudget())
	done, abandoned := h.typeHumanized(sid, head, typing, true)
	if abandoned {
		// The ONLY outcome that leaves part of a credential in a live field, so it
		// is also the one an operator most needs in the log - and it was the single
		// refusal that answered without logging. On the default driver the CDP error
		// does not survive the driver's retry loop, so this line was the whole
		// record of it, and there was none.
		logWarn("secrets: typing %s into %s was abandoned after %d of %d characters - the field holds"+
			" a PARTIAL value (seed=%s)", name, tgt.describe(), done, len(runes), h.seed)
		h.answerError(id, sid, fmt.Sprintf(
			"cuttle: typing secret %s stopped after %d of %d characters - the field holds a partial value;"+
				" clear it before retrying.", name, done, len(runes),
		))
		return nil, true
	}
	if len(tail) > 0 {
		// AWAITED, not injected: inject returns as soon as the frame is written, so
		// the success answered below could otherwise outrun the text it reports.
		// insertText's reply is posted by the renderer after it commits - and it is
		// CHECKED, because a tail that never commits leaves the head alone in the
		// field, which every other channel would report as a clean success.
		if _, ok := h.callWithin(sid, methodInsertText, map[string]any{cdpText: string(tail)},
			budgetFor(deadline, secretCommitTimeout)); !ok {
			logWarn("secrets: the tail of %s never committed into %s - the field holds a PARTIAL value,"+
				" %d of %d characters (seed=%s)", name, tgt.describe(), len(head), len(runes), h.seed)
			h.answerError(id, sid, fmt.Sprintf(
				"cuttle: typing secret %s stopped after %d of %d characters - the field holds a partial value;"+
					" clear it before retrying.", name, len(head), len(runes),
			))
			return nil, true
		}
	}
	h.noteFilled(name, tgt)
	// The audit line: the only record that a credential entered a page, and the
	// only way an agent can confirm the type without reading the field back - which
	// is itself the leak. Shape only, never the value.
	//
	// tgt.length is what the field already held, read by the PRE-FLIGHT before a
	// single keystroke. insertText inserts at the caret rather than replacing, so a fill
	// into a non-empty field appends: the page ends up with prefix+secret, which is
	// a wrong credential that every other channel reports as a clean success. A
	// driver that selects the field first (playwright's fill does) never sees it;
	// a raw-CDP driver that does not, does.
	if tgt.length > 0 {
		logWarn("secrets: typed %s (%d characters) into %s on %s, which ALREADY held %d characters -"+
			" insertText appends, so the field now holds both; clear it and retype if that is not what you meant (seed=%s)",
			name, len(runes), tgt.describe(), tgt.origin, tgt.length, h.seed)
	} else {
		logInfo("secrets: typed %s (%d characters) into %s on %s (seed=%s)",
			name, len(runes), tgt.describe(), tgt.origin, h.seed)
	}
	_ = h.clientSend(websocket.MessageText, okResponse(id, sid))
	return nil, true
}

// noteFilled records the origin a secret reached and warns on drift. Called only
// once the value has landed: a refused or abandoned fill must not bind the name
// to a page the credential never reached.
func (h *humanizer) noteFilled(name string, tgt fillTarget) {
	if prev := h.secrets.noteOrigin(h.seed, name, tgt.origin); prev != "" && prev != tgt.origin {
		logWarn("secrets: %s was first used on %s and is now being typed on %s", name, prev, tgt.origin)
	}
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
	name, id                                  string
	disabled, readOnly, editable              bool
	maxLength, length                         int
	// nothingFocused is set when the probe ran and found no focused element, as
	// opposed to not running at all. Only the refusal text depends on it.
	nothingFocused bool
}

// refuse reports why a secret must not be typed into this target, or "".
//
// Every branch names the element. Three of them used to say only "the focused
// element", which on a page with more than one field is unactionable - an agent
// that aimed at one input and got a refusal about another concluded cuttle was
// confused, while the log line carried the identity the error had withheld.
func (t fillTarget) refuse(runes int) string {
	switch {
	case t.disabled:
		return "the focused element " + t.describe() + " is disabled, so the value would land in whatever was focused before it"
	case t.readOnly:
		return "the focused element " + t.describe() + " is readonly - the page still receives every character, but the field keeps none"
	case !t.editable:
		return "the focused element " + t.describe() + " is not an editable field"
	case t.maxLength >= 0 && t.maxLength < runes:
		return fmt.Sprintf("%s has a maxlength of %d, shorter than the value (%d characters), which would truncate it silently",
			t.describe(), t.maxLength, runes)
	default:
		return ""
	}
}

// describe names the element in a refusal. It prints the attributes the refusal
// actually turns on - including the one that MATCHED, or the message names an
// innocent neighbour and hides the reason: a numeric zip box once rendered as
// the bare `<input type=text>`, which reads as "cuttle refuses every text box".
func (t fillTarget) describe() string {
	var d strings.Builder
	d.WriteString("<" + t.tag)
	for _, a := range [][2]string{
		{"type", t.typ},
		{"name", t.name},
		{"id", t.id},
		{"autocomplete", t.autocomplete},
		{"inputmode", t.inputMode},
	} {
		if a[1] != "" {
			d.WriteString(" " + a[0] + "=" + a[1])
		}
	}
	d.WriteString(">")
	return d.String()
}

// activeElementJS resolves the element a fill will actually reach, walking into
// a same-origin iframe: a login form inside one otherwise reports tag IFRAME and
// dodges every check below it. A cross-document hop that throws is reported as
// unavailable, and the caller decides (fail-open for a literal, closed for a
// sentinel).
const activeElementJS = `var d=document,e=d.activeElement;` +
	`try{if(e&&e.tagName==='IFRAME'&&e.contentDocument&&e.contentDocument.activeElement){d=e.contentDocument;e=d.activeElement;}}catch(x){return null;}` +
	`var o='';try{o=location.origin;}catch(x){}`

// maxLengthNoCap is a sanity ceiling: a cap this large cannot truncate a
// credential, so a page reporting one is read as declaring no cap at all. An
// input that genuinely declares none reports -1, which the ml<0 half catches.
const maxLengthNoCap = "1000000"

// preflightProbeJS reports the SHAPE of the focused element and the page origin,
// never any value.
const preflightProbeJS = `(function(){` + activeElementJS +
	`if(!e||e===d.body||e===d.documentElement)return{ok:false,nofocus:true,origin:o};` +
	`var tag=(e.tagName||'').toLowerCase();` +
	`var ml=-1;try{if(typeof e.maxLength==='number')ml=e.maxLength;}catch(x){}` +
	`if(ml<0||ml>` + maxLengthNoCap + `)ml=-1;` +
	`var n=-1;try{if(typeof e.value==='string')n=e.value.length;}catch(x){}` +
	`var at=function(n){try{return((e.getAttribute&&e.getAttribute(n))||'').toLowerCase();}catch(x){return'';}};` +
	`return{ok:true,tag:tag,type:at('type'),disabled:!!e.disabled,readonly:!!e.readOnly,` +
	`editable:(tag==='input'||tag==='textarea'||e.isContentEditable===true),maxLength:ml,length:n,` +
	`name:at('name'),id:at('id'),` +
	`autocomplete:at('autocomplete'),inputmode:at('inputmode'),origin:o};})()`

// preflight runs the mandatory target check. ok=false means the probe could not
// run at all (no isolated world, a dead session, a cross-origin focused frame) -
// which is a different thing from a target it refuses.
func (h *humanizer) preflight(sid string, deadline time.Time) (fillTarget, bool) {
	val, ok := h.probe(sid, preflightProbeJS, deadline)
	if !ok {
		return fillTarget{}, false
	}
	if !asBool(val["ok"]) {
		// The probe RAN and found nothing focused. That is a different thing from a
		// probe that could not run, and worth its own answer: a disabled or readonly
		// input silently refuses focus, so "focus the field first" is what an agent
		// hears after it has just done exactly that.
		return fillTarget{nothingFocused: asBool(val["nofocus"])}, false
	}
	return fillTarget{
		tag: asString(val["tag"]), typ: asString(val["type"]),
		autocomplete: asString(val["autocomplete"]), inputMode: asString(val["inputmode"]),
		name: asString(val["name"]), id: asString(val["id"]),
		origin:   asString(val["origin"]),
		disabled: asBool(val["disabled"]), readOnly: asBool(val["readonly"]),
		editable:  asBool(val["editable"]),
		maxLength: int(asFloat(val["maxLength"])), length: int(asFloat(val["length"])),
	}, true
}

// probe evaluates one of the constant expressions above, and ONLY in the
// session's isolated world. query falls back to the page's main world when no
// isolated one can be built - right for the settle gate, wrong here: the page
// authors every field this probe reads, so a sentinel fill would treat its
// answer as a verified target instead of refusing. No isolated world, no probe.
//
// No value is ever formatted into an expression either - the probes take no
// arguments at all, which is what keeps a secret out of script text.
func (h *humanizer) probe(sid, expr string, deadline time.Time) (map[string]any, bool) {
	// The world build draws on the SAME deadline as the evaluate. Leaving it on
	// its own clock is what made the budget stop being one: two setup calls at
	// worldTimeout each sat outside the fill's ceiling entirely.
	world := h.isolatedWorldWithin(sid, budgetFor(deadline, secretWorldTimeout))
	if world == 0 {
		return nil, false
	}
	// Evaluated in THAT world id. queryWithin resolves the world a second time and,
	// when a rebuild fails, sends the probe with no contextId at all - which answers
	// out of the page's MAIN world, where the page authors every field this check
	// reads. It also retries once, and a world retired mid-fill is a refusal here,
	// not something to paper over for a credential.
	val, ok, _ := h.evaluateIn(sid, expr, world, budgetFor(deadline, secretProbeTimeout))
	if !ok || val == nil {
		return nil, false
	}
	return val, true
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}
