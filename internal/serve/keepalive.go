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

// createKeepAlivePage opens a daemon-owned about:blank on the seed's browser
// endpoint and returns its targetId. The tab is hidden from every attached driver
// (see hideKeepAlive) and its close is refused (see keepAliveCloseResponse), so
// Chrome always retains at least one page target: a driver that closes its working
// tab(s) on teardown can no longer take the whole browser down with it. This
// replaces the earlier count-based guard, which raced under pipelined closes (a
// separate getTargets could not observe an in-flight close on another session).
// Best-effort - "" means the launch simply has no keep-alive guard.
func createKeepAlivePage(ctx context.Context, port int) string {
	ctx, cancel := context.WithTimeout(ctx, keepAliveTimeout)
	defer cancel()

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
// an enumerate-and-close-everything teardown. This is a backstop: the tab is
// hidden, so a well-behaved driver never learns its id to close it in the first
// place. Returns nil if the frame cannot be decoded (the caller then forwards it).
func keepAliveCloseResponse(data []byte) []byte {
	msg, ok := decodeCDP(data)
	if !ok {
		return nil
	}
	resp := map[string]any{"result": map[string]any{"success": true}}
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

// hideKeepAlive removes the daemon-owned keep-alive tab from a Chrome->client
// frame so no driver ever sees, adopts, navigates, or closes it. It drops the
// tab's own lifecycle events (targetCreated / attachedToTarget / targetInfoChanged)
// and strips it from a Target.getTargets result. Callers gate this on
// bytes.Contains(data, keepAliveID) so only the rare frame that mentions the tab
// pays the decode. Returns (out, drop): drop=true means swallow the frame entirely.
func hideKeepAlive(data []byte, keepAliveID string) ([]byte, bool) {
	msg, ok := decodeCDP(data)
	if !ok {
		return data, false
	}
	switch asString(msg[cdpMethod]) {
	case "Target.targetCreated", "Target.attachedToTarget", "Target.targetInfoChanged":
		params, _ := msg[cdpParams].(map[string]any)
		info, _ := params["targetInfo"].(map[string]any)
		if info != nil && asString(info["targetId"]) == keepAliveID {
			return nil, true
		}
		return data, false
	}
	// A Target.getTargets response: strip the keep-alive from targetInfos.
	result, _ := msg["result"].(map[string]any)
	if result == nil {
		return data, false
	}
	infos, _ := result["targetInfos"].([]any)
	if infos == nil {
		return data, false
	}
	filtered := make([]any, 0, len(infos))
	removed := false
	for _, it := range infos {
		if m, _ := it.(map[string]any); m != nil && asString(m["targetId"]) == keepAliveID {
			removed = true
			continue
		}
		filtered = append(filtered, it)
	}
	if !removed {
		return data, false
	}
	result["targetInfos"] = filtered
	out, err := json.Marshal(msg)
	if err != nil {
		return data, false
	}
	return out, false
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
