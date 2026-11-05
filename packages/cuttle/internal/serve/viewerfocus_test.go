package serve

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestViewerFocusRejectsRemotePeer(t *testing.T) {
	m := &multiplexer{}
	req := httptest.NewRequest(http.MethodGet, "http://localhost:9222/viewer/focus", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	response := httptest.NewRecorder()
	m.handleViewerFocus(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
}

// fakeTabsBrowser serves tab targets the way Chrome does: attaching to a tab and
// auto-attaching on that session announces its page before the reply. Each tab
// is named by its id; tabActive is the tab strip's state (nil leaves it out),
// and focused names the pages whose sample reports focus - under a driver's
// focus emulation that is every page, foreground or not.
func fakeTabsBrowser(t *testing.T, tabActive map[string]any, focused ...string) (*cdpRecorder, int) {
	t.Helper()
	tabs := make([]any, 0, 2)
	for _, id := range []string{"A", "B"} {
		tab := map[string]any{"targetId": id, "type": "tab"}
		if active, ok := tabActive[id]; ok {
			tab["embedderData"] = map[string]any{"tabActive": active}
		}
		tabs = append(tabs, tab)
	}
	rec, wsURL := startCDPBrowser(t, func(cmd map[string]any) map[string]any {
		params, _ := cmd[cdpParams].(map[string]any)
		sid := asString(cmd[cdpSessionID])
		switch cmd[cdpMethod] {
		case "Target.getTargets":
			return map[string]any{"targetInfos": tabs}
		case "Target.attachToTarget":
			return map[string]any{"sessionId": "tab-" + asString(params[keyTargetID])}
		case "Page.getFrameTree":
			return map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "F"}}}
		case "Page.createIsolatedWorld":
			return map[string]any{"executionContextId": 7}
		case "Runtime.callFunctionOn":
			if slices.Contains(focused, strings.TrimPrefix(sid, "page-")) {
				return map[string]any{cdpResult: map[string]any{cdpValue: map[string]any{"fields": []any{}, "focus": nil}}}
			}
			return map[string]any{cdpResult: map[string]any{cdpValue: nil}}
		}
		return map[string]any{}
	}, func(cmd map[string]any) ([]map[string]any, []map[string]any) {
		sid := asString(cmd[cdpSessionID])
		if cmd[cdpMethod] != "Target.setAutoAttach" || !strings.HasPrefix(sid, "tab-") {
			return nil, nil
		}
		id := strings.TrimPrefix(sid, "tab-")
		announce := func(page string, info map[string]any) map[string]any {
			info["targetId"], info["type"] = "page-target-"+page, targetPage
			return map[string]any{cdpMethod: methodAttachedToTarget, cdpSessionID: sid, cdpParams: map[string]any{
				"sessionId": "page-" + page, "targetInfo": info,
			}}
		}
		// The tab's prerendering page is announced too, on either side of the one it shows.
		return []map[string]any{
			announce(id+"-prerender", map[string]any{"subtype": "prerender"}),
			announce(id, map[string]any{}),
			announce(id+"-prerender2", map[string]any{"subtype": "prerender"}),
		}, nil
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"` + wsURL + `"}`))
	}))
	t.Cleanup(srv.Close)
	return rec, serverPort(t, srv)
}

func sampledPages(rec *cdpRecorder) []string {
	var sids []string
	for _, cmd := range rec.received() {
		if cmd[cdpMethod] == "Runtime.callFunctionOn" {
			sids = append(sids, asString(cmd[cdpSessionID]))
		}
	}
	return sids
}

// Focus emulation makes the background tab claim focus too; the tab strip says
// which one X input reaches, so only that one is sampled and reported.
func TestViewerFocusReportsForegroundTabUnderFocusEmulation(t *testing.T) {
	t.Parallel()
	rec, port := fakeTabsBrowser(t, map[string]any{"A": false, "B": true}, "A", "B")
	state, err := readViewerFocus(t.Context(), port)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state["target"] != "page-target-B" {
		t.Fatalf("state = %v, want the foreground tab's page", state)
	}
	if got := sampledPages(rec); !slices.Equal(got, []string{"page-B"}) {
		t.Errorf("sampled %v, want only the foreground page", got)
	}
}

// Two windows each have a foreground tab; with both claiming focus, which one
// holds X focus is unknown, so nothing is predicted.
func TestViewerFocusRefusesTwoFocusedForegroundTabs(t *testing.T) {
	t.Parallel()
	_, port := fakeTabsBrowser(t, map[string]any{"A": true, "B": true}, "A", "B")
	state, err := readViewerFocus(t.Context(), port)
	if err != nil || state != nil {
		t.Fatalf("state = %v, err = %v, want nil", state, err)
	}
}

// A browser that reports no tab-strip state keeps every tab a candidate, so the
// page's own focus still decides.
func TestViewerFocusWithoutTabStripStateFallsBackToPageFocus(t *testing.T) {
	t.Parallel()
	_, port := fakeTabsBrowser(t, nil, "A")
	state, err := readViewerFocus(t.Context(), port)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state["target"] != "page-target-A" {
		t.Fatalf("state = %v, want the focused page", state)
	}
}
