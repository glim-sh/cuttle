package serve

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// A. The prefilter needles are byte-level, the protocol is JSON.
// ---------------------------------------------------------------------------

// raw sends a hand-built frame straight at the two client-side hooks in the
// order wsproxy's preprocessClient runs them, and reports whether the frame
// would be forwarded to Chrome untouched.
func (hs *secretHarness) raw(frame string) (forwardedVerbatim bool) {
	out, handled := hs.h.handleSecretFrame([]byte(frame))
	if handled {
		return false
	}
	if hs.h.enabled && hs.h.handleClientFrame(out) {
		return false
	}
	return string(out) == frame
}

func TestRedTeamEscapedMethodSkipsEveryGuardrail(t *testing.T) {
	t.Run("sentinel typed literally into a password field", func(t *testing.T) {
		hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2", sourceStdin), true)
		hs.preflight = passwordInput()
		// "Input.insertText" unescapes to "Input.insertText"; the raw bytes
		// contain neither `"Input.i` nor `{{cuttle:`.
		frame := `{"id":7,"method":"Input.insertText","params":{"text":"{{cuttle:GH_PASS}}"}}`
		if !hs.raw(frame) {
			t.Fatal("the escaped frame was intercepted after all")
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(frame), &m); err != nil {
			t.Fatalf("frame is not valid JSON: %v", err)
		}
		if m["method"] != methodInsertText {
			t.Fatalf("method decodes to %q", m["method"])
		}
		if got := m["params"].(map[string]any)["text"]; got != "{{cuttle:GH_PASS}}" {
			t.Fatalf("text decodes to %q", got)
		}
		t.Logf("FORWARDED VERBATIM: %s", frame)
	})

	t.Run("literal credential typed with no refusal", func(t *testing.T) {
		hs := newSecretHarness(t, newSecretStore(), true)
		hs.preflight = passwordInput()
		frame := `{"id":7,"method":"Input.insertText","params":{"text":"hunter2"}}`
		if !hs.raw(frame) {
			t.Fatal("the escaped frame was intercepted after all")
		}
		if len(hs.answered) != 0 {
			t.Fatalf("something was answered: %v", hs.answered)
		}
		t.Logf("FORWARDED VERBATIM into a password field: %s", frame)
	})

	t.Run("Runtime.evaluate sentinel refusal skipped", func(t *testing.T) {
		hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2", sourceStdin), true)
		frame := `{"id":9,"method":"Runtime.evaluate","params":{"expression":"e.value='{{cuttle:GH_PASS}}'"}}`
		if !hs.raw(frame) {
			t.Fatal("the escaped expression was refused after all")
		}
		t.Logf("FORWARDED VERBATIM: %s", frame)
	})
}

func TestRedTeamSendMessageToTargetTunnelsAnInsertText(t *testing.T) {
	hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2", sourceStdin), true)
	hs.preflight = passwordInput()
	inner, _ := json.Marshal(map[string]any{
		"id": 2, "method": methodInsertText,
		"params": map[string]any{"text": "{{cuttle:GH_PASS}}"},
	})
	outer, _ := json.Marshal(map[string]any{
		"id": 1, "method": "Target.sendMessageToTarget",
		"params": map[string]any{"sessionId": "S1", "message": string(inner)},
	})
	if !hs.raw(string(outer)) {
		t.Fatal("the tunneled frame was intercepted")
	}
	t.Logf("FORWARDED VERBATIM (prefilter matched, method switch did not): %s", outer)
}

// ---------------------------------------------------------------------------
// B. asInt only accepts json.Number integers, and !hasID is a forward.
// ---------------------------------------------------------------------------

func TestRedTeamNonIntegerIDForwardsALiteralIntoACredentialField(t *testing.T) {
	for _, id := range []string{"7.0", "7e0", "\"7\"", "12345678901234567890123"} {
		t.Run(id, func(t *testing.T) {
			hs := newSecretHarness(t, newSecretStore(), true)
			hs.preflight = passwordInput()
			frame := `{"id":` + id + `,"method":"Input.insertText","params":{"text":"hunter2"}}`
			out, handled := hs.h.handleSecretFrame([]byte(frame))
			if handled {
				t.Fatalf("refused after all: %v", hs.answered)
			}
			if hs.h.handleClientFrame(out) {
				t.Fatal("the humanizer swallowed it")
			}
			if string(out) != frame {
				t.Fatalf("rewritten: %s", out)
			}
			t.Logf("FORWARDED VERBATIM into a password field: %s", frame)
		})
	}
}

