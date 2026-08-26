package serve

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// recordingHumanizer returns a humanizer whose injected commands are captured.
func recordingHumanizer(seed uint64, got *[]map[string]any) *humanizer {
	return &humanizer{
		ctx: context.Background(),
		rng: newTestRNG(seed),
		cdpSend: func(_ websocket.MessageType, data []byte) error {
			var m map[string]any
			_ = json.Unmarshal(data, &m)
			*got = append(*got, m)
			return nil
		},
		nextID:  humanizeIDBase,
		pending: map[int64]struct{}{},
	}
}

func TestEmitTypoInjectsWrongKeyThenBackspace(t *testing.T) {
	var got []map[string]any
	h := recordingHumanizer(3, &got)
	h.emitTypo("a", "SID")

	if len(got) != 4 {
		t.Fatalf("emitTypo injected %d key events, want 4 (wrong down/up + backspace down/up)", len(got))
	}
	for i, m := range got {
		if m["method"] != "Input.dispatchKeyEvent" {
			t.Fatalf("event %d is %v, want Input.dispatchKeyEvent", i, m["method"])
		}
		if id, _ := m["id"].(float64); int64(id) < humanizeIDBase {
			t.Fatalf("event %d has non-injected id %v", i, m["id"])
		}
		if m["sessionId"] != "SID" {
			t.Fatalf("event %d dropped sessionId: %v", i, m["sessionId"])
		}
	}
	// The wrong key must be a QWERTY neighbor of 'a'.
	first := got[0]["params"].(map[string]any)
	if w, _ := first["text"].(string); !charIn("qwsz", w) {
		t.Fatalf("typo key %q is not adjacent to 'a'", w)
	}
	// The correction must be Backspace (last two events).
	for _, m := range got[2:] {
		p := m["params"].(map[string]any)
		if p["key"] != "Backspace" {
			t.Fatalf("correction key %v, want Backspace", p["key"])
		}
	}
}

func TestAdjacentKeyAndTypoable(t *testing.T) {
	rng := newTestRNG(1)
	if !isTypoable("a") {
		t.Fatal("'a' should be typoable")
	}
	for _, bad := range []string{"1", "ab", "", " ", "."} {
		if isTypoable(bad) {
			t.Fatalf("%q should not be typoable", bad)
		}
	}
	if g := adjacentKey(rng, "a"); !charIn("qwsz", g) {
		t.Fatalf("adjacent('a')=%q, want one of qwsz", g)
	}
	if g := adjacentKey(rng, "A"); !charIn("QWSZ", g) {
		t.Fatalf("adjacent('A')=%q, want an uppercase neighbor", g)
	}
	if g := adjacentKey(rng, "5"); g != "" {
		t.Fatalf("adjacent('5')=%q, want empty", g)
	}
}

func TestKeyTimingPositiveAndSkewed(t *testing.T) {
	h := &humanizer{rng: newTestRNG(2)}
	gaps := make([]float64, 0, 800)
	for range 800 {
		if h.keyHold() <= 0 {
			t.Fatal("key hold must be positive")
		}
		gaps = append(gaps, float64(h.interKeyDelay().Microseconds()))
	}
	if meanF(gaps) <= medianF(gaps) {
		t.Fatalf("inter-key gaps not right-skewed: mean %.0f <= median %.0f", meanF(gaps), medianF(gaps))
	}
}

// charIn reports whether s is a single character present in set.
func charIn(set, s string) bool {
	return len(s) == 1 && strings.IndexByte(set, s[0]) >= 0
}

// typingHumanizer records both the commands injected at the browser and the
// frames answered back to the driver.
func typingHumanizer(seed uint64, injected, answered *[]map[string]any) *humanizer {
	h := recordingHumanizer(seed, injected)
	h.enabled = true
	h.clientSend = func(_ websocket.MessageType, data []byte) error {
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		*answered = append(*answered, m)
		return nil
	}
	return h
}

