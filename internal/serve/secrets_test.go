package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// secretHarness drives the secrets path end to end without a browser: it records
// every frame sent to the browser and every frame answered to the driver, and it
// answers the isolated-world probes with scripted values. The probe half is the
// piece recordingHumanizer cannot do - its waiters and clientSend are nil, so a
// pre-flight would nil-map-panic and then time out with no responder.
type secretHarness struct {
	h           *humanizer
	injected    []map[string]any // frames the browser received
	answered    []map[string]any // frames the driver received
	answeredRaw [][]byte         // ... and their bytes, for assertions about fidelity
	probes      []string         // probe expressions evaluated
	logs        bytes.Buffer

	// preflight is the probe answer; nil means the probe could not run (an
	// evaluate error, exactly like a page with no isolated world).
	preflight map[string]any
	// probeCtx is the contextId each probe carried, nil for one sent without any -
	// which means the PAGE answered it.
	probeCtx []any
	// withholdSetup leaves one isolated-world setup call unanswered, which is what
	// a target with no Page domain (or a detached one) looks like: the call is
	// sent and nothing ever comes back. "*" withholds both.
	withholdSetup string
	// withholdSetupAfter stops answering world-build calls once this many have been
	// answered, so a REBUILD fails while the first build succeeded.
	withholdSetupAfter int
	setupAnswered      int
	// failProbes answers this many probes with a CDP error before the scripted
	// preflight - the shape of a world retired under a fill.
	failProbes int
	// withholdCommit leaves the tail insertText unanswered, so the commit the
	// success answer speaks for never arrives.
	withholdCommit bool
}

const testSeed = "S"

// textInput is the ordinary, unremarkable fill target: an editable text box with
// no length cap. Individual tests copy it and change the one field under test.
func textInput() map[string]any {
	return map[string]any{
		"ok": true, "tag": "input", "type": "text",
		"disabled": false, "readonly": false, "editable": true, "maxLength": float64(-1),
		"autocomplete": "", "inputmode": "", "origin": "https://example.com",
	}
}

func passwordInput() map[string]any {
	t := textInput()
	t["type"] = "password"
	return t
}

func newSecretHarness(t *testing.T, store *secretStore, enabled bool) *secretHarness {
	t.Helper()
	hs := &secretHarness{preflight: textInput()}
	hs.h = &humanizer{
		ctx:          context.Background(),
		enabled:      enabled,
		rng:          newTestRNG(9),
		secrets:      store,
		seed:         testSeed,
		typeBudget:   time.Minute,
		secretBudget: time.Minute,
		nextID:       humanizeIDBase,
		pending:      map[int64]struct{}{},
		waiters:      map[int64]chan []byte{},
		worlds:       map[string]int64{},
	}
	hs.h.cdpSend = func(_ websocket.MessageType, data []byte) error {
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		hs.injected = append(hs.injected, m)
		hs.answerSetup(m)
		if m["method"] == "Runtime.evaluate" {
			hs.answerProbe(m)
		}
		if m["method"] == methodInsertText && !hs.withholdCommit {
			hs.answerCommit(m)
		}
		return nil
	}
	hs.h.clientSend = func(_ websocket.MessageType, data []byte) error {
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		hs.answered = append(hs.answered, m)
		hs.answeredRaw = append(hs.answeredRaw, append([]byte(nil), data...))
		return nil
	}
	// Capture the daemon's own log lines: a secret in a log line is a durable leak
	// (the session log is teed to the profile volume), so the tests assert on them.
	prev := logger
	logger = slog.New(slog.NewTextHandler(&hs.logs, nil))
	t.Cleanup(func() { logger = prev })
	return hs
}

// answerSetup answers the two calls that build a session's isolated world, so a
// probe costs one round-trip in a test the way it does against a live browser.
// The secret path refuses to probe at all without one.
func (hs *secretHarness) answerSetup(cmd map[string]any) {
	var value any
	method, _ := cmd["method"].(string)
	if hs.withholdSetup == "*" || hs.withholdSetup == method {
		return
	}
	if hs.withholdSetupAfter > 0 && hs.setupAnswered >= hs.withholdSetupAfter {
		return
	}
	hs.setupAnswered++
	switch method {
	case "Page.getFrameTree":
		value = map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "F"}}}
	case "Page.createIsolatedWorld":
		value = map[string]any{"executionContextId": 1}
	default:
		return
	}
	id := int64(cmd["id"].(float64))
	hs.h.mu.Lock()
	ch := hs.h.waiters[id]
	hs.h.mu.Unlock()
	if ch == nil {
		return
	}
	body, _ := json.Marshal(map[string]any{"id": id, "result": value})
	ch <- body
}

// answerProbe replies to a probe the way the browser would, on the same
// goroutine: callWithin registers the waiter before it sends, and the channel is
// buffered, so this never blocks.
func (hs *secretHarness) answerProbe(cmd map[string]any) {
	params, _ := cmd["params"].(map[string]any)
	expr, _ := params["expression"].(string)
	hs.probes = append(hs.probes, expr)
	hs.probeCtx = append(hs.probeCtx, params["contextId"])
	value := hs.preflight
	id := int64(cmd["id"].(float64))
	if hs.failProbes > 0 {
		hs.failProbes--
		hs.h.mu.Lock()
		ch := hs.h.waiters[id]
		hs.h.mu.Unlock()
		if ch != nil {
			ch <- []byte(`{"id":` + strconv.FormatInt(id, 10) + `,"error":{"code":-32000,"message":"Cannot find context with specified id"}}`)
		}
		return
	}
	hs.h.mu.Lock()
	ch := hs.h.waiters[id]
	hs.h.mu.Unlock()
	if ch == nil {
		return
	}
	if value == nil {
		ch <- []byte(`{"id":` + strconv.FormatInt(id, 10) + `,"error":{"code":-32000,"message":"probe unavailable"}}`)
		return
	}
	body, _ := json.Marshal(map[string]any{
		"id": id, "result": map[string]any{"result": map[string]any{"value": value}},
	})
	ch <- body
}

