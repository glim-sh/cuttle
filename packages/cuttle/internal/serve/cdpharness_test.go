package serve

import (
	"context"
	"encoding/json"
	"math"
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
	return startCDPBrowser(t, result, nil)
}

// startCDPBrowser is startCDPRecorder whose ack carries the command's sessionId,
// with the events announce returns for a command sent around that ack, the way
// Chrome orders them: before it, then after it. Like Chrome, it refuses an id
// past 2^31 without echoing it.
func startCDPBrowser(t *testing.T, result func(cmd map[string]any) map[string]any, announce func(cmd map[string]any) (before, after []map[string]any)) (*cdpRecorder, string) {
	t.Helper()
	f := &cdpRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		send := func(v map[string]any) {
			b, _ := json.Marshal(v)
			_ = conn.Write(context.Background(), websocket.MessageText, b)
		}
		for {
			_, data, rerr := conn.Read(context.Background())
			if rerr != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			if id, _ := m["id"].(float64); id > math.MaxInt32 {
				send(map[string]any{"error": map[string]any{"message": "Message must have integer 'id' property"}})
				continue
			}
			f.mu.Lock()
			f.got = append(f.got, m)
			f.mu.Unlock()
			res := map[string]any{}
			if result != nil {
				res = result(m)
			}
			var before, after []map[string]any
			if announce != nil {
				before, after = announce(m)
			}
			for _, ev := range before {
				send(ev)
			}
			ack := map[string]any{"id": m["id"], "result": res}
			if sid, ok := m["sessionId"]; ok {
				ack["sessionId"] = sid
			}
			send(ack)
			for _, ev := range after {
				send(ev)
			}
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
