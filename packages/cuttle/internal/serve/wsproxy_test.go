package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestStampSWContext(t *testing.T) {
	t.Parallel()

	t.Run("stamps empty service_worker context", func(t *testing.T) {
		t.Parallel()
		in := []byte(`{"method":"Target.attachedToTarget","params":{"targetInfo":{"type":"service_worker","browserContextId":""}}}`)
		out := decode(t, stampSWContext(in))
		ti := out["params"].(map[string]any)["targetInfo"].(map[string]any)
		if ti["browserContextId"] != synthBrowserContextID {
			t.Errorf("browserContextId=%v", ti["browserContextId"])
		}
	})

	t.Run("stamps missing service_worker context", func(t *testing.T) {
		t.Parallel()
		in := []byte(`{"method":"Target.attachedToTarget","params":{"targetInfo":{"type":"service_worker"}}}`)
		out := decode(t, stampSWContext(in))
		ti := out["params"].(map[string]any)["targetInfo"].(map[string]any)
		if ti["browserContextId"] != synthBrowserContextID {
			t.Errorf("browserContextId=%v", ti["browserContextId"])
		}
	})

	t.Run("leaves populated context untouched", func(t *testing.T) {
		t.Parallel()
		in := []byte(`{"method":"Target.attachedToTarget","params":{"targetInfo":{"type":"service_worker","browserContextId":"REAL"}}}`)
		if string(stampSWContext(in)) != string(in) {
			t.Errorf("should be unchanged")
		}
	})

	t.Run("leaves non-service-worker untouched", func(t *testing.T) {
		t.Parallel()
		in := []byte(`{"method":"Target.attachedToTarget","params":{"targetInfo":{"type":"page","browserContextId":""}}}`)
		if string(stampSWContext(in)) != string(in) {
			t.Errorf("should be unchanged")
		}
	})

	t.Run("leaves unrelated frames byte-identical", func(t *testing.T) {
		t.Parallel()
		in := []byte(`{"id":1,"result":{}}`)
		if string(stampSWContext(in)) != string(in) {
			t.Errorf("should be unchanged")
		}
	})
}

func TestRewriteFetchEnable(t *testing.T) {
	t.Parallel()

	t.Run("adds handleAuthRequests", func(t *testing.T) {
		t.Parallel()
		out := decode(t, rewriteFetchEnable([]byte(`{"id":5,"method":"Fetch.enable","params":{}}`)))
		if out["params"].(map[string]any)["handleAuthRequests"] != true {
			t.Errorf("handleAuthRequests not set: %v", out)
		}
	})

	t.Run("adds params when absent", func(t *testing.T) {
		t.Parallel()
		out := decode(t, rewriteFetchEnable([]byte(`{"id":5,"method":"Fetch.enable"}`)))
		if out["params"].(map[string]any)["handleAuthRequests"] != true {
			t.Errorf("handleAuthRequests not set: %v", out)
		}
	})

	t.Run("already-true left byte-identical", func(t *testing.T) {
		t.Parallel()
		in := []byte(`{"id":5,"method":"Fetch.enable","params":{"handleAuthRequests":true}}`)
		if string(rewriteFetchEnable(in)) != string(in) {
			t.Errorf("should be unchanged")
		}
	})

	t.Run("non-fetch untouched", func(t *testing.T) {
		t.Parallel()
		in := []byte(`{"id":5,"method":"Page.enable"}`)
		if string(rewriteFetchEnable(in)) != string(in) {
			t.Errorf("should be unchanged")
		}
	})
}