// typedText reconstructs what the page receives from a recorded stream: a keyDown
// carrying text appends, Backspace deletes, an insertText run appends whole. This
// is the invariant that matters - injected typos correct themselves, so the NET
// text must always equal what the driver asked for.
func typedText(events []map[string]any) string {
	var out []rune
	for _, m := range events {
		p, _ := m["params"].(map[string]any)
		if p == nil {
			continue
		}
		if m["method"] == methodInsertText {
			t, _ := p["text"].(string)
			out = append(out, []rune(t)...)
			continue
		}
		if p["type"] != "keyDown" && p["type"] != "rawKeyDown" {
			continue
		}
		if p["key"] == "Backspace" {
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			continue
		}
		if t, _ := p["text"].(string); t != "" {
			out = append(out, []rune(t)...)
		}
	}
	return string(out)
}

func insertTextFrame(id int64, text string) (map[string]any, map[string]any) {
	params := map[string]any{"text": text}
	return map[string]any{cdpID: json.Number(strconv.FormatInt(id, 10)), cdpMethod: methodInsertText, cdpParams: params}, params
}

// fill() commits a whole value through Input.insertText, which reaches the page
// as one IME-style edit with zero key events - the tell the humanizer exists to
// erase. The rewrite must produce real keystrokes with the identical net text.
func TestInsertTextBecomesRealKeystrokes(t *testing.T) {
	var injected, answered []map[string]any
	h := typingHumanizer(7, &injected, &answered)

	msg, params := insertTextFrame(42, "Ok, go!")
	if !h.handleInsertText(msg, params, "SID") {
		t.Fatal("a short ASCII value must be rewritten, not forwarded")
	}
	if got := typedText(injected); got != "Ok, go!" {
		t.Fatalf("net typed text %q, want %q", got, "Ok, go!")
	}
	for _, m := range injected {
		if m["method"] == methodInsertText {
			t.Fatalf("pure-ASCII text must not fall back to insertText: %v", m)
		}
		if m["sessionId"] != "SID" {
			t.Fatalf("injected key dropped sessionId: %v", m)
		}
	}
	// The driver's command is answered exactly once, under its own id, since the
	// original never reaches the browser.
	if len(answered) != 1 {
		t.Fatalf("answered %d frames, want exactly 1", len(answered))
	}
	if id, _ := answered[0]["id"].(float64); int64(id) != 42 {
		t.Fatalf("answered id %v, want the driver's 42", answered[0]["id"])
	}
	if _, isErr := answered[0]["error"]; isErr {
		t.Fatalf("rewrite must answer success: %v", answered[0])
	}
}

// A capital needs Shift genuinely held: Playwright sends a bare key with
// modifiers 0, leaving event.shiftKey false on an uppercase letter.
func TestInsertTextHoldsShiftForCapitals(t *testing.T) {
	var injected, answered []map[string]any
	h := typingHumanizer(11, &injected, &answered)

	msg, params := insertTextFrame(1, "A")
	if !h.handleInsertText(msg, params, "") {
		t.Fatal("expected a rewrite")
	}

	var sawShiftDown, sawShiftUp, sawChar bool
	for _, m := range injected {
		p := m["params"].(map[string]any)
		switch p["key"] {
		case "Shift":
			if p["type"] == cdpKeyUp {
				sawShiftUp = true
			} else {
				sawShiftDown = true
			}
			if p["code"] != "ShiftLeft" {
				t.Errorf("shift code %v, want ShiftLeft", p["code"])
			}
		case "A":
			sawChar = true
			if p["type"] != cdpKeyUp {
				if mod, _ := p["modifiers"].(float64); int(mod) != shiftModifier {
					t.Errorf("modifiers %v, want %d so event.shiftKey is true", p["modifiers"], shiftModifier)
				}
				if p["unmodifiedText"] != "a" {
					t.Errorf("unmodifiedText %v, want the unshifted char", p["unmodifiedText"])
				}
				if p["code"] != "KeyA" {
					t.Errorf("code %v, want KeyA", p["code"])
				}
			}
		}
	}
	if !sawShiftDown || !sawShiftUp || !sawChar {
		t.Fatalf("want Shift held around the char: down=%v up=%v char=%v", sawShiftDown, sawShiftUp, sawChar)
	}
	if !sawShiftDown || injected[0]["params"].(map[string]any)["key"] != "Shift" {
		t.Error("Shift must go down BEFORE the character, not after")
	}
}

