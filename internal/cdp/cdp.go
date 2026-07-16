package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/storage"
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

// Extract connects to the seed's browser and reads its storage state. Cookies
// are browser-global; localStorage is origin-scoped, so each origin is visited
// on a scratch tab (localStorage is only readable from a document of that
// origin). An origin that fails to load contributes no localStorage rather than
// failing the whole extract; its origin string is returned in failed so the
// caller can tell a transient load failure apart from a genuinely-empty origin
// and preserve the last-known localStorage for the former.
func Extract(ctx context.Context, cdpBase, seed string, origins []string) (*StorageState, []string, error) {
	taskCtx, cancel, err := connect(ctx, cdpBase, seed)
	if err != nil {
		return nil, nil, err
	}
	defer cancel()

	st := &StorageState{Cookies: []Cookie{}, Origins: []Origin{}}
	if err := chromedp.Run(taskCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		cs, err := getAllCookies(ctx)
		if err != nil {
			return err
		}
		st.Cookies = fromCDPCookies(cs)
		return nil
	})); err != nil {
		return nil, nil, err //nolint:wrapcheck // getAllCookies already wraps
	}

	var failed []string
	for _, origin := range origins {
		var items map[string]string
		read := chromedp.ActionFunc(func(ctx context.Context) error {
			m, err := readLocalStorage(ctx)
			items = m
			return err
		})
		if err := chromedp.Run(taskCtx, chromedp.Navigate(origin), read); err != nil {
			failed = append(failed, origin)
			continue
		}
		if len(items) > 0 {
			st.Origins = append(st.Origins, Origin{Origin: origin, LocalStorage: mapToItems(items)})
		}
	}
	return st, failed, nil
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
