package serve

import (
	"encoding/json"
	"testing"
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

	t.Run("our injected response swallowed, no command", func(t *testing.T) {
		t.Parallel()
		ids := map[int64]struct{}{injectedIDBase: {}}
		in := []byte(`{"id":2000000000,"result":{}}`)
		swallow, cmd := handleProxyAuth(in, ids, injectedIDBase+1, "bob", "secret")
		if !swallow || cmd != nil {
			t.Errorf("swallow=%v cmd=%v", swallow, cmd)
		}
		if _, ok := ids[injectedIDBase]; ok {
			t.Errorf("injected id should be discarded")
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
