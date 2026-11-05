package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Two places where forwarding a call as-is would undo what masking promises:
// Playwright's browser_find searches the raw page, so it answers questions about
// values the agent is only ever shown masked; and a {{cuttle:NAME}} the fill path
// refuses reaches the agent as a bare Playwright timeout.

const (
	toolFind         = "browser_find"
	toolType         = "browser_type"
	toolFillForm     = "browser_fill_form"
	toolSnapshot     = "browser_snapshot"
	findContextLines = 3
)

// find answers browser_find from the masked snapshot: upstream searches the raw
// accessibility tree and masks only its output, so a match count would confirm
// a held secret or a password field's value one character at a time. Upstream's
// arguments and output shape, but inline only (no filename), and the regex runs
// as RE2 (see compileJSRegex).
func (a *aggregator) find(ctx context.Context, s *agentSession, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Text       string   `json:"text"`
		Regex      string   `json:"regex"`
		MaxResults *float64 `json:"maxResults"`
		Filename   string   `json:"filename"`
	}
	if err := decodeArgs(req, &in); err != nil {
		return errorResult("cuttle: browser_find arguments: " + err.Error()), nil //nolint:nilerr // a bad argument is the agent's error, not the call's
	}
	switch {
	case in.Text == "" && in.Regex == "":
		return errorResult(`Provide either "text" or "regex" to search for.`), nil
	case in.Text != "" && in.Regex != "":
		return errorResult(`Provide only one of "text" or "regex", not both.`), nil
	case in.MaxResults != nil && (*in.MaxResults <= 0 || *in.MaxResults != float64(int(*in.MaxResults))):
		return errorResult(`"maxResults" must be a positive integer.`), nil
	case in.Filename != "":
		return errorResult("cuttle: browser_find answers inline only - drop filename."), nil
	}
	needle := strings.ToLower(in.Text)
	query, match := `"`+in.Text+`"`, func(line string) bool {
		return strings.Contains(strings.ToLower(line), needle)
	}
	if in.Regex != "" {
		re, shown, err := compileJSRegex(in.Regex)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		query, match = shown, re.MatchString
	}
	maxResults := 0
	if in.MaxResults != nil {
		maxResults = int(*in.MaxResults)
	}
	res, err := a.call(ctx, s, toolSnapshot, map[string]any{}, req)
	if err != nil || res.IsError {
		return res, err
	}
	a.mask(ctx, res)
	snapshot, ok := snapshotYAML(resultText(res))
	if !ok {
		return errorResult("cuttle: browser_find could not read the page snapshot:\n" + resultText(res)), nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
		Text: "### Result\n" + findInSnapshot(snapshot, query, match, maxResults),
	}}}, nil
}

var snapshotBlockEnd = regexp.MustCompile("(?m)^```$")

// snapshotYAML is the aria tree browser_snapshot inlines under "### Snapshot".
func snapshotYAML(text string) (string, bool) {
	const open = "### Snapshot\n```yaml\n"
	_, body, ok := strings.Cut(text, open)
	if !ok {
		return "", false
	}
	end := snapshotBlockEnd.FindStringIndex(body)
	if end == nil {
		return "", false
	}
	return strings.TrimSuffix(body[:end[0]], "\n"), true
}