// Characters with no US-layout keycode keep their runs on insertText - the same
// carve-out Playwright's own keyboard.type makes - and order must be preserved.
func TestInsertTextKeepsUntypeableRunsOnInsertText(t *testing.T) {
	var injected, answered []map[string]any
	h := typingHumanizer(5, &injected, &answered)

	const value = "oké☕" // "oke" + e-acute + hot-beverage emoji
	msg, params := insertTextFrame(9, value)
	if !h.handleInsertText(msg, params, "") {
		t.Fatal("expected a rewrite")
	}
	if got := typedText(injected); got != value {
		t.Fatalf("net typed text %q, want %q", got, value)
	}

	var fellBack bool
	for _, m := range injected {
		if m["method"] == methodInsertText {
			fellBack = true
			if txt := m["params"].(map[string]any)["text"]; txt != "é☕" {
				t.Errorf("fallback run %q, want the untypeable tail batched together", txt)
			}
		}
	}
	if !fellBack {
		t.Error("characters with no keycode must fall back to insertText")
	}
}

func TestInsertTextForwardsWhatItCannotPace(t *testing.T) {
	for name, text := range map[string]string{
		"empty":       "",
		"no keycodes": "☕☕",
	} {
		t.Run(name, func(t *testing.T) {
			var injected, answered []map[string]any
			h := typingHumanizer(2, &injected, &answered)
			msg, params := insertTextFrame(3, text)
			if h.handleInsertText(msg, params, "") {
				t.Fatal("must forward the original verbatim rather than rewrite it")
			}
			if len(injected) != 0 || len(answered) != 0 {
				t.Fatalf("a forwarded command must emit nothing: injected=%d answered=%d", len(injected), len(answered))
			}
		})
	}
}

// Above the cap the value is split rather than forwarded whole: the head is typed
// as real keystrokes so the keystroke record is never empty, and the tail rides a
// single insertText. Forwarding verbatim used to hand exactly the longest (most
// sensitive) values to the IME path with zero keydown/keyup.
func TestInsertTextSplitsAboveTheCap(t *testing.T) {
	var injected, answered []map[string]any
	h := typingHumanizer(4, &injected, &answered)
	// Wall-clock independent: the humanized pacing of a full head is ~2.8s against
	// a 4.5s production budget, and CI drift under -race must not tip this into the
	// abandoned path and fail the success assertion below.
	h.typeBudget = time.Minute

	const tail = "TAIL"
	value := strings.Repeat("a", insertTextMaxRunes) + tail
	msg, params := insertTextFrame(7, value)
	if !h.handleInsertText(msg, params, "") {
		t.Fatal("expected a rewrite")
	}
	if got := typedText(injected); got != value {
		t.Fatalf("net typed text %q, want %q", got, value)
	}

	var commits []string
	keystrokes := 0
	for _, m := range injected {
		switch m["method"] {
		case methodInsertText:
			commits = append(commits, m["params"].(map[string]any)["text"].(string))
		case methodKey:
			keystrokes++
		}
	}
	if keystrokes == 0 {
		t.Error("the head must go out as real keystrokes")
	}
	if len(commits) != 1 || commits[0] != tail {
		t.Errorf("tail commits = %q, want exactly one %q", commits, tail)
	}
	if len(answered) != 1 {
		t.Fatalf("driver answered %d times, want 1", len(answered))
	}
	if _, isErr := answered[0]["error"]; isErr {
		t.Error("a completed split must ack success, not error")
	}
}

