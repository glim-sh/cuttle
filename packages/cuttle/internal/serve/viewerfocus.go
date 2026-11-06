package serve

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

//go:embed viewerfocus.js
var viewerFocusScript string

const (
	viewerFocusInterval = 200 * time.Millisecond // the documented five reports per second per stream
	viewerFocusTimeout  = 2 * time.Second        // bound on one sample's CDP round-trips
	// viewerFocusMaxStreams bounds the CDP sampling load: every stream samples
	// on its own, and the viewer port that proxies it carries no auth.
	viewerFocusMaxStreams = 4

	targetTab = "tab" // a tab-strip entry; the page it shows is its child target
)

func (m *multiplexer) handleViewerFocus(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	if m.viewerFocusStreams.Add(1) > viewerFocusMaxStreams {
		m.viewerFocusStreams.Add(-1)
		http.Error(w, "too many viewer focus streams", http.StatusServiceUnavailable)
		return
	}
	defer m.viewerFocusStreams.Add(-1)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}
	tick := time.NewTicker(viewerFocusInterval)
	defer tick.Stop()
	for !m.draining.Load() {
		ctx, cancel := context.WithTimeout(r.Context(), viewerFocusTimeout)
		inst := m.pool.focusedInstance(ctx)
		if inst == nil {
			cancel()
			return
		}
		state, err := readViewerFocus(ctx, inst.cdpPort)
		cancel()
		// A failed sample sends nothing: a null would close the phone keyboard
		// mid-typing, while the client's staleness check covers a lasting failure.
		if err == nil {
			data, err := json.Marshal(state)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}

func readViewerFocus(ctx context.Context, port int) (map[string]any, error) {
	c, err := dialBrowser(ctx, port)
	if err != nil {
		return nil, err
	}
	defer c.close()
	tabs, err := c.call(ctx, "", "Target.getTargets", map[string]any{
		cdpFilter: []any{map[string]any{cdpType: targetTab}},
	})
	if err != nil {
		return nil, err
	}
	infos, _ := tabs["targetInfos"].([]any)
	var focused map[string]any
	var failed error
	for _, raw := range infos {
		tab, _ := raw.(map[string]any)
		if !foregroundTab(tab) {
			continue
		}
		state, err := sampleViewerFocus(ctx, c, asString(tab[keyTargetID]))
		if err != nil {
			failed = err
			continue
		}
		if state != nil {
			// Two windows' foreground tabs that both claim focus (automation focus
			// emulation) leave X input's target unknown, so no prediction beats a
			// wrong one.
			if focused != nil {
				return nil, nil
			}
			focused = state
		}
	}
	// A tab that failed (closing, navigating, busy) may be the focused one, so
	// "nothing focused" is only reported when every tab answered.
	if focused == nil && failed != nil {
		return nil, failed
	}
	return focused, nil
}

// foregroundTab reports whether a tab target is the one its window shows. The
// page's own visibilityState and hasFocus() cannot tell: a driver's focus
// emulation captures every tab it attaches to, so each one reports visible and
// focused. The tab strip's tabActive is browser state no page override touches;
// a browser that does not report it leaves every tab a candidate.
func foregroundTab(tab map[string]any) bool {
	embedder, _ := tab["embedderData"].(map[string]any)
	active, ok := embedder["tabActive"].(bool)
	return !ok || active
}

// sampleViewerFocus reads the focus state of the page a tab target holds,
// through a session on the tab whose auto-attach hands over its page. Detaching
// the tab session ends the page session with it.
func sampleViewerFocus(ctx context.Context, c *cdpConn, tabID string) (map[string]any, error) {
	tabSID, err := c.attach(ctx, tabID)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = c.call(ctx, "", "Target.detachFromTarget", map[string]any{cdpSessionID: tabSID})
	}()
	sid, targetID, err := c.attachTabPage(ctx, tabSID)
	if err != nil {
		return nil, err
	}
	window, err := c.call(ctx, "", "Browser.getWindowForTarget", map[string]any{keyTargetID: targetID})
	if err != nil {
		return nil, err
	}
	metrics, err := c.call(ctx, sid, "Page.getLayoutMetrics", nil)
	if err != nil {
		return nil, err
	}
	geometry, err := json.Marshal(map[string]any{"bounds": window["bounds"], "viewport": metrics["cssVisualViewport"]})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errCDPCall, err)
	}
	res, err := c.callInWorld(ctx, sid, viewerFocusScript, string(geometry), true)
	if err != nil {
		return nil, err
	}
	result, _ := res[cdpResult].(map[string]any)
	state, _ := result[cdpValue].(map[string]any)
	if state != nil {
		state["target"] = targetID
	}
	return state, nil
}

// attachTabPage auto-attaches a tab session to the page it holds and returns
// that page's session and target id. Chrome announces an existing child before
// it answers setAutoAttach, so a reply with no page announced means none. A
// prerendering page is a page child of the tab too, but not the one it shows.
func (c *cdpConn) attachTabPage(ctx context.Context, tabSID string) (string, string, error) {
	id, err := c.send(ctx, tabSID, "Target.setAutoAttach", autoAttachPages())
	if err != nil {
		return "", "", err
	}
	var sid, targetID, cdpErr string
	awaitErr := c.await(ctx, func(msg map[string]any, _ []byte) bool {
		if got, ok := asInt(msg[cdpID]); ok && got == id {
			if e, ok := msg["error"].(map[string]any); ok {
				cdpErr = asString(e["message"])
			}
			return true
		}
		if msg[cdpMethod] == methodAttachedToTarget && asString(msg[cdpSessionID]) == tabSID {
			params, _ := msg[cdpParams].(map[string]any)
			info, _ := params["targetInfo"].(map[string]any)
			if sid == "" && asString(info[cdpType]) == targetPage && asString(info["subtype"]) == "" {
				sid, targetID = asString(params[cdpSessionID]), asString(info[keyTargetID])
			}
		}
		return false
	})
	switch {
	case awaitErr != nil:
		return "", "", awaitErr
	case cdpErr != "":
		return "", "", fmt.Errorf("%w: Target.setAutoAttach: %s", errCDPCall, cdpErr)
	case sid == "" || targetID == "":
		return "", "", fmt.Errorf("%w: the tab holds no page", errCDPCall)
	}
	return sid, targetID, nil
}