// answerCommit acks an insertText the way the renderer does once it has committed.
func (hs *secretHarness) answerCommit(cmd map[string]any) {
	id, ok := cmd["id"].(float64)
	if !ok {
		return
	}
	hs.h.mu.Lock()
	ch := hs.h.waiters[int64(id)]
	hs.h.mu.Unlock()
	if ch != nil {
		ch <- []byte(`{"id":` + strconv.FormatInt(int64(id), 10) + `,"result":{}}`)
	}
}

// secretFrame drives the proxy's own path - decode, then the secrets hook - so a
// test cannot pass through a prefilter production does not have.
func (hs *secretHarness) secretFrame(data []byte) ([]byte, bool) {
	msg, ok := decodeCDP(data)
	if !ok {
		return nil, false
	}
	return hs.h.handleSecretMsg(msg)
}

func (hs *secretHarness) send(t *testing.T, method string, params map[string]any, id int64) ([]byte, bool) {
	t.Helper()
	frame := map[string]any{cdpMethod: method, cdpParams: params}
	if id != 0 {
		frame[cdpID] = json.Number(strconv.FormatInt(id, 10))
	}
	return hs.secretFrame(mustJSON(t, frame))
}

func (hs *secretHarness) fill(t *testing.T, text string) ([]byte, bool) {
	t.Helper()
	return hs.send(t, methodInsertText, map[string]any{cdpText: text}, 7)
}

// errorText returns the message of the single CDP error answered to the driver.
func (hs *secretHarness) errorText(t *testing.T) string {
	t.Helper()
	if len(hs.answered) != 1 {
		t.Fatalf("answered %d frames, want exactly 1: %v", len(hs.answered), hs.answered)
	}
	e, ok := hs.answered[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("frame is not a CDP error: %v", hs.answered[0])
	}
	msg, _ := e["message"].(string)
	return msg
}

// typedFrames are the frames that carry input, ignoring the probes.
func (hs *secretHarness) typedFrames() []map[string]any {
	out := []map[string]any{}
	for _, m := range hs.injected {
		if strings.HasPrefix(m["method"].(string), "Input.") {
			out = append(out, m)
		}
	}
	return out
}

func storeWith(t *testing.T, name, value, source string) *secretStore {
	t.Helper()
	s := newSecretStore()
	s.put(testSeed, name, []byte(value), source, secretTTLDefault)
	return s
}

// expireNow fires an entry's TTL early, the way its own timer would. expire
// takes the generation it was armed for - so that a fired timer cannot clear the
// value a later put installed - and a test reaching the expired state deliberately
// wants whatever generation is current.
func (s *secretStore) expireNow(name string) {
	s.mu.Lock()
	var gen uint64
	if e := s.m[testSeed][name]; e != nil {
		gen = e.gen
	}
	s.mu.Unlock()
	s.expire(testSeed, name, gen)
}

func TestSentinelParsing(t *testing.T) {
	for text, want := range map[string]sentinelKind{
		"{{cuttle:GH_PASS}}":          sentinelWhole,
		"{{cuttle:A}}":                sentinelWhole,
		"hunter2":                     sentinelNone,
		"":                            sentinelNone,
		"Bearer {{cuttle:TOKEN}}":     sentinelEmbedded,
		"{{cuttle:A}}{{cuttle:B}}":    sentinelEmbedded,
		"{{cuttle:}}":                 sentinelEmbedded,
		"{{cuttle:has-dash}}":         sentinelEmbedded,
		"{{cuttle:GH_PASS}} trailing": sentinelEmbedded,
	} {
		if _, got := parseSentinel(text); got != want {
			t.Errorf("parseSentinel(%q) = %v, want %v", text, got, want)
		}
	}
	if name, _ := parseSentinel("{{cuttle:GH_PASS}}"); name != "GH_PASS" {
		t.Errorf("name = %q, want GH_PASS", name)
	}
}