func TestHandleProxyAuth(t *testing.T) {
	t.Parallel()

	t.Run("proxy challenge answered with credentials and swallowed", func(t *testing.T) {
		t.Parallel()
		in := []byte(`{"method":"Fetch.authRequired","sessionId":"S1","params":{"requestId":"R1","authChallenge":{"source":"Proxy"}}}`)
		swallow, cmd := handleProxyAuth(in, map[int64]string{}, injectedIDBase, "bob", "secret")
		if !swallow {
			t.Fatal("authRequired must be swallowed")
		}
		out := decode(t, cmd)
		if out["method"] != "Fetch.continueWithAuth" || out["sessionId"] != "S1" {
			t.Errorf("cmd=%v", out)
		}
		resp := out["params"].(map[string]any)["authChallengeResponse"].(map[string]any)
		if resp["response"] != "ProvideCredentials" || resp["username"] != "bob" || resp["password"] != "secret" {
			t.Errorf("auth response=%v", resp)
		}
	})

	t.Run("non-proxy challenge answered with default", func(t *testing.T) {
		t.Parallel()
		in := []byte(`{"method":"Fetch.authRequired","params":{"requestId":"R1","authChallenge":{"source":"Server"}}}`)
		swallow, cmd := handleProxyAuth(in, map[int64]string{}, injectedIDBase, "bob", "secret")
		if !swallow {
			t.Fatal("must swallow")
		}
		resp := decode(t, cmd)["params"].(map[string]any)["authChallengeResponse"].(map[string]any)
		if resp["response"] != "Default" {
			t.Errorf("want Default response, got %v", resp)
		}
	})

	t.Run("ordinary frame forwarded", func(t *testing.T) {
		t.Parallel()
		in := []byte(`{"id":7,"result":{"ok":true}}`)
		swallow, cmd := handleProxyAuth(in, map[int64]string{}, injectedIDBase, "bob", "secret")
		if swallow || cmd != nil {
			t.Errorf("ordinary frame must pass through: swallow=%v", swallow)
		}
	})
}

func TestBlockContextCreation(t *testing.T) {
	blocked, resp := blockContextCreation([]byte(
		`{"id":42,"sessionId":"S1","method":"Target.createBrowserContext","params":{}}`,
	))
	if !blocked {
		t.Fatal("Target.createBrowserContext must be blocked")
	}
	msg := decode(t, resp)
	if msg["id"] != float64(42) {
		t.Errorf("id = %v, want 42", msg["id"])
	}
	if msg["sessionId"] != "S1" {
		t.Errorf("sessionId = %v, want S1", msg["sessionId"])
	}
	if _, ok := msg["error"]; !ok {
		t.Error("blocked response must carry an error object")
	}

	if b, _ := blockContextCreation([]byte(`{"id":1,"method":"Target.createTarget","params":{}}`)); b {
		t.Error("Target.createTarget must pass through")
	}
	// A mere mention of the method inside an unrelated command must not trip it.
	if b, _ := blockContextCreation([]byte(
		`{"id":1,"method":"Runtime.evaluate","params":{"expression":"Target.createBrowserContext"}}`,
	)); b {
		t.Error("substring mention must not be blocked")
	}
}

// A created context may exist, but it may not pick its own identity: the
// fingerprint rides launch flags, so a driver-supplied proxy would egress
// elsewhere while still wearing the seed's fingerprint and timezone.
func TestStripContextIdentityOverrides(t *testing.T) {
	t.Parallel()

	t.Run("drops proxy params, keeps the rest", func(t *testing.T) {
		t.Parallel()
		in := []byte(`{"id":3,"method":"Target.createBrowserContext","params":` +
			`{"proxyServer":"http://evil:8080","proxyBypassList":"<local>","disposeOnDetach":true}}`)
		out := decode(t, stripContextIdentityOverrides(in))
		params := out["params"].(map[string]any)
		for _, k := range []string{"proxyServer", "proxyBypassList"} {
			if _, present := params[k]; present {
				t.Errorf("%s must be stripped: %v", k, params)
			}
		}
		if params["disposeOnDetach"] != true {
			t.Errorf("unrelated params must survive: %v", params)
		}
		if out["id"] != float64(3) || out["method"] != "Target.createBrowserContext" {
			t.Errorf("id/method must survive: %v", out)
		}
	})

	t.Run("clean command left byte-identical", func(t *testing.T) {
		t.Parallel()
		in := []byte(`{"id":3,"method":"Target.createBrowserContext","params":{}}`)
		if string(stripContextIdentityOverrides(in)) != string(in) {
			t.Error("a command with nothing to strip must not be re-serialized")
		}
	})

	t.Run("other methods untouched", func(t *testing.T) {
		t.Parallel()
		in := []byte(`{"id":1,"method":"Target.createTarget","params":{"proxyServer":"http://x:1"}}`)
		if string(stripContextIdentityOverrides(in)) != string(in) {
			t.Error("only createBrowserContext is rewritten")
		}
	})
}

