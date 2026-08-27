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
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/glim-sh/cuttle/internal/mask"
)

// navigate points the browser's active page at url over CDP and returns its
// title (best effort). It lists targets via the multiplexer's /json endpoint,
// picks the page a human would be driving, and issues Page.navigate on the
// per-page WebSocket the list hands back.
func navigate(ctx context.Context, host string, port int, targetURL string, vncPort int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	s, closeSession, err := dialActivePage(ctx, host, port, vncPort)
	if err != nil {
		return "", err
	}
	defer closeSession()

	if _, cerr := s.call(ctx, "Page.navigate", map[string]any{"url": targetURL}); cerr != nil {
		return "", cerr
	}
	res, err := s.call(ctx, "Runtime.evaluate", map[string]any{
		"expression": "document.title", "returnByValue": true,
	})
	if err != nil {
		return "", nil
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

func listTargets(ctx context.Context, host string, port int) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	endpoint := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/json"
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
		if strings.HasPrefix(u, "chrome://") {
			continue
		}
		if viewerPrefix != "" && strings.HasPrefix(u, viewerPrefix) {
			continue
		}
		return t
	}
	if len(pages) > 0 {
		return pages[0]
	}
	return nil
}

// dialActivePage opens a CDP session on the page a human is driving - the one
// pickPage resolves - and returns it with its closer.
func dialActivePage(ctx context.Context, host string, port, vncPort int) (*cdpSession, func(), error) {
	targets, err := listTargets(ctx, host, port)
	if err != nil {
		return nil, nil, err
	}
	target := pickPage(targets, vncPort)
	wsURL, _ := target["webSocketDebuggerUrl"].(string)
	if wsURL == "" {
		return nil, nil, errNoPageTarget
	}
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to CDP page: %w", err)
	}
	return &cdpSession{conn: conn}, func() { _ = conn.Close(websocket.StatusNormalClosure, "") }, nil
}