// The three sentinel failures are three different fixes, so they must be three
// different errors - and none of them may type anything.
func TestSentinelFailsClosedWithATeachingError(t *testing.T) {
	t.Run("unknown name lists what exists", func(t *testing.T) {
		store := storeWith(t, "GH_PASS", "hunter2", sourceStdin)
		hs := newSecretHarness(t, store, true)
		if _, done := hs.fill(t, "{{cuttle:NOPE}}"); !done {
			t.Fatal("an unknown sentinel must be answered, never forwarded")
		}
		msg := hs.errorText(t)
		for _, want := range []string{"unknown secret", "cuttle secret set NOPE", "GH_PASS"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q does not mention %q", msg, want)
			}
		}
		if n := len(hs.typedFrames()); n != 0 {
			t.Fatalf("%d input frames reached the browser; want none", n)
		}
	})

	t.Run("stale exec secret names refresh", func(t *testing.T) {
		store := storeWith(t, "GH_TOTP", "123456", sourceExec)
		store.expireNow("GH_TOTP")
		hs := newSecretHarness(t, store, true)
		hs.fill(t, "{{cuttle:GH_TOTP}}")
		if msg := hs.errorText(t); !strings.Contains(msg, "cuttle secret refresh GH_TOTP") {
			t.Errorf("error %q must name the refresh verb", msg)
		}
	})

	t.Run("stale stdin secret does not name refresh", func(t *testing.T) {
		store := storeWith(t, "GH_PASS", "hunter2", sourceStdin)
		store.expireNow("GH_PASS")
		hs := newSecretHarness(t, store, true)
		hs.fill(t, "{{cuttle:GH_PASS}}")
		msg := hs.errorText(t)
		if strings.Contains(msg, "refresh") {
			t.Errorf("error %q names refresh, but there is no recipe to re-run", msg)
		}
		if !strings.Contains(msg, "cuttle secret set GH_PASS") {
			t.Errorf("error %q must say to set it again", msg)
		}
	})

	t.Run("embedded sentinel", func(t *testing.T) {
		store := storeWith(t, "TOKEN", "abc123", sourceStdin)
		hs := newSecretHarness(t, store, true)
		if _, done := hs.fill(t, "Bearer {{cuttle:TOKEN}}"); !done {
			t.Fatal("an embedded sentinel must be refused, not forwarded")
		}
		if msg := hs.errorText(t); !strings.Contains(msg, "WHOLE value") {
			t.Errorf("error %q must name the whole-string rule", msg)
		}
		if n := len(hs.typedFrames()); n != 0 {
			t.Fatalf("%d input frames reached the browser; want none", n)
		}
	})
}

// The load-bearing invariant. The value must reach Chrome - it is being typed -
// so the assertion is about every OTHER surface: nothing the driver sees, no
// error, no log line carries it, and no frame sent to the browser carries the
// un-substituted sentinel (which would prove substitution happened too late).
func TestTypedSecretNeverLeavesTheDaemon(t *testing.T) {
	const value = "hunter2"
	store := storeWith(t, "GH_PASS", value, sourceStdin)
	hs := newSecretHarness(t, store, true)
	hs.preflight = passwordInput()

	if _, done := hs.fill(t, "{{cuttle:GH_PASS}}"); !done {
		t.Fatal("a live sentinel must be handled here, not forwarded")
	}
	if len(hs.answered) != 1 {
		t.Fatalf("answered %d frames, want exactly 1", len(hs.answered))
	}
	if _, isErr := hs.answered[0]["error"]; isErr {
		t.Fatalf("want success, got %v", hs.answered[0])
	}
	for _, m := range hs.answered {
		blob := string(mustJSON(t, m))
		if strings.Contains(blob, value) || strings.Contains(blob, sentinelPrefix) {
			t.Fatalf("a client-bound frame carries the secret or the sentinel: %s", blob)
		}
	}
	for _, m := range hs.injected {
		if strings.Contains(string(mustJSON(t, m)), sentinelPrefix) {
			t.Fatalf("an injected frame carries the un-substituted sentinel: %v", m)
		}
	}
	if got := typedText(hs.typedFrames()); got != value {
		t.Fatalf("net typed text %q, want the secret", got)
	}
	if strings.Contains(hs.logs.String(), value) {
		t.Fatalf("the daemon log carries the value: %s", hs.logs.String())
	}
}

// With humanization off there is no keystroke path to use, so the frame itself
// carries the substituted value under the driver's own id - forwarded, not
// swallowed, so the browser answers the driver directly.
func TestSubstitutionRunsWithHumanizeOff(t *testing.T) {
	const value = "hunter2"
	store := storeWith(t, "GH_PASS", value, sourceStdin)
	hs := newSecretHarness(t, store, false)
	hs.preflight = passwordInput()

	out, done := hs.fill(t, "{{cuttle:GH_PASS}}")
	if done {
		t.Fatal("the raw path must forward the rewritten frame, not answer it")
	}
	var frame map[string]any
	if err := json.Unmarshal(out, &frame); err != nil {
		t.Fatalf("forwarded frame is not JSON: %v", err)
	}
	if frame[cdpMethod] != methodInsertText {
		t.Fatalf("method %v, want %s", frame[cdpMethod], methodInsertText)
	}
	if got := frame[cdpParams].(map[string]any)[cdpText]; got != value {
		t.Fatalf("forwarded text %q, want the substituted value", got)
	}
	if id, _ := frame[cdpID].(float64); int64(id) != 7 {
		t.Fatalf("forwarded id %v, want the driver's own 7", frame[cdpID])
	}
	if len(hs.answered) != 0 {
		t.Fatalf("the driver must be answered by the browser, not by cuttle: %v", hs.answered)
	}
	if n := len(hs.typedFrames()); n != 0 {
		t.Fatalf("the raw path must emit no keystrokes, got %d", n)
	}
}

// cuttle does not judge a literal, in any field, including a password box. The
// refusal that used to live here was cut: it was invisible on the default driver
// (playwright's fill retries a protocol error into a bare timeout), it never
// covered the per-character path two of three drivers use, and its field
// predicate fired on ordinary zip and date inputs. What must NOT come back is
// the probe it needed - an ordinary fill costs no isolated-world round trip.
func TestALiteralIsNeverRefusedAndNeverProbed(t *testing.T) {
	for name, attrs := range map[string]map[string]string{
		"password field":     {"type": "password", "name": "password"},
		"one-time-code":      {"autocomplete": "one-time-code", "name": "otp"},
		"govuk date part":    {"inputmode": "numeric", "name": "dob-day"},
		"braintree expiry":   {"inputmode": "numeric", "name": "expirationDate"},
		"uswds zip":          {"inputmode": "numeric", "name": "zip"},
		"shipping (not pin)": {"inputmode": "numeric", "name": "shipping"},
	} {
		t.Run(name, func(t *testing.T) {
			f := textInput()
			for k, v := range attrs {
				f[k] = v
			}
			hs := newSecretHarness(t, newSecretStore(), true)
			hs.preflight = f
			rewritten, done := hs.fill(t, "hunter2")
			if done {
				t.Fatalf("a literal was refused: %s", hs.errorText(t))
			}
			if rewritten != nil {
				t.Fatalf("the frame was rewritten: %s", rewritten)
			}
			if len(hs.probes) != 0 {
				t.Fatalf("an ordinary fill cost %d isolated-world probes; it must cost none", len(hs.probes))
			}
		})
	}
}

