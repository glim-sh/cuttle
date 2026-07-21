package serve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/glim-sh/cuttle/internal/cdp"
)

// Repeated string literals shared across the serve package.
const (
	schemeHTTP      = "http"
	schemeHTTPS     = "https"
	keyFingerprint  = "fingerprint"
	keyLocale       = "locale"
	keyProxy        = "proxy"
	keyTimezone     = "timezone"
	keyError        = "error"
	msgChromeFailed = "Chrome failed to start"
	msgInvalidSeed  = "Invalid fingerprint seed"
)

// specialParams are handled explicitly; any other query param becomes a generic
// --fingerprint-{key}={val} passthrough.
var specialParams = map[string]struct{}{
	keyFingerprint: {}, keyProxy: {}, "geoip": {}, keyLocale: {}, keyTimezone: {},
}

var trustedWSOrigins = map[string]struct{}{
	"devtools://devtools":        {},
	"chrome-devtools://devtools": {},
}

// multiplexer holds the shared pool and the advertised port for URL rewrites.
type multiplexer struct {
	pool *chromePool
	port int
}

func (m *multiplexer) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", m.handleRoot)
	for _, p := range []string{"GET /json/version", "GET /json/version/"} {
		mux.HandleFunc(p, m.handleJSONVersion)
	}
	for _, p := range []string{"GET /json/list", "GET /json/list/", "GET /json", "GET /json/"} {
		mux.HandleFunc(p, m.handleJSONList)
	}
	mux.HandleFunc("GET /profile/{seed}/state", m.handleGetState)
	mux.HandleFunc("PUT /profile/{seed}/state", m.handlePutState)
	mux.HandleFunc("GET /fingerprint/{seed}/devtools/{path...}", m.handleWSSeed)
	mux.HandleFunc("GET /devtools/{path...}", m.handleWSDefault)
	return mux
}

// stateBodyLimit caps a PUT storage-state body. Auth state is small (cookies +
// per-origin localStorage); the cap just stops a pathological upload.
const stateBodyLimit = 8 << 20

// handleGetState returns a seed's current storage state as Playwright-shaped JSON
// with an ETag. When the seed's Chrome is running it live-extracts the fresh
// state for the body; otherwise it serves the last snapshot; 404 when neither
// exists. GET is side-effect-free: it never writes the store, so a concurrent
// reader can never rotate the ETag out from under another client's
// GET-then-If-Match-PUT. The returned ETag is the stored snapshot's tag (the
// token a PUT If-Match compares against) when one exists, else a content hash of
// the live body. The seed name is validated with the same grammar as a
// fingerprint seed.
func (m *multiplexer) handleGetState(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedState(w, r) {
		return
	}
	seed := r.PathValue("seed")
	if !validSeed(seed) {
		writeJSON(w, http.StatusBadRequest, map[string]any{keyError: msgInvalidSeed})
		return
	}
	stored, hasStored := m.pool.store.get(seed)
	if inst := m.pool.runningInstance(seed); inst != nil {
		ctx, cancel := context.WithTimeout(r.Context(), captureTimeout)
		defer cancel()
		var prior *cdp.StorageState
		if hasStored {
			prior = stored.State
		}
		if st, ok := m.pool.extractSeedState(ctx, loopbackBase(inst.cdpPort), prior); ok {
			etag := etagOf(st)
			if hasStored {
				etag = stored.ETag
			}
			w.Header().Set("ETag", etag)
			writeJSON(w, http.StatusOK, st)
			return
		}
	}
	if hasStored {
		w.Header().Set("ETag", stored.ETag)
		writeJSON(w, http.StatusOK, stored.State)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{keyError: "no state for seed"})
}

// handlePutState records a seed's storage state and marks the seed supervised.
// Semantic (one, on purpose): the snapshot is always stored; it is injected into
// the seed's Chrome immediately when the seed is running, and otherwise rides the
// seed's next launch (getOrLaunch re-injects any stored snapshot). If-Match is
// honored for optimistic concurrency (412 on mismatch); a PUT without If-Match is
// last-writer-wins. Body is Playwright-shaped storage-state JSON.
func (m *multiplexer) handlePutState(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedState(w, r) {
		return
	}
	seed := r.PathValue("seed")
	if !validSeed(seed) {
		writeJSON(w, http.StatusBadRequest, map[string]any{keyError: msgInvalidSeed})
		return
	}
	var st cdp.StorageState
	if err := json.NewDecoder(io.LimitReader(r.Body, stateBodyLimit)).Decode(&st); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{keyError: "invalid storage-state body"})
		return
	}
	etag, conflict, err := m.pool.store.put(seed, &st, true, r.Header.Get("If-Match"))
	if conflict {
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{keyError: "ETag mismatch"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{keyError: "persisting state failed"})
		return
	}
	if inst := m.pool.runningInstance(seed); inst != nil {
		ctx, cancel := context.WithTimeout(r.Context(), captureTimeout)
		defer cancel()
		if ierr := m.pool.injectSeedState(ctx, inst, &st); ierr != nil {
			logWarn("state PUT: inject into running seed=%s failed: %v", seed, ierr)
		}
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "etag": etag})
}

