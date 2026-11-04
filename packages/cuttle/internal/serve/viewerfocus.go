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

func (m *multiplexer) handleViewerFocus(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	_, inst := m.runningSeedInstance(w, r)
	if inst == nil {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		state := readViewerFocus(ctx, inst.cdpPort)
		cancel()
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
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}

func readViewerFocus(ctx context.Context, port int) any {
	c, err := dialBrowser(ctx, port)
	if err != nil {
		return nil
	}
	defer c.close()
	targets, err := c.call(ctx, "", "Target.getTargets", nil)
	if err != nil {
		return nil
	}
	infos, _ := targets["targetInfos"].([]any)
	var focused map[string]any
	for _, raw := range infos {
		target, _ := raw.(map[string]any)
		if asString(target["type"]) != "page" {
			continue
		}
		sid, err := c.attach(ctx, asString(target["targetId"]))
		if err != nil {
			continue
		}
		window, err := c.call(ctx, "", "Browser.getWindowForTarget", map[string]any{keyTargetID: target["targetId"]})
		if err != nil {
			return nil
		}
		metrics, err := c.call(ctx, sid, "Page.getLayoutMetrics", nil)
		if err != nil {
			return nil
		}
		geometry, err := json.Marshal(map[string]any{"bounds": window["bounds"], "viewport": metrics["cssVisualViewport"]})
		if err != nil {
			return nil
		}
		res, err := c.callInWorld(ctx, sid, viewerFocusScript, string(geometry), true)
		_, _ = c.call(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": sid})
		if err != nil {
			continue
		}
		result, _ := res["result"].(map[string]any)
		state, _ := result["value"].(map[string]any)
		if state != nil {
			if focused != nil {
				return nil
			}
			state["target"] = target["targetId"]
			focused = state
		}
	}
	return focused
}
