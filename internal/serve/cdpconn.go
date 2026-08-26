package serve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/coder/websocket"
)

// A small raw CDP client for the daemon's OWN calls - the ones with no driver
// on the other end: grabbing bytes out of an authenticated page, reading an
// element or the clipboard, summarizing the cookie jar, opening the keep-alive
// tab.
//
// chromedp is deliberately not used for these. Its session management creates
// targets of its own, and several of these calls exist precisely to avoid
// opening a tab the user did not ask for. This is also a strict superset of what
// a plain request/response helper can do: it carries a sessionId, it surfaces
// CDP `error` objects instead of silently returning nothing, and its await loop
// can watch for an event and a command's reply at the same time - which is what
// reading a response body off a navigation needs.

var errCDPCall = errors.New("CDP call failed")

// cdpConn is one websocket to the browser endpoint, with flat sessions.
type cdpConn struct {
	ws     *websocket.Conn
	nextID int64
}

func dialBrowser(ctx context.Context, port int) (*cdpConn, error) {
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := fetchCDP(ctx, port, "/json/version", &v); err != nil || v.WebSocketDebuggerURL == "" {
		return nil, fmt.Errorf("%w: the browser's CDP endpoint did not answer", errCDPCall)
	}
	ws, resp, err := websocket.Dial(ctx, v.WebSocketDebuggerURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errCDPCall, err)
	}
	ws.SetReadLimit(wsReadLimit)
	return &cdpConn{ws: ws}, nil
}

func (c *cdpConn) close() { _ = c.ws.Close(websocket.StatusNormalClosure, "") }

// send issues one command and returns its id, so the caller can recognize the
// reply among the events streaming past.
func (c *cdpConn) send(ctx context.Context, sid, method string, params map[string]any) (int64, error) {
	c.nextID++
	cmd := map[string]any{cdpID: c.nextID, cdpMethod: method}
	if params != nil {
		cmd[cdpParams] = params
	}
	if sid != "" {
		cmd[cdpSessionID] = sid
	}
	b, err := json.Marshal(cmd)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errCDPCall, err)
	}
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		return 0, fmt.Errorf("%w: %w", errCDPCall, err)
	}
	return c.nextID, nil
}

// await reads frames until fn reports it has what it wanted. fn sees every
// decoded frame - replies and events alike - which is what lets one wait watch
// for a response event and its command's reply at the same time.
func (c *cdpConn) await(ctx context.Context, fn func(msg map[string]any, data []byte) bool) error {
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return fmt.Errorf("%w: %w", errCDPCall, err)
		}
		msg, ok := decodeCDP(data)
		if !ok {
			continue
		}
		if fn(msg, data) {
			return nil
		}
	}
}

// call sends a command and returns its result object.
func (c *cdpConn) call(ctx context.Context, sid, method string, params map[string]any) (map[string]any, error) {
	raw, err := c.callRaw(ctx, sid, method, params)
	if err != nil {
		return nil, err
	}
	msg, ok := decodeCDP(raw)
	if !ok {
		return nil, fmt.Errorf("%w: %s answered with something that is not a CDP frame", errCDPCall, method)
	}
	result, _ := msg[cdpResult].(map[string]any)
	return result, nil
}

// callRaw is call for a reply the caller wants to unmarshal into a typed struct
// rather than walk as a map.
func (c *cdpConn) callRaw(ctx context.Context, sid, method string, params map[string]any) ([]byte, error) {
	id, err := c.send(ctx, sid, method, params)
	if err != nil {
		return nil, err
	}
	var reply []byte
	var cdpErr string
	awaitErr := c.await(ctx, func(msg map[string]any, data []byte) bool {
		if got, ok := asInt(msg[cdpID]); !ok || got != id {
			return false
		}
		if e, ok := msg["error"].(map[string]any); ok {
			cdpErr = asString(e["message"])
			return true
		}
		reply = data
		return true
	})
	if awaitErr != nil {
		return nil, awaitErr
	}
	if cdpErr != "" {
		return nil, fmt.Errorf("%w: %s: %s", errCDPCall, method, cdpErr)
	}
	return reply, nil
}

func (c *cdpConn) attach(ctx context.Context, targetID string) (string, error) {
	res, err := c.call(ctx, "", "Target.attachToTarget", map[string]any{
		"targetId": targetID, "flatten": true,
	})
	if err != nil {
		return "", err
	}
	sid := asString(res["sessionId"])
	if sid == "" {
		return "", fmt.Errorf("%w: could not attach to the tab", errCDPCall)
	}
	return sid, nil
}

// because leaving it unset truncates large documents.
func (c *cdpConn) readStream(ctx context.Context, sid, handle string) ([]byte, error) {
	defer func() {
		_, _ = c.call(ctx, sid, "IO.close", map[string]any{"handle": handle})
	}()
	var out []byte
	for {
		chunk, err := c.call(ctx, sid, "IO.read", map[string]any{"handle": handle, "size": ioReadChunk})
		if err != nil {
			return nil, err
		}
		data := asString(chunk["data"])
		if asBool(chunk["base64Encoded"]) {
			decoded, derr := base64.StdEncoding.DecodeString(data)
			if derr != nil {
				return nil, fmt.Errorf("%w: a chunk of the response did not decode", errGrabFailed)
			}
			out = append(out, decoded...)
		} else {
			out = append(out, data...)
		}
		if _, err := checkGrabSize(out); err != nil {
			return nil, err
		}
		if asBool(chunk["eof"]) {
			return out, nil
		}
	}
}

func (c *cdpConn) isolatedWorld(ctx context.Context, sid string) (int64, error) {
	tree, err := c.call(ctx, sid, "Page.getFrameTree", map[string]any{})
	if err != nil {
		return 0, err
	}
	frameTree, _ := tree["frameTree"].(map[string]any)
	frame, _ := frameTree["frame"].(map[string]any)
	frameID := asString(frame["id"])
	if frameID == "" {
		return 0, fmt.Errorf("%w: the tab has no main frame", errCDPCall)
	}
	// Created fresh at point of use rather than cached: numeric context ids are
	// recycled across navigations, so a cached one can silently address a
	// different document.
	world, err := c.call(ctx, sid, "Page.createIsolatedWorld", map[string]any{
		"frameId": frameID, "worldName": "cuttle_grab",
	})
	if err != nil {
		return 0, err
	}
	id, ok := asInt(world["executionContextId"])
	if !ok || id == 0 {
		return 0, fmt.Errorf("%w: no isolated world to evaluate in", errCDPCall)
	}
	return id, nil
}

// callInWorld runs one function in a FRESH isolated world of the attached page.
// Its argument travels as a structured CallArgument rather than being formatted
// into the function text: that is what keeps a URL's or a selector's own
// punctuation from becoming script, and it is the shape a value would need if
// one ever rode this path. byValue=false returns an objectId instead, which is
// how a Blob comes back for IO.resolveBlob.
func (c *cdpConn) callInWorld(ctx context.Context, sid, fn, arg string, byValue bool) (map[string]any, error) {
	worldID, err := c.isolatedWorld(ctx, sid)
	if err != nil {
		return nil, err
	}
	params := map[string]any{
		"functionDeclaration": fn,
		"executionContextId":  worldID,
		"awaitPromise":        true,
		cdpReturnByValue:      byValue,
	}
	if arg != "" {
		params["arguments"] = []any{map[string]any{"value": arg}}
	}
	return c.call(ctx, sid, "Runtime.callFunctionOn", params)
}
