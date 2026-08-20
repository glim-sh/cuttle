package serve

import (
	"context"
	"encoding/json"
	"time"

	"github.com/coder/websocket"
)

// keepAliveTimeout bounds the launch-time keep-alive tab creation so a wedged
// browser can never stall a seed's launch.
const keepAliveTimeout = 5 * time.Second

// keepAlivePage picks the page target whose close the proxy refuses, so Chrome
// always retains at least one page: a driver that closes its working tab(s) on
// teardown can no longer take the whole browser down with it. (A count-based
// guard came first and raced under pipelined closes - a separate getTargets
// cannot observe an in-flight close on another session.)
//
// It ADOPTS the browser's first page rather than opening one of its own. The
// image launches Chrome on an about:blank so a headed window maps at all, and a
// second daemon-owned tab beside it meant the person in the viewer opened onto
// two identical blank tabs. One tab, shared: the human sees it, drivers list it
// and may drive it, and nobody can close it over CDP. Only a browser that came
// up with no page at all gets one created.
//
// Best-effort - "" means the launch simply has no keep-alive guard.
func keepAlivePage(ctx context.Context, port int) string {
	ctx, cancel := context.WithTimeout(ctx, keepAliveTimeout)
	defer cancel()

	var pages []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if fetchCDP(ctx, port, "/json/list", &pages) == nil {
		for _, t := range pages {
			if t.Type == "page" && t.ID != "" {
				return t.ID
			}
		}
	}

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

	resp := cdpRequest(ctx, conn, 1, "Target.createTarget", map[string]any{"url": "about:blank"})
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
// tab with a success it never actually performs, so the immortal tab survives even
// an enumerate-and-close-everything teardown. Returns nil if the frame cannot be
// decoded (the caller then forwards it).
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