// Every printable ASCII character must be typeable, or fill() silently degrades
// to the IME path for values containing it.
func TestCharKeysCoverPrintableASCII(t *testing.T) {
	for r := rune(0x20); r <= 0x7e; r++ {
		k, ok := charKeys[r]
		if !ok {
			t.Errorf("no key produces %q", r)
			continue
		}
		if k.char != r {
			t.Errorf("%q maps to a key producing %q", r, k.char)
		}
	}
}

// The injected typo must carry the same full key identity as a real keystroke.
// A bare {key,text} pair lands a keydown with code "", keyCode 0 and - on a
// capital - shiftKey false, which is the exact anomaly typeKey holds Shift to
// avoid, injected right beside the character it imitates.
func TestEmitTypoCarriesFullKeyIdentity(t *testing.T) {
	var got []map[string]any
	h := recordingHumanizer(3, &got)
	h.emitTypo("A", "SID")

	var sawShift bool
	for _, m := range got {
		p := m["params"].(map[string]any)
		if p["key"] == "Shift" {
			sawShift = true
			continue
		}
		if p["key"] == "Backspace" {
			continue
		}
		if p["code"] == "" || p["code"] == nil {
			t.Errorf("typo key has no code: %v", p)
		}
		if vk, _ := p["windowsVirtualKeyCode"].(float64); vk == 0 {
			t.Errorf("typo key has no virtual-key code: %v", p)
		}
		if p["type"] != cdpKeyUp {
			if mod, _ := p["modifiers"].(float64); int(mod) != shiftModifier {
				t.Errorf("uppercase typo must hold Shift, got modifiers %v", p["modifiers"])
			}
		}
	}
	if !sawShift {
		t.Error("an uppercase typo must press Shift like a real capital does")
	}
}

// Two frames the rewrite must decline BEFORE typing anything, since after a
// keystroke has gone out, forwarding the original too would type it twice.
func TestInsertTextDeclinesBeforeTyping(t *testing.T) {
	t.Run("no id to answer", func(t *testing.T) {
		var injected, answered []map[string]any
		h := typingHumanizer(4, &injected, &answered)
		msg := map[string]any{cdpMethod: methodInsertText, cdpParams: map[string]any{"text": "hi"}}
		if h.handleInsertText(msg, map[string]any{"text": "hi"}, "") {
			t.Fatal("a frame with no id must be forwarded, not answered")
		}
		if len(injected) != 0 {
			t.Fatalf("nothing may be typed before the decision: %v", injected)
		}
	})

	t.Run("nothing typeable", func(t *testing.T) {
		var injected, answered []map[string]any
		h := typingHumanizer(4, &injected, &answered)
		msg, params := insertTextFrame(8, "☕🙂")
		if h.handleInsertText(msg, params, "") {
			t.Fatal("an all-untypeable value must be forwarded so the browser answers it")
		}
		if len(injected) != 0 || len(answered) != 0 {
			t.Fatalf("forwarded command must emit nothing: injected=%v answered=%v", injected, answered)
		}
	})
}

func compositionFrame(id int64, text string, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	params["text"] = text
	return map[string]any{
		cdpID: json.Number(strconv.FormatInt(id, 10)), cdpMethod: methodIMEComposition, cdpParams: params,
	}
}