// A fill into a field that already holds text APPENDS - insertText inserts at the
// caret rather than replacing - so the page ends up with prefix+secret, a wrong
// credential every other channel reports as a clean success. The pre-flight knows
// the prior length before a single keystroke, so this costs nothing to say.
func TestAppendIntoANonEmptyFieldIsWarned(t *testing.T) {
	pre := passwordInput()
	pre["length"] = float64(3)
	hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2", sourceStdin), true)
	hs.preflight = pre
	hs.fill(t, "{{cuttle:GH_PASS}}")

	if _, isErr := hs.answered[0]["error"]; isErr {
		t.Fatalf("an append is not a failure: %v", hs.answered[0])
	}
	logs := hs.logs.String()
	if !strings.Contains(logs, "ALREADY held 3") {
		t.Errorf("the log must say the field was not empty, got: %s", logs)
	}
	if strings.Contains(logs, "hunter2") {
		t.Fatalf("the log carries the value: %s", logs)
	}
}

// A disabled or readonly input silently refuses focus, so a fill aimed at one
// lands with activeElement on <body>. Answering "focus the field first" to an
// agent that just did exactly that is a dead end; the probe RAN, so it can say so.
func TestNothingFocusedSaysWhy(t *testing.T) {
	hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2", sourceStdin), true)
	hs.preflight = map[string]any{"ok": false, "nofocus": true, "origin": "https://example.com"}
	if _, done := hs.fill(t, "{{cuttle:GH_PASS}}"); !done {
		t.Fatal("a sentinel with no focused target must be refused")
	}
	msg := hs.errorText(t)
	if !strings.Contains(msg, "disabled or readonly") {
		t.Errorf("error %q must explain that a disabled field refuses focus", msg)
	}
	if n := len(hs.typedFrames()); n != 0 {
		t.Fatalf("%d input frames reached the browser; want none", n)
	}
}

// playwright's fill retries a protocol error until its own timeout and reports
// only that timeout, dropping cuttle's text - so on the default driver the log is
// the only channel that survives. Every sentinel refusal has to reach it.
func TestEverySentinelRefusalIsLogged(t *testing.T) {
	for name, tc := range map[string]struct{ fill, want string }{
		"unknown":  {"{{cuttle:NOPE}}", "no secret of that name"},
		"embedded": {"Bearer {{cuttle:GH_PASS}}", "embedded in other text"},
	} {
		t.Run(name, func(t *testing.T) {
			hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2", sourceStdin), true)
			hs.preflight = passwordInput()
			hs.fill(t, tc.fill)
			if logs := hs.logs.String(); !strings.Contains(logs, tc.want) {
				t.Errorf("refusal not logged; want %q in: %s", tc.want, logs)
			}
		})
	}

	t.Run("expired", func(t *testing.T) {
		store := storeWith(t, "GH_TOTP", "123456", sourceExec)
		store.expireNow("GH_TOTP")
		hs := newSecretHarness(t, store, true)
		hs.preflight = passwordInput()
		hs.fill(t, "{{cuttle:GH_TOTP}}")
		logs := hs.logs.String()
		if !strings.Contains(logs, "its value expired") {
			t.Errorf("an expired secret is the likeliest failure and must be logged: %s", logs)
		}
		if strings.Contains(logs, "123456") {
			t.Fatalf("the log carries the value: %s", logs)
		}
	})
}

// An abandoned type is the only outcome that leaves part of a credential in a
// live field, so it is the one an operator most needs in the log - and on the
// default driver the CDP error does not survive the driver's retry loop, which
// makes the log line the whole record of it.
func TestAnAbandonedTypeIsLogged(t *testing.T) {
	hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2hunter2", sourceStdin), true)
	hs.preflight = passwordInput()
	// A budget no keystroke can fit in: typeHumanized checks the deadline before
	// the first character, so it abandons having typed nothing.
	hs.h.secretBudget = time.Nanosecond
	if _, done := hs.fill(t, "{{cuttle:GH_PASS}}"); !done {
		t.Fatal("an abandoned type must answer the driver")
	}
	if msg := hs.errorText(t); !strings.Contains(msg, "partial value") {
		t.Errorf("error %q must say the field holds a partial value", msg)
	}
	logs := hs.logs.String()
	if !strings.Contains(logs, "PARTIAL value") {
		t.Errorf("an abandoned type must reach the log, got: %s", logs)
	}
	if strings.Contains(logs, "hunter2") {
		t.Fatalf("the log carries the value: %s", logs)
	}
}

func TestOrdinaryFillIsUntouched(t *testing.T) {
	hs := newSecretHarness(t, newSecretStore(), true)
	rewritten, done := hs.fill(t, "an ordinary search query")
	if done {
		t.Fatal("a literal into a non-credential field must be forwarded")
	}
	// nil means "forward the original untouched" - the secrets path rewrites a
	// frame only when it has substituted a value into it.
	if rewritten != nil {
		t.Fatalf("the frame was rewritten: %s", rewritten)
	}
	if len(hs.answered) != 0 {
		t.Fatalf("nothing may be answered: %v", hs.answered)
	}
}

