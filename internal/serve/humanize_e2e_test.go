//go:build e2e

// End-to-end humanizer checks against a REAL headless Chrome driven through the
// real proxyCDPWebsocket. Excluded from the default suite (needs a local Chrome);
// run explicitly:
//
//	go test -tags e2e -run TestE2E -count=1 ./internal/serve/
//
// Chrome binary: $CUTTLE_E2E_CHROME, else the newest Playwright
// chrome-headless-shell in the ms-playwright cache, else the test skips.
package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// widgetSetupJS builds a Material/CDK-style select in the page (no data-URL
// encoding to trip over): the trigger toggles aria-expanded and opens a portaled
// panel plus a full-viewport backdrop that closes it on an outside click - the
// shape that dismissed the old off-centre re-aim. Returns 1 on success.
const widgetSetupJS = `(function(){
 document.body.style.margin='0';
 var t=document.createElement('button');t.id='trigger';t.setAttribute('aria-expanded','false');
 t.style.cssText='position:absolute;left:120px;top:140px;width:220px;height:52px';t.textContent='Select';
 var bd=document.createElement('div');bd.id='backdrop';bd.style.cssText='display:none;position:fixed;inset:0;background:transparent';
 var p=document.createElement('div');p.id='panel';p.style.cssText='display:none;position:absolute;left:120px;top:196px;width:220px;background:#eee';
 document.body.appendChild(t);document.body.appendChild(bd);document.body.appendChild(p);
 function open(){t.setAttribute('aria-expanded','true');bd.style.display='block';p.style.display='block';}
 function close(){t.setAttribute('aria-expanded','false');bd.style.display='none';p.style.display='none';}
 t.addEventListener('click',function(){t.getAttribute('aria-expanded')==='true'?close():open();});
 bd.addEventListener('click',close);
 return 1;
})()`

