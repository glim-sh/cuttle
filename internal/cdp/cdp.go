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

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// detachTimeout bounds the DetachFromTarget that closes out a per-tab read.
const detachTimeout = 2 * time.Second

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
// A bare Storage.getCookies resolves to the DEFAULT browser context only. That
// is the whole story for a driver that attaches, but one running under
// --allow-context-creation logs in inside a context it made, and capturing the
// default view alone would persist a cookie-less snapshot OVER a good one and
// then drop the profile dir with it. So union every context.
//
// This keeps the snapshot honest; it does not make those cookies reusable.
// Restore writes into the default context (setCookies rides a scratch tab), so a
// driver that creates a fresh context next session still starts logged out.
func getAllCookies(ctx context.Context) ([]*network.Cookie, error) {
	cs, err := storage.GetCookies().Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("Storage.getCookies: %w", err)
	}
	// A browser that never created a context returns none here, so this costs one
	// extra round trip and nothing else. An enumeration failure degrades to the
	// default-context view rather than failing a capture that used to succeed.
	if ids, defaultID, cerr := target.GetBrowserContexts().Do(ctx); cerr == nil {
		for _, id := range ids {
			if id == defaultID {
				continue // already covered by the bare call above
			}
			extra, gerr := storage.GetCookies().WithBrowserContextID(id).Do(ctx)
			if gerr != nil {
				continue
			}
			cs = append(cs, extra...)
		}
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
	// context.WithoutCancel is load-bearing, not defensive. detachNotClose below
	// clears chromedp's c.Target so its cleanup goroutine skips closing this
	// pre-existing tab, and that goroutine reads c.Target unsynchronised: it
	// checks `c.Target == nil` and then dereferences the field. While the
	// goroutine is still parked on ctx.Done() our write is safely ordered before
	// it - but if the CALLER's deadline fires, Done is already closed, the
	// goroutine is running concurrently, and it can pass its own nil guard and
	// then dereference the field we just nilled. That nil deref panics the whole
	// daemon: it happens in chromedp's goroutine, so no recover of ours can reach
	// it, and cuttle is a single-replica farm. Detaching Done from the caller
	// means only the cancel inside detachNotClose closes it, strictly after the
	// write, so the goroutine always observes nil.
	tctx, cancel := chromedp.NewContext(context.WithoutCancel(parent), chromedp.WithTargetID(id))

	type result struct {
		items map[string]string
		err   error
	}
	// Run and tear down in ONE goroutine, so the c.Target write can never race a
	// teardown triggered from elsewhere. The caller is still bounded by its own
	// deadline via the select below; a read that outlives it finishes and cleans
	// up on its own. A browser that accepts the attach and then never answers
	// would strand this goroutine - the websocket erroring is the backstop, which
	// is the price of never panicking the daemon.
	ch := make(chan result, 1)
	go func() {
		var items map[string]string
		err := chromedp.Run(tctx, chromedp.ActionFunc(func(ctx context.Context) error {
			m, rerr := readLocalStorage(ctx)
			items = m
			return rerr
		}))
		detachNotClose(tctx, cancel)
		ch <- result{items, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return nil, false
		}
		return r.items, true
	case <-parent.Done():
		// Unreadable within the caller's budget: the origin falls through to
		// carry-forward rather than being cleared.
		return nil, false
	}
}

// detachNotClose tears down a WithTargetID context WITHOUT closing the attached
// tab. chromedp's own cancel() runs Target.closeTarget on the pre-existing target
// (chromedp.go: detach + CloseTarget); closing the user's last open tab makes the
// browser exit, so a periodic checkpoint would tear the whole session down every
// time it ran. We detach the flat session ourselves and clear c.Target so
// chromedp's cancel becomes a no-op teardown (it skips detach+close when
// Target==nil) - the tab stays open and the session is not leaked.
//
// CALLER CONTRACT: tctx MUST come from a context that nothing but this function
// can cancel (see readTargetLocalStorage's context.WithoutCancel). Clearing
// c.Target is a write to a field chromedp's cleanup goroutine reads without
// synchronisation, and that goroutine is only safely parked while Done is open.
// Hand this a caller-cancellable context and a deadline firing mid-read turns
// the write into a data race whose loser dereferences nil and panics the daemon.
func detachNotClose(tctx context.Context, cancel context.CancelFunc) {
	if c := chromedp.FromContext(tctx); c != nil && c.Target != nil && c.Browser != nil {
		sid := c.Target.SessionID
		c.Target = nil
		dctx, dcancel := context.WithTimeout(context.Background(), detachTimeout)
		_ = target.DetachFromTarget().WithSessionID(sid).Do(cdp.WithExecutor(dctx, c.Browser))
		dcancel()
	}
	cancel()
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
		// chromedp.Cancel rather than the plain context cancel: it runs the same
		// teardown but waits for it (cancel + closedTarget.Wait), so the scratch tab
		// is closed before the capture path terminates the browser instead of after.
		// Tidiness, not a crash fix - the daemon panic this once claimed to solve
		// was the c.Target data race in readTargetLocalStorage, and cancelling
		// synchronously here did nothing for it.
		_ = chromedp.Cancel(taskCtx)
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
