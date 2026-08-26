package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Two small session-facing surfaces that exist to make a human handoff cost
// less: what the browser is already signed into, and getting the right window in
// front of the person taking over.

// authTimeout bounds one live cookie read. It is a single browser-level CDP call
// against a local port, so anything slower than this is a browser in trouble.
const authTimeout = 5 * time.Second

// originAuth is one domain's cookie SHAPE. No name and no value: the whole point
// of the verb is to answer "is this session worth reusing" without handing the
// answer's evidence to the transcript.
type originAuth struct {
	Domain        string `json:"domain"`
	Cookies       int    `json:"cookies"`
	Session       int    `json:"session_cookies"`
	SoonestExpiry string `json:"soonest_expiry,omitempty"`
}

// handleAuthStatus reports which domains the running browser holds cookies for.
//
// It live-reads the browser-global cookie jar rather than the snapshot store,
// because in session mode - the CLI's only mode - the per-seed state API is
// closed and the daemon holds ZERO snapshots, so "when state was captured" has
// no answer there. Reading over the browser endpoint also means no scratch tab:
// Storage.getCookies is browser-level.
func (m *multiplexer) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	_, inst := m.runningSeedInstance(w, r)
	if inst == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), authTimeout)
	defer cancel()
	domains, ok := browserCookieDomains(ctx, inst.cdpPort)
	if !ok {
		writeJSON(w, http.StatusBadGateway, map[string]any{keyError: "could not read the browser's cookies"})
		return
	}
	if filter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("origin"))); filter != "" {
		domains = filterDomains(domains, filter)
	}
	writeJSON(w, http.StatusOK, map[string]any{"origins": domains})
}

// filterDomains keeps the domains an origin argument is about: an exact match or
// a parent of it, so `auth status https://app.example.com` also reports the
// `.example.com` cookie that actually carries the session.
func filterDomains(domains []originAuth, filter string) []originAuth {
	if host := hostOfOriginArg(filter); host != "" {
		filter = host
	}
	out := []originAuth{}
	for _, d := range domains {
		bare := strings.TrimPrefix(d.Domain, ".")
		if bare == filter || strings.HasSuffix(filter, "."+bare) {
			out = append(out, d)
		}
	}
	return out
}

func hostOfOriginArg(arg string) string {
	if i := strings.Index(arg, "://"); i >= 0 {
		arg = arg[i+3:]
	}
	arg = strings.TrimSuffix(strings.SplitN(arg, "/", 2)[0], ".")
	return strings.SplitN(arg, ":", 2)[0]
}

// browserCookieDomains reads the browser's cookie jar and folds it to per-domain
// counts.
//
// It unions EVERY browser context, not just the default one. A bare
// Storage.getCookies resolves to the default context alone, and a driver running
// under --allow-context-creation logs in inside a context it made - so the
// default view is empty and this verb would report "assume signed out" for a
// session that is signed in, which is the exact false negative it exists to
// prevent. internal/cdp's getAllCookies carries the same union for the same
// reason over chromedp; if one changes, change both.
//
// The raw CDP path is used rather than that chromedp one because chromedp's
// session management opens a scratch tab, which a status verb has no business
// doing to a page a human is looking at.
func browserCookieDomains(ctx context.Context, port int) ([]originAuth, bool) {
	conn, err := dialBrowser(ctx, port)
	if err != nil {
		return nil, false
	}
	defer conn.close()

	cookies, ok := contextCookies(ctx, conn, "")
	if !ok {
		return nil, false
	}
	// A browser that never created a context returns none here, so this costs one
	// round trip and nothing else. An enumeration failure degrades to the
	// default-context view rather than failing a read that would have worked.
	if ids, defaultID, err := browserContexts(ctx, conn); err == nil {
		for _, id := range ids {
			if id == defaultID {
				continue // already covered by the bare call above
			}
			if extra, got := contextCookies(ctx, conn, id); got {
				cookies = append(cookies, extra...)
			}
		}
	}
	return foldCookieDomains(cookies), true
}

func contextCookies(ctx context.Context, conn *cdpConn, browserContextID string) ([]rawCookie, bool) {
	params := map[string]any{}
	if browserContextID != "" {
		params["browserContextId"] = browserContextID
	}
	resp, err := conn.callRaw(ctx, "", "Storage.getCookies", params)
	if err != nil {
		return nil, false
	}
	var parsed struct {
		Result struct {
			Cookies []rawCookie `json:"cookies"`
		} `json:"result"`
	}
	if json.Unmarshal(resp, &parsed) != nil {
		return nil, false
	}
	return parsed.Result.Cookies, true
}

