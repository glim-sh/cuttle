package serve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// addUpstreamTool registers one more tool on the fake Playwright MCP, counting
// its calls; it must run before the agent connects.
func (h *mcpHarness) addUpstreamTool(name string, readOnly bool, handle mcp.ToolHandler) {
	h.upstream.server.AddTool(&mcp.Tool{
		Name: name, Description: name + " upstream", InputSchema: map[string]any{"type": "object"},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		h.upstream.mu.Lock()
		h.upstream.calls[name]++
		h.upstream.mu.Unlock()
		return handle(ctx, req)
	})
}

func (h *mcpHarness) upstreamCalls(name string) int {
	h.upstream.mu.Lock()
	defer h.upstream.mu.Unlock()
	return h.upstream.calls[name]
}

const findSnapshot = "### Page\n- Page URL: https://forms.example/post\n### Snapshot\n```yaml\n" +
	"- generic [ref=e1]:\n" +
	"  - textbox \"Customer name\" [ref=e2]: tok_stress_123456\n" +
	"  - textbox \"Pass\" [ref=e3]: hunter2-secret-value\n" +
	"  - button \"Submit\" [ref=e4]\n" +
	"```\n"

// browser_find must never match on a masked value: upstream searches the raw
// tree, so its match count was an oracle for a held secret or a password
// field's value, one character at a time.
func TestFindSearchesTheMaskedSnapshot(t *testing.T) {
	h := newMCPHarness(t)
	h.r.m.pool.secrets.put(h.seed, "STRESS_TOK", []byte("tok_stress_123456"), sourceCapture, time.Minute)
	h.browser.fields = []string{"hunter2-secret-value"}
	h.upstream.snapshot = findSnapshot
	// The real upstream: a search of the unmasked tree, its output masked after.
	h.addUpstreamTool(toolFind, true, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Regex string `json:"regex"`
			Text  string `json:"text"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "### Result\nFound 1 match for " + in.Regex + in.Text}}}, nil
	})
	cs := h.connect(t, testAgentToken, nil)

	for _, tc := range []struct {
		args       map[string]any
		want       string
		wantInside []string
	}{
		{map[string]any{"regex": "tok_stress_12345[0-9]"}, "No matches found for /tok_stress_12345[0-9]/.", nil},
		{map[string]any{"text": "hunter2"}, `No matches found for "hunter2".`, nil},
		{map[string]any{"regex": "/HUNTER2-SECRET/i"}, "No matches found for /HUNTER2-SECRET/i.", nil},
		{map[string]any{"text": "stress_tok"}, `Found 1 match for "stress_tok":`, []string{
			"- generic [ref=e1]:", `textbox "Customer name" [ref=e2]: {{cuttle:STRESS_TOK}}`,
			`textbox "Pass" [ref=e3]: ` + passwordPlaceholder,
		}},
		{map[string]any{"regex": "/textbox/gi", "maxResults": 1}, "Found 2 matches for /textbox/i (showing first 1):", nil},
	} {
		res := callTool(t, cs, toolFind, tc.args)
		text := resultText(res)
		if res.IsError || !strings.HasPrefix(text, "### Result\n"+tc.want) {
			t.Errorf("find %v = %q, want it to start with %q", tc.args, text, tc.want)
		}
		for _, w := range tc.wantInside {
			if !strings.Contains(text, w) {
				t.Errorf("find %v = %q, want it to contain %q", tc.args, text, w)
			}
		}
	}
	h.browser.mu.Lock()
	reads := h.browser.reads
	h.browser.mu.Unlock()
	callTool(t, cs, toolFind, map[string]any{"text": "Submit"})
	h.browser.mu.Lock()
	reads = h.browser.reads - reads
	h.browser.mu.Unlock()
	if reads != 1 {
		t.Errorf("one browser_find read the password fields %d times, want once", reads)
	}
	if n := h.upstreamCalls(toolFind); n != 0 {
		t.Fatalf("upstream browser_find ran %d times; it searches the unmasked page", n)
	}
	for args, want := range map[string]string{
		`{"regex":"a(?=b)"}`:          "RE2",
		`{"regex":"/a/x"}`:            "not supported",
		`{}`:                          `Provide either "text" or "regex"`,
		`{"text":"a","regex":"b"}`:    "not both",
		`{"text":"a","maxResults":0}`: "positive integer",
	} {
		var m map[string]any
		_ = json.Unmarshal([]byte(args), &m)
		if res := callTool(t, cs, toolFind, m); !res.IsError || !strings.Contains(resultText(res), want) {
			t.Errorf("find %s = %q, want an error naming %q", args, resultText(res), want)
		}
	}
}

func TestFindInSnapshotShape(t *testing.T) {
	t.Parallel()
	snap := strings.Join([]string{
		"- main:",       // 0
		"  - list:",     // 1
		"    - item a",  // 2
		"    - item b",  // 3
		"    - item c",  // 4
		"    - item d",  // 5
		"    - item e",  // 6
		"    - item f",  // 7
		"    - item g",  // 8
		"    - needle",  // 9
		"    - item h",  // 10
		"  - footer",    // 11
		"  - needle 2",  // 12
		"  - copyright", // 13
	}, "\n")
	got := findInSnapshot(snap, `"needle"`, func(l string) bool { return strings.Contains(l, "needle") }, 0)
	want := "Found 2 matches for \"needle\":\n\n" +
		"- main:\n  - list:\n    - item e\n    - item f\n    - item g\n    - needle\n    - item h\n  - footer\n  - needle 2\n  - copyright"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

// A {{cuttle:NAME}} the fill path would refuse is refused before the call is
// forwarded, with cuttle's own reason: Playwright turns the fill path's
// refusal into a bare action timeout.
func TestSentinelRefusedBeforeForwarding(t *testing.T) {
	h := newMCPHarness(t)
	store := h.r.m.pool.secrets
	store.put(h.seed, "LIVE", []byte("live-secret-value"), sourceStdin, time.Minute)
	store.put(h.seed, "OLD", []byte("old-secret-value"), sourceStdin, time.Minute)
	store.expire(h.seed, "OLD", store.m[h.seed]["OLD"].gen)
	store.putBound(h.seed, "SITE_PW", []byte("site-secret-value"), sourceCapture, time.Minute, "https://other.example")
	ok := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "typed"}}}, nil
	}
	h.addUpstreamTool(toolType, false, ok)
	h.addUpstreamTool(toolFillForm, false, ok)
	cs := h.connect(t, testAgentToken, nil)
	callTool(t, cs, "browser_navigate", map[string]any{"url": "https://site.example/login"})

	form := func(target, value string) map[string]any {
		return map[string]any{"fields": []any{
			map[string]any{"target": "e1", "name": "user", "type": "textbox", "value": "me"},
			map[string]any{"target": target, "name": "pw", "type": "textbox", "value": value},
		}}
	}
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string // "" = forwarded
	}{
		{toolType, map[string]any{"target": "#u", "text": "{{cuttle:NOPE}}"}, `unknown secret "NOPE" - ask the person for it with cuttle_request_secret`},
		{toolFillForm, form("e2", "{{cuttle:NOPE}}"), "cuttle_secret_capture"},
		{toolType, map[string]any{"target": "#u", "text": "{{cuttle:OLD}}"}, "secret OLD expired"},
		{toolType, map[string]any{"target": "#u", "text": "Bearer {{cuttle:LIVE}}"}, "as the WHOLE value"},
		{toolFillForm, form("e2", "{{cuttle:SITE_PW}}"), "bound to https://other.example and your tab is on https://site.example"},
		{toolFillForm, form("f1e2", "{{cuttle:SITE_PW}}"), ""},                                                                  // a child frame's origin is the fill path's to check
		{toolType, map[string]any{"target": "iframe >> internal:control=enter-frame >> #pw", "text": "{{cuttle:SITE_PW}}"}, ""}, // so is a selector's
		{toolType, map[string]any{"target": "#u", "text": "{{cuttle:LIVE}}"}, ""},
		{toolType, map[string]any{"target": "#u", "text": "plain"}, ""},
	} {
		before := h.upstreamCalls(tc.tool)
		res := callTool(t, cs, tc.tool, tc.args)
		forwarded := h.upstreamCalls(tc.tool) > before
		switch {
		case tc.want == "" && (res.IsError || !forwarded):
			t.Errorf("%s %v: %q, want it forwarded", tc.tool, tc.args, resultText(res))
		case tc.want != "" && (forwarded || !res.IsError || !strings.Contains(resultText(res), tc.want)):
			t.Errorf("%s %v: forwarded=%v %q, want a refusal containing %q", tc.tool, tc.args, forwarded, resultText(res), tc.want)
		}
	}
}
