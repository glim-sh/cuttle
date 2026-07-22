package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// lsReadExpr returns the whole localStorage as a plain object so ReturnByValue
// serializes it to a {key:value} JSON map.
const lsReadExpr = "Object.assign({}, window.localStorage)"

// The write expression is a marker-wrapped IIFE fed the items as a JSON literal;
// the marker/suffix let a test harness recover the payload deterministically.
const (
	lsWritePrefix = "/*cuttle-ls-write*/(function(d){for(var k in d){try{window.localStorage.setItem(k,d[k])}catch(e){}}})("
	lsWriteSuffix = ")"
)

var (
	errEval        = errors.New("localStorage evaluation failed")
	errNoWSURL     = errors.New("multiplexer did not return a webSocketDebuggerUrl")
	errBadResponse = errors.New("bad /json/version response")
)

func lsWriteExpr(items map[string]string) string {
	b, _ := json.Marshal(items)
	return lsWritePrefix + string(b) + lsWriteSuffix
}

// getAllCookies and setCookies operate on browser-global cookies through the ctx
// executor, so the same code path is exercised by the real chromedp connection
// and by a fake CDP endpoint in tests. Storage.getCookies returns every cookie
// in the browser context, unlike Network.getCookies which is scoped to the
// current tab's URLs (empty on the scratch tab we connect on).
func getAllCookies(ctx context.Context) ([]*network.Cookie, error) {
	cs, err := storage.GetCookies().Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("Storage.getCookies: %w", err)
	}
	return cs, nil
}

func setCookies(ctx context.Context, params []*network.CookieParam) error {
	if len(params) == 0 {
		return nil
	}
	if err := network.SetCookies(params).Do(ctx); err != nil {
		return fmt.Errorf("Network.setCookies: %w", err)
	}
	return nil
}

func readLocalStorage(ctx context.Context) (map[string]string, error) {
	p := runtime.Evaluate(lsReadExpr)
	p.ReturnByValue = true
	res, exc, err := p.Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("Runtime.evaluate(read): %w", err)
	}
	if exc != nil {
		return nil, fmt.Errorf("%w: %s", errEval, exc.Text)
	}
	m := map[string]string{}
	if res != nil && len(res.Value) > 0 {
		if err := json.Unmarshal([]byte(res.Value), &m); err != nil {
			return nil, fmt.Errorf("decoding localStorage: %w", err)
		}
	}
	return m, nil
}

func writeLocalStorage(ctx context.Context, items map[string]string) error {
	if len(items) == 0 {
		return nil
	}
	p := runtime.Evaluate(lsWriteExpr(items))
	_, exc, err := p.Do(ctx)
	if err != nil {
		return fmt.Errorf("Runtime.evaluate(write): %w", err)
	}
	if exc != nil {
		return fmt.Errorf("%w: %s", errEval, exc.Text)
	}
	return nil
}

// Extract connects to the seed's browser and reads its storage state WITHOUT
// perturbing the live session. Cookies are a pure browser-global
// Storage.getCookies read. localStorage is read IN PLACE from each already-open
// page target - never by navigating the scratch tab to a live origin. That
// navigation was the bug: the scratch tab shares the browser-global cookie jar,
// so re-fetching a live origin as the user's session let the server rotate a
// mid-login cookie (e.g. github.com's _gh_sess), invalidating the CSRF token
// bound to the login form the user was about to submit ("What? your browser did
// something unexpected"). A checkpoint now issues zero requests to any origin and
// cannot mutate the live jar.
//
// origins is the set the caller expects to see (its prior snapshot's origins plus
// cookie-derived ones); any origin without an open tab to read is returned in
// failed so the caller carries its last-known localStorage forward rather than
// clearing it - closing a tab must not drop its persisted localStorage. Origins
// discovered from open tabs beyond that set are captured too, so a brand-new
// login is snapshotted on its very first checkpoint.
func Extract(ctx context.Context, cdpBase, seed string, origins []string) (*StorageState, []string, error) {
	taskCtx, cancel, err := connect(ctx, cdpBase, seed)
	if err != nil {
		return nil, nil, err
	}
	defer cancel()

	st := &StorageState{Cookies: []Cookie{}, Origins: []Origin{}}
	var targets []*target.Info
	if err := chromedp.Run(taskCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		cs, cerr := getAllCookies(ctx)
		if cerr != nil {
			return cerr
		}
		st.Cookies = fromCDPCookies(cs)
		ts, terr := chromedp.Targets(ctx)
		if terr != nil {
			return fmt.Errorf("Target.getTargets: %w", terr)
		}
		targets = ts
		return nil
	})); err != nil {
		return nil, nil, err //nolint:wrapcheck // getAllCookies already wraps
	}

	origins2, failed := foldLocalStorage(readOpenLocalStorage(taskCtx, targets), origins)
	st.Origins = origins2
	return st, failed, nil
}