// TestAllowContextCreation pins the --allow-context-creation contract end to end
// through a real proxyCDPWebsocket: with the opt-out on, the driver's
// Target.createBrowserContext must reach the browser instead of being answered
// with the guardrail error. The default (off) path is asserted alongside it, so a
// regression that inverts the flag fails here rather than in a consumer.
func TestAllowContextCreation(t *testing.T) {
	t.Parallel()

	const createCtx = `{"id":9,"method":"Target.createBrowserContext","params":{}}`

	for _, tc := range []struct {
		name          string
		allowContexts bool
		wantForwarded bool
	}{
		{"blocked by default", false, false},
		{"forwarded when allowed", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			browser, target := startCDPRecorder(t, func(map[string]any) map[string]any {
				return map[string]any{"browserContextId": "BC1"}
			})
			proxy := startCDPProxy(t, target, cdpSessionOpts{allowContexts: tc.allowContexts})
			cl := dialCDPClient(ctx, t, proxy)

			if werr := cl.Write(ctx, websocket.MessageText, []byte(createCtx)); werr != nil {
				t.Fatalf("client write: %v", werr)
			}

			_, respData, err := cl.Read(ctx)
			if err != nil {
				t.Fatalf("client read: %v", err)
			}
			got := decode(t, respData)
			if id, _ := got["id"].(float64); id != 9 {
				t.Fatalf("response id = %v, want 9", got["id"])
			}
			_, isError := got["error"]
			if tc.wantForwarded == isError {
				t.Fatalf("allowContexts=%v: error-in-response=%v, want %v",
					tc.allowContexts, isError, !tc.wantForwarded)
			}

			forwarded := slices.ContainsFunc(browser.received(), func(m map[string]any) bool {
				return m["method"] == "Target.createBrowserContext"
			})
			if forwarded != tc.wantForwarded {
				t.Errorf("allowContexts=%v: reached browser=%v, want %v",
					tc.allowContexts, forwarded, tc.wantForwarded)
			}
		})
	}
}

// Responses to proxy-originated commands are consumed centrally (they used to be
// handled inside handleProxyAuth, which only covered proxy-auth ids).
func TestBlockBrowserTeardown(t *testing.T) {
	blocked, resp := blockBrowserTeardown([]byte(
		`{"id":7,"sessionId":"S1","method":"Browser.close"}`,
	))
	if !blocked {
		t.Fatal("Browser.close must not reach the browser")
	}
	msg := decode(t, resp)
	if msg["id"] != float64(7) {
		t.Errorf("id = %v, want 7", msg["id"])
	}
	if msg["sessionId"] != "S1" {
		t.Errorf("sessionId = %v, want S1", msg["sessionId"])
	}
	// connectOverCDP treats close as "I am done"; answering success and dropping
	// this client is that, without taking the seed down for everyone else.
	if _, ok := msg["error"]; ok {
		t.Error("teardown must be acked as success, not refused")
	}

	for _, m := range []string{"Browser.crash", "Browser.crashGpuProcess"} {
		if b, _ := blockBrowserTeardown([]byte(`{"id":1,"method":"` + m + `"}`)); !b {
			t.Errorf("%s must be blocked", m)
		}
	}
	if b, _ := blockBrowserTeardown([]byte(`{"id":1,"method":"Browser.getVersion"}`)); b {
		t.Error("Browser.getVersion must pass through")
	}
	if b, _ := blockBrowserTeardown([]byte(
		`{"id":1,"method":"Runtime.evaluate","params":{"expression":"Browser.close"}}`,
	)); b {
		t.Error("substring mention must not be blocked")
	}
}

