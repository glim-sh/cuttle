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
)

func (m *multiplexer) handleViewerFocus(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	seedKey, inst := m.runningSeedInstance(w, r)
	if inst == nil {
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
	// Ending the stream when the browser is replaced makes the client reconnect
	// to the new instance instead of sampling a dead CDP port forever.
	for m.pool.runningInstance(seedKey) == inst {
		ctx, cancel := context.WithTimeout(r.Context(), viewerFocusTimeout)
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
	targets, err := c.call(ctx, "", "Target.getTargets", nil)
	if err != nil {
		return nil, err
	}
	infos, _ := targets["targetInfos"].([]any)
	var focused map[string]any
	var failed error
	for _, raw := range infos {
		target, _ := raw.(map[string]any)
		if asString(target["type"]) != "page" {
			continue
		}
		state, err := sampleViewerFocus(ctx, c, target["targetId"])
		if err != nil {
			failed = err
			continue
		}
		if state != nil {
			// Two focused tabs (automation focus emulation) leave X input's target
			// unknown, so no prediction beats a wrong one.
			if focused != nil {
				return nil, nil
			}
			state["target"] = target["targetId"]
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

func sampleViewerFocus(ctx context.Context, c *cdpConn, targetID any) (map[string]any, error) {
	sid, err := c.attach(ctx, asString(targetID))
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = c.call(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": sid})
	}()
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
	result, _ := res["result"].(map[string]any)
	state, _ := result["value"].(map[string]any)
	return state, nil
}
