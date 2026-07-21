package serve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
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
	mux.HandleFunc("GET /fingerprint/{seed}/devtools/{path...}", m.handleWSSeed)
	mux.HandleFunc("GET /devtools/{path...}", m.handleWSDefault)
	return mux
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
