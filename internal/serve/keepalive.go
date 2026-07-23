package serve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// keepAliveTimeout bounds the whole last-page check so a wedged browser can never
// stall a driver's Target.closeTarget.
const keepAliveTimeout = 3 * time.Second

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

// ensureKeepAlivePage guarantees Chrome is never left with zero page targets.
// Called on a driver's Target.closeTarget BEFORE it is forwarded: if the target
// being closed is the last open page, it creates a fresh about:blank first, so a
// driver closing its working tab on teardown can no longer take the whole browser
// down. Race-free (the create precedes the forwarded close) and best-effort - any
// hiccup just lets the close proceed unchanged.
func ensureKeepAlivePage(ctx context.Context, cdpBase, closingID string) {
	if closingID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, keepAliveTimeout)
	defer cancel()

	wsURL := browserDebuggerURL(ctx, cdpBase)
	if wsURL == "" {
		return
	}
	conn, dialResp, err := websocket.Dial(ctx, wsURL, nil)
	if dialResp != nil && dialResp.Body != nil {
		_ = dialResp.Body.Close()
	}
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	conn.SetReadLimit(wsReadLimit)

	pages, closingIsPage := pageTargetCount(ctx, conn, closingID)
	if !closingIsPage || pages > 1 {
		return // a non-page target, or other pages remain: the close is safe
	}
	// Closing the last page would exit Chrome - open a keep-alive first.
	_ = cdpRequest(ctx, conn, 2, "Target.createTarget", map[string]any{"url": "about:blank"})
}

// browserDebuggerURL fetches the browser-level DevTools WebSocket URL from the
// seed's loopback CDP.
func browserDebuggerURL(ctx context.Context, cdpBase string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cdpBase+"/json/version", nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return v.WebSocketDebuggerURL
}

// pageTargetCount reports how many page targets exist and whether closingID is
// one of them.
func pageTargetCount(ctx context.Context, conn *websocket.Conn, closingID string) (int, bool) {
	resp := cdpRequest(ctx, conn, 1, "Target.getTargets", nil)
	if resp == nil {
		return 0, false
	}
	var r struct {
		Result struct {
			TargetInfos []struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
			} `json:"targetInfos"`
		} `json:"result"`
	}
	if json.Unmarshal(resp, &r) != nil {
		return 0, false
	}
	count, closingIsPage := 0, false
	for _, t := range r.Result.TargetInfos {
		if t.Type != "page" {
			continue
		}
		count++
		if t.TargetID == closingID {
			closingIsPage = true
		}
	}
	return count, closingIsPage
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
