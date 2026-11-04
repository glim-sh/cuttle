package serve

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The remote listener: the one surface a caller outside the pod reaches. Every
// route on it needs a credential except the probes, and each route family takes
// exactly one kind - a bearer on the machine surface (/mcp, /files), a one-time
// ticket on /t, a viewer cookie on /v - so no credential opens a door it was not
// minted for. The internal listener keeps every other route on loopback.

var (
	errRemoteMode               = errors.New("remote access requires session mode")
	errRemotePublicURL          = errors.New("public-url must be an HTTPS origin")
	errRemotePrincipalsRequired = errors.New("remote access requires principals-file")
	errRemoteViewerURL          = errors.New("viewer-upstream must be a loopback HTTP origin")
	errPrincipalFormat          = errors.New("invalid principals file: expected name sha256:hex")
	errPrincipalHash            = errors.New("invalid principal hash")
	errPrincipalDuplicate       = errors.New("duplicate principal name or hash")
	errTooManyTickets           = errors.New("too many outstanding links for this principal - wait for some to expire")
)

const (
	viewerCookie = "__Host-cuttle_view"
	// ticketsPerPrincipal caps outstanding links, so a looping agent cannot fill
	// daemon memory with them.
	ticketsPerPrincipal = 50
	viewSessionMax      = 60 * time.Minute
	fileHandleTTL       = time.Hour
	formBodyLimit       = 64 << 10

	ticketViewer = "viewer"
	ticketSecret = "secret"
)

type remoteConfig struct {
	listen, publicURL, principalsFile, viewerUpstream string
}

func (c remoteConfig) validate(mode serveMode) error {
	if c.listen == "" {
		return nil
	}
	if mode != modeSession {
		return errRemoteMode
	}
	u, err := url.Parse(c.publicURL)
	if err != nil || u.Scheme != schemeHTTPS || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errRemotePublicURL
	}
	if c.principalsFile == "" {
		return errRemotePrincipalsRequired
	}
	v, err := url.Parse(c.viewerUpstream)
	if err != nil || v.Scheme != schemeHTTP || !isLoopbackHost(v.Hostname()) || v.User != nil || v.RawQuery != "" || v.Fragment != "" || (v.Path != "" && v.Path != "/") {
		return errRemoteViewerURL
	}
	return nil
}

type remotePrincipal struct {
	name string
	hash [sha256.Size]byte
}

type principalTable struct {
	mu      sync.RWMutex
	entries []remotePrincipal
}

type principalContextKey struct{}

func principalOf(ctx context.Context) string {
	name, _ := ctx.Value(principalContextKey{}).(string)
	return name
}

func readPrincipals(file string) ([]remotePrincipal, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("read principals: %w", err)
	}
	defer func() { _ = f.Close() }()
	var entries []remotePrincipal
	names := map[string]bool{}
	hashes := map[[sha256.Size]byte]bool{}
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !validSecretName(fields[0]) || !strings.HasPrefix(fields[1], "sha256:") {
			return nil, errPrincipalFormat
		}
		raw, err := hex.DecodeString(strings.TrimPrefix(fields[1], "sha256:"))
		if err != nil || len(raw) != sha256.Size {
			return nil, errPrincipalHash
		}
		hash := [sha256.Size]byte(raw)
		if names[fields[0]] || hashes[hash] {
			return nil, errPrincipalDuplicate
		}
		names[fields[0]], hashes[hash] = true, true
		entries = append(entries, remotePrincipal{fields[0], hash})
	}
	if err := scan.Err(); err != nil {
		return nil, fmt.Errorf("read principals: %w", err)
	}
	return entries, nil
}

func (p *principalTable) reload(file string) error {
	entries, err := readPrincipals(file)
	// A missing or invalid replacement must not keep a revoked bearer alive.
	p.mu.Lock()
	p.entries = entries
	p.mu.Unlock()
	return err
}

// watch reloads on SIGHUP and whenever the file's size or mtime moves. A
// projected Secret swaps a symlink, which os.Stat follows, so its update shows
// up here without inotify.
func (p *principalTable) watch(ctx context.Context, file string) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := fileStamp(file)
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
		case <-tick.C:
			if fileStamp(file) == last {
				continue
			}
		}
		last = fileStamp(file)
		err := p.reload(file)
		switch {
		case err != nil && !failing:
			logWarn("remote: principals reload failed, every bearer is refused until the file is valid again: %v", err)
		case err == nil:
			logInfo("remote: principals reloaded")
		}
		failing = err != nil
	}
}