func findChrome(t *testing.T) string {
	if p := os.Getenv("CUTTLE_E2E_CHROME"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	glob := filepath.Join(home, "Library/Caches/ms-playwright/chromium_headless_shell-*/chrome-headless-shell-*/chrome-headless-shell")
	matches, _ := filepath.Glob(glob)
	if len(matches) == 0 {
		t.Skip("no local chrome-headless-shell found; set CUTTLE_E2E_CHROME")
	}
	sort.Strings(matches) // version dirs sort lexically; last is newest-enough
	return matches[len(matches)-1]
}

// launchChrome starts headless Chrome and returns its browser CDP ws URL.
func launchChrome(t *testing.T) string {
	t.Helper()
	bin := findChrome(t)
	dir := t.TempDir()
	cmd := exec.Command(bin, "--headless", "--disable-gpu", "--no-sandbox",
		"--remote-debugging-port=0", "--user-data-dir="+dir, "about:blank")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start chrome: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	re := regexp.MustCompile(`DevTools listening on (ws://\S+)`)
	scan := bufio.NewScanner(stderr)
	deadline := time.Now().Add(15 * time.Second)
	for scan.Scan() {
		if m := re.FindStringSubmatch(scan.Text()); m != nil {
			return m[1]
		}
		if time.Now().After(deadline) {
			break
		}
	}
	t.Fatal("chrome never printed a DevTools ws endpoint")
	return ""
}

// cdpClient is a tiny flat-session CDP client over one websocket: it correlates
// responses by id and lets the caller wait for a specific event.
type cdpClient struct {
	ws  *websocket.Conn
	ctx context.Context

	mu      sync.Mutex
	nextID  int
	pending map[int]chan json.RawMessage
	events  map[string]chan json.RawMessage
}

func dialCDP(ctx context.Context, t *testing.T, url string) *cdpClient {
	t.Helper()
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	ws.SetReadLimit(-1)
	c := &cdpClient{ws: ws, ctx: ctx, nextID: 1, pending: map[int]chan json.RawMessage{}, events: map[string]chan json.RawMessage{}}
	go c.readLoop()
	return c
}

func (c *cdpClient) readLoop() {
	for {
		_, data, err := c.ws.Read(c.ctx)
		if err != nil {
			return
		}
		var msg struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Params json.RawMessage `json:"params"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		c.mu.Lock()
		if msg.ID != 0 {
			if ch := c.pending[msg.ID]; ch != nil {
				delete(c.pending, msg.ID)
				res := msg.Result
				if msg.Error != nil {
					res = msg.Error
				}
				ch <- res
			}
		} else if ch := c.events[msg.Method]; ch != nil {
			select {
			case ch <- msg.Params:
			default:
			}
		}
		c.mu.Unlock()
	}
}

func (c *cdpClient) call(t *testing.T, sid, method string, params map[string]any) json.RawMessage {
	t.Helper()
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	ch := make(chan json.RawMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	frame := map[string]any{"id": id, "method": method}
	if params != nil {
		frame["params"] = params
	}
	if sid != "" {
		frame["sessionId"] = sid
	}
	b, _ := json.Marshal(frame)
	if err := c.ws.Write(c.ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}
	select {
	case res := <-ch:
		return res
	case <-time.After(10 * time.Second):
		t.Fatalf("timeout waiting for %s", method)
		return nil
	}
}

func (c *cdpClient) evalNumber(t *testing.T, sid, expr string) float64 {
	t.Helper()
	res := c.call(t, sid, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true})
	var r struct {
		Result struct {
			Value float64 `json:"value"`
		} `json:"result"`
	}
	_ = json.Unmarshal(res, &r)
	return r.Result.Value
}

func (c *cdpClient) evalString(t *testing.T, sid, expr string) string {
	t.Helper()
	res := c.call(t, sid, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true})
	var r struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	_ = json.Unmarshal(res, &r)
	return r.Result.Value
}

// TestE2EHumanizedClickTogglesRealWidget drives many humanized clicks through the
// real proxy against a real Chrome and asserts each one flips aria-expanded - i.e.
// the humanized click reliably registers on a blur-sensitive, overlay-backed
// widget, with no dropped or double clicks.
func TestE2EHumanizedClickTogglesRealWidget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	browserWS := launchChrome(t)

	// Stand up the real proxy in front of the real browser endpoint.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientWS, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		proxyCDPWebsocket(context.Background(), clientWS, browserWS, "e2e", cdpSessionOpts{humanize: true})
	}))
	defer proxy.Close()

	c := dialCDP(ctx, t, "ws"+strings.TrimPrefix(proxy.URL, "http"))

	// Create a page, attach flat, load the widget.
	tgt := c.call(t, "", "Target.createTarget", map[string]any{"url": "about:blank"})
	var tt struct {
		TargetID string `json:"targetId"`
	}
	_ = json.Unmarshal(tgt, &tt)
	att := c.call(t, "", "Target.attachToTarget", map[string]any{"targetId": tt.TargetID, "flatten": true})
	var at struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(att, &at)
	sid := at.SessionID
	if sid == "" {
		t.Fatal("no sessionId from attachToTarget")
	}

	c.call(t, sid, "Page.enable", nil)
	c.call(t, sid, "Runtime.enable", nil)
	if c.evalNumber(t, sid, widgetSetupJS) != 1 {
		t.Fatal("widget setup did not return 1")
	}

	cx := c.evalNumber(t, sid, "(function(){var r=trigger.getBoundingClientRect();return r.left+r.width/2;})()")
	cy := c.evalNumber(t, sid, "(function(){var r=trigger.getBoundingClientRect();return r.top+r.height/2;})()")
	t.Logf("trigger centre = (%.0f,%.0f)", cx, cy)
	if cx < 1 || cy < 1 {
		t.Fatalf("bad trigger coords (%.0f,%.0f) - widget not laid out", cx, cy)
	}

	// Control: a direct DOM click must toggle the widget (proves the widget,
	// session, and read path all work before we exercise the humanizer).
	c.call(t, sid, "Runtime.evaluate", map[string]any{"expression": "trigger.click()"})
	if got := c.evalString(t, sid, "trigger.getAttribute('aria-expanded')"); got != "true" {
		t.Fatalf("control DOM click left aria-expanded=%q, want true", got)
	}
	c.call(t, sid, "Runtime.evaluate", map[string]any{"expression": "trigger.click()"}) // reset to false

	const iterations = 10
	for i := 1; i <= iterations; i++ {
		want := "true"
		if i%2 == 0 {
			want = "false"
		}
		// One humanized click = move + press + release, exactly as a driver sends it.
		c.call(t, sid, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": cx, "y": cy})
		c.call(t, sid, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": cx, "y": cy, "button": "left", "clickCount": 1, "buttons": 1})
		c.call(t, sid, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": cx, "y": cy, "button": "left", "clickCount": 1, "buttons": 0})

		// Let the post-click verify (and any re-click) settle, then read state.
		var got string
		for j := 0; j < 20; j++ {
			time.Sleep(50 * time.Millisecond)
			got = c.evalString(t, sid, "document.getElementById('trigger').getAttribute('aria-expanded')")
			if got == want {
				break
			}
		}
		if got != want {
			t.Fatalf("click %d: aria-expanded=%q, want %q (humanized click did not register cleanly)", i, got, want)
		}
	}
	fmt.Printf("e2e: %d humanized clicks each toggled the widget cleanly\n", iterations)
}