// parseConnectionParams parses a raw query string into a connection request,
// mirroring parse_qs(keep_blank_values=False): blank values are dropped and the
// first value per key wins. Unknown params map to --fingerprint-{key}={val}.
func parseConnectionParams(raw string) connectRequest {
	req := connectRequest{}
	seen := map[string]struct{}{}
	for pair := range strings.SplitSeq(raw, "&") {
		if pair == "" {
			continue
		}
		rawKey, rawVal, _ := strings.Cut(pair, "=")
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			continue
		}
		val, err := url.QueryUnescape(rawVal)
		if err != nil || val == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		switch key {
		case keyFingerprint:
			req.seed = val
		case keyTimezone:
			req.timezone = val
		case keyLocale:
			req.locale = val
		case keyProxy:
			req.proxy = val
		case "geoip":
			l := strings.ToLower(val)
			req.geoip = l == "true" || l == "1" || l == "yes"
		default:
			if _, special := specialParams[key]; !special {
				req.extraArgs = append(req.extraArgs, "--fingerprint-"+key+"="+val)
			}
		}
	}
	return req
}

func (m *multiplexer) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, m.pool.status())
}

func (m *multiplexer) handleJSONVersion(w http.ResponseWriter, r *http.Request) {
	params := parseConnectionParams(r.URL.RawQuery)
	cp, err := m.pool.getOrLaunch(r.Context(), params)
	if err != nil {
		writeLaunchError(w, err)
		return
	}

	var data map[string]any
	if err := fetchCDP(r.Context(), cp.cdpPort, "/json/version", &data); err != nil {
		logError("failed to reach Chrome CDP (port %d): %v", cp.cdpPort, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{keyError: "CDP endpoint unreachable"})
		return
	}

	host := externalHost(r, m.port)
	scheme := wsScheme(r)
	wsPath := "devtools/browser"
	if params.seed != "" {
		wsPath = "fingerprint/" + params.seed + "/devtools/browser"
	}
	guid := ""
	if orig, ok := data["webSocketDebuggerUrl"].(string); ok && strings.Contains(orig, "/devtools/") {
		guid = orig[strings.LastIndex(orig, "/")+1:]
	}
	data["webSocketDebuggerUrl"] = scheme + "://" + host + "/" + wsPath + "/" + guid
	writeJSON(w, http.StatusOK, data)
}

func (m *multiplexer) handleJSONList(w http.ResponseWriter, r *http.Request) {
	params := parseConnectionParams(r.URL.RawQuery)
	cp, err := m.pool.getOrLaunch(r.Context(), params)
	if err != nil {
		writeLaunchError(w, err)
		return
	}

	var data []map[string]any
	if err := fetchCDP(r.Context(), cp.cdpPort, "/json/list", &data); err != nil {
		logError("failed to reach Chrome CDP (port %d): %v", cp.cdpPort, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{keyError: "CDP endpoint unreachable"})
		return
	}

	host := externalHost(r, m.port)
	scheme := wsScheme(r)
	for _, entry := range data {
		orig, ok := entry["webSocketDebuggerUrl"].(string)
		if !ok {
			continue
		}
		tail := orig[strings.LastIndex(orig, "/devtools/")+len("/devtools/"):]
		if params.seed != "" {
			entry["webSocketDebuggerUrl"] = scheme + "://" + host + "/fingerprint/" + params.seed + "/devtools/" + tail
		} else {
			entry["webSocketDebuggerUrl"] = scheme + "://" + host + "/devtools/" + tail
		}
	}
	writeJSON(w, http.StatusOK, data)
}

func writeLaunchError(w http.ResponseWriter, err error) {
	var le *launchError
	if errors.As(err, &le) {
		writeJSON(w, le.status, map[string]any{keyError: le.msg})
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{keyError: msgChromeFailed})
}

func fetchCDP(ctx context.Context, port int, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	endpoint := "http://127.0.0.1:" + strconv.Itoa(port) + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err //nolint:wrapcheck
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err //nolint:wrapcheck
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err //nolint:wrapcheck
	}
	return json.Unmarshal(body, out) //nolint:wrapcheck
}