func fileStamp(file string) string {
	info, err := os.Stat(file)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("%d/%d", info.Size(), info.ModTime().UnixNano())
}

func (p *principalTable) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := r.Header.Values("Authorization")
		token := ""
		if len(headers) == 1 && strings.HasPrefix(headers[0], "Bearer ") {
			token = strings.TrimPrefix(headers[0], "Bearer ")
		}
		sum := sha256.Sum256([]byte(token))
		name := ""
		p.mu.RLock()
		for _, entry := range p.entries {
			if subtle.ConstantTimeCompare(sum[:], entry.hash[:]) == 1 {
				name = entry.name
			}
		}
		p.mu.RUnlock()
		if token == "" || strings.ContainsAny(token, " \t\r\n") || name == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, name)))
	})
}

// ticket is one outstanding link: a viewer handoff or a secret request. Only its
// hash is a map key, so a heap dump holds no redeemable link.
type ticket struct {
	kind, principal, reason string
	name, origin            string // secret requests only
	expires                 time.Time
}

// viewSession is a redeemed viewer link. Its upgraded sockets are tracked so
// expiry can close them: a reverse proxy gives no handle on an upgraded
// connection, and a viewer left open must not outlive the session it rode in on.
type viewSession struct {
	principal string
	expires   time.Time
	timer     *time.Timer
	conns     map[net.Conn]struct{}
}

type fileHandle struct {
	principal, path, name string
	expires               time.Time
}

// remote owns everything behind the remote listener.
type remote struct {
	m          *multiplexer
	cfg        remoteConfig
	publicURL  string
	viewer     *url.URL
	principals *principalTable
	proxy      *httputil.ReverseProxy
	mcp        *aggregator

	mu      sync.Mutex
	tickets map[[sha256.Size]byte]*ticket
	views   map[[sha256.Size]byte]*viewSession
	files   map[string]*fileHandle
	closed  bool
}

// newRemote builds the remote surface. upstream is the Playwright MCP endpoint
// the aggregator drives.
func newRemote(m *multiplexer, cfg remoteConfig, upstream string, browser browserOps) (*remote, error) {
	principals := &principalTable{}
	if err := principals.reload(cfg.principalsFile); err != nil {
		return nil, fmt.Errorf("principals-file: %w", err)
	}
	viewer, err := url.Parse(cfg.viewerUpstream)
	if err != nil {
		return nil, errRemoteViewerURL
	}
	r := &remote{
		m: m, cfg: cfg, viewer: viewer, principals: principals,
		publicURL: strings.TrimSuffix(cfg.publicURL, "/"),
		tickets:   map[[sha256.Size]byte]*ticket{},
		views:     map[[sha256.Size]byte]*viewSession{},
		files:     map[string]*fileHandle{},
	}
	r.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Path = viewerPath(pr.In.URL.Path)
			pr.Out.URL.RawPath = ""
			pr.SetURL(r.viewer)
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			logWarn("remote: viewer upstream unreachable: %v", err)
			http.Error(w, "the viewer is not reachable", http.StatusBadGateway)
		},
	}
	r.mcp = newAggregator(r, upstream, browser)
	return r, nil
}

func viewerPath(p string) string {
	if p = strings.TrimPrefix(p, "/v"); p == "" {
		return "/"
	}
	return p
}

func (r *remote) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", r.m.handleHealthz)
	mux.HandleFunc("GET /readyz", r.m.handleReadyz)
	mux.Handle("/mcp", r.principals.authenticate(r.mcp.handler()))
	mux.Handle("/files/", r.principals.authenticate(http.HandlerFunc(r.handleFile)))
	mux.HandleFunc("GET /t/{tkt}", r.handleTicketPage)
	cop := http.NewCrossOriginProtection()
	if err := cop.AddTrustedOrigin(r.publicURL); err != nil {
		logWarn("remote: public-url is not a valid trusted origin: %v", err)
	}
	mux.Handle("POST /t/{tkt}", cop.Handler(http.HandlerFunc(r.handleTicketPost)))
	mux.HandleFunc("/v/", r.handleViewer)
	return mux
}

// server is the remote http.Server. A hijacked viewer socket clears the
// deadlines these set (see proxyUpgrade).
func (r *remote) server() *http.Server {
	return &http.Server{
		Addr: r.cfg.listen, Handler: r.handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, MaxHeaderValueCount: 100,
	}
}

