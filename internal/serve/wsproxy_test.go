package serve

import (
	"bytes"
	"context"
	"encoding/json"
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
		swallow, cmd := handleProxyAuth(in, map[int64]struct{}{}, injectedIDBase, "bob", "secret")
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
		swallow, cmd := handleProxyAuth(in, map[int64]struct{}{}, injectedIDBase, "bob", "secret")
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
		swallow, cmd := handleProxyAuth(in, map[int64]struct{}{}, injectedIDBase, "bob", "secret")
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

			var mu sync.Mutex
			var browserGot []map[string]any

			browser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
					mu.Lock()
					browserGot = append(browserGot, m)
					mu.Unlock()
					ack, _ := json.Marshal(map[string]any{
						"id":     m["id"],
						"result": map[string]any{"browserContextId": "BC1"},
					})
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
				proxyCDPWebsocket(context.Background(), clientWS, target, "test",
					cdpSessionOpts{allowContexts: tc.allowContexts})
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

			mu.Lock()
			defer mu.Unlock()
			forwarded := slices.ContainsFunc(browserGot, func(m map[string]any) bool {
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
func TestSwallowInjected(t *testing.T) {
	t.Parallel()

	t.Run("our response swallowed and id discarded", func(t *testing.T) {
		t.Parallel()
		ids := map[int64]struct{}{injectedIDBase: {}}
		if !swallowInjected([]byte(`{"id":2000000000,"result":{}}`), ids) {
			t.Fatal("response to an injected id must be swallowed")
		}
		if _, ok := ids[injectedIDBase]; ok {
			t.Error("injected id should be discarded after its response")
		}
	})

	t.Run("client's own response forwarded", func(t *testing.T) {
		t.Parallel()
		ids := map[int64]struct{}{injectedIDBase: {}}
		if swallowInjected([]byte(`{"id":7,"result":{"ok":true}}`), ids) {
			t.Error("a client id must never be swallowed")
		}
		if len(ids) != 1 {
			t.Error("unrelated frame must not consume an injected id")
		}
	})

	t.Run("event without an id forwarded", func(t *testing.T) {
		t.Parallel()
		if swallowInjected([]byte(`{"method":"Page.loadEventFired","params":{}}`), map[int64]struct{}{injectedIDBase: {}}) {
			t.Error("events carry no id and must pass through")
		}
	})
}

// The fork's --fingerprint-locale moves navigator.language but not ICU's default
// locale, so Intl keeps reporting en-US. The proxy pins it per page session.
func TestInjectLocaleOverride(t *testing.T) {
	t.Parallel()

	attached := func(targetType, sessionID string) []byte {
		return []byte(`{"method":"Target.attachedToTarget","params":{"sessionId":"` + sessionID +
			`","targetInfo":{"type":"` + targetType + `","targetId":"T1"}}}`)
	}

	t.Run("page session pinned to the seed locale", func(t *testing.T) {
		t.Parallel()
		cmd := injectLocaleOverride(attached("page", "S1"), "pt-PT", injectedIDBase)
		if cmd == nil {
			t.Fatal("a page attach must yield a locale override")
		}
		out := decode(t, cmd)
		if out["method"] != "Emulation.setLocaleOverride" || out["sessionId"] != "S1" {
			t.Fatalf("cmd=%v", out)
		}
		if got := out["params"].(map[string]any)["locale"]; got != "pt-PT" {
			t.Errorf("locale=%v, want pt-PT", got)
		}
		// decode() uses plain json.Unmarshal, so numbers land as float64 (the
		// production decodeCDP uses UseNumber, which is what asInt expects).
		if id, ok := out["id"].(float64); !ok || int64(id) != injectedIDBase {
			t.Errorf("id=%v, want the injected id so its response is swallowed", out["id"])
		}
	})

	t.Run("non-page targets skipped", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []string{"service_worker", "worker", "browser"} {
			if cmd := injectLocaleOverride(attached(tt, "S1"), "pt-PT", injectedIDBase); cmd != nil {
				t.Errorf("%s: setLocaleOverride is page-only, got %s", tt, cmd)
			}
		}
	})

	t.Run("attach without a session skipped", func(t *testing.T) {
		t.Parallel()
		if cmd := injectLocaleOverride(attached("page", ""), "pt-PT", injectedIDBase); cmd != nil {
			t.Errorf("no sessionId to address, got %s", cmd)
		}
	})

	t.Run("unrelated frame skipped", func(t *testing.T) {
		t.Parallel()
		if cmd := injectLocaleOverride([]byte(`{"id":7,"result":{}}`), "pt-PT", injectedIDBase); cmd != nil {
			t.Errorf("only attachedToTarget triggers the override, got %s", cmd)
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