// externalHost is the public host used in rewritten CDP WebSocket URLs, so they
// are correct behind kubectl port-forward / ssh -L. X-Forwarded-Host wins, then
// the request Host header, then a localhost fallback.
func externalHost(r *http.Request, port int) string {
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		if h := strings.TrimSpace(strings.SplitN(fwd, ",", 2)[0]); h != "" {
			return h
		}
	}
	if r.Host != "" {
		return r.Host
	}
	return "localhost:" + strconv.Itoa(port)
}

func wsScheme(r *http.Request) string {
	if requestScheme(r) == schemeHTTPS {
		return "wss"
	}
	return "ws"
}

func requestScheme(r *http.Request) string {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		if r.TLS != nil {
			return schemeHTTPS
		}
		return schemeHTTP
	}
	return strings.ToLower(strings.TrimSpace(strings.SplitN(proto, ",", 2)[0]))
}

// ---------------------------------------------------------------------------
// WebSocket Origin allow-list
// ---------------------------------------------------------------------------

// rejectUntrustedOrigin blocks browser-origin WebSocket upgrades that would
// expose local CDP, while still allowing non-browser clients (which omit Origin)
// and same-origin loopback clients (kubectl port-forward / ssh -L). Returns true
// when it wrote a 403.
// rejectUntrustedState guards the plain-HTTP state API, which exposes raw
// cookies + localStorage. Unlike the WebSocket path, a browser same-origin GET
// omits Origin, so the Origin allow-list alone cannot stop a DNS-rebinding page
// (attacker.com rebound to 127.0.0.1) from reading a seed's session. Requiring a
// loopback Host defeats the rebind - the Host header stays attacker.com even
// after the DNS flips - and every legitimate reach is loopback (the CLI hits the
// standing tunnel's local end; ssh -L / kubectl port-forward terminate at
// 127.0.0.1). The Origin check still runs as defense-in-depth for a present,
// cross-origin Origin. Returns true when it wrote a 403.
func (m *multiplexer) rejectUntrustedState(w http.ResponseWriter, r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if !isLoopbackHost(host) {
		logWarn("rejected state request for non-loopback Host %q", r.Host)
		http.Error(w, "Forbidden: non-loopback host", http.StatusForbidden)
		return true
	}
	return m.rejectUntrustedOrigin(w, r)
}

func (m *multiplexer) rejectUntrustedOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin, present := r.Header["Origin"]
	value := ""
	if present && len(origin) > 0 {
		value = origin[0]
	}
	if originIsAllowed(value, present, r.Host, requestScheme(r)) {
		return false
	}
	logWarn("rejected CDP WebSocket from untrusted Origin %q for Host %q", value, r.Host)
	http.Error(w, "Forbidden: untrusted WebSocket origin", http.StatusForbidden)
	return true
}

func originIsAllowed(origin string, present bool, host, scheme string) bool {
	if !present {
		// Playwright/Puppeteer and other non-browser CDP clients omit Origin.
		return true
	}
	origin = strings.TrimSpace(origin)
	if origin == "" || strings.EqualFold(origin, "null") {
		return false
	}
	if _, ok := trustedWSOrigins[origin]; ok {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS {
		return false
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return false
	}
	originDefaultPort := 80
	if u.Scheme == schemeHTTPS {
		originDefaultPort = 443
	}
	rs := strings.ToLower(strings.TrimSpace(strings.SplitN(scheme, ",", 2)[0]))
	requestDefaultPort := 80
	if rs == schemeHTTPS || rs == "wss" {
		requestDefaultPort = 443
	}
	originHost, originPort, ok := hostPortFromNetloc(u.Host, originDefaultPort)
	if !ok {
		return false
	}
	requestHost, requestPort, ok := hostPortFromNetloc(host, requestDefaultPort)
	if !ok {
		return false
	}
	if !isLoopbackHost(requestHost) {
		return false
	}
	return originHost == requestHost && originPort == requestPort
}

func hostPortFromNetloc(netloc string, defaultPort int) (string, int, bool) {
	if strings.Contains(netloc, ",") {
		return "", 0, false
	}
	u, err := url.Parse("//" + strings.TrimSpace(netloc))
	if err != nil {
		return "", 0, false
	}
	if u.Hostname() == "" || u.User != nil || strings.HasSuffix(u.Host, ":") ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", 0, false
	}
	port := defaultPort
	if ps := u.Port(); ps != "" {
		p, err := strconv.Atoi(ps)
		if err != nil {
			return "", 0, false
		}
		port = p
	}
	return strings.ToLower(u.Hostname()), port, true
}

func isLoopbackHost(hostname string) bool {
	hostname = strings.ToLower(strings.TrimRight(strings.Trim(hostname, "[]"), "."))
	if hostname == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(hostname)
	if err != nil {
		return false
	}
	return addr.IsLoopback()
}