// close ends every viewer socket and upstream session; the listener itself is
// shut down by the caller.
func (r *remote) close() {
	r.mu.Lock()
	r.closed = true
	views := r.views
	r.views = map[[sha256.Size]byte]*viewSession{}
	r.mu.Unlock()
	for _, s := range views {
		s.timer.Stop()
		for c := range s.conns {
			_ = c.Close()
		}
	}
	r.mcp.close()
}

func newToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return base64.RawURLEncoding.EncodeToString(b)
}

func tokenHash(token string) [sha256.Size]byte { return sha256.Sum256([]byte(token)) }

// mintTicket stores t and returns its link token.
func (r *remote) mintTicket(t ticket) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	outstanding := 0
	for h, cur := range r.tickets {
		switch {
		case !now.Before(cur.expires):
			delete(r.tickets, h)
		case cur.principal == t.principal:
			outstanding++
		}
	}
	if outstanding >= ticketsPerPrincipal {
		return "", errTooManyTickets
	}
	token := newToken(32)
	r.tickets[tokenHash(token)] = &t
	return token, nil
}

// peekTicket reads a live ticket without consuming it.
func (r *remote) peekTicket(token string) (ticket, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tickets[tokenHash(token)]
	if t == nil || !time.Now().Before(t.expires) {
		return ticket{}, false
	}
	return *t, true
}

// takeTicket consumes a live ticket of the given kind, exactly once.
func (r *remote) takeTicket(token, kind string) (ticket, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := tokenHash(token)
	t := r.tickets[h]
	if t == nil || t.kind != kind || !time.Now().Before(t.expires) {
		return ticket{}, false
	}
	delete(r.tickets, h)
	return *t, true
}

func (r *remote) link(token string) string { return r.publicURL + "/t/" + token }