// cdpSession is a single WebSocket connection to one CDP target with id-matched
// calls.
type cdpSession struct {
	conn    *websocket.Conn
	nextID  int
	worldID int // cached isolated-world context; 0 = not built yet or retired
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

// ---------------------------------------------------------------------------
// open --until: print-and-wait
// ---------------------------------------------------------------------------

// A predicate is a small CLOSED set, not open JS eval, so `open --until` reads
// the same way for everyone and an agent cannot smuggle page-visible script into
// a handoff. `js:` is the escape hatch when the other three cannot express it.
const (
	predURL   = "url"   // the URL matches a glob
	predGone  = "gone"  // the URL NO LONGER matches - "left the sign-in origin"
	predTitle = "title" // document.title contains a substring
	predJS    = "js"    // an expression, coerced to boolean
)

const (
	waitPollGap     = 500 * time.Millisecond
	defaultWaitFor  = 5 * time.Minute
	worldCreateWait = 5 * time.Second
)

var (
	errBadPredicate = errors.New("unknown --until predicate")
	errWaitTimeout  = errors.New("timed out waiting")
)

type predicate struct {
	kind, arg string
	implicit  bool // the default derived from the launch URL, not typed by anyone
}

func (p predicate) String() string { return p.kind + ":" + p.arg }

// holds evaluates the predicate against what the page currently reports.
func (p predicate) holds(href, title string, js bool) bool {
	switch p.kind {
	case predURL:
		return globMatch(p.arg, href)
	case predGone:
		return !globMatch(p.arg, href)
	case predTitle:
		return strings.Contains(title, p.arg)
	case predJS:
		return js
	default:
		return false
	}
}

// parsePredicate reads a --until spec. An empty spec is the default condition:
// block until the URL leaves the origin the session was opened at, which is the
// "the human finished signing in and got redirected" case.
func parsePredicate(spec, launchURL string) (predicate, error) {
	if spec == "" {
		origin := originPrefix(launchURL)
		if origin == "" {
			return predicate{}, fmt.Errorf("%w: nothing to derive a default from - pass --until 'url:...' or open a URL", errBadPredicate)
		}
		return predicate{kind: predGone, arg: origin + "*", implicit: true}, nil
	}
	kind, arg, found := strings.Cut(spec, ":")
	if !found || arg == "" {
		return predicate{}, fmt.Errorf("%w %q: use url:<glob>, gone:<glob>, title:<substring> or js:<expression>", errBadPredicate, spec)
	}
	switch kind {
	case predURL, predGone, predTitle, predJS:
		return predicate{kind: kind, arg: arg}, nil
	default:
		return predicate{}, fmt.Errorf("%w %q: use url:<glob>, gone:<glob>, title:<substring> or js:<expression>", errBadPredicate, kind)
	}
}

// originPrefix is scheme://host of a URL, the unit "left the sign-in origin"
// means. It returns "" for anything that is not an absolute http(s) URL.
func originPrefix(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// globMatch is `*`-only wildcard matching. path.Match is deliberately not used:
// it treats `/` as a separator a `*` cannot cross, which is wrong for URLs -
// `https://x.example*` would then fail to match its own paths.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(s, part)
		if i < 0 {
			return false
		}
		s = s[i+len(part):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// A landed URL is printed through mask.Params: the default predicate waits for
// the page to LEAVE the sign-in origin, which lands it on the OAuth or
// magic-link callback - and that URL carries the code in a query parameter.
//
// waitUntil blocks until the predicate holds, polling the page in its ISOLATED
// world - never the page's main world, which a sign-in page can trap and read as
// automation. It is strictly print-and-wait: the moment it clicked anything,
// cuttle would have become the driver.
func waitUntil(ctx context.Context, out io.Writer, host string, port, vncPort int, p predicate, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	s, closeSession, err := dialActivePage(ctx, host, port, vncPort)
	if err != nil {
		return err
	}
	defer closeSession()

	href := ""
	for {
		state, serr := s.pageState(ctx, p)
		if serr == nil {
			href = state.href
			if p.holds(state.href, state.title, state.js) {
				if p.implicit {
					// "left the sign-in origin", not "signed in". The default predicate is
					// satisfied by any redirect off that origin - an SSO hop to an identity
					// provider mid-login satisfies it - and `auth status` is careful not to
					// claim a session from weaker evidence than this.
					fmt.Fprintf(out, "left %s - now at %s\n", p.arg, mask.Params(state.href))
				} else {
					fmt.Fprintf(out, "condition met (%s): %s\n", p, mask.Params(state.href))
				}
				return nil
			}
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(out, "timed out after %s; still at %s\n", timeout, mask.Params(href))
			return fmt.Errorf("%w for %s after %s", errWaitTimeout, p, timeout)
		case <-time.After(waitPollGap):
		}
	}
}

type pageState struct {
	href, title string
	js          bool
}

// pageState reads the page's location and title (and the js: predicate's own
// result) in one evaluate. The isolated world is rebuilt whenever it has gone -
// which is exactly what a navigation does, and a navigation is the event this is
// usually waiting for.
func (s *cdpSession) pageState(ctx context.Context, p predicate) (pageState, error) {
	expr := "({href:location.href,title:document.title,js:false})"
	if p.kind == predJS {
		expr = "({href:location.href,title:document.title,js:!!(" + p.arg + ")})"
	}
	res, err := s.evalIsolated(ctx, expr)
	if err != nil {
		return pageState{}, err
	}
	href, _ := res["href"].(string)
	title, _ := res["title"].(string)
	js, _ := res["js"].(bool)
	return pageState{href: href, title: title, js: js}, nil
}

// evalIsolated evaluates expr in the session's isolated world, creating it on
// first use and once more if it has been retired under us.
func (s *cdpSession) evalIsolated(ctx context.Context, expr string) (map[string]any, error) {
	for attempt := range 2 {
		if s.worldID == 0 {
			if err := s.createWorld(ctx); err != nil {
				return nil, err
			}
		}
		res, err := s.call(ctx, "Runtime.evaluate", map[string]any{
			"expression": expr, "returnByValue": true, "contextId": s.worldID,
		})
		if err != nil {
			s.worldID = 0 // the world went with the document; rebuild and retry once
			if attempt == 0 {
				continue
			}
			return nil, err
		}
		result, _ := res["result"].(map[string]any)
		value, _ := result["value"].(map[string]any)
		return value, nil
	}
	return nil, errNoPageTarget
}

func (s *cdpSession) createWorld(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, worldCreateWait)
	defer cancel()
	tree, err := s.call(ctx, "Page.getFrameTree", map[string]any{})
	if err != nil {
		return err
	}
	frameTree, _ := tree["frameTree"].(map[string]any)
	frame, _ := frameTree["frame"].(map[string]any)
	frameID, _ := frame["id"].(string)
	if frameID == "" {
		return errNoPageTarget
	}
	world, err := s.call(ctx, "Page.createIsolatedWorld", map[string]any{
		"frameId": frameID, "worldName": "cuttle_wait",
	})
	if err != nil {
		return err
	}
	id, _ := world["executionContextId"].(float64)
	if id == 0 {
		return errNoPageTarget
	}
	s.worldID = int(id)
	return nil
}