// compileJSRegex reads upstream's regex argument - a JavaScript pattern, or
// /pattern/flags - into RE2, refusing what RE2 cannot run rather than letting
// the raw page answer it. shown is the query as upstream prints it.
func compileJSRegex(source string) (*regexp.Regexp, string, error) {
	pattern, flags := source, ""
	if m := jsRegexLiteral.FindStringSubmatch(source); m != nil {
		pattern, flags = m[1], m[2]
	}
	prefix, kept, anchored := "", "", false
	for _, f := range flags {
		if strings.ContainsRune(kept, f) {
			return nil, "", fmt.Errorf("cuttle: invalid regular expression flags %q", flags) //nolint:err113 // refusal text
		}
		switch f {
		case 'g':
			continue // upstream drops it: every line is tested from its start
		case 'i', 'm', 's':
			prefix += string(f)
		case 'y':
			anchored = true
		case 'd', 'u':
		default:
			return nil, "", fmt.Errorf("cuttle: regular expression flag %q is not supported by browser_find", f) //nolint:err113 // refusal text
		}
		kept += string(f)
	}
	expr := pattern
	if anchored {
		expr = `\A(?:` + expr + `)`
	}
	if prefix != "" {
		expr = "(?" + prefix + ")" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, "", fmt.Errorf("cuttle: browser_find runs the regex itself, so a search never sees an unmasked value,"+ //nolint:err113 // refusal text
			" and in RE2 syntax: %q does not compile there (%v). Lookarounds and backreferences are not supported;"+
			" rewrite the pattern or search with text", source, err)
	}
	return re, "/" + pattern + "/" + canonicalFlags(kept), nil
}

var jsRegexLiteral = regexp.MustCompile(`^/(.*)/([a-z]*)$`)

// canonicalFlags orders flags the way JavaScript prints a RegExp.
func canonicalFlags(flags string) string {
	var b strings.Builder
	for _, f := range "dimsuy" {
		if strings.ContainsRune(flags, f) {
			b.WriteRune(f)
		}
	}
	return b.String()
}

// findInSnapshot is upstream's browser_find rendering: each match with a few
// lines of context, under its path from the root, overlapping windows merged.
func findInSnapshot(snapshot, query string, match func(string) bool, maxResults int) string {
	lines := strings.Split(snapshot, "\n")
	indents := make([]int, len(lines))
	for i, l := range lines {
		indents[i] = len(l) - len(strings.TrimLeftFunc(l, unicode.IsSpace))
	}
	var matched []int
	for i, l := range lines {
		if match(l) {
			matched = append(matched, i)
		}
	}
	if len(matched) == 0 {
		return "No matches found for " + query + "."
	}
	total := len(matched)
	if maxResults > 0 && maxResults < total {
		matched = matched[:maxResults]
	}
	type window struct{ start, end int }
	var windows []window
	for _, m := range matched {
		start, end := max(0, m-findContextLines), min(len(lines)-1, m+findContextLines)
		if n := len(windows); n > 0 && start <= windows[n-1].end+1 {
			windows[n-1].end = max(windows[n-1].end, end)
			continue
		}
		windows = append(windows, window{start, end})
	}
	parents := parentLines(lines, indents)
	onPath := map[int]bool{}
	for _, m := range matched {
		onPath[m] = true
		for p := parents[m]; p >= 0 && !onPath[p]; p = parents[p] {
			onPath[p] = true
		}
	}
	snippets := make([]string, 0, len(windows))
	for _, w := range windows {
		var idx []int
		for p := parents[w.start]; p >= 0; p = parents[p] {
			idx = append(idx, p)
		}
		slices.Reverse(idx)
		for i := w.start; i <= w.end; i++ {
			idx = append(idx, i)
		}
		out := make([]string, 0, len(idx))
		for i, n := range idx {
			if i > 0 && n > idx[i-1]+1 && !onPath[n] && !onPath[idx[i-1]] {
				out = append(out, strings.Repeat(" ", indents[n])+"...")
			}
			out = append(out, lines[n])
		}
		snippets = append(snippets, strings.Join(out, "\n"))
	}
	word := "matches"
	if total == 1 {
		word = "match"
	}
	header := fmt.Sprintf("Found %d %s for %s:", total, word, query)
	if len(matched) < total {
		header = fmt.Sprintf("Found %d %s for %s (showing first %d):", total, word, query, len(matched))
	}
	return header + "\n\n" + strings.Join(snippets, "\n\n----\n\n")
}

