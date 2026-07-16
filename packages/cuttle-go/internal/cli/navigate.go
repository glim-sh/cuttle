package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/coder/websocket"
)

// navigate points the browser's active page at url over CDP and returns its
// title (best effort). It lists targets via the multiplexer's /json endpoint,
// picks the page a human would be driving, and issues Page.navigate on the
// per-page WebSocket the list hands back.
func navigate(ctx context.Context, host string, port int, targetURL string, vncPort int, seed string) (string, error) {
	targets, err := listTargets(ctx, host, port, seed)
	if err != nil {
		return "", err
	}
	target := pickPage(targets, vncPort)
	wsURL, _ := target["webSocketDebuggerUrl"].(string)
	if wsURL == "" {
		return "", errNoPageTarget
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return "", fmt.Errorf("connecting to CDP page: %w", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	s := &cdpSession{conn: conn}
	if _, cerr := s.call(ctx, "Page.navigate", map[string]any{"url": targetURL}); cerr != nil {
		return "", cerr
	}
	res, err := s.call(ctx, "Runtime.evaluate", map[string]any{
		"expression": "document.title", "returnByValue": true,
	})
	if err != nil {
		return "", nil //nolint:nilerr // title is best-effort; a nav that worked still counts
	}
	if r, ok := res["result"].(map[string]any); ok {
		if v, ok := r["value"].(string); ok {
			return v, nil
		}
	}
	return "", nil
}

var (
	errNoPageTarget = errors.New("no page target found to navigate")
	errCDPError     = errors.New("CDP error")
)

func listTargets(ctx context.Context, host string, port int, seed string) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	endpoint := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/json"
	if seed != "" {
		endpoint += "?fingerprint=" + url.QueryEscape(seed)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing CDP targets: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	var targets []map[string]any
	if err := json.Unmarshal(body, &targets); err != nil {
		return nil, err //nolint:wrapcheck
	}
	return targets, nil
}

// pickPage chooses the page target a human is most likely driving, skipping
// worker/service targets, the built-in tab-search UI (chrome://), and the local
// VNC viewer page itself.
func pickPage(targets []map[string]any, vncPort int) map[string]any {
	viewerPrefix := ""
	if vncPort != 0 {
		viewerPrefix = "http://127.0.0.1:" + strconv.Itoa(vncPort) + "/"
	}
	var pages []map[string]any
	for _, t := range targets {
		if ty, _ := t["type"].(string); ty == "page" {
			pages = append(pages, t)
		}
	}
	for _, t := range pages {
		u, _ := t["url"].(string)
		if hasPrefix(u, "chrome://") {
			continue
		}
		if viewerPrefix != "" && hasPrefix(u, viewerPrefix) {
			continue
		}
		return t
	}
	if len(pages) > 0 {
		return pages[0]
	}
	return nil
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// cdpSession is a single WebSocket connection to one CDP target with id-matched
// calls.
type cdpSession struct {
	conn   *websocket.Conn
	nextID int
}

func (s *cdpSession) call(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	s.nextID++
	id := s.nextID
	payload, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	if err := s.conn.Write(ctx, websocket.MessageText, payload); err != nil {
		return nil, fmt.Errorf("CDP write: %w", err)
	}
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("CDP read: %w", err)
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		mid, ok := msg["id"].(float64)
		if !ok || int(mid) != id {
			continue
		}
		if e, ok := msg["error"].(map[string]any); ok {
			m, _ := e["message"].(string)
			return nil, fmt.Errorf("%w: %s: %s", errCDPError, method, m)
		}
		result, _ := msg["result"].(map[string]any)
		return result, nil
	}
}