// A bfcached execution context stays VALID across a navigation, so the
// evaluate-error path never fires; without passive invalidation the settle gate
// and toggle verify silently probe the document the page already left.
func TestInvalidateWorld(t *testing.T) {
	newH := func() *humanizer {
		h := newHumanizer(context.Background(), true, nil, "", nil, nil)
		h.worlds["S1"] = 99
		return h
	}

	for _, tc := range []struct {
		name  string
		frame string
		gone  bool
	}{
		{"main frame navigation", `{"sessionId":"S1","method":"Page.frameNavigated","params":{"frame":{"id":"F1"}}}`, true},
		{"subframe navigation", `{"sessionId":"S1","method":"Page.frameNavigated","params":{"frame":{"id":"F2","parentId":"F1"}}}`, false},
		{"our context destroyed", `{"sessionId":"S1","method":"Runtime.executionContextDestroyed","params":{"executionContextId":99}}`, true},
		{"other context destroyed", `{"sessionId":"S1","method":"Runtime.executionContextDestroyed","params":{"executionContextId":5}}`, false},
		{"contexts cleared", `{"sessionId":"S1","method":"Runtime.executionContextsCleared","params":{}}`, true},
		{"unrelated event", `{"sessionId":"S1","method":"Page.loadEventFired","params":{}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newH()
			h.invalidateWorld([]byte(tc.frame))
			_, cached := h.worlds["S1"]
			if cached == tc.gone {
				t.Errorf("cached=%v, want dropped=%v", cached, tc.gone)
			}
		})
	}
}

func TestSwallowInjected(t *testing.T) {
	t.Parallel()

	t.Run("our response swallowed and id discarded", func(t *testing.T) {
		t.Parallel()
		ids := map[int64]string{injectedIDBase: "Emulation.setFocusEmulationEnabled"}
		if !swallowInjected([]byte(`{"id":2000000000,"result":{}}`), ids) {
			t.Fatal("response to an injected id must be swallowed")
		}
		if _, ok := ids[injectedIDBase]; ok {
			t.Error("injected id should be discarded after its response")
		}
	})

	t.Run("client's own response forwarded", func(t *testing.T) {
		t.Parallel()
		ids := map[int64]string{injectedIDBase: "Emulation.setFocusEmulationEnabled"}
		if swallowInjected([]byte(`{"id":7,"result":{"ok":true}}`), ids) {
			t.Error("a client id must never be swallowed")
		}
		if len(ids) != 1 {
			t.Error("unrelated frame must not consume an injected id")
		}
	})

	// The locale collision is expected (the claim is per session, the ICU override
	// per renderer process) so it is demoted - but ONLY for that exact pairing. A
	// blanket swallow would hide a real injected-command failure.
	t.Run("known locale collision is swallowed, other failures are not", func(t *testing.T) {
		t.Parallel()
		const errFrame = `{"id":2000000000,"error":{"code":-32000,"message":%q}}`
		for _, tc := range []struct {
			name, method, message string
		}{
			{"locale collision", methodSetLocaleOverride, "Another locale override is already in effect"},
			{"other locale failure", methodSetLocaleOverride, "Invalid locale"},
			{"other method, same text", methodSetFocusEmulation, "Another locale override is already in effect"},
		} {
			ids := map[int64]string{injectedIDBase: tc.method}
			frame := fmt.Appendf(nil, errFrame, tc.message)
			if !swallowInjected(frame, ids) {
				t.Errorf("%s: an injected id's response must always be swallowed", tc.name)
			}
			if _, ok := ids[injectedIDBase]; ok {
				t.Errorf("%s: injected id should be discarded", tc.name)
			}
		}
	})

	t.Run("event without an id forwarded", func(t *testing.T) {
		t.Parallel()
		if swallowInjected([]byte(`{"method":"Page.loadEventFired","params":{}}`), map[int64]string{injectedIDBase: "Emulation.setFocusEmulationEnabled"}) {
			t.Error("events carry no id and must pass through")
		}
	})
}

// Both per-page pins key off the same frame: focus emulation (so a background tab
// keeps compositing and a driver's rAF-based actionability wait cannot hang) and
// the ICU/Intl locale (the fork's --fingerprint-locale moves navigator.language
// but not ICU's default, so Intl keeps reporting en-US).
func TestAttachedPagePins(t *testing.T) {
	t.Parallel()

	attached := func(targetType, sessionID string) []byte {
		return []byte(`{"method":"Target.attachedToTarget","params":{"sessionId":"` + sessionID +
			`","targetInfo":{"type":"` + targetType + `","targetId":"T1"}}}`)
	}

	t.Run("page attach yields a session id", func(t *testing.T) {
		t.Parallel()
		if got := attachedPageSession(attached("page", "S1")); got != "S1" {
			t.Fatalf("sessionID=%q, want S1", got)
		}
	})

	t.Run("non-page targets skipped", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []string{"service_worker", "worker", "browser"} {
			if got := attachedPageSession(attached(tt, "S1")); got != "" {
				t.Errorf("%s: the pins are page-only, got %q", tt, got)
			}
		}
	})

	t.Run("attach without a session skipped", func(t *testing.T) {
		t.Parallel()
		if got := attachedPageSession(attached("page", "")); got != "" {
			t.Errorf("no sessionId to address, got %q", got)
		}
	})

	t.Run("unrelated frame skipped", func(t *testing.T) {
		t.Parallel()
		if got := attachedPageSession([]byte(`{"id":7,"result":{}}`)); got != "" {
			t.Errorf("only attachedToTarget triggers the pins, got %q", got)
		}
	})

	t.Run("focus emulation pinned on the attached session", func(t *testing.T) {
		t.Parallel()
		out := decode(t, dispatchCmd(injectedIDBase, methodSetFocusEmulation, "S1", map[string]any{"enabled": true}))
		if out["method"] != "Emulation.setFocusEmulationEnabled" || out["sessionId"] != "S1" {
			t.Fatalf("cmd=%v", out)
		}
		if enabled := out["params"].(map[string]any)["enabled"]; enabled != true {
			t.Errorf("enabled=%v, want true - a disabled pin leaves the tab un-clickable", enabled)
		}
		// decode() uses plain json.Unmarshal, so numbers land as float64 (the
		// production decodeCDP uses UseNumber, which is what asInt expects).
		if id, ok := out["id"].(float64); !ok || int64(id) != injectedIDBase {
			t.Errorf("id=%v, want the injected id so its response is swallowed", out["id"])
		}
	})

	t.Run("page-endpoint clients pin without a session id", func(t *testing.T) {
		t.Parallel()
		// A /devtools/page/<id> client drives its target directly, so the command
		// carries no sessionId - it must still be addressed, not dropped.
		out := decode(t, dispatchCmd(injectedIDBase, methodSetFocusEmulation, "", map[string]any{"enabled": true}))
		if _, ok := out["sessionId"]; ok {
			t.Errorf("a direct page session must not carry a sessionId: %v", out)
		}
	})

	t.Run("locale pinned to the seed locale", func(t *testing.T) {
		t.Parallel()
		out := decode(t, dispatchCmd(injectedIDBase, methodSetLocaleOverride, "S1", map[string]any{keyLocale: "pt-PT"}))
		if out["method"] != "Emulation.setLocaleOverride" || out["sessionId"] != "S1" {
			t.Fatalf("cmd=%v", out)
		}
		if got := out["params"].(map[string]any)["locale"]; got != "pt-PT" {
			t.Errorf("locale=%v, want pt-PT", got)
		}
	})
}

// The hot-path prefilter is coupled to injectedIDBase: if the base moves, the
// byte guard silently stops matching and every injected response is forwarded to
// the driver instead of being swallowed.
func TestInjectedIDPrefilterMatchesBase(t *testing.T) {
	t.Parallel()
	frame := []byte(`{"id":` + strconv.FormatInt(injectedIDBase, 10) + `,"result":{}}`)
	if !bytes.Contains(frame, injectedIDPrefilter) {
		t.Fatalf("prefilter %q does not match an id at injectedIDBase (%d)", injectedIDPrefilter, injectedIDBase)
	}
	// And it must not match an ordinary driver id.
	driverFrame := []byte(`{"id":7,"result":{}}`)
	if bytes.Contains(driverFrame, injectedIDPrefilter) {
		t.Errorf("prefilter %q matches a driver id", injectedIDPrefilter)
	}
}

// startDialogBrowser serves a fake browser in which the command Test.alert opens
// a native dialog on the sending session and Page.handleJavaScriptDialog closes
// it, each announced the way Chrome does. Test.chain opens one whose dismissal
// sets off a second, as a cancelled confirm that alerts does.
func startDialogBrowser(t *testing.T) (*cdpRecorder, string) {
	t.Helper()
	var mu sync.Mutex
	chained := map[any]bool{} // sessions whose next dismissal opens another dialog
	event := func(method string, cmd, params map[string]any) map[string]any {
		return map[string]any{"method": method, "params": params, "sessionId": cmd["sessionId"]}
	}
	opening := func(cmd map[string]any) map[string]any {
		return event(methodDialogOpening, cmd, map[string]any{"type": "confirm", "message": "1"})
	}
	return startCDPBrowser(t, nil, func(cmd map[string]any) ([]map[string]any, []map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		switch cmd["method"] {
		case "Test.chain":
			chained[cmd["sessionId"]] = true
			return nil, []map[string]any{opening(cmd)}
		case "Test.alert":
			return nil, []map[string]any{opening(cmd)}
		case methodHandleDialog:
			closed := []map[string]any{event(methodDialogClosed, cmd, map[string]any{"result": false})}
			if chained[cmd["sessionId"]] {
				delete(chained, cmd["sessionId"])
				return closed, []map[string]any{opening(cmd)}
			}
			return closed, nil
		}
		return nil, nil
	})
}

// A dialog the driver leaves open blocks its tab for every later client - Chrome
// only lets the sessions that saw it open answer it - so the proxy dismisses it
// over the departing session, however the driver leaves, and only if it is still
// open.
func TestLeftOpenDialogIsDismissed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		sends []string
		leave func(ctx context.Context, cl *websocket.Conn)
		want  int // Page.handleJavaScriptDialog commands the browser receives
		// accept is the last one's verdict: the proxy dismisses, the driver here accepts.
		accept bool
	}{
		{
			name:  "driver disconnects",
			sends: []string{`{"id":1,"sessionId":"S1","method":"Test.alert"}`},
			leave: func(context.Context, *websocket.Conn) {},
			want:  1,
		},
		{
			name:  "driver sends Browser.close",
			sends: []string{`{"id":1,"sessionId":"S1","method":"Test.alert"}`},
			leave: func(ctx context.Context, cl *websocket.Conn) {
				_ = cl.Write(ctx, websocket.MessageText, []byte(`{"id":9,"method":"Browser.close"}`))
			},
			want: 1,
		},
		{
			name:  "the dismissal sets off another dialog",
			sends: []string{`{"id":1,"sessionId":"S1","method":"Test.chain"}`},
			leave: func(context.Context, *websocket.Conn) {},
			want:  2,
		},
		{
			name: "driver answered it itself",
			sends: []string{
				`{"id":1,"sessionId":"S1","method":"Test.alert"}`,
				`{"id":2,"sessionId":"S1","method":"Page.handleJavaScriptDialog","params":{"accept":true}}`,
			},
			leave:  func(context.Context, *websocket.Conn) {},
			want:   1, // the driver's own
			accept: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			browser, target := startDialogBrowser(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cl := dialCDPClient(ctx, t, startCDPProxy(t, target, cdpSessionOpts{}))
			for _, s := range tt.sends {
				if err := cl.Write(ctx, websocket.MessageText, []byte(s)); err != nil {
					t.Fatal(err)
				}
			}
			// The browser's announcement of the last event is what the driver waits
			// for before it leaves; two frames per command (ack + event).
			for range 2 * len(tt.sends) {
				if _, _, err := cl.Read(ctx); err != nil {
					t.Fatal(err)
				}
			}
			tt.leave(ctx, cl)
			_ = cl.Close(websocket.StatusNormalClosure, "")

			handled := func() []map[string]any {
				var out []map[string]any
				for _, m := range browser.received() {
					if m["method"] == methodHandleDialog {
						out = append(out, m)
					}
				}
				return out
			}
			for len(handled()) < tt.want && ctx.Err() == nil {
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(100 * time.Millisecond) // room for a wrong extra dismissal to arrive
			got := handled()
			if len(got) != tt.want {
				t.Fatalf("browser got %d Page.handleJavaScriptDialog, want %d: %v", len(got), tt.want, browser.received())
			}
			last := got[len(got)-1]
			if last["sessionId"] != "S1" || last["params"].(map[string]any)["accept"] != tt.accept {
				t.Errorf("last Page.handleJavaScriptDialog = %v, want session S1 with accept=%v", last, tt.accept)
			}
		})
	}
}