// One Input.imeSetComposition places a whole value with zero key events and no
// insertText - the humanizer's blind spot. It must come out as real keystrokes
// with the identical net text, answered under the driver's own id.
func TestCompositionBecomesRealKeystrokes(t *testing.T) {
	var injected, answered []map[string]any
	h := typingHumanizer(6, &injected, &answered)

	msg := compositionFrame(21, "hunter2", nil)
	if !h.handleClientFrame(mustJSON(t, msg)) {
		t.Fatal("a composition placing a typeable value must be rewritten, not forwarded")
	}
	if got := typedText(injected); got != "hunter2" {
		t.Fatalf("net typed text %q, want %q", got, "hunter2")
	}
	for _, m := range injected {
		if m["method"] == methodIMEComposition {
			t.Fatalf("the composition itself must never reach the browser: %v", m)
		}
	}
	if len(answered) != 1 {
		t.Fatalf("answered %d frames, want exactly 1", len(answered))
	}
	if id, _ := answered[0]["id"].(float64); int64(id) != 21 {
		t.Fatalf("answered id %v, want the driver's 21", answered[0]["id"])
	}
	if _, isErr := answered[0]["error"]; isErr {
		t.Fatalf("rewrite must answer success: %v", answered[0])
	}
}

// The driver commits a composition with an insertText carrying the same value.
// Since the composition was already typed out, the commit must be answered
// rather than typed - or the field ends up holding the value twice.
func TestCompositionCommitIsNotTypedTwice(t *testing.T) {
	var injected, answered []map[string]any
	h := typingHumanizer(6, &injected, &answered)

	msg := compositionFrame(1, "s3cret", nil)
	if !h.handleClientFrame(mustJSON(t, msg)) {
		t.Fatal("expected the composition to be rewritten")
	}
	typedByComposition := len(injected)

	commit, _ := insertTextFrame(2, "s3cret")
	if !h.handleClientFrame(mustJSON(t, commit)) {
		t.Fatal("the commit must be answered, not forwarded to the browser")
	}
	if len(injected) != typedByComposition {
		t.Fatalf("the commit typed %d more events; it must type nothing", len(injected)-typedByComposition)
	}
	if got := typedText(injected); got != "s3cret" {
		t.Fatalf("net typed text %q, want the value exactly once", got)
	}
	// A later insertText of the same text is an ordinary fill again: the record is
	// consumed by the first commit, never left arming a silent swallow.
	again, _ := insertTextFrame(3, "s3cret")
	if !h.handleClientFrame(mustJSON(t, again)) {
		t.Fatal("expected the second fill to be rewritten")
	}
	if got := typedText(injected); got != "s3crets3cret" {
		t.Fatalf("net typed text %q after a second fill, want it typed again", got)
	}
}

// A real IME edit is left alone. Both carve-outs still log, because the value
// reaches the field unhumanized either way.
func TestCompositionForwardsGenuineIMEEdits(t *testing.T) {
	cases := map[string]struct {
		text   string
		params map[string]any
	}{
		"replacement range": {"ni", map[string]any{"replacementStart": json.Number("0"), "replacementEnd": json.Number("2")}},
		"no typeable rune":  {"に", nil},
		"empty text":        {"", nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var injected, answered []map[string]any
			h := typingHumanizer(6, &injected, &answered)
			msg := compositionFrame(4, tc.text, tc.params)
			if h.handleClientFrame(mustJSON(t, msg)) {
				t.Fatal("must forward the composition verbatim")
			}
			if len(injected) != 0 || len(answered) != 0 {
				t.Fatalf("a forwarded composition must emit nothing: injected=%v answered=%v", injected, answered)
			}
		})
	}
}

// A composition with no id has nothing to answer, so rewriting it would strand
// the browser's own reply.
func TestCompositionWithoutIDIsForwarded(t *testing.T) {
	var injected, answered []map[string]any
	h := typingHumanizer(6, &injected, &answered)
	msg := map[string]any{cdpMethod: methodIMEComposition, cdpParams: map[string]any{"text": "abc"}}
	if h.handleClientFrame(mustJSON(t, msg)) {
		t.Fatal("a composition with no id must be forwarded")
	}
	if len(injected) != 0 {
		t.Fatalf("nothing may be typed before the decision: %v", injected)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling frame: %v", err)
	}
	return b
}