// ---------------------------------------------------------------------------
// /t: interstitial and redemption
// ---------------------------------------------------------------------------

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>cuttle</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:2rem auto;padding:0 1rem;color:#111;background:#fff}
@media (prefers-color-scheme:dark){body{color:#eee;background:#111}input,select{background:#222;color:#eee}}
label{display:block;margin:.8rem 0 .2rem;font-weight:600}
input,select{width:100%;box-sizing:border-box;padding:.6rem;font-size:1rem}
button{margin-top:1.2rem;width:100%;padding:.8rem;font-size:1.1rem}
.muted{opacity:.75}.err{color:#c00}
</style></head><body>
{{if eq .Kind "viewer"}}
<h1>Open the cuttle viewer</h1>
<p>Requested by <b>{{.Principal}}</b>: {{.Reason}}</p>
<form method="post"><button type="submit">Open the viewer</button></form>
<p class="muted">The link works once. The viewer closes at {{.Expires}}.</p>
{{else if eq .Kind "secret"}}
<h1>Give cuttle a secret</h1>
<p><b>{{.Principal}}</b> asks for <b>{{.Name}}</b>: {{.Reason}}</p>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="post" autocomplete="off">
<label for="origin">Only usable on</label>
<input id="origin" name="origin" value="{{.Origin}}" required>
<label for="ttl">Keep it for</label>
<select id="ttl" name="ttl"><option value="15m">15 minutes</option><option value="1h" selected>1 hour</option><option value="12h">12 hours</option></select>
<label for="value">Value</label>
<input id="value" name="value" type="password" required>
<button type="submit">Save</button>
</form>
<p class="muted">The value goes to the browser session only; the agent never sees it. Check the site above is the one you expect.</p>
{{else}}
<h1>{{.Title}}</h1><p>{{.Message}}</p>
{{end}}
</body></html>`))

type pageData struct {
	Kind, Principal, Reason, Name, Origin, Expires, Error, Title, Message string
}

func writePage(w http.ResponseWriter, status int, d pageData) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.WriteHeader(status)
	_ = pageTemplate.Execute(w, d)
}

func writeMessage(w http.ResponseWriter, status int, title, message string) {
	writePage(w, status, pageData{Title: title, Message: message})
}

const msgLinkGone = "This link has expired or was already used - ask the agent for a new one."

// handleTicketPage is the interstitial. It never consumes the ticket: a chat
// app's link preview fetches the URL before the human taps it, and that fetch
// must not spend the link.
func (r *remote) handleTicketPage(w http.ResponseWriter, req *http.Request) {
	t, ok := r.peekTicket(req.PathValue("tkt"))
	if !ok {
		writeMessage(w, http.StatusGone, "Link expired", msgLinkGone)
		return
	}
	writePage(w, http.StatusOK, pageData{
		Kind: t.kind, Principal: t.principal, Reason: t.reason, Name: t.name, Origin: t.origin,
		Expires: t.expires.UTC().Format("15:04 UTC"),
	})
}

func (r *remote) handleTicketPost(w http.ResponseWriter, req *http.Request) {
	token := req.PathValue("tkt")
	t, ok := r.peekTicket(token)
	if !ok {
		writeMessage(w, http.StatusGone, "Link expired", msgLinkGone)
		return
	}
	if t.kind == ticketSecret {
		r.redeemSecret(w, req, token, t)
		return
	}
	t, ok = r.takeTicket(token, ticketViewer)
	if !ok {
		writeMessage(w, http.StatusGone, "Link expired", msgLinkGone)
		return
	}
	session := newToken(32)
	lifetime := min(time.Until(t.expires), viewSessionMax)
	r.startView(tokenHash(session), t.principal, lifetime)
	http.SetCookie(w, &http.Cookie{
		Name: viewerCookie, Value: session, Path: "/", MaxAge: int(lifetime.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	logInfo("remote: viewer link from %s redeemed (session ends in %s)", t.principal, lifetime.Round(time.Second))
	http.Redirect(w, req, "/v/", http.StatusSeeOther)
}

var secretTTLChoices = map[string]time.Duration{"15m": 15 * time.Minute, "1h": time.Hour, "12h": 12 * time.Hour}

// redeemSecret stores a human-typed value bound to the origin the human
// confirmed. Invalid input re-renders the form without spending the link.
func (r *remote) redeemSecret(w http.ResponseWriter, req *http.Request, token string, t ticket) {
	req.Body = http.MaxBytesReader(w, req.Body, formBodyLimit)
	if err := req.ParseForm(); err != nil {
		writeMessage(w, http.StatusBadRequest, "Not saved", "The form could not be read.")
		return
	}
	value := req.PostForm.Get("value")
	origin, err := secretOrigin(strings.TrimSpace(req.PostForm.Get("origin")))
	ttl, okTTL := secretTTLChoices[req.PostForm.Get("ttl")]
	page := pageData{Kind: ticketSecret, Principal: t.principal, Reason: t.reason, Name: t.name, Origin: req.PostForm.Get("origin")}
	switch {
	case value == "":
		page.Error = "Type the value."
	case err != nil || origin == "":
		page.Error = "Give the site as an origin, like https://example.com."
	case !okTTL:
		page.Error = "Pick how long to keep it."
	}
	if page.Error != "" {
		writePage(w, http.StatusBadRequest, page)
		return
	}
	if _, ok := r.takeTicket(token, ticketSecret); !ok {
		writeMessage(w, http.StatusGone, "Link expired", msgLinkGone)
		return
	}
	seed, lerr := r.m.pool.seedKeyFor("")
	if lerr != nil {
		writeMessage(w, http.StatusInternalServerError, "Not saved", "The session is not available.")
		return
	}
	held := r.m.pool.secrets.putBound(seed, t.name, []byte(value), sourceHumanForm, ttl, origin)
	logInfo("secrets: %s set through a request form from %s (%d bytes, bound to %s, ttl %s)", t.name, t.principal, len(value), origin, held)
	writeMessage(w, http.StatusOK, "Saved", "Saved - you can close this page.")
}

// ---------------------------------------------------------------------------
// /v: the viewer proxy
// ---------------------------------------------------------------------------

func (r *remote) startView(h [sha256.Size]byte, principal string, lifetime time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &viewSession{principal: principal, expires: time.Now().Add(lifetime), conns: map[net.Conn]struct{}{}}
	s.timer = time.AfterFunc(lifetime, func() { r.endView(h) })
	r.views[h] = s
}

func (r *remote) endView(h [sha256.Size]byte) {
	r.mu.Lock()
	s := r.views[h]
	delete(r.views, h)
	r.mu.Unlock()
	if s == nil {
		return
	}
	for c := range s.conns {
		_ = c.Close()
	}
	logInfo("remote: viewer session from %s ended", s.principal)
}

func (r *remote) viewSessionOf(req *http.Request) ([sha256.Size]byte, bool) {
	c, err := req.Cookie(viewerCookie)
	if err != nil {
		return [sha256.Size]byte{}, false
	}
	h := tokenHash(c.Value)
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.views[h]
	return h, s != nil && time.Now().Before(s.expires)
}

// track registers an upgraded socket pair with its session; false means the
// session ended in the meantime and the caller must close them.
func (r *remote) track(h [sha256.Size]byte, conns ...net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.views[h]
	if s == nil || r.closed {
		return false
	}
	for _, c := range conns {
		s.conns[c] = struct{}{}
	}
	return true
}

func (r *remote) untrack(h [sha256.Size]byte, conns ...net.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.views[h]; s != nil {
		for _, c := range conns {
			delete(s.conns, c)
		}
	}
}

func (r *remote) handleViewer(w http.ResponseWriter, req *http.Request) {
	h, ok := r.viewSessionOf(req)
	if !ok {
		writeMessage(w, http.StatusUnauthorized, "Session ended", "This viewer session has ended - ask the agent for a new link.")
		return
	}
	if !headerHasToken(req.Header, "Connection", "upgrade") {
		r.proxy.ServeHTTP(w, req)
		return
	}
	// CrossOriginProtection passes every GET, and a WebSocket handshake is one:
	// the cookie alone would let any page the owner visits open the viewer.
	if req.Header.Get("Origin") != r.publicURL {
		logWarn("remote: refused a viewer upgrade from Origin %q", req.Header.Get("Origin"))
		http.Error(w, "Forbidden: cross-origin viewer connection", http.StatusForbidden)
		return
	}
	r.proxyUpgrade(w, req, h)
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for part := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// proxyUpgrade relays one upgraded connection by hand, so the session can close
// it at expiry.
func (r *remote) proxyUpgrade(w http.ResponseWriter, req *http.Request, h [sha256.Size]byte) {
	out := req.Clone(req.Context())
	out.URL = &url.URL{Path: viewerPath(req.URL.Path), RawQuery: req.URL.RawQuery}
	out.Host = r.viewer.Host
	out.RequestURI = ""
	out.Header.Del("Cookie")
	out.Header.Del("Authorization")
	out.Header.Set("Origin", r.viewer.Scheme+"://"+r.viewer.Host)
	d := net.Dialer{Timeout: 5 * time.Second}
	up, err := d.DialContext(req.Context(), "tcp", r.viewer.Host)
	if err != nil {
		logWarn("remote: viewer upstream unreachable: %v", err)
		http.Error(w, "the viewer is not reachable", http.StatusBadGateway)
		return
	}
	upReader := bufio.NewReader(up)
	_ = up.SetDeadline(time.Now().Add(10 * time.Second))
	if werr := out.Write(up); werr != nil {
		_ = up.Close()
		http.Error(w, "the viewer is not reachable", http.StatusBadGateway)
		return
	}
	resp, err := http.ReadResponse(upReader, out)
	if err != nil {
		_ = up.Close()
		http.Error(w, "the viewer did not answer", http.StatusBadGateway)
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer func() { _ = up.Close(); _ = resp.Body.Close() }()
		maps.Copy(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	_ = up.SetDeadline(time.Time{})
	client, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		_ = up.Close()
		return
	}
	_ = client.SetDeadline(time.Time{})
	if !r.track(h, client, up) {
		_ = client.Close()
		_ = up.Close()
		return
	}
	defer r.untrack(h, client, up)
	if _, err := brw.WriteString("HTTP/1.1 101 Switching Protocols\r\n"); err == nil {
		_ = resp.Header.Write(brw)
		_, _ = brw.WriteString("\r\n")
		_ = brw.Flush()
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, brw.Reader); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upReader); done <- struct{}{} }()
	<-done
	_ = client.Close()
	_ = up.Close()
	<-done
}

// ---------------------------------------------------------------------------
// /files: download handles
// ---------------------------------------------------------------------------

func (r *remote) mintFile(principal, path, name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for h, f := range r.files {
		if !now.Before(f.expires) {
			delete(r.files, h)
		}
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	h := hex.EncodeToString(b)
	r.files[h] = &fileHandle{principal: principal, path: path, name: name, expires: now.Add(fileHandleTTL)}
	return h
}

func (r *remote) handleFile(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h := strings.TrimPrefix(req.URL.Path, "/files/")
	r.mu.Lock()
	f := r.files[h]
	r.mu.Unlock()
	// A handle another principal minted is answered exactly like a missing one.
	if f == nil || f.principal != principalOf(req.Context()) || !time.Now().Before(f.expires) {
		writeJSON(w, http.StatusNotFound, map[string]any{keyError: "no such file handle"})
		return
	}
	serveDownload(w, f.path, f.name)
}