func TestPreflightRefusesUnusableTargets(t *testing.T) {
	cases := map[string]struct {
		target map[string]any
		want   string
	}{
		"no focused element": {map[string]any{"ok": false, "origin": "https://example.com"}, "could not be inspected"},
		"disabled": {func() map[string]any {
			f := passwordInput()
			f["disabled"] = true
			return f
		}(), "disabled"},
		"readonly": {func() map[string]any {
			f := passwordInput()
			f["readonly"] = true
			return f
		}(), "readonly"},
		"short maxlength": {func() map[string]any {
			f := passwordInput()
			f["maxLength"] = float64(3)
			return f
		}(), "maxlength"},
		"not editable": {func() map[string]any {
			f := textInput()
			f["tag"] = "div"
			f["editable"] = false
			return f
		}(), "not an editable field"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2", sourceStdin), true)
			hs.preflight = tc.target
			if _, done := hs.fill(t, "{{cuttle:GH_PASS}}"); !done {
				t.Fatal("the sentinel must be refused")
			}
			if msg := hs.errorText(t); !strings.Contains(strings.ToLower(msg), tc.want) {
				t.Errorf("error %q does not name %q", msg, tc.want)
			}
			if n := len(hs.typedFrames()); n != 0 {
				t.Fatalf("%d input frames reached the browser; want none", n)
			}
		})
	}
}

// An unavailable probe is fail-open for an ordinary fill (it is on every fill,
// and a page with no isolated world must stay usable) and fail-closed for a
// sentinel (the value would land in an unknown field).
func TestProbeUnavailableSplitsBySentinel(t *testing.T) {
	t.Run("sentinel refuses", func(t *testing.T) {
		hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2", sourceStdin), true)
		hs.preflight = nil
		if _, done := hs.fill(t, "{{cuttle:GH_PASS}}"); !done {
			t.Fatal("a sentinel with no target check must be refused")
		}
		if msg := hs.errorText(t); !strings.Contains(msg, "could not be inspected") {
			t.Errorf("error %q must say why", msg)
		}
	})
	t.Run("literal forwards", func(t *testing.T) {
		hs := newSecretHarness(t, newSecretStore(), true)
		hs.preflight = nil
		if _, done := hs.fill(t, "hunter2"); done {
			t.Fatal("an ordinary fill must still work on a page with no isolated world")
		}
		if len(hs.answered) != 0 {
			t.Fatalf("nothing may be answered: %v", hs.answered)
		}
	})
}

// Playwright's own fill sends the value as a callFunctionOn ARGUMENT before the
// insertText. Refusing on raw frame bytes would break the primary flow on frame
// one; only script text is scanned.
func TestRuntimeFramesScanScriptTextOnly(t *testing.T) {
	hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2", sourceStdin), true)
	_, done := hs.send(t, "Runtime.callFunctionOn", map[string]any{
		"functionDeclaration": "function(value){this.value=value}",
		"arguments":           []any{map[string]any{"value": "{{cuttle:GH_PASS}}"}},
		"objectId":            "1",
	}, 11)
	if done {
		t.Fatal("a sentinel in an ARGUMENT must be forwarded untouched")
	}
	if len(hs.answered) != 0 {
		t.Fatalf("nothing may be answered: %v", hs.answered)
	}

	for _, tc := range []struct{ method, field string }{
		{"Runtime.evaluate", "expression"},
		{"Runtime.callFunctionOn", "functionDeclaration"},
	} {
		hs := newSecretHarness(t, newSecretStore(), true)
		if _, done := hs.send(t, tc.method, map[string]any{tc.field: "el.value='{{cuttle:GH_PASS}}'"}, 12); !done {
			t.Fatalf("%s carrying a sentinel in %s must be refused", tc.method, tc.field)
		}
		if msg := hs.errorText(t); !strings.Contains(msg, "only be typed") {
			t.Errorf("error %q must say a secret cannot be evaluated", msg)
		}
	}
}

func TestCompositionRefusesASentinel(t *testing.T) {
	hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2", sourceStdin), true)
	if _, done := hs.send(t, methodIMEComposition, map[string]any{cdpText: "{{cuttle:GH_PASS}}"}, 3); !done {
		t.Fatal("a sentinel must never ride the composition path")
	}
	if msg := hs.errorText(t); !strings.Contains(msg, "IME composition") {
		t.Errorf("error %q must name the method", msg)
	}
	if n := len(hs.typedFrames()); n != 0 {
		t.Fatalf("%d input frames reached the browser; want none", n)
	}
}

func TestOriginBindingWarnsWithoutBlocking(t *testing.T) {
	const value = "hunter2"
	store := storeWith(t, "GH_PASS", value, sourceStdin)
	first := newSecretHarness(t, store, true)
	first.preflight = passwordInput()
	first.fill(t, "{{cuttle:GH_PASS}}")
	if got := store.list(testSeed)[0].Origin; got != "https://example.com" {
		t.Fatalf("origin recorded as %q, want the page it was first typed on", got)
	}

	second := newSecretHarness(t, store, true)
	second.preflight = passwordInput()
	second.preflight["origin"] = "https://evil.example"
	second.fill(t, "{{cuttle:GH_PASS}}")
	if _, isErr := second.answered[0]["error"]; isErr {
		t.Fatalf("an origin mismatch warns, it does not block: %v", second.answered[0])
	}
	log := second.logs.String()
	if !strings.Contains(log, "https://example.com") || !strings.Contains(log, "https://evil.example") {
		t.Errorf("the warning must name both origins: %s", log)
	}
}