// readOpenLocalStorage reads localStorage in place from every already-open page
// target, keyed by origin. An origin present in the result was genuinely read (an
// open tab exists for it), so an origin absent from it has no readable tab and is
// left to the caller's carry-forward. Non-http(s) targets (about:blank,
// chrome://newtab) hold no site localStorage and are skipped; a same-origin
// duplicate tab is read once (localStorage is origin-scoped, so the reads match).
func readOpenLocalStorage(taskCtx context.Context, targets []*target.Info) map[string]map[string]string {
	byOrigin := map[string]map[string]string{}
	for _, t := range targets {
		if t == nil || t.Type != "page" {
			continue
		}
		origin := originOf(t.URL)
		if origin == "" {
			continue
		}
		if _, done := byOrigin[origin]; done {
			continue
		}
		items, ok := readTargetLocalStorage(taskCtx, t.TargetID)
		if !ok {
			continue // unreadable tab: leave the origin to carry-forward, don't clear it
		}
		byOrigin[origin] = items
	}
	return byOrigin
}

// readTargetLocalStorage attaches to one existing page target and reads its
// localStorage in that tab's own context - no navigation, no network. A tab that
// closed mid-pass or refuses the read yields ok=false so its origin falls through
// to carry-forward rather than being cleared.
func readTargetLocalStorage(parent context.Context, id target.ID) (map[string]string, bool) {
	tctx, cancel := chromedp.NewContext(parent, chromedp.WithTargetID(id))
	defer cancel()
	var items map[string]string
	err := chromedp.Run(tctx, chromedp.ActionFunc(func(ctx context.Context) error {
		m, rerr := readLocalStorage(ctx)
		items = m
		return rerr
	}))
	if err != nil {
		return nil, false
	}
	return items, true
}

// originOf reduces a page target URL to its storage origin (scheme://host[:port])
// in the same canonical form profile.CandidateOrigins produces, so a freshly-read
// origin matches the caller's carry-forward bookkeeping and stays byte-stable
// across checkpoints. Non-http(s) targets return "".
func originOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
}

// foldLocalStorage turns the per-origin localStorage read from open tabs into the
// snapshot's Origin list (origins carrying items, sorted for a stable
// snapshot/ETag) and reports which requested origins had no open tab to read.
// An origin that was read but is genuinely empty yields no Origin entry yet is not
// reported failed - it was observed empty, not merely unreadable, so it must not
// resurrect a stale carry-forward.
func foldLocalStorage(byOrigin map[string]map[string]string, requested []string) ([]Origin, []string) {
	keys := make([]string, 0, len(byOrigin))
	for o := range byOrigin {
		keys = append(keys, o)
	}
	slices.Sort(keys)
	origins := make([]Origin, 0, len(keys))
	for _, o := range keys {
		if items := byOrigin[o]; len(items) > 0 {
			origins = append(origins, Origin{Origin: o, LocalStorage: mapToItems(items)})
		}
	}
	var failed []string
	for _, o := range requested {
		if _, ok := byOrigin[o]; !ok {
			failed = append(failed, o)
		}
	}
	return origins, failed
}

// Inject writes the storage state into the seed's fresh browser: cookies first
// (browser-global), then per-origin localStorage on a scratch tab navigated to
// each origin.
func Inject(ctx context.Context, cdpBase, seed string, st *StorageState) error {
	taskCtx, cancel, err := connect(ctx, cdpBase, seed)
	if err != nil {
		return err
	}
	defer cancel()

	if err := chromedp.Run(taskCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return setCookies(ctx, toCookieParams(st.Cookies))
	})); err != nil {
		return err //nolint:wrapcheck // setCookies already wraps
	}

	for _, o := range st.Origins {
		items := itemsToMap(o.LocalStorage)
		if len(items) == 0 {
			continue
		}
		write := chromedp.ActionFunc(func(ctx context.Context) error {
			return writeLocalStorage(ctx, items)
		})
		if err := chromedp.Run(taskCtx, chromedp.Navigate(o.Origin), write); err != nil {
			return fmt.Errorf("seeding localStorage for %s: %w", o.Origin, err)
		}
	}
	return nil
}

// connect resolves the seed's browser WebSocket URL through the multiplexer and
// opens a chromedp context bound to a fresh scratch tab. NoModifyURL keeps the
// resolved ?fingerprint routing intact, and the remote allocator guarantees
// chromedp attaches to the running browser instead of launching one.
func connect(ctx context.Context, cdpBase, seed string) (context.Context, context.CancelFunc, error) {
	wsURL, err := browserWSURL(ctx, cdpBase, seed)
	if err != nil {
		return nil, nil, err
	}
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, wsURL, chromedp.NoModifyURL)
	taskCtx, cancelTask := chromedp.NewContext(allocCtx)
	cancel := func() {
		cancelTask()
		cancelAlloc()
	}
	return taskCtx, cancel, nil
}

// browserWSURL asks the multiplexer for the seed's browser CDP endpoint. The
// multiplexer rewrites webSocketDebuggerUrl to its own host, so the returned URL
// is correct behind a port-forward / ssh -L.
func browserWSURL(ctx context.Context, cdpBase, seed string) (string, error) {
	base, err := url.Parse(cdpBase)
	if err != nil {
		return "", fmt.Errorf("parsing CDP base %q: %w", cdpBase, err)
	}
	base.Path = "/json/version"
	if seed != "" {
		base.RawQuery = "fingerprint=" + url.QueryEscape(seed)
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base.String(), nil)
	if err != nil {
		return "", fmt.Errorf("building /json/version request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("reaching CDP: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading /json/version: %w", err)
	}
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("%w: %w", errBadResponse, err)
	}
	if v.WebSocketDebuggerURL == "" {
		return "", errNoWSURL
	}
	return v.WebSocketDebuggerURL, nil
}
