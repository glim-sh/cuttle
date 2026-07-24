package serve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