// A secret is typed without the humanizer's injected typos: emitTypo corrects
// with a blind Backspace, which on a segmented OTP field lands in the next box.
func TestTypoSuppressionForSecrets(t *testing.T) {
	h := &humanizer{rng: newTestRNG(3)}
	for range 10000 {
		if h.shouldTypo("a", true) {
			t.Fatal("a secret must never take the typo path")
		}
	}
	fired := 0
	for range 10000 {
		if h.shouldTypo("a", false) {
			fired++
		}
	}
	if fired == 0 {
		t.Fatal("an ordinary type still fumbles occasionally, or this test proves nothing")
	}
}

// A TTL expiry that lands mid-type must not zero the buffer being typed: the
// interception copies the value out under the store mutex first.
func TestTTLExpiryDoesNotCorruptAnInFlightType(t *testing.T) {
	const value = "hunter2"
	store := storeWith(t, "GH_PASS", value, sourceStdin)
	copied, _, status := store.take(testSeed, "GH_PASS")
	if status != secretLive {
		t.Fatalf("status = %v, want live", status)
	}
	store.expireNow("GH_PASS")
	if string(copied) != value {
		t.Fatalf("the in-flight copy was zeroed with the store's buffer: %q", copied)
	}
	if _, _, status := store.take(testSeed, "GH_PASS"); status != secretStale {
		t.Fatalf("after expiry status = %v, want stale (the registration survives)", status)
	}
}

func TestStoreLifecycle(t *testing.T) {
	store := storeWith(t, "GH_PASS", "hunter2", sourceStdin)

	t.Run("expiry keeps the registration", func(t *testing.T) {
		store.expireNow("GH_PASS")
		info := store.list(testSeed)
		if len(info) != 1 || info[0].Live || info[0].TTLRemaining != 0 || info[0].Length != 0 {
			t.Fatalf("expired entry = %+v, want a live-less registration with no length", info)
		}
	})

	t.Run("list never carries a value", func(t *testing.T) {
		store.put(testSeed, "GH_PASS", []byte("hunter2"), sourceStdin, secretTTLDefault)
		blob := string(mustJSON(t, store.list(testSeed)))
		if strings.Contains(blob, "hunter2") {
			t.Fatalf("the list API leaked a value: %s", blob)
		}
	})

	t.Run("rm makes the name unknown again", func(t *testing.T) {
		if !store.remove(testSeed, "GH_PASS") {
			t.Fatal("remove reported nothing to remove")
		}
		if _, _, status := store.take(testSeed, "GH_PASS"); status != secretUnknown {
			t.Fatalf("status after rm = %v, want unknown", status)
		}
		if store.remove(testSeed, "GH_PASS") {
			t.Fatal("removing twice must report false")
		}
	})

	t.Run("seeds are isolated", func(t *testing.T) {
		store.put("other", "GH_PASS", []byte("hunter2"), sourceStdin, secretTTLDefault)
		if _, _, status := store.take(testSeed, "GH_PASS"); status != secretUnknown {
			t.Fatalf("a secret leaked across seeds: %v", status)
		}
	})
}

// With no isolated world the probe must not fall back to the page's MAIN world:
// the pre-flight stamps a marker there that a sign-in page could read, and a
// sentinel would be typed against a target nothing vouched for.
func TestProbeNeverFallsBackToTheMainWorld(t *testing.T) {
	hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2", sourceStdin), true)
	hs.withholdSetup = "*"

	if _, done := hs.fill(t, "{{cuttle:GH_PASS}}"); !done {
		t.Fatal("a sentinel with no isolated world must be refused")
	}
	if msg := hs.errorText(t); !strings.Contains(msg, "could not be inspected") {
		t.Errorf("error %q must say the target could not be checked", msg)
	}
	for _, m := range hs.injected {
		if m["method"] != "Runtime.evaluate" {
			continue
		}
		p, _ := m["params"].(map[string]any)
		if _, scoped := p["contextId"]; !scoped {
			t.Fatalf("a probe ran in the page's main world: %v", m)
		}
	}
}

// A reaped seed's browser and profile dir are gone; a value that outlived them
// would sit in daemon memory for the rest of its TTL, belonging to nothing.
func TestDropSeedForgetsEverything(t *testing.T) {
	store := storeWith(t, "GH_PASS", "hunter2000", sourceStdin)

	store.dropSeed(testSeed)
	if _, _, status := store.take(testSeed, "GH_PASS"); status != secretUnknown {
		t.Fatalf("status after dropSeed = %v, want unknown", status)
	}
	if got := maskWith(store, "hunter2000"); got != "hunter2000" {
		t.Fatalf("masked = %q - a dropped value must leave the masker too", got)
	}
}

// An over-max TTL clamps rather than resetting: silently turning `--ttl 24h`
// into 15 minutes looks like the value expired for no reason.
func TestPutClampsTheTTL(t *testing.T) {
	store := newSecretStore()
	if got := store.put(testSeed, "A", []byte("hunter2"), sourceStdin, 48*time.Hour); got != secretTTLMax {
		t.Fatalf("ttl = %s, want it clamped to %s", got, secretTTLMax)
	}
	if got := store.put(testSeed, "B", []byte("hunter2"), sourceStdin, 0); got != secretTTLDefault {
		t.Fatalf("ttl = %s, want the default %s", got, secretTTLDefault)
	}
}

