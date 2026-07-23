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

// TestHumanizeClicksOffCentre drives a real proxy: a driver's centre-targeted
// mousePressed must be rewritten to the off-centre point the in-page query
// returns (via Runtime.evaluate), not forwarded at the driver's coordinates.
func TestHumanizeClicksOffCentre(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var mu sync.Mutex
	var presses []map[string]any
	sawEvaluate := false

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
			result := map[string]any{}
			switch m["method"] {
			case "Runtime.evaluate":
				mu.Lock()
				sawEvaluate = true
				mu.Unlock()
				// Simulate elementFromPoint: an element box (100,200,50x40) and a
				// chosen off-centre point inside it. Same rect on both samples =
				// stable.
				result = map[string]any{"result": map[string]any{"value": map[string]any{
					"px": 105.0, "py": 210.0, "exact": false,
					"x": 100.0, "y": 200.0, "w": 50.0, "h": 40.0,
				}}}
			case "Input.dispatchMouseEvent":
				if p, _ := m["params"].(map[string]any); p["type"] == "mousePressed" {
					mu.Lock()
					presses = append(presses, p)
					mu.Unlock()
				}
			}
			ack, _ := json.Marshal(map[string]any{"id": m["id"], "result": result})
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
		proxyCDPWebsocket(context.Background(), clientWS, target, "", "test", "", "", true)
	}))
	defer proxy.Close()

	cl, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http"), nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer cl.Close(websocket.StatusNormalClosure, "")

	// Driver presses at the element's centre (125,220) with id 1.
	press := `{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":125,"y":220,"button":"left","clickCount":1}}`
	if werr := cl.Write(ctx, websocket.MessageText, []byte(press)); werr != nil {
		t.Fatalf("client write: %v", werr)
	}
	// The driver's own id must come back (the browser's real ack of the rewritten
	// press), never an injected id.
	_, respData, err := cl.Read(ctx)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(respData, &got)
	if id, _ := got["id"].(float64); id != 1 {
		t.Fatalf("driver got response id %v, want 1", got["id"])
	}

	mu.Lock()
	defer mu.Unlock()
	if !sawEvaluate {
		t.Fatal("no Runtime.evaluate issued - actionability query never ran")
	}
	if len(presses) != 1 {
		t.Fatalf("browser received %d presses, want 1", len(presses))
	}
	px, _ := presses[0]["x"].(float64)
	py, _ := presses[0]["y"].(float64)
	if px != 105 || py != 210 {
		t.Fatalf("press forwarded at (%v,%v), want the off-centre (105,210) not the driver's (125,220)", px, py)
	}
	if presses[0]["clickCount"].(float64) != 1 || presses[0]["button"] != "left" {
		t.Fatalf("press lost button/clickCount: %v", presses[0])
	}
}
