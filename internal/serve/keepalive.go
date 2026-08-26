package serve

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	// keepAliveTimeout bounds the launch-time keep-alive tab lookup so a wedged
	// browser can never stall a seed's launch.
	keepAliveTimeout = 5 * time.Second
	// keepAlivePoll is how often the launch's own page target is looked for.
	keepAlivePoll = 50 * time.Millisecond
	// keepAliveAdoptWindow is how long to wait for it before opening one instead.
	// The target normally registers within a few polls; this only has to cover a
	// CPU-starved start, not a wedged browser (keepAliveTimeout does that).
	keepAliveAdoptWindow = time.Second
)

// keepAlivePage picks the page target that keeps Chrome alive, so a driver that
// closes its working tab(s) on teardown cannot take the whole browser down with
// it. (A count-based guard came first and raced under pipelined closes - a
// separate getTargets cannot observe an in-flight close on another session.)
//
// It ADOPTS the browser's first page rather than opening one of its own. The
// image launches Chrome on an about:blank so a headed window maps at all, and a
// second daemon-owned tab beside it meant the person in the viewer opened onto
// two identical blank tabs. One tab, shared: the human sees it and drivers list
// and drive it. Only a browser that came up with no page of its own gets one
// created - and the launch URL's target registers slightly after the DevTools
// HTTP server starts answering, so the list is polled rather than read once (a
// single read on a loaded machine would create the second tab this exists to
// avoid).
//
// Best-effort - "" means the launch simply has no keep-alive guard.
//
// expectPage says whether Chrome was given a URL to open, i.e. whether a page of
// its own is coming. Only then is the list worth polling: the DevTools HTTP
// server answers before the launch URL's target registers, and a single read on
// a loaded machine would miss it and create the second tab this exists to avoid.
// A browser launched with no URL has no page to wait for, so it gets one at once.
func keepAlivePage(ctx context.Context, port int, expectPage bool) string {
	ctx, cancel := context.WithTimeout(ctx, keepAliveTimeout)
	defer cancel()

	if expectPage {
		adopt, cancelAdopt := context.WithTimeout(ctx, keepAliveAdoptWindow)
		defer cancelAdopt()
		for {
			if id := firstPage(adopt, port); id != "" {
				return id
			}
			select {
			case <-adopt.Done():
			case <-time.After(keepAlivePoll):
				continue
			}
			break
		}
	}
	if id := firstPage(ctx, port); id != "" {
		return id
	}
	return createPage(ctx, port)
}

// firstPage returns the browser's first ordinary page target, or "". Chrome's own
// UI surfaces (devtools://, chrome://) are skipped: they are pages by type, but
// adopting one would make a devtools window the thing that holds the browser up.
func firstPage(ctx context.Context, port int) string {
	var pages []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if fetchCDP(ctx, port, "/json/list", &pages) != nil {
		return ""
	}
	for _, t := range pages {
		if t.Type != targetPage || t.ID == "" {
			continue
		}
		if strings.HasPrefix(t.URL, "devtools://") || strings.HasPrefix(t.URL, "chrome://") {
			continue
		}
		return t.ID
	}
	return ""
}

// createPage opens an about:blank over the browser endpoint and returns its
// targetId, or "" if it could not.
func createPage(ctx context.Context, port int) string {
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if fetchCDP(ctx, port, "/json/version", &v) != nil || v.WebSocketDebuggerURL == "" {
		return ""
	}
	conn, dialResp, err := websocket.Dial(ctx, v.WebSocketDebuggerURL, nil)
	if dialResp != nil && dialResp.Body != nil {
		_ = dialResp.Body.Close()
	}
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	conn.SetReadLimit(wsReadLimit)

	resp := cdpRequest(ctx, conn, 1, "Target.createTarget", map[string]any{cdpURL: "about:blank"})
	if resp == nil {
		return ""
	}
	var r struct {
		Result struct {
			TargetID string `json:"targetId"`
		} `json:"result"`
	}
	if json.Unmarshal(resp, &r) != nil {
		return ""
	}
	return r.Result.TargetID
}

// closeTargetID returns the targetId of a Target.closeTarget frame, or "" if the
// frame is anything else.
func closeTargetID(data []byte) string {
	msg, ok := decodeCDP(data)
	if !ok || asString(msg[cdpMethod]) != "Target.closeTarget" {
		return ""
	}
	params, _ := msg[cdpParams].(map[string]any)
	if params == nil {
		return ""
	}
	return asString(params["targetId"])
}

// keepAliveCloseResponse answers a driver's Target.closeTarget for the keep-alive
// tab with a success it never actually performs. It is the LAST resort: used only
// when a replacement tab could not be opened, because a close answered this way
// never produces Target.targetDestroyed, and a driver that waits for the target
// to die (Playwright's page.close() awaits exactly that, with no timeout) would
// hang. Returns nil if the frame cannot be decoded (the caller then forwards it).
func keepAliveCloseResponse(data []byte) []byte {
	msg, ok := decodeCDP(data)
	if !ok {
		return nil
	}
	resp := map[string]any{cdpResult: map[string]any{"success": true}}
	if id, ok := msg[cdpID]; ok {
		resp[cdpID] = id
	}
	if sid := asString(msg[cdpSessionID]); sid != "" {
		resp[cdpSessionID] = sid
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return nil
	}
	return out
}

// cdpRequest sends one CDP command over conn and returns the raw response frame
// whose id matches, draining any event/other frames in between. nil on error.
func cdpRequest(ctx context.Context, conn *websocket.Conn, id int64, method string, params map[string]any) []byte {
	cmd := map[string]any{cdpID: id, cdpMethod: method}
	if params != nil {
		cmd[cdpParams] = params
	}
	b, err := json.Marshal(cmd)
	if err != nil {
		return nil
	}
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		return nil
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return nil
		}
		var head struct {
			ID int64 `json:"id"`
		}
		if json.Unmarshal(data, &head) == nil && head.ID == id {
			return data
		}
	}
}
