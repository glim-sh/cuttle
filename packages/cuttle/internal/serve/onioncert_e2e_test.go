//go:build e2e

package serve

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestE2EOnionCertificates(t *testing.T) {
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "https://redirect.test/", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "<title>onion-test</title>ok")
	}))
	defer tls.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusBadRequest)
			return
		}
		upstream, err := net.DialTimeout("tcp", tls.Listener.Addr().String(), time.Second)
		if err != nil {
			http.Error(w, "upstream", http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		downstream, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer downstream.Close()
		_, _ = io.WriteString(downstream, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { _, _ = io.Copy(upstream, downstream) }()
		_, _ = io.Copy(downstream, upstream)
	}))
	defer proxy.Close()
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			endpoint := launchChrome(t, "--proxy-server="+proxy.URL, "--disable-background-networking")
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			dial := func() *cdpConn {
				ws, _, err := websocket.Dial(ctx, endpoint, nil)
				if err != nil {
					t.Fatal(err)
				}
				c := &cdpConn{ws: ws}
				t.Cleanup(c.close)
				return c
			}
			if enabled {
				policy := dial()
				if err := enableOnionCertificates(ctx, policy); err != nil {
					t.Fatal(err)
				}
				go func() { _ = runOnionCertificates(ctx, policy) }()
			}
			client := dial()
			version, err := client.call(ctx, "", "Browser.getVersion", nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("browser: %v", version["product"])
			if enabled {
				res, err := client.call(ctx, "", "Target.createTarget", map[string]any{"url": "https://unattached.onion/"})
				if err != nil {
					t.Fatal(err)
				}
				sid, err := client.attach(ctx, asString(res["targetId"]))
				if err != nil {
					t.Fatal(err)
				}
				awaitTitle := func(session string) {
					t.Helper()
					for ctx.Err() == nil {
						v, err := client.call(ctx, session, "Runtime.evaluate", map[string]any{"expression": "document.title"})
						if err != nil {
							t.Fatal(err)
						}
						r, _ := v["result"].(map[string]any)
						if r["value"] == "onion-test" {
							return
						}
						time.Sleep(10 * time.Millisecond)
					}
					t.Fatal("page did not load")
				}
				awaitTitle(sid)
				_, err = client.call(ctx, sid, "Runtime.evaluate", map[string]any{"expression": "window.open('https://popup.onion/')", "userGesture": true})
				if err != nil {
					t.Fatal(err)
				}
				targets, err := client.call(ctx, "", "Target.getTargets", nil)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, raw := range targets["targetInfos"].([]any) {
					info := raw.(map[string]any)
					if info["openerId"] == res["targetId"] {
						popup, err := client.attach(ctx, asString(info["targetId"]))
						if err != nil {
							t.Fatal(err)
						}
						awaitTitle(popup)
						found = true
					}
				}
				if !found {
					t.Fatal("popup missing")
				}
				v, err := client.call(ctx, sid, "Runtime.evaluate", map[string]any{"expression": "fetch('https://resource.test/', {mode:'no-cors'}).then(()=>false,()=>true)", "awaitPromise": true})
				if err != nil {
					t.Fatal(err)
				}
				if v["result"].(map[string]any)["value"] != true {
					t.Fatal("clearnet subresource accepted")
				}
				nav, err := client.call(ctx, sid, "Page.navigate", map[string]any{"url": "https://unattached.onion/redirect"})
				if err != nil || asString(nav["errorText"]) == "" {
					t.Fatalf("clearnet redirect accepted: %v %v", nav, err)
				}
			}
			for _, host := range []string{"first.onion", "clear.test", "second.onion", "fake.onion.test"} {
				res, err := client.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"})
				if err != nil {
					t.Fatal(err)
				}
				sid, err := client.attach(ctx, asString(res["targetId"]))
				if err != nil {
					t.Fatal(err)
				}
				nav, err := client.call(ctx, sid, "Page.navigate", map[string]any{"url": "https://" + host + "/"})
				if err != nil {
					t.Fatal(err)
				}
				wantSuccess := enabled && strings.HasSuffix(host, ".onion")
				if got := asString(nav["errorText"]); (got == "") != wantSuccess {
					t.Fatalf("%s: navigation=%v, want success=%v", host, nav, wantSuccess)
				}
			}
		})
	}
}