func TestRedTeamNoIDSentinelIsDroppedSilently(t *testing.T) {
	hs := newSecretHarness(t, storeWith(t, "GH_PASS", "hunter2", sourceStdin), true)
	hs.preflight = passwordInput()
	out, handled := hs.h.handleSecretFrame(
		[]byte(`{"method":"Input.insertText","params":{"text":"{{cuttle:GH_PASS}}"}}`))
	if !handled || out != nil {
		t.Fatalf("out=%s handled=%v", out, handled)
	}
	if len(hs.answered) != 0 {
		t.Fatalf("answered: %v", hs.answered)
	}
	t.Logf("dropped with no answer; log: %s", strings.TrimSpace(hs.logs.String()))
}

// ---------------------------------------------------------------------------
// C. h.composed as a credential-refusal bypass.
// ---------------------------------------------------------------------------

func TestRedTeamComposedStringBypassesTheCredentialRefusal(t *testing.T) {
	hs := newSecretHarness(t, newSecretStore(), true)
	// Stage 1: a composition into a harmless field. handleComposition does NO
	// pre-flight, so the target is never inspected; it just records the text.
	hs.preflight = textInput()
	comp := mustJSON(t, map[string]any{
		cdpID: json.Number("1"), cdpMethod: methodIMEComposition,
		cdpParams: map[string]any{cdpText: "hunter2"},
	})
	out, done := hs.h.handleSecretFrame(comp)
	if done {
		t.Fatal("the composition was refused")
	}
	if !hs.h.handleClientFrame(out) {
		t.Fatal("the composition was not typed out")
	}
	if hs.h.composed != "hunter2" {
		t.Fatalf("composed = %q", hs.h.composed)
	}

	// Stage 2: focus is now a password field, and the driver sends the "commit"
	// WITHOUT an id. secretInsertText short-circuits on text == h.composed before
	// any credential check; handleInsertText forwards anything with no id.
	hs.preflight = passwordInput()
	frame := `{"method":"Input.insertText","params":{"text":"hunter2"}}`
	out, done = hs.h.handleSecretFrame([]byte(frame))
	if done {
		t.Fatalf("refused after all: %v", hs.answered)
	}
	if hs.h.handleClientFrame(out) {
		t.Fatal("the humanizer swallowed it")
	}
	if string(out) != frame {
		t.Fatalf("rewritten: %s", out)
	}
	t.Logf("FORWARDED VERBATIM into a password field via h.composed: %s", frame)
}

// ---------------------------------------------------------------------------
// D. parseSentinel under adversarial text.
// ---------------------------------------------------------------------------

func TestRedTeamSentinelParserNeverOpens(t *testing.T) {
	long := strings.Repeat("A", 64)   // 64 chars: the max validSecretName allows
	tooLong := strings.Repeat("A", 65)
	cases := []string{
		"{{cuttle:}}",
		"{{cuttle:{{cuttle:X}}}}",
		"{{cuttle:X}}}}",
		"{{cuttle:X}}\n",
		"{{cuttle:X}}\x00",
		"{{cuttle:X}} ",
		" {{cuttle:X}}",
		"{{cuttle:X​}}",
		"{{cuttle:İ}}", // dotted capital I
		"{{cuttle:X}}{{cuttle:X}}",
		"{{cuttle:" + tooLong + "}}",
		"{{cuttle:X",
		"cuttle:X}}",
		"{{cuttle:X}}{{",
		"{{{{cuttle:X}}}}",
	}
	for _, text := range cases {
		name, kind := parseSentinel(text)
		if kind == sentinelWhole {
			t.Errorf("OPENED: parseSentinel(%q) = (%q, whole)", text, name)
		}
	}
	if name, kind := parseSentinel("{{cuttle:" + long + "}}"); kind != sentinelWhole || name != long {
		t.Errorf("a 64-char name should still resolve, got (%q,%v)", name, kind)
	}
	// A sentinel that only exists after JSON unescaping is not seen at all.
	var m map[string]any
	_ = json.Unmarshal([]byte(`{"text":"{{cuttle:X}}"}`), &m)
	if _, kind := parseSentinel(m["text"].(string)); kind != sentinelEmbedded {
		t.Logf("post-unescape kind = %v", kind)
	}
}

// ---------------------------------------------------------------------------
// E. Masking.
// ---------------------------------------------------------------------------

