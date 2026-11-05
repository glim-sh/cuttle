package serve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errPageRead = errors.New("cuttle: could not read the current page - take a new snapshot and retry")

// evalPage consumes the unmasked result internally. The caller holds callMu;
// upstream error text is withheld because a page can put a secret in an error.
func (a *aggregator) evalPage(ctx context.Context, s *agentSession, fn string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, browserCallTimeout)
	defer cancel()
	if s.isRevoked() {
		return nil, errRevoked
	}
	res, err := a.call(ctx, s, "browser_evaluate", map[string]any{"function": fn}, nil)
	if err != nil {
		return nil, errPageRead
	}
	return evalResult(res)
}

func evalResult(res *mcp.CallToolResult) (json.RawMessage, error) {
	if res == nil || res.IsError {
		return nil, errPageRead
	}
	text := resultText(res)
	const prefix = "### Result\n"
	if !strings.HasPrefix(text, prefix) {
		return nil, errPageRead
	}
	var raw json.RawMessage
	if json.NewDecoder(strings.NewReader(strings.TrimPrefix(text, prefix))).Decode(&raw) != nil {
		return nil, errPageRead
	}
	return raw, nil
}

func (a *aggregator) waitPage(ctx context.Context, s *agentSession, out *waitOut) {
	s.callMu.Lock()
	defer s.callMu.Unlock()
	// A session that has not acted yet has no tab: reading one would open it and take the lease.
	s.mu.Lock()
	idle := s.cs == nil || s.fresh
	s.mu.Unlock()
	if idle {
		return
	}
	raw, err := a.evalPage(ctx, s, "() => ({url: location.href, title: document.title})")
	if err != nil {
		return
	}
	var page struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if json.Unmarshal(raw, &page) == nil {
		out.URL, out.Title = page.URL, page.Title
	}
}

func (a *aggregator) allowClipboard(ctx context.Context, s *agentSession) error {
	origin := a.currentOrigin(ctx, s)
	if origin == "" {
		return errSecretOrigin
	}
	inst := a.r.m.pool.runningInstance(reservedSeed)
	if inst == nil {
		return errBrowserDown
	}
	conn, err := dialBrowser(ctx, inst.cdpPort)
	if err != nil {
		return errPageRead
	}
	defer conn.close()
	_, err = conn.call(ctx, "", "Browser.setPermission", map[string]any{
		"origin":     origin,
		"permission": map[string]any{keyName: "clipboard-read"},
		"setting":    "granted",
	})
	if err != nil {
		return errPageRead
	}
	return nil
}