func browserContexts(ctx context.Context, conn *cdpConn) ([]string, string, error) {
	resp, err := conn.callRaw(ctx, "", "Target.getBrowserContexts", map[string]any{})
	if err != nil {
		return nil, "", err
	}
	var parsed struct {
		Result struct {
			BrowserContextIDs       []string `json:"browserContextIds"`
			DefaultBrowserContextID string   `json:"defaultBrowserContextId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return nil, "", fmt.Errorf("%w: %w", errCDPCall, err)
	}
	return parsed.Result.BrowserContextIDs, parsed.Result.DefaultBrowserContextID, nil
}

// rawCookie is the only part of a cookie this file reads. Its name and value are
// deliberately not in the struct: a field that does not exist cannot be logged
// or serialized by accident.
type rawCookie struct {
	Domain  string  `json:"domain"`
	Expires float64 `json:"expires"`
}

// foldCookieDomains reduces a jar to per-domain counts and the first expiry.
func foldCookieDomains(cookies []rawCookie) []originAuth {
	byDomain := map[string]*originAuth{}
	soonest := map[string]float64{}
	for _, c := range cookies {
		d := byDomain[c.Domain]
		if d == nil {
			d = &originAuth{Domain: c.Domain}
			byDomain[c.Domain] = d
		}
		d.Cookies++
		// Chrome reports a session cookie as expires -1.
		if c.Expires <= 0 {
			d.Session++
			continue
		}
		if cur, ok := soonest[c.Domain]; !ok || c.Expires < cur {
			soonest[c.Domain] = c.Expires
		}
	}
	out := make([]originAuth, 0, len(byDomain))
	for domain, d := range byDomain {
		if exp, ok := soonest[domain]; ok {
			d.SoonestExpiry = time.Unix(int64(exp), 0).UTC().Format(time.RFC3339)
		}
		out = append(out, *d)
	}
	slices.SortFunc(out, func(a, b originAuth) int { return strings.Compare(a.Domain, b.Domain) })
	return out
}

// raiseTimeout bounds the window raise. `xdotool search --sync` blocks until a
// window matches, so on a headless daemon (no X server, no window) it would wait
// forever without this.
const raiseTimeout = 3 * time.Second

// handleWindowRaise brings a seed's Chrome window to the front of the shared X
// display, so a human handed the viewer link sees the tab they were asked about
// rather than whichever window the window manager had on top.
func (m *multiplexer) handleWindowRaise(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	_, inst := m.runningSeedInstance(w, r)
	if inst == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), raiseTimeout)
	defer cancel()
	if !raiseWindowOfPID(ctx, inst.process.pid()) {
		// Not an error worth failing a handoff over: a headless daemon has no window
		// to raise, and the viewer link still works.
		writeJSON(w, http.StatusOK, map[string]any{keyStatus: "no window to raise"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{keyStatus: "raised"})
}

// raiseWindowOfPID activates and raises the first visible window belonging to
// pid. The window id is captured FIRST and then acted on: the tempting chained
// form (`xdotool search ... windowactivate windowraise`) is broken - xdotool eats
// `windowactivate` as part of the search pattern, and a chained command defaults
// to only the first result rather than the window you meant.
func raiseWindowOfPID(ctx context.Context, pid int) bool {
	if pid <= 0 {
		return false
	}
	//nolint:gosec // fixed argv; the only variable is a pid from our own process table
	search := exec.CommandContext(ctx, "xdotool",
		"search", "--sync", "--onlyvisible", "--pid", strconv.Itoa(pid))
	// `search --sync` blocks until a window matches, so the context deadline is
	// the only thing that ends it on a headless daemon - and WaitDelay is what
	// makes that kill real if a descendant holds the pipe open.
	search.WaitDelay = time.Second
	found, err := search.Output()
	if err != nil {
		return false
	}
	wid := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(found)), "\n", 2)[0])
	if wid == "" {
		return false
	}
	// wid comes from xdotool above and is never shell-interpreted.
	raise := exec.CommandContext(ctx, "xdotool", "windowactivate", "--sync", wid, "windowraise", wid)
	raise.WaitDelay = time.Second
	if err := raise.Run(); err != nil {
		logWarn("window raise for pid %d failed: %v", pid, err)
		return false
	}
	return true
}