// parentLines maps each line to the nearest non-blank line above it with less
// indent, the line that encloses it, or -1: upstream's ancestor walk, computed
// once, so a search matching every line stays linear.
func parentLines(lines []string, indents []int) []int {
	parents := make([]int, len(lines))
	var open []int // enclosing candidates, indents strictly increasing
	for i, l := range lines {
		blank := strings.TrimSpace(l) == ""
		if !blank {
			for len(open) > 0 && indents[open[len(open)-1]] >= indents[i] {
				open = open[:len(open)-1]
			}
		}
		parents[i] = -1
		for _, o := range slices.Backward(open) {
			if indents[o] < indents[i] {
				parents[i] = o
				break
			}
		}
		if !blank {
			open = append(open, i)
		}
	}
	return parents
}

// mainFrameRef is a snapshot ref in the tab's own document. Any other target -
// a child frame's ref (f<N>e<N>) or a selector, which may enter a frame - lands
// in a document whose origin may not be the tab's: its check is the fill path's.
var mainFrameRef = regexp.MustCompile(`^e\d+$`)

// sentinelRefusal resolves every {{cuttle:NAME}} a browser_type or
// browser_fill_form would type before the call is forwarded. The fill path
// refuses the same cases, but Playwright retries its refusal into a bare action
// timeout, so the agent never sees why; here it gets the fix at once.
func (a *aggregator) sentinelRefusal(ctx context.Context, s *agentSession, name string, req *mcp.CallToolRequest) string {
	type field struct {
		Target string `json:"target"`
		Text   string `json:"text"`
		Value  string `json:"value"`
	}
	var in struct {
		field
		Fields []field `json:"fields"`
	}
	if decodeArgs(req, &in) != nil {
		return ""
	}
	fields := in.Fields
	if name == toolType {
		fields = []field{{Target: in.Target, Value: in.Text}}
	}
	var held []secretInfo
	tabOrigin, looked := "", false
	for _, f := range fields {
		secret, kind := parseSentinel(f.Value)
		switch kind {
		case sentinelNone:
			continue
		case sentinelEmbedded:
			return msgEmbeddedSentinel
		case sentinelWhole:
		}
		if held == nil {
			held = a.r.m.pool.secrets.list(reservedSeed)
		}
		i := slices.IndexFunc(held, func(e secretInfo) bool { return e.Name == secret })
		switch {
		case i < 0:
			names := make([]string, len(held))
			for j, e := range held {
				names[j] = e.Name
			}
			return fmt.Sprintf("cuttle: unknown secret %q - ask the person for it with cuttle_request_secret, or store"+
				" a value from your tab with cuttle_secret_capture.%s Nothing was typed.", secret, registeredNames(names))
		case !held[i].Live:
			return fmt.Sprintf("cuttle: secret %s expired and the daemon holds no copy - ask the person for it again"+
				" with cuttle_request_secret, or capture it again with cuttle_secret_capture. Nothing was typed.", secret)
		}
		bound := held[i].BoundOrigin
		if bound == "" || !mainFrameRef.MatchString(f.Target) {
			continue
		}
		if !looked {
			tabOrigin, looked = a.currentOrigin(ctx, s), true
		}
		if tabOrigin != "" && tabOrigin != bound {
			return fmt.Sprintf("cuttle: secret %s is bound to %s and your tab is on %s; refusing. Navigate there first,"+
				" or get a secret for this site with cuttle_request_secret. Nothing was typed.", secret, bound, tabOrigin)
		}
	}
	return ""
}

// currentOrigin is the origin of the principal's current tab, "" when unknown.
func (a *aggregator) currentOrigin(ctx context.Context, s *agentSession) string {
	raw, err := a.evalPage(ctx, s, "() => location.origin")
	if err != nil {
		return ""
	}
	var origin string
	if json.Unmarshal(raw, &origin) != nil {
		return ""
	}
	origin, _ = secretOrigin(origin)
	return origin
}
