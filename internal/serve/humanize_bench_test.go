package serve

import (
	"context"
	"testing"

	"github.com/coder/websocket"
)

func benchHumanizer() *humanizer {
	noop := func(websocket.MessageType, []byte) error { return nil }
	return newHumanizer(context.Background(), true, newSecretStore(), reservedSeed, noop, noop)
}

// BenchmarkClientFrameNonInput measures what the PROXY actually runs per
// client->browser frame: one decode feeding both client-side hooks.
//
// It deliberately does not call handleClientFrame, whose byte prefilter survives
// only as a byte-level entry point for tests. Benchmarking that instead reported
// 7ns and zero allocations for a path production had stopped taking - so the
// decode this feature added (a prefilter cannot be sound against JSON, see
// decodeClientFrame) was invisible to the one instrument meant to catch it.
func BenchmarkClientFrameNonInput(b *testing.B) {
	h := benchHumanizer()
	frame := []byte(`{"id":42,"method":"Runtime.evaluate","params":{"expression":"document.title"}}`)
	b.ReportAllocs()
	for range b.N {
		if msg, ok := h.decodeClientFrame(frame); ok {
			_, _ = h.handleSecretMsg(msg)
			_ = h.handleClientMsg(msg)
		}
	}
}

// BenchmarkClientFrameLargeScript is the frame shape that costs the most: a
// driver's injected script, which Playwright sends for nearly every action. The
// decode is proportional to the payload, so this is the ceiling the prefilter
// used to hide.
func BenchmarkClientFrameLargeScript(b *testing.B) {
	h := benchHumanizer()
	script := make([]byte, 0, 4096)
	script = append(script, `{"id":7,"method":"Runtime.callFunctionOn","params":{"functionDeclaration":"`...)
	for range 3000 {
		script = append(script, 'x')
	}
	script = append(script, `","objectId":"1.2.3"}}`...)
	b.ReportAllocs()
	for range b.N {
		if msg, ok := h.decodeClientFrame(script); ok {
			_, _ = h.handleSecretMsg(msg)
			_ = h.handleClientMsg(msg)
		}
	}
}

// BenchmarkBrowserFrameSteadyState measures the browser->client per-frame cost:
// maybeSwallow's inFlight==0 gate on an ordinary browser event that is not an
// injected response: the per-frame cost every attached driver pays.
func BenchmarkBrowserFrameSteadyState(b *testing.B) {
	h := benchHumanizer()
	frame := []byte(`{"method":"Page.frameNavigated","params":{"frame":{"id":"F1","url":"https://example.com/a/b/c","loaderId":"L1"}}}`)
	b.ReportAllocs()
	for range b.N {
		_ = h.maybeSwallow(frame)
	}
}
