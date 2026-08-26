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

// BenchmarkClientFrameNonInput measures the whole per-frame cost humanize adds on
// the client->browser path for a non-Input command (the steady state: a driver
// mostly issues Page/Runtime/DOM commands, not Input). With humanize off this call
// is skipped entirely, so the ns/op here IS the added overhead.
func BenchmarkClientFrameNonInput(b *testing.B) {
	h := benchHumanizer()
	frame := []byte(`{"id":42,"method":"Runtime.evaluate","params":{"expression":"document.title"}}`)
	b.ReportAllocs()
	for range b.N {
		_ = h.handleClientFrame(frame)
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
