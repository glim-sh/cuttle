package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// The shared scaffolding for driving a REAL proxyCDPWebsocket end to end: a fake
// browser on one side, a fake driver on the other. Several tests need the same
// three pieces, and hand-rolling them per test is how two copies quietly drift
// into testing slightly different things.

// cdpRecorder records every command a proxied session forwards to the browser.
type cdpRecorder struct {
	mu  sync.Mutex
	got []map[string]any
}

// received returns a snapshot of the commands the browser has seen so far. Safe
// to call while the session is live - the fake writes from its own goroutine.
func (f *cdpRecorder) received() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.got...)
}

// startCDPRecorder serves a websocket that records each command and acks it. result
// supplies the ack's "result" object for a given command; nil means an empty one.
func startCDPRecorder(t *testing.T, result func(cmd map[string]any) map[string]any) (*cdpRecorder, string) {
	t.Helper()
	f := &cdpRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		for {
			_, data, rerr := conn.Read(context.Background())
			if rerr != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			f.mu.Lock()
			f.got = append(f.got, m)
			f.mu.Unlock()
			res := map[string]any{}
			if result != nil {
				res = result(m)
			}
			ack, _ := json.Marshal(map[string]any{"id": m["id"], "result": res})
			_ = conn.Write(context.Background(), websocket.MessageText, ack)
		}
	}))
	t.Cleanup(srv.Close)
	return f, "ws" + strings.TrimPrefix(srv.URL, "http")
}

// startCDPProxy fronts target with the real proxyCDPWebsocket and returns the ws
// URL a driver connects to.
func startCDPProxy(t *testing.T, target string, opts cdpSessionOpts) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientWS, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		proxyCDPWebsocket(context.Background(), clientWS, target, "test", opts)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// dialCDPClient connects a fake driver to the proxy.
func dialCDPClient(ctx context.Context, t *testing.T, url string) *websocket.Conn {
	t.Helper()
	cl, resp, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { cl.Close(websocket.StatusNormalClosure, "") })
	return cl
}