// TestPinGateHoldsNavigateUntilPinAnswered: a driver's first command on a freshly
// attached page must not reach the browser while the proxy's focus pin on it is
// unanswered - a navigation overtaking that pin crashes the tab's renderer.
func TestPinGateHoldsNavigateUntilPinAnswered(t *testing.T) {
	t.Parallel()
	releasePin := make(chan struct{})
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		var wmu sync.Mutex
		write := func(v any) {
			b, _ := json.Marshal(v)
			wmu.Lock()
			defer wmu.Unlock()
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
			method, _ := m["method"].(string)
			mu.Lock()
			seen = append(seen, method)
			mu.Unlock()
			switch method {
			case "Target.attachToTarget":
				write(map[string]any{"method": methodAttachedToTarget, "params": map[string]any{
					"sessionId": "S1", "targetInfo": map[string]any{"type": "page", "targetId": "T1"},
				}})
				write(map[string]any{"id": m["id"], "result": map[string]any{"sessionId": "S1"}})
			case methodSetFocusEmulation:
				go func(id any) {
					<-releasePin
					write(map[string]any{"id": id, "sessionId": "S1", "result": map[string]any{}})
				}(m["id"])
			default:
				write(map[string]any{"id": m["id"], "result": map[string]any{}})
			}
		}
	}))
	t.Cleanup(srv.Close)

	proxy := startCDPProxy(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/devtools/browser/x", cdpSessionOpts{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cl := dialCDPClient(ctx, t, proxy)
	send := func(s string) {
		if err := cl.Write(ctx, websocket.MessageText, []byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string]any {
		_, b, err := cl.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return decode(t, b)
	}
	sawNavigate := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(seen, "Page.navigate")
	}

	send(`{"id":1,"method":"Target.attachToTarget","params":{"targetId":"T1","flatten":true}}`)
	read() // attachedToTarget
	read() // the attach reply
	send(`{"id":2,"method":"Page.navigate","params":{"url":"https://example.com"},"sessionId":"S1"}`)

	time.Sleep(200 * time.Millisecond)
	if sawNavigate() {
		t.Fatal("Page.navigate reached the browser while the focus pin on its session was unanswered")
	}
	close(releasePin)
	if got := read(); got["id"] != json.Number("2") && got["id"] != float64(2) {
		t.Fatalf("want the navigate's reply once the pin is answered, got %v", got)
	}
	if !sawNavigate() {
		t.Fatal("Page.navigate never reached the browser after the pin was answered")
	}
}
