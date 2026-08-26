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
	h        *humanizer
	injected []map[string]any // frames the browser received
	answered []map[string]any // frames the driver received
	probes   []string         // probe expressions evaluated
	logs     bytes.Buffer

	// preflight and verify are the probe answers; nil means the probe could not
	// run (an evaluate error, exactly like a page with no isolated world).
	preflight map[string]any
	verify    map[string]any
	// noWorld makes isolated-world creation fail, which is a real state: a target
	// with no Page domain, or a detached one.
	noWorld bool
}

const testSeed = "S"

// textInput is the ordinary, unremarkable fill target: an editable text box with
// no length cap. Individual tests copy it and change the one field under test.
func textInput() map[string]any {
	return map[string]any{
		"ok": true, "token": float64(1), "tag": "input", "type": "text",
		"disabled": false, "readonly": false, "editable": true, "maxLength": float64(-1),
		"autocomplete": "", "inputmode": "", "origin": "https://example.com",
	}
}

func passwordInput() map[string]any {
	t := textInput()
	t["type"] = "password"
	return t
}

func verifiedAs(length int) map[string]any {
	return map[string]any{
		"ok": true, "same": true, "token": float64(1),
		"length": float64(length), "origin": "https://example.com",
	}
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
		return nil
	}
	hs.h.clientSend = func(_ websocket.MessageType, data []byte) error {
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		hs.answered = append(hs.answered, m)
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
	if hs.noWorld {
		return
	}
	switch cmd["method"] {
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
	value := hs.preflight
	if strings.Contains(expr, "same:") {
		value = hs.verify
	}
	id := int64(cmd["id"].(float64))
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

func (hs *secretHarness) send(t *testing.T, method string, params map[string]any, id int64) ([]byte, bool) {
	t.Helper()
	frame := map[string]any{cdpMethod: method, cdpParams: params}
	if id != 0 {
		frame[cdpID] = json.Number(strconv.FormatInt(id, 10))
	}
	return hs.h.handleSecretFrame(mustJSON(t, frame))
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
		store.expire(testSeed, "GH_TOTP")
		hs := newSecretHarness(t, store, true)
		hs.fill(t, "{{cuttle:GH_TOTP}}")
		if msg := hs.errorText(t); !strings.Contains(msg, "cuttle secret refresh GH_TOTP") {
			t.Errorf("error %q must name the refresh verb", msg)
		}
	})

	t.Run("stale stdin secret does not name refresh", func(t *testing.T) {
		store := storeWith(t, "GH_PASS", "hunter2", sourceStdin)
		store.expire(testSeed, "GH_PASS")
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
	hs.verify = verifiedAs(len(value))

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

func TestLiteralIntoACredentialFieldIsRefused(t *testing.T) {
	fields := map[string]map[string]any{
		"password": passwordInput(),
		"one-time-code": func() map[string]any {
			f := textInput()
			f["autocomplete"] = "one-time-code"
			return f
		}(),
		"numeric inputmode": func() map[string]any {
			f := textInput()
			f["inputmode"] = "numeric"
			return f
		}(),
	}
	for name, target := range fields {
		for _, humanize := range []bool{true, false} {
			t.Run(name+"/humanize="+strconv.FormatBool(humanize), func(t *testing.T) {
				hs := newSecretHarness(t, newSecretStore(), humanize)
				hs.preflight = target
				if _, done := hs.fill(t, "hunter2"); !done {
					t.Fatal("a literal into a credential field must be refused in both modes")
				}
				msg := hs.errorText(t)
				head := msg
				if len(head) > 80 {
					head = head[:80]
				}
				if !strings.Contains(head, sentinelPrefix) && !strings.Contains(head, "cuttle secret allow-literal") {
					t.Errorf("the first 80 chars must carry the action, got %q", head)
				}
				if n := len(hs.typedFrames()); n != 0 {
					t.Fatalf("%d input frames reached the browser; want none", n)
				}
			})
		}
	}
}

func TestOrdinaryFillIsUntouched(t *testing.T) {
	hs := newSecretHarness(t, newSecretStore(), true)
	data, done := hs.fill(t, "an ordinary search query")
	if done {
		t.Fatal("a literal into a non-credential field must be forwarded")
	}
	if len(hs.answered) != 0 {
		t.Fatalf("nothing may be answered: %v", hs.answered)
	}
	var frame map[string]any
	_ = json.Unmarshal(data, &frame)
	if frame[cdpParams].(map[string]any)[cdpText] != "an ordinary search query" {
		t.Fatalf("the forwarded frame was rewritten: %v", frame)
	}
}

func TestAllowLiteralIsSingleUseAndOnlyForRefusableFills(t *testing.T) {
	store := newSecretStore()
	store.armLiteral(testSeed, allowLiteralTTL)

	// A fill that was never going to be refused must not eat the exemption.
	plain := newSecretHarness(t, store, true)
	if _, done := plain.fill(t, "not a credential"); done {
		t.Fatal("an ordinary fill must be forwarded")
	}
	if !store.literalArmed(testSeed) {
		t.Fatal("an ordinary fill consumed the exemption")
	}

	first := newSecretHarness(t, store, true)
	first.preflight = passwordInput()
	if _, done := first.fill(t, "hunter2"); done {
		t.Fatal("the armed exemption must let this fill through")
	}
	if store.literalArmed(testSeed) {
		t.Fatal("the exemption must be consumed by the fill it allowed")
	}
	if !strings.Contains(first.logs.String(), "allow-literal") {
		t.Errorf("consuming the exemption must be logged: %s", first.logs.String())
	}

	second := newSecretHarness(t, store, true)
	second.preflight = passwordInput()
	if _, done := second.fill(t, "hunter2"); !done {
		t.Fatal("the next credential fill must be refused again")
	}
}

func TestAllowLiteralExpiresOnItsOwn(t *testing.T) {
	store := newSecretStore()
	store.armLiteral(testSeed, 20*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for store.literalArmed(testSeed) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if store.literalArmed(testSeed) {
		t.Fatal("an armed-and-forgotten exemption must expire")
	}
	hs := newSecretHarness(t, store, true)
	hs.preflight = passwordInput()
	if _, done := hs.fill(t, "hunter2"); !done {
		t.Fatal("after expiry the refusal is armed again")
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

func TestPostTypeVerification(t *testing.T) {
	const value = "hunter2"
	t.Run("length mismatch reports without retyping", func(t *testing.T) {
		hs := newSecretHarness(t, storeWith(t, "GH_PASS", value, sourceStdin), true)
		hs.preflight = passwordInput()
		hs.verify = verifiedAs(3)
		hs.fill(t, "{{cuttle:GH_PASS}}")
		msg := hs.errorText(t)
		if !strings.Contains(msg, "7") || !strings.Contains(msg, "3") {
			t.Errorf("error %q must name both lengths", msg)
		}
		if got := typedText(hs.typedFrames()); got != value {
			t.Fatalf("net typed text %q - the value must be typed exactly once, never retyped", got)
		}
	})

	t.Run("focus left the field is success", func(t *testing.T) {
		hs := newSecretHarness(t, storeWith(t, "GH_TOTP", "123456", sourceStdin), true)
		hs.preflight = passwordInput()
		hs.verify = map[string]any{"ok": true, "same": false, "token": float64(9), "length": float64(1)}
		hs.fill(t, "{{cuttle:GH_TOTP}}")
		if len(hs.answered) != 1 {
			t.Fatalf("answered %d frames, want 1", len(hs.answered))
		}
		if _, isErr := hs.answered[0]["error"]; isErr {
			t.Fatalf("OTP auto-advance is normal, not an error: %v", hs.answered[0])
		}
		if got := typedText(hs.typedFrames()); got != "123456" {
			t.Fatalf("net typed text %q, want it typed once", got)
		}
	})

	t.Run("unverifiable is success", func(t *testing.T) {
		hs := newSecretHarness(t, storeWith(t, "GH_PASS", value, sourceStdin), true)
		hs.preflight = passwordInput()
		hs.verify = nil
		hs.fill(t, "{{cuttle:GH_PASS}}")
		if _, isErr := hs.answered[0]["error"]; isErr {
			t.Fatalf("a probe that cannot run must not fail the type: %v", hs.answered[0])
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
	first.verify = verifiedAs(len(value))
	first.fill(t, "{{cuttle:GH_PASS}}")
	if got := store.list(testSeed)[0].Origin; got != "https://example.com" {
		t.Fatalf("origin recorded as %q, want the page it was first typed on", got)
	}

	second := newSecretHarness(t, store, true)
	second.preflight = passwordInput()
	second.preflight["origin"] = "https://evil.example"
	second.verify = verifiedAs(len(value))
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
	store.expire(testSeed, "GH_PASS")
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
		store.expire(testSeed, "GH_PASS")
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

// A composition places the whole value in one call and the driver commits it
// with an insertText carrying the same text. That commit is not a fresh fill:
// judging it would answer "Nothing was typed" about a value that was typed, and
// burn an armed exemption on a fill that already happened.
func TestCompositionCommitIsNotJudgedAsALiteralFill(t *testing.T) {
	store := newSecretStore()
	store.armLiteral(testSeed, allowLiteralTTL)
	hs := newSecretHarness(t, store, true)
	hs.preflight = passwordInput()

	composition := mustJSON(t, map[string]any{
		cdpID: json.Number("1"), cdpMethod: methodIMEComposition,
		cdpParams: map[string]any{cdpText: "hunter2"},
	})
	if out, done := hs.h.handleSecretFrame(composition); done {
		t.Fatal("a composition with no sentinel must reach the humanizer")
	} else if !hs.h.handleClientFrame(out) {
		t.Fatal("expected the composition to be typed out")
	}

	commit, _ := insertTextFrame(2, "hunter2")
	data, done := hs.h.handleSecretFrame(mustJSON(t, commit))
	if done {
		t.Fatalf("the commit must not be refused - the value is already in the field: %v", hs.answered)
	}
	if !store.literalArmed(testSeed) {
		t.Fatal("the commit consumed the allow-literal exemption meant for the NEXT fill")
	}
	if !hs.h.handleClientFrame(data) {
		t.Fatal("the commit must be answered by the humanizer, not typed again")
	}
	if got := typedText(hs.typedFrames()); got != "hunter2" {
		t.Fatalf("net typed text %q, want the value exactly once", got)
	}
}

// With no isolated world the probe must not fall back to the page's MAIN world:
// the pre-flight stamps a marker there that a sign-in page could read, and a
// sentinel would be typed against a target nothing vouched for.
func TestProbeNeverFallsBackToTheMainWorld(t *testing.T) {
	hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2", sourceStdin), true)
	hs.noWorld = true

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
	store.armLiteral(testSeed, allowLiteralTTL)

	store.dropSeed(testSeed)
	if _, _, status := store.take(testSeed, "GH_PASS"); status != secretUnknown {
		t.Fatalf("status after dropSeed = %v, want unknown", status)
	}
	if store.literalArmed(testSeed) {
		t.Fatal("the seed's armed exemption survived its browser")
	}
	if got := maskWith(store, "hunter2000"); got != "hunter2000" {
		t.Fatalf("masked = %q - a dropped value must leave the masker too", got)
	}
}

// An over-max TTL clamps rather than resetting: silently turning `--ttl 24h`
// into 15 minutes looks like the value expired for no reason.
func TestPutClampsTheTTL(t *testing.T) {
	store := newSecretStore()
	store.put(testSeed, "A", []byte("hunter2"), sourceStdin, 48*time.Hour)
	if got := store.ttlOf(testSeed, "A"); got != secretTTLMax {
		t.Fatalf("ttl = %s, want it clamped to %s", got, secretTTLMax)
	}
	store.put(testSeed, "B", []byte("hunter2"), sourceStdin, 0)
	if got := store.ttlOf(testSeed, "B"); got != secretTTLDefault {
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

// The deadline is the gate, not the map entry: the expiry timer and a consuming
// fill both take the mutex, and the timer does not always get there first.
func TestExpiredLiteralTokenIsNotConsumable(t *testing.T) {
	store := newSecretStore()
	store.armLiteral(testSeed, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if store.takeLiteral(testSeed) {
		t.Fatal("an expired exemption was consumed as if armed")
	}
}

// Re-arming must not schedule its own disarm: the previous token's timer firing
// later would otherwise delete the replacement.
func TestReArmingLiteralSurvivesTheOldTimer(t *testing.T) {
	store := newSecretStore()
	store.armLiteral(testSeed, 10*time.Millisecond)
	store.armLiteral(testSeed, time.Minute)
	time.Sleep(40 * time.Millisecond)
	if !store.literalArmed(testSeed) {
		t.Fatal("the replaced token's timer disarmed the new one")
	}
}
