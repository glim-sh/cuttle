package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestHumanizeExpandsMouseMove drives a real proxyCDPWebsocket with humanize=on
// through a fake browser and a fake client, asserting the end-to-end contract:
// one driver mouseMoved becomes a multi-sample trajectory at the browser, the
// driver gets exactly one response for its command, and no injected id leaks back.
func TestHumanizeExpandsMouseMove(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var mu sync.Mutex
	var browserGot []map[string]any

	// Fake browser: record every command, ack each with {id,result:{}}.
	browser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			mu.Lock()
			browserGot = append(browserGot, m)
			mu.Unlock()
			ack, _ := json.Marshal(map[string]any{"id": m["id"], "result": map[string]any{}})
			_ = conn.Write(context.Background(), websocket.MessageText, ack)
		}
	}))
	defer browser.Close()
	target := "ws" + strings.TrimPrefix(browser.URL, "http")

	// Proxy server: run the real proxy with humanize enabled.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientWS, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		proxyCDPWebsocket(context.Background(), clientWS, target, "", "test", "", "", true)
	}))
	defer proxy.Close()

	cl, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http"), nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer cl.Close(websocket.StatusNormalClosure, "")

	move := `{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"mouseMoved","x":640,"y":480}}`
	if werr := cl.Write(ctx, websocket.MessageText, []byte(move)); werr != nil {
		t.Fatalf("client write: %v", werr)
	}

	// The driver must get exactly one frame back: the synthetic response to id=1.
	// No injected id (>= humanizeIDBase) may ever reach it.
	_, respData, err := cl.Read(ctx)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(respData, &got); err != nil {
		t.Fatalf("client resp not json: %v", err)
	}
	if id, _ := got["id"].(float64); id != 1 {
		t.Fatalf("driver got response for id %v, want 1 (injected id leaked?)", got["id"])
	}

	// The browser must have received a TRAJECTORY (many mouseMoved), all injected
	// (id >= humanizeIDBase), the last landing exactly on the target - not the
	// single teleport the driver sent. The injected frames may still be draining
	// into the browser as the driver's response returns, so poll briefly.
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(browserGot)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(browserGot) < 2 {
		t.Fatalf("browser received %d commands, expected a multi-sample trajectory", len(browserGot))
	}
	for i, m := range browserGot {
		if m["method"] != "Input.dispatchMouseEvent" {
			t.Fatalf("browser cmd %d is %v, want Input.dispatchMouseEvent", i, m["method"])
		}
		if id, _ := m["id"].(float64); int64(id) < humanizeIDBase {
			t.Fatalf("browser cmd %d has non-injected id %v", i, m["id"])
		}
	}
	last := browserGot[len(browserGot)-1]["params"].(map[string]any)
	if last["x"].(float64) != 640 || last["y"].(float64) != 480 {
		t.Fatalf("final injected move at (%v,%v), want (640,480)", last["x"], last["y"])
	}
}

// TestHumanizeDisabledIsPassthrough confirms humanize=off forwards the driver's
// mouseMoved verbatim - one command, unchanged id.
func TestHumanizeDisabledIsPassthrough(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var mu sync.Mutex
	var browserGot []map[string]any
	browser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			mu.Lock()
			browserGot = append(browserGot, m)
			mu.Unlock()
			ack, _ := json.Marshal(map[string]any{"id": m["id"], "result": map[string]any{}})
			_ = conn.Write(context.Background(), websocket.MessageText, ack)
		}
	}))
	defer browser.Close()
	target := "ws" + strings.TrimPrefix(browser.URL, "http")

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientWS, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		proxyCDPWebsocket(context.Background(), clientWS, target, "", "test", "", "", false)
	}))
	defer proxy.Close()

	cl, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http"), nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer cl.Close(websocket.StatusNormalClosure, "")

	move := `{"id":7,"method":"Input.dispatchMouseEvent","params":{"type":"mouseMoved","x":640,"y":480}}`
	if werr := cl.Write(ctx, websocket.MessageText, []byte(move)); werr != nil {
		t.Fatalf("client write: %v", werr)
	}
	if _, _, err := cl.Read(ctx); err != nil {
		t.Fatalf("client read: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(browserGot) != 1 {
		t.Fatalf("passthrough sent %d commands, want 1", len(browserGot))
	}
	if id, _ := browserGot[0]["id"].(float64); id != 7 {
		t.Fatalf("passthrough rewrote id to %v, want 7", browserGot[0]["id"])
	}
}