func TestRedTeamMaskingHoles(t *testing.T) {
	t.Run("short value never masked", func(t *testing.T) {
		store := newSecretStore()
		store.put(testSeed, "PIN", []byte("abc"), sourceStdin, secretTTLDefault)
		store.put(testSeed, "OTP", []byte("12345"), sourceStdin, secretTTLDefault)
		for _, v := range []string{"abc", "12345"} {
			if got := maskWith(store, "value is "+v); strings.Contains(got, v) {
				t.Logf("UNMASKED (documented floor): %q -> %q", v, got)
			} else {
				t.Errorf("unexpectedly masked %q", v)
			}
		}
	})

	t.Run("stale value stops being masked", func(t *testing.T) {
		store := storeWith(t, "GH_PASS", "hunter2000", sourceStdin)
		if got := maskWith(store, "hunter2000"); got == "hunter2000" {
			t.Fatal("a live value was not masked")
		}
		store.expire(testSeed, "GH_PASS")
		if got := maskWith(store, "hunter2000"); got != "hunter2000" {
			t.Fatalf("still masked: %q", got)
		}
		t.Log("after TTL expiry the value is no longer redacted from cuttle-authored text")
	})

	t.Run("encodings the variant list does not cover", func(t *testing.T) {
		store := storeWith(t, "GH_PASS", "hunter2000", sourceStdin)
		for label, enc := range map[string]string{
			"base64url":        "aHVudGVyMjAwMA",     // no padding
			"unicode-escaped":  `hunter2000`,
			"percent-uppercase": "hunter2000",
		} {
			if got := maskWith(store, enc); strings.Contains(got, "<secret") {
				t.Logf("%s masked", label)
			} else {
				t.Logf("UNMASKED %s: %q", label, got)
			}
		}
	})

	t.Run("non-string log attr is not masked", func(t *testing.T) {
		store := storeWith(t, "GH_PASS", "hunter2000", sourceStdin)
		logMaskStore.Store(store)
		t.Cleanup(func() { logMaskStore.Store(nil) })
		if got := maskAttr(slogAny("v", []byte("hunter2000"))); strings.Contains(logLine(got), "hunter2000") {
			t.Logf("UNMASKED attr: %s", logLine(got))
		}
	})
}

func TestRedTeamMaskingSubstringOrdering(t *testing.T) {
	store := newSecretStore()
	store.put(testSeed, "SHORT", []byte("passw"), sourceStdin, secretTTLDefault)
	store.put(testSeed, "LONG", []byte("password12"), sourceStdin, secretTTLDefault)
	got := maskWith(store, "password12")
	if strings.Contains(got, "ord12") || strings.Contains(got, "passw") {
		t.Errorf("partial leak: %q", got)
	}
	t.Logf("masked = %q", got)
}

// ---------------------------------------------------------------------------
// F. allow-literal races and lifecycle.
// ---------------------------------------------------------------------------

func TestRedTeamConcurrentFillsRaceOneToken(t *testing.T) {
	for range 50 {
		store := newSecretStore()
		store.armLiteral(testSeed, time.Minute)
		var wg sync.WaitGroup
		allowed := make(chan bool, 8)
		for range 8 {
			wg.Go(func() { allowed <- store.takeLiteral(testSeed) })
		}
		wg.Wait()
		close(allowed)
		n := 0
		for ok := range allowed {
			if ok {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%d fills were allowed by one token", n)
		}
	}
}

func TestRedTeamSecretSurvivesAReplacedBrowser(t *testing.T) {
	pool := newTestPool(t, serveConfig{}, (&fakeLauncher{port: 5300}).toLauncher())
	pool.secrets.put("s1", "GH_PASS", []byte("hunter2"), sourceStdin, secretTTLDefault)
	pool.secrets.armLiteral("s1", time.Minute)
	// Nothing here reaps: only removeProcess/idleReap call dropSeed.
	if _, _, st := pool.secrets.take("s1", "GH_PASS"); st != secretLive {
		t.Fatalf("status = %v", st)
	}
	t.Log("a secret only dies via removeProcess/idleReap")
}

// ---------------------------------------------------------------------------
// G. Injected-id collision: the driver picks an id in the humanizer's range.
// ---------------------------------------------------------------------------

func TestRedTeamDriverIDCollisionSwallowsItsOwnResponse(t *testing.T) {
	hs := newSecretHarness(t, newSecretStore(), true)
	id := hs.h.allocID() // the id the humanizer would use for an injected command
	resp := []byte(`{"id":` + strconv.FormatInt(id, 10) + `,"result":{"root":{"nodeId":1}}}`)
	if !hs.h.maybeSwallow(resp) {
		t.Fatal("not swallowed")
	}
	t.Logf("a driver command with id %d has its response eaten by maybeSwallow", id)
}
