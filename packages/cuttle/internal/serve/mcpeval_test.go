package serve

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func evalText(value any) *mcp.CallToolResult {
	raw, _ := json.Marshal(value)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "### Result\n" + string(raw) + "\n### Ran Playwright code\n..."}}}
}

func (f *fakeUpstream) evaluatePage(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Function string `json:"function"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
		return nil, err
	}
	f.mu.Lock()
	current := f.current[req.Session.ID()]
	f.mu.Unlock()
	f.browser.mu.Lock()
	defer f.browser.mu.Unlock()
	var page pageInfo
	for _, p := range f.browser.list {
		if p.ID == current {
			page = p
		}
	}
	switch in.Function {
	case "() => location.origin":
		return evalText(originOfURL(page.URL)), nil
	case "() => ({url: location.href, title: document.title})":
		return evalText(map[string]string{"url": page.URL, "title": page.Title}), nil
	default:
		return evalText(map[string]any{"ok": true, "value": f.browser.value, "origin": originOfURL(page.URL)}), nil
	}
}

func TestEvalResult(t *testing.T) {
	for _, res := range []*mcp.CallToolResult{nil, errorResult("private-value"), evalText(nil), {Content: []mcp.Content{&mcp.TextContent{Text: "### Result\nnot json private-value"}}}} {
		raw, err := evalResult(res)
		if res != nil && !res.IsError && string(raw) == "null" {
			continue
		}
		if !errors.Is(err, errPageRead) || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("unexpected result/error: %v", err)
		}
	}
	raw, err := evalResult(evalText(map[string]string{"value": "a\n### Result\nb"}))
	var got map[string]string
	if err != nil || json.Unmarshal(raw, &got) != nil || got["value"] != "a\n### Result\nb" {
		t.Fatal("JSON result not preserved")
	}
}

func TestCapturePrivateEvaluateAndGlobalHold(t *testing.T) {
	h := newMCPHarness(t)
	cs := h.connect(t, testAgentToken, nil)
	callTool(t, cs, "browser_navigate", map[string]any{"url": "https://example.com"})
	h.addUpstreamTool("browser_evaluate", true, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		_ = json.Unmarshal(req.Params.Arguments, &args)
		if len(args) != 1 || !strings.Contains(args["function"].(string), `"#code\\\""`) {
			t.Error("evaluate must pass only function with JSON-encoded selector")
		}
		return evalText(map[string]any{"ok": true, "value": "private-capture", "origin": "https://example.com"}), nil
	})
	res := callTool(t, cs, "cuttle_secret_capture", map[string]any{"name": "CODE", "selector": "#code\\\""})
	if res.IsError || strings.Contains(resultText(res), "private-capture") {
		t.Fatal("capture failed or disclosed value")
	}
	if h.r.mcp.screenshotRefusal(&agentSession{}) == "" {
		t.Fatal("another session escaped screenshot hold")
	}
}

func TestOriginReadFailureSkipsPrecheck(t *testing.T) {
	h := newMCPHarness(t)
	h.r.m.pool.secrets.putBound(h.seed, "CODE", []byte("private-code"), sourceCapture, time.Minute, "https://other.example")
	h.addUpstreamTool("browser_evaluate", true, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return errorResult("private-code"), nil
	})
	h.addUpstreamTool(toolType, false, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return evalText("typed"), nil
	})
	cs := h.connect(t, testAgentToken, nil)
	callTool(t, cs, "browser_navigate", map[string]any{"url": "https://example.com"})
	before := h.upstreamCalls(toolType)
	res := callTool(t, cs, toolType, map[string]any{"target": "e1", "text": "{{cuttle:CODE}}"})
	if res.IsError || h.upstreamCalls(toolType) != before+1 {
		t.Fatal("failed origin read must defer to fill path")
	}
}

func TestWaitReadsPageOnlyAtReturnAndProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newMCPHarness(t)
		cs := h.connect(t, testAgentToken, func(*mcp.ProgressNotificationParams) {})
		callTool(t, cs, "browser_navigate", map[string]any{"url": "https://example.com"})
		before := h.upstreamCalls("browser_evaluate")
		params := &mcp.CallToolParams{Name: "cuttle_wait", Arguments: map[string]any{"until": "secret:MISSING", "timeout_minutes": 1}}
		params.SetProgressToken("wait")
		if _, err := cs.CallTool(t.Context(), params); err != nil {
			t.Fatal(err)
		}
		if got := h.upstreamCalls("browser_evaluate") - before; got != 4 {
			t.Fatalf("page reads = %d, want 3 progress + return", got)
		}
	})
}

func TestFindDownloadSettles(t *testing.T) {
	h := newMCPHarness(t)
	dir := driverDownloadsDir(h.r.m.pool.dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "download.txt")
	if err := os.WriteFile(file, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"latest", "download.txt"} {
		if _, _, err := h.r.mcp.findDownload(name); !errors.Is(err, errDownloadBusy) {
			t.Fatalf("expected busy, got %v", err)
		}
	}
	old := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.r.mcp.findDownload("latest"); err != nil {
		t.Fatal(err)
	}
}

func TestFindDownloadLatestWaitsForNewest(t *testing.T) {
	h := newMCPHarness(t)
	dir := driverDownloadsDir(h.r.m.pool.dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	older := filepath.Join(dir, "older.txt")
	if err := os.WriteFile(older, []byte("done"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(older, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "newer.txt"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.r.mcp.findDownload("latest"); !errors.Is(err, errDownloadBusy) {
		t.Fatalf("latest with the newest unsettled = %v, want busy", err)
	}
	if _, info, err := h.r.mcp.findDownload("older.txt"); err != nil || info.Name() != "older.txt" {
		t.Fatalf("exact name = %v, %v", info, err)
	}
}

func TestClipboardCaptureGrantsOnlyCurrentOrigin(t *testing.T) {
	h := newMCPHarness(t)
	rec, port := fakeTabsBrowser(t, nil)
	h.r.m.pool.mu.Lock()
	h.r.m.pool.processes[h.seed] = &chromeInstance{cdpPort: port, process: newFakeProcess()}
	h.r.m.pool.mu.Unlock()
	h.browser.value = "clipboard-capture"
	cs := h.connect(t, testAgentToken, nil)
	callTool(t, cs, "browser_navigate", map[string]any{"url": "https://example.com/form"})
	before := h.upstreamCalls("browser_evaluate")
	res := callTool(t, cs, "cuttle_secret_capture", map[string]any{"name": "CLIP", "from_clipboard": true})
	if res.IsError || strings.Contains(resultText(res), "clipboard-capture") {
		t.Fatal("clipboard capture failed or disclosed value")
	}
	if got := h.upstreamCalls("browser_evaluate") - before; got != 2 {
		t.Fatalf("clipboard reads = %d, want origin and capture", got)
	}
	calls := rec.received()
	if len(calls) != 1 || calls[0][cdpMethod] != "Browser.setPermission" {
		t.Fatal("capture must use CDP only for permission")
	}
	params := calls[0][cdpParams].(map[string]any)
	permission := params["permission"].(map[string]any)
	if params["origin"] != "https://example.com" || params["setting"] != "granted" || permission[keyName] != "clipboard-read" {
		t.Fatal("permission must be scoped to the current origin")
	}
}