// The stale-value error has to name a verb that can actually produce the value
// again, which depends entirely on where the last one came from.
func TestStaleSecretErrorNamesTheRightVerb(t *testing.T) {
	for source, want := range map[string]string{
		sourceExec:    "cuttle secret refresh GH_TOTP",
		sourcePrompt:  "cuttle secret prompt GH_TOTP",
		sourceStdin:   "cuttle secret set GH_TOTP --stdin",
		sourceCapture: "cuttle secret set GH_TOTP --stdin",
	} {
		if got := staleSecretError("GH_TOTP", source); !strings.Contains(got, want) {
			t.Errorf("source %q error %q does not name %q", source, got, want)
		}
	}
	// A value a human typed in cannot be re-set from a pipe, so that advice must
	// not be what a prompt secret gets.
	if got := staleSecretError("SMS", sourcePrompt); strings.Contains(got, "--stdin") {
		t.Errorf("a prompt secret must not be told to pipe one in: %q", got)
	}
}

// The pre-flight needs an isolated world, and a page that cannot host one makes
// cuttle wait out a CDP timeout to find that out. This pins what that costs,
// because the number is what makes the fill budget a budget: the whole sequence
// has to finish inside the driver's action timeout, or the driver gives up and
// retries the credential into the field.
//
// Measured here: one worldTimeout for the stage that goes unanswered, paid ONCE
// per CDP session - the "no world" answer is cached, so the next fill pays
// nothing - and the refusal lands well inside the budget with nothing typed.
func TestWorldSetupCostIsPaidOncePerSession(t *testing.T) {
	for name, withhold := range map[string]string{
		"no Page domain at all":     "Page.getFrameTree",
		"world creation unanswered": "Page.createIsolatedWorld",
	} {
		t.Run(name, func(t *testing.T) {
			hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2", sourceStdin), true)
			hs.withholdSetup = withhold

			start := time.Now()
			if _, done := hs.fill(t, "{{cuttle:GH_PASS}}"); !done {
				t.Fatal("a sentinel with no isolated world must be refused")
			}
			// The ceiling that matters is the world build's own draw, not the whole
			// fill budget: an assertion against 4.5s passes even when the world
			// build sits outside the budget entirely, which is how it escaped once.
			first := time.Since(start)
			if ceiling := secretWorldTimeout + secretProbeTimeout; first >= ceiling {
				t.Fatalf("the first fill took %s, over the probe's own ceiling of %s - the world build is not drawing on the fill deadline",
					first, ceiling)
			}
			if n := len(hs.typedFrames()); n != 0 {
				t.Fatalf("%d input frames reached the browser; want none", n)
			}

			start = time.Now()
			hs.fill(t, "{{cuttle:GH_PASS}}")
			if second := time.Since(start); second > worldTimeout/2 {
				t.Fatalf("the second fill took %s - the unavailable world must be cached, not re-probed", second)
			}
		})
	}
}

// Every teardown that leaves the daemon running has to take the seed's secrets
// with it, not just the idle reaper - the respawn path tears down a dead browser
// and removes the same profile dir. Both go through removeProcess or idleReap.
func TestRemoveProcessDropsTheSeedsSecrets(t *testing.T) {
	pool := newTestPool(t, serveConfig{}, (&fakeLauncher{port: 5100}).toLauncher())
	pool.secrets.put("s1", "GH_PASS", []byte("hunter2"), sourceStdin, secretTTLDefault)

	pool.removeProcess("s1")
	if _, _, status := pool.secrets.take("s1", "GH_PASS"); status != secretUnknown {
		t.Fatalf("status after removeProcess = %v, want unknown - the value outlived its browser", status)
	}
}

// A byte prefilter cannot be more precise than the JSON parser it guards. Chrome
// acts on a method name however it is spelled, so every spelling has to reach
// the switch that re-checks the decoded name.
func TestEscapedMethodNameIsStillIntercepted(t *testing.T) {
	store := storeWith(t, "GH_PASS", "hunter2", sourceStdin)
	hs := newSecretHarness(t, store, true)
	hs.preflight = passwordInput()

	// "Input.insertText" - legal JSON, and the same method to Chrome. A sentinel is
	// the observable: if the escape hides the frame from the secrets path, the
	// sentinel is forwarded and Chrome types `{{cuttle:GH_PASS}}` as literal text
	// into a live password field, which is the fail-open this feature exists to
	// prevent.
	frame := []byte(`{"id":9,"method":"Input.\u0069nsertText","params":{"text":"{{cuttle:GH_PASS}}"}}`)
	if _, done := hs.secretFrame(frame); !done {
		t.Fatal("an escaped method name hid the frame from the secrets path")
	}
	if got := typedText(hs.typedFrames()); got != "hunter2" {
		t.Fatalf("net typed text %q, want the substituted value", got)
	}
	for _, m := range hs.injected {
		if b, _ := json.Marshal(m); strings.Contains(string(b), sentinelPrefix) {
			t.Fatalf("an un-substituted sentinel reached the browser: %s", b)
		}
	}
}

