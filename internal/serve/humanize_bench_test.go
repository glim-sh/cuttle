package serve

import (
	"bytes"
	"context"
	"testing"

	"github.com/coder/websocket"
)

func benchHumanizer() *humanizer {
	noop := func(websocket.MessageType, []byte) error { return nil }
	return newHumanizer(context.Background(), true, noop, noop)
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
// maybeSwallow's inFlight==0 gate plus the keep-alive prefilter, on an ordinary
// event that is neither an injected response nor the keep-alive tab. Both are
// skipped with humanize off / no keep-alive.
func BenchmarkBrowserFrameSteadyState(b *testing.B) {
	h := benchHumanizer()
	frame := []byte(`{"method":"Page.frameNavigated","params":{"frame":{"id":"F1","url":"https://example.com/a/b/c","loaderId":"L1"}}}`)
	keepAliveID := "034216362DDEDA3CFF4E6EC62053ACF9"
	kaBytes := []byte(keepAliveID)
	b.ReportAllocs()
	for range b.N {
		_ = h.maybeSwallow(frame)
		if bytes.Contains(frame, kaBytes) {
			_, _ = hideKeepAlive(frame, keepAliveID)
		}
	}
}
