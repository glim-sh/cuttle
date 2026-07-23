package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeCreateTargetBrowser serves /json/version + a browser-level CDP socket that
// answers Target.createTarget with a fixed targetId, so createKeepAlivePage can be
// exercised without a real browser.
func fakeCreateTargetBrowser(t *testing.T, newID string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/version" {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"webSocketDebuggerUrl": "ws://" + r.Host + "/devtools/browser/x",
			})
			return
		}
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
			if m["method"] == "Target.createTarget" {
				result["targetId"] = newID
			}
			ack, _ := json.Marshal(map[string]any{"id": m["id"], "result": result})
			_ = conn.Write(context.Background(), websocket.MessageText, ack)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCreateKeepAlivePageReturnsTargetID(t *testing.T) {
	srv := fakeCreateTargetBrowser(t, "KEEPALIVE")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if id := createKeepAlivePage(ctx, srv.URL); id != "KEEPALIVE" {
		t.Fatalf("createKeepAlivePage = %q, want KEEPALIVE", id)
	}
}

func TestCreateKeepAlivePageBadEndpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if id := createKeepAlivePage(ctx, "http://127.0.0.1:1"); id != "" {
		t.Fatalf("createKeepAlivePage on a dead endpoint = %q, want empty", id)
	}
}

func TestCloseTargetIDParsing(t *testing.T) {
	got := closeTargetID([]byte(`{"id":5,"method":"Target.closeTarget","params":{"targetId":"ABC"}}`))
	if got != "ABC" {
		t.Fatalf("closeTargetID = %q, want ABC", got)
	}
	if id := closeTargetID([]byte(`{"id":5,"method":"Page.navigate","params":{"url":"x"}}`)); id != "" {
		t.Fatalf("non-closeTarget returned %q, want empty", id)
	}
}

func TestKeepAliveCloseResponseEchoesIDs(t *testing.T) {
	out := keepAliveCloseResponse([]byte(`{"id":7,"sessionId":"S1","method":"Target.closeTarget","params":{"targetId":"KA"}}`))
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["sessionId"] != "S1" {
		t.Fatalf("sessionId = %v, want S1", m["sessionId"])
	}
	result, _ := m["result"].(map[string]any)
	if result == nil || result["success"] != true {
		t.Fatalf("result = %v, want {success:true}", m["result"])
	}
	if _, ok := m["id"]; !ok {
		t.Fatalf("response dropped the command id: %v", m)
	}
}

func TestHideKeepAliveDropsLifecycleEvents(t *testing.T) {
	for _, method := range []string{"Target.targetCreated", "Target.attachedToTarget", "Target.targetInfoChanged"} {
		frame := []byte(`{"method":"` + method + `","params":{"targetInfo":{"targetId":"KA","type":"page"}}}`)
		if _, drop := hideKeepAlive(frame, "KA"); !drop {
			t.Fatalf("%s for the keep-alive was not dropped", method)
		}
		other := []byte(`{"method":"` + method + `","params":{"targetInfo":{"targetId":"OTHER","type":"page"}}}`)
		if _, drop := hideKeepAlive(other, "KA"); drop {
			t.Fatalf("%s for a different target was dropped", method)
		}
	}
}

func TestHideKeepAliveStripsGetTargets(t *testing.T) {
	frame := []byte(`{"id":1,"result":{"targetInfos":[` +
		`{"targetId":"KA","type":"page"},` +
		`{"targetId":"REAL","type":"page"}]}}`)
	out, drop := hideKeepAlive(frame, "KA")
	if drop {
		t.Fatal("a getTargets result should be rewritten, not dropped")
	}
	var m struct {
		Result struct {
			TargetInfos []struct {
				TargetID string `json:"targetId"`
			} `json:"targetInfos"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m.Result.TargetInfos) != 1 || m.Result.TargetInfos[0].TargetID != "REAL" {
		t.Fatalf("targetInfos = %+v, want only REAL", m.Result.TargetInfos)
	}
}

func TestHideKeepAlivePassesUnrelatedFrames(t *testing.T) {
	frame := []byte(`{"id":2,"result":{"frameId":"F1"}}`)
	out, drop := hideKeepAlive(frame, "KA")
	if drop || string(out) != string(frame) {
		t.Fatalf("unrelated frame was altered: drop=%v out=%s", drop, out)
	}
}