// The pre-flat-session transport carries one command inside another as a JSON
// string. Nothing downstream decodes that payload, so a fill tunneled this way
// reached Chrome untouched - and a sentinel in it typed as its own literal text,
// which is the fail-open this whole feature exists to prevent.
func TestTunneledInputIsRefused(t *testing.T) {
	for name, nested := range map[string]string{
		"sentinel": `{\"id\":2,\"method\":\"Input.insertText\",\"params\":{\"text\":\"{{cuttle:GH_PASS}}\"}}`,
		"literal":  `{\"id\":2,\"method\":\"Input.insertText\",\"params\":{\"text\":\"hunter2\"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2", sourceStdin), true)
			frame := []byte(`{"id":1,"method":"Target.sendMessageToTarget","params":{"message":"` + nested + `","sessionId":"S1"}}`)
			if _, done := hs.secretFrame(frame); !done {
				t.Fatal("a tunneled input command was forwarded uninspected")
			}
			if msg := hs.errorText(t); !strings.Contains(msg, "flatten") {
				t.Errorf("error %q must tell the driver how to attach instead", msg)
			}
		})
	}

	// An unrelated tunneled command is none of this feature's business.
	hs := newSecretHarness(t, newSecretStore(), true)
	frame := []byte(`{"id":1,"method":"Target.sendMessageToTarget","params":{"message":"{\"id\":2,\"method\":\"Page.reload\"}","sessionId":"S1"}}`)
	if _, done := hs.secretFrame(frame); done {
		t.Fatal("a tunneled Page.reload must pass through")
	}
}

// CDP ids are integers by spec, but Chrome answers whatever JSON accepts with
// the same token it was sent. Parsing to int64 and refusing the rest meant a
// non-canonical id read as "no id at all" and skipped the refusal.
func TestNonCanonicalIDsStillGetRefused(t *testing.T) {
	for _, id := range []string{`7`, `7.0`, `7e0`, `"7"`, `12345678901234567890123`} {
		t.Run(id, func(t *testing.T) {
			hs := newSecretHarness(t, newSecretStore(), true)
			hs.preflight = passwordInput()
			// An unknown sentinel is the observable: it must be refused whatever
			// spelling the id arrived in, and the refusal answered under that exact id.
			frame := []byte(`{"id":` + id + `,"method":"Input.insertText","params":{"text":"{{cuttle:NOPE}}"}}`)
			if _, done := hs.secretFrame(frame); !done {
				t.Fatalf("id %s skipped the sentinel refusal", id)
			}
			// The reply must carry the id the driver sent, byte for byte, or the
			// driver cannot match it to its own pending command. Asserted on the
			// bytes: decoding and re-encoding turns 7.0 into 7 and loses an
			// oversized integer entirely, which is the fidelity at issue.
			if len(hs.answeredRaw) != 1 {
				t.Fatalf("answered %d frames, want 1", len(hs.answeredRaw))
			}
			if raw := string(hs.answeredRaw[0]); !strings.Contains(raw, `"id":`+id) {
				t.Errorf("reply %s does not echo the id %s verbatim", raw, id)
			}
		})
	}
}

// A world retired under a fill must REFUSE, not fall back to the page's main
// world. queryWithin rebuilds and, when that fails, evaluates with no contextId
// at all - which asks the PAGE for the disabled/readonly/maxlength answers the
// refusal is built on, so a hostile page could vouch for its own field.
func TestRetiredWorldRefusesRatherThanAskingThePage(t *testing.T) {
	hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2", sourceStdin), true)
	hs.preflight = passwordInput()
	hs.failProbes = 1         // the world went with the document
	hs.withholdSetupAfter = 2 // ... and the rebuild never completes

	if _, done := hs.fill(t, "{{cuttle:GH_PASS}}"); !done {
		t.Fatal("the fill must be handled, not forwarded")
	}
	if msg := hs.errorText(t); !strings.Contains(msg, "Nothing was typed") {
		t.Fatalf("want a refusal that typed nothing, got %q", msg)
	}
	if n := len(hs.typedFrames()); n != 0 {
		t.Fatalf("%d input frames reached the browser; want none", n)
	}
	for i, ctx := range hs.probeCtx {
		if ctx == nil {
			t.Fatalf("probe %d was sent with no contextId - the page answered it", i)
		}
	}
}

// The tail insertText carries everything past secretMaxRunes, so most real
// credentials ride it. If it never commits the field holds only the head, and
// answering ok reports a wrong credential as a clean success.
func TestUncommittedTailIsNotReportedAsSuccess(t *testing.T) {
	const value = "correct-horse-battery" // longer than secretMaxRunes
	store := storeWith(t, "GH_PASS", value, sourceStdin)
	hs := newSecretHarness(t, store, true)
	hs.preflight = passwordInput()
	hs.withholdCommit = true

	if _, done := hs.fill(t, "{{cuttle:GH_PASS}}"); !done {
		t.Fatal("the fill must be handled, not forwarded")
	}
	msg := hs.errorText(t)
	if !strings.Contains(msg, "partial value") {
		t.Fatalf("want a partial-value refusal, got %q", msg)
	}
	if strings.Contains(hs.logs.String(), "typed GH_PASS ("+strconv.Itoa(len(value))+" characters)") {
		t.Error("the audit line claimed the whole value landed")
	}
	// And the origin must not be bound to a page the credential never fully reached.
	if got := store.list(testSeed)[0].Origin; got != "" {
		t.Errorf("origin recorded as %q after a partial fill; want it unbound", got)
	}
}

// The positive half of the same path: a value longer than the keystroke head is
// typed as head + tail and answered ok once the tail has committed.
func TestLongSecretCommitsItsTail(t *testing.T) {
	const value = "correct-horse-battery"
	hs := newSecretHarness(t, storeWith(t, "GH_PASS", value, sourceStdin), true)
	hs.preflight = passwordInput()

	if _, done := hs.fill(t, "{{cuttle:GH_PASS}}"); !done {
		t.Fatal("the fill must be handled, not forwarded")
	}
	if _, isErr := hs.answered[0]["error"]; isErr {
		t.Fatalf("want success, got %v", hs.answered[0])
	}
	if got := typedText(hs.typedFrames()); got != value {
		t.Fatalf("net typed text %q, want the whole value", got)
	}
}
