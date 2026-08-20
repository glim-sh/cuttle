package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeCreateTargetBrowser serves /json/list + /json/version + a browser-level CDP
// socket that answers Target.createTarget with a fixed targetId, so keepAlivePage
// can be exercised without a real browser. existing is the page list it reports.
func fakeCreateTargetBrowser(t *testing.T, newID string, existing ...map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/list" {
			list := make([]map[string]string, 0, len(existing))
			list = append(list, existing...)
			_ = json.NewEncoder(w).Encode(list)
			return
		}
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

func serverPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("server port: %v", err)
	}
	return p
}

// The browser the image launches already has a tab (its argv ends in
// about:blank). Adopting it is what keeps the viewer down to ONE tab - creating a
// second one put two identical blank tabs in front of the person watching.
func TestKeepAliveAdoptsTheExistingTab(t *testing.T) {
	srv := fakeCreateTargetBrowser(
		t, "CREATED",
		map[string]string{"id": "SW", "type": "service_worker"},
		map[string]string{"id": "EXISTING", "type": "page"},
		map[string]string{"id": "SECOND", "type": "page"},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if id := keepAlivePage(ctx, serverPort(t, srv), true); id != "EXISTING" {
		t.Fatalf("keepAlivePage = %q, want the first existing page EXISTING", id)
	}
}

// A browser with no page at all (a bare `cuttle serve` outside the image, whose
// argv carries no about:blank) still needs one, or the first driver teardown that
// closes its own tab takes the browser with it.
func TestKeepAliveCreatesOneWhenThereIsNoPage(t *testing.T) {
	srv := fakeCreateTargetBrowser(
		t, "CREATED",
		map[string]string{"id": "SW", "type": "service_worker"},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if id := keepAlivePage(ctx, serverPort(t, srv), false); id != "CREATED" {
		t.Fatalf("keepAlivePage = %q, want CREATED", id)
	}
}

func TestKeepAlivePageBadEndpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if id := keepAlivePage(ctx, 1, false); id != "" {
		t.Fatalf("keepAlivePage on a dead endpoint = %q, want empty", id)
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

// A close of the immortal tab must be HONORED once a replacement exists. The old
// behavior - answering success without performing it - never produced
// Target.targetDestroyed, and a driver that waits for the target to die
// (Playwright's page.close() does, with no timeout) hung forever.
func TestKeepAliveCloseIsForwardedAfterASwap(t *testing.T) {
	srv := fakeCreateTargetBrowser(
		t, "REPLACEMENT",
		map[string]string{"id": "ADOPTED", "type": "page"},
	)
	inst := &chromeInstance{cdpPort: serverPort(t, srv), keepAlive: "ADOPTED"}

	if !inst.replaceKeepAlive(context.Background(), "ADOPTED") {
		t.Fatal("a reachable browser must yield a replacement tab")
	}
	if got := inst.keepAliveID(); got != "REPLACEMENT" {
		t.Fatalf("keepAliveID = %q, want the replacement to have taken over", got)
	}

	// A second client racing the same close finds the swap already done, and its
	// close is free to go through: the tab it names no longer holds anything up.
	if !inst.replaceKeepAlive(context.Background(), "ADOPTED") {
		t.Fatal("a close of an already-replaced tab must be allowed through")
	}
	if got := inst.keepAliveID(); got != "REPLACEMENT" {
		t.Fatalf("the racing close opened a second replacement: %q", got)
	}
}

// With no browser to open a replacement in, the close is refused instead: a hung
// close beats a browser that exits under everyone using it.
func TestKeepAliveCloseRefusedWhenNoReplacement(t *testing.T) {
	inst := &chromeInstance{cdpPort: 1, keepAlive: "ADOPTED"}
	if inst.replaceKeepAlive(context.Background(), "ADOPTED") {
		t.Fatal("an unreachable browser cannot have yielded a replacement")
	}
	if got := inst.keepAliveID(); got != "ADOPTED" {
		t.Fatalf("keepAliveID moved to %q despite the failure", got)
	}
}

// The launch URL's target registers slightly after the DevTools HTTP server
// starts answering. When Chrome was given a URL, the list is polled for it; when
// it was not, there is nothing to wait for and a tab is opened at once.
func TestKeepAliveWaitsOnlyWhenChromeWasGivenAURL(t *testing.T) {
	if opensPage([]string{"--headless=false", "--window-size=1,2"}) {
		t.Fatal("a flags-only passthrough opens no page")
	}
	if !opensPage([]string{"--headless=false", "about:blank"}) {
		t.Fatal("a positional URL opens a page")
	}

	srv := fakeCreateTargetBrowser(t, "CREATED") // reports no pages, ever
	start := time.Now()
	if id := keepAlivePage(context.Background(), serverPort(t, srv), false); id != "CREATED" {
		t.Fatalf("keepAlivePage = %q, want CREATED", id)
	}
	if waited := time.Since(start); waited > keepAliveAdoptWindow {
		t.Fatalf("waited %s for a page that was never coming", waited)
	}
}
