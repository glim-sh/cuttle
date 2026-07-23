package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeCDPBrowser serves /json/version + a browser-level CDP socket answering
// Target.getTargets (with pageCount page targets ids PAGE0..) and recording
// Target.createTarget calls.
func fakeCDPBrowser(t *testing.T, pageCount int, created *int, mu *sync.Mutex) *httptest.Server {
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
			switch m["method"] {
			case "Target.getTargets":
				infos := []map[string]any{{"targetId": "WORKER", "type": "service_worker"}}
				for i := range pageCount {
					infos = append(infos, map[string]any{"targetId": fmt.Sprintf("PAGE%d", i), "type": "page"})
				}
				result["targetInfos"] = infos
			case "Target.createTarget":
				mu.Lock()
				*created++
				mu.Unlock()
				result["targetId"] = "NEWBLANK"
			}
			ack, _ := json.Marshal(map[string]any{"id": m["id"], "result": result})
			_ = conn.Write(context.Background(), websocket.MessageText, ack)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runKeepAlive(t *testing.T, pageCount int, closingID string) int {
	t.Helper()
	var mu sync.Mutex
	created := 0
	srv := fakeCDPBrowser(t, pageCount, &created, &mu)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ensureKeepAlivePage(ctx, srv.URL, closingID)
	mu.Lock()
	defer mu.Unlock()
	return created
}

func TestKeepAliveCreatesBlankWhenClosingLastPage(t *testing.T) {
	if n := runKeepAlive(t, 1, "PAGE0"); n != 1 {
		t.Fatalf("closing the last page created %d blanks, want 1", n)
	}
}

func TestKeepAliveNoOpWhenOtherPagesRemain(t *testing.T) {
	if n := runKeepAlive(t, 3, "PAGE0"); n != 0 {
		t.Fatalf("closing 1 of 3 pages created %d blanks, want 0", n)
	}
}

func TestKeepAliveNoOpForNonPageTarget(t *testing.T) {
	if n := runKeepAlive(t, 1, "WORKER"); n != 0 {
		t.Fatalf("closing a non-page target created %d blanks, want 0", n)
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
