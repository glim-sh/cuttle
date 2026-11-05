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
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The remote listener: the one surface a caller outside the pod reaches. Every
// route on it needs a credential except the probes, and each route family takes
// its own kind - a bearer on the machine surface (/mcp, /files), a one-time
// ticket on /t, a trusted network peer on raw CDP, and on the viewer a cookie or
// a trusted network peer - so no credential opens a door it was not minted for.
// The internal listener keeps every other route on loopback.

var (
	errRemoteMode               = errors.New("remote access requires session mode")
	errRemotePublicURL          = errors.New("public-url must be an HTTPS origin")
	errRemotePrincipalsRequired = errors.New("remote access requires principals-file")
	errRemoteViewerURL          = errors.New("viewer-upstream must be a loopback HTTP origin or unix:/absolute/socket/path")
	errTrustWithoutRemote       = errors.New("trust-network-peers requires remote-listen")
	errPrincipalFormat          = errors.New("invalid principals file: expected name sha256:hex [seed=name]")
	errPrincipalHash            = errors.New("invalid principal hash")
	errPrincipalDuplicate       = errors.New("duplicate principal name or hash")
	errTooManyTickets           = errors.New("too many outstanding links for this principal - wait for some to expire")
)

const (
	mainSeedName = "main"
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
	// trustNetworkPeers opens the viewer and raw CDP to any peer that is not
	// the pod itself (see networkPeers).
	trustNetworkPeers bool
}

func (c remoteConfig) validate(mode serveMode) error {
	if c.listen == "" {
		if c.trustNetworkPeers {
			return errTrustWithoutRemote
		}
		return nil
	}
	if mode != modeSession {
		return errRemoteMode
	}
	if origin, err := secretOrigin(c.publicURL); err != nil || !strings.HasPrefix(origin, schemeHTTPS+"://") {
		return errRemotePublicURL
	}
	if c.principalsFile == "" {
		return errRemotePrincipalsRequired
	}
	if _, _, err := viewerDialer(c.viewerUpstream); err != nil {
		return err
	}
	return nil
}

// viewerDialer resolves --viewer-upstream to the Host the viewer is sent and a
// dialer for it. A Unix socket is the image's default: no page in the browser
// can reach one, where a loopback port answers any DNS name for 127.0.0.1.
func viewerDialer(raw string) (string, func(context.Context) (net.Conn, error), error) {
	v, err := url.Parse(raw)
	if err != nil || v.User != nil || v.RawQuery != "" || v.Fragment != "" {
		return "", nil, errRemoteViewerURL
	}
	var d net.Dialer
	switch {
	case v.Scheme == "unix" && v.Host == "" && v.Opaque == "" && filepath.IsAbs(v.Path):
		return "localhost", func(ctx context.Context) (net.Conn, error) {
			return d.DialContext(ctx, "unix", v.Path)
		}, nil
	case v.Scheme == schemeHTTP && isLoopbackHost(v.Hostname()) && v.Port() != "" && (v.Path == "" || v.Path == "/"):
		return v.Host, func(ctx context.Context) (net.Conn, error) {
			return d.DialContext(ctx, "tcp", v.Host)
		}, nil
	}
	return "", nil, errRemoteViewerURL
}

type remotePrincipal struct {
	name string
	seed string
	hash [sha256.Size]byte
}

type principalTable struct {
	mu        sync.RWMutex
	remoteMu  *sync.Mutex
	bindSeeds func(map[string]bool)
	entries   []remotePrincipal
}

type principalContextKey struct{}

func principalOf(ctx context.Context) string {
	name, _ := ctx.Value(principalContextKey{}).(string)
	return name
}

type principalSeedContextKey struct{}

func principalSeedOf(ctx context.Context) string {
	seed, _ := ctx.Value(principalSeedContextKey{}).(string)
	return boundSeedName(seed)
}

func boundSeedName(seed string) string {
	if seed == "" {
		return mainSeedName
	}
	return seed
}

func boundSeedKey(seed string) string {
	if boundSeedName(seed) == mainSeedName {
		return reservedSeed
	}
	return seed
}

func validBoundSeed(name string) bool {
	if len(name) == 0 || len(name) > 32 || name == "state" || name == "logs" || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func (p *principalTable) matches(name, seed string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, e := range p.entries {
		if e.name == name && boundSeedName(e.seed) == boundSeedName(seed) {
			return true
		}
	}
	return false
}

func (p *principalTable) seeds() map[string]bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	seeds := map[string]bool{}
	for _, e := range p.entries {
		if seed := boundSeedName(e.seed); seed != mainSeedName {
			seeds[seed] = true
		}
	}
	return seeds
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
		if (len(fields) != 2 && len(fields) != 3) || !validSecretName(fields[0]) || !strings.HasPrefix(fields[1], "sha256:") {
			return nil, errPrincipalFormat
		}
		seed := mainSeedName
		if len(fields) == 3 {
			var ok bool
			seed, ok = strings.CutPrefix(fields[2], "seed=")
			if !ok || !validBoundSeed(seed) {
				return nil, errPrincipalFormat
			}
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
		entries = append(entries, remotePrincipal{name: fields[0], seed: seed, hash: hash})
	}
	if err := scan.Err(); err != nil {
		return nil, fmt.Errorf("read principals: %w", err)
	}
	return entries, nil
}

func (p *principalTable) reload(file string) error {
	entries, err := readPrincipals(file)
	// A missing or invalid replacement must not keep a revoked bearer alive.
	if p.remoteMu != nil {
		p.remoteMu.Lock()
		defer p.remoteMu.Unlock()
	}
	p.mu.Lock()
	p.entries = entries
	p.mu.Unlock()
	if p.bindSeeds != nil {
		p.bindSeeds(p.seeds())
	}
	return err
}

// watch reloads on SIGHUP and whenever the file's size or mtime moves, then
// calls revoked to sweep what the reload unbound. A projected Secret swaps a
// symlink, which os.Stat follows, so its update shows up here without inotify.
func (p *principalTable) watch(ctx context.Context, file string, revoked func()) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	if revoked != nil {
		revoked()
	}
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
		if revoked != nil {
			revoked()
		}
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
		name, seed := "", mainSeedName
		p.mu.RLock()
		for _, entry := range p.entries {
			if subtle.ConstantTimeCompare(sum[:], entry.hash[:]) == 1 {
				name, seed = entry.name, boundSeedName(entry.seed)
			}
		}
		p.mu.RUnlock()
		if token == "" || strings.ContainsAny(token, " \t\r\n") || name == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), principalContextKey{}, name)
		ctx = context.WithValue(ctx, principalSeedContextKey{}, seed)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ticket is one outstanding link: a viewer handoff or a secret request. Only its
// hash is a map key, so a heap dump holds no redeemable link.
type ticket struct {
	kind, principal, reason string
	seed                    string
	name, origin            string // secret requests only
	expires                 time.Time
}

// viewSession is a redeemed viewer link. Every request it carries runs under
// its context, so ending the session ends the viewer's sockets and streams
// with it (Selkies' focus stream is a plain GET that never finishes on its own):
// a viewer left open must not outlive the session it rode in on.
type viewSession struct {
	principal string
	seed      string
	expires   time.Time
	timer     *time.Timer
	ctx       context.Context //nolint:containedctx // the session's lifetime, not a request's
	end       context.CancelFunc
}

type fileHandle struct {
	principal, dir, name string
	seed                 string
	expires              time.Time
}

// remote owns everything behind the remote listener.
type remote struct {
	m          *multiplexer
	cfg        remoteConfig
	publicURL  string
	publicHost string
	publicPort int
	cdpWSBase  string // wss://<public host>, what /json advertises here
	viewerHost string
	principals *principalTable
	peers      *networkPeers // nil unless trustNetworkPeers
	proxy      *httputil.ReverseProxy
	mcp        *aggregator

	raiseSeed func(context.Context, string) bool
	// viewers counts in-flight viewer requests, a trusted peer's included, which
	// no ticket or view session records.
	viewers atomic.Int64
	mu      sync.Mutex
	tickets map[[sha256.Size]byte]*ticket
	views   map[[sha256.Size]byte]*viewSession
	files   map[string]*fileHandle
}

// newRemote builds the remote surface. upstream is the Playwright MCP endpoint
// the aggregator drives, and upstreamHost the only Host it accepts.
func newRemote(m *multiplexer, cfg remoteConfig, upstream, upstreamHost string, browser browserOps) (*remote, error) {
	principals := &principalTable{}
	if err := principals.reload(cfg.principalsFile); err != nil {
		return nil, fmt.Errorf("principals-file: %w", err)
	}
	viewerHost, dial, err := viewerDialer(cfg.viewerUpstream)
	if err != nil {
		return nil, err
	}
	r := &remote{
		m: m, cfg: cfg, viewerHost: viewerHost, principals: principals,
		publicURL: publicOrigin(cfg.publicURL),
		tickets:   map[[sha256.Size]byte]*ticket{},
		views:     map[[sha256.Size]byte]*viewSession{},
		files:     map[string]*fileHandle{},
	}
	principals.remoteMu = &r.mu
	if u, perr := url.Parse(r.publicURL); perr == nil {
		r.publicHost, r.publicPort, _ = hostPortFromNetloc(u.Host, 443)
		r.cdpWSBase = "wss://" + u.Host
	}
	if cfg.trustNetworkPeers {
		r.peers = &networkPeers{ownAddrs: net.InterfaceAddrs}
		own, perr := r.peers.own()
		if perr != nil {
			return nil, fmt.Errorf("trust-network-peers: read the pod's addresses: %w", perr)
		}
		logWarn("remote: trusting network peers on %s: any caller that is not this pod (%s) gets the viewer and raw CDP without a credential", cfg.listen, own)
	}
	r.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: schemeHTTP, Host: viewerHost})
			pr.Out.URL.Path = viewerPath(pr.In.URL.Path)
			pr.Out.URL.RawPath = ""
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
			// The viewer's own same-origin check compares Origin with Host, so
			// the public Origin, which the daemon checked, is re-spelled for it;
			// any other Origin goes through as is and the viewer refuses it.
			if pr.Out.Header.Get("Origin") == r.publicURL {
				pr.Out.Header.Set("Origin", schemeHTTP+"://"+viewerHost)
			}
		},
		Transport: &http.Transport{
			Proxy:       nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		},
		// Selkies sends this by default; the daemon does not lean on it, since a
		// framed viewer is a live remote desktop under another site's clicks.
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Add("Content-Security-Policy", "frame-ancestors 'self'")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			logWarn("remote: viewer upstream unreachable: %v", err)
			http.Error(w, "the viewer is not reachable", http.StatusBadGateway)
		},
	}
	r.mcp = newAggregator(r, upstream, upstreamHost, browser)
	return r, nil
}

// publicOrigin is public-url in the one spelling a browser sends as Origin
// (lower-case host, no default port, no slash), so the viewer's Origin check
// compares like with like.
func publicOrigin(raw string) string {
	origin, err := secretOrigin(raw)
	if err != nil {
		return strings.TrimSuffix(raw, "/")
	}
	return origin
}

// viewerPath maps a public path to the viewer's: /v/ is an alias of the root,
// kept so links and bookmarks from before the viewer moved there still work.
func viewerPath(p string) string {
	if p == "/v" {
		return "/"
	}
	if rest, ok := strings.CutPrefix(p, "/v/"); ok {
		return "/" + rest
	}
	return p
}

func (r *remote) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", r.m.handleHealthz)
	mux.HandleFunc("GET /readyz", r.m.handleReadyz)
	mux.Handle("/mcp", r.principals.authenticate(r.mcp.handler()))
	mux.Handle("GET /files/{h}", r.principals.authenticate(http.HandlerFunc(r.handleFile)))
	mux.HandleFunc("GET /t/{tkt}", r.handleTicketPage)
	cop := http.NewCrossOriginProtection()
	if err := cop.AddTrustedOrigin(r.publicURL); err != nil {
		logWarn("remote: public-url is not a valid trusted origin: %v", err)
	}
	mux.Handle("POST /t/{tkt}", cop.Handler(http.HandlerFunc(r.handleTicketPost)))
	// Raw CDP is the daemon's own mux, with the same handlers, humanizer,
	// substitution and Origin rule as on the internal listener; only the
	// credential differs.
	cdp := http.NewServeMux()
	r.m.cdpRoutes(cdp)
	gated := r.trustedPeerOnly(cdp)
	for _, p := range []string{"/json", "/json/", "/devtools/", "/fingerprint/"} {
		mux.Handle(p, gated)
	}
	// The viewer owns every other path. An unsafe method there is held to the
	// public origin: the cookie is SameSite=Lax, which a sibling host on the
	// same site still gets, and every page in the owner's browser shares a
	// trusted peer's network position.
	mux.Handle("/", cop.Handler(http.HandlerFunc(r.handleViewer)))
	return mux
}

// networkPeers decides which TCP peers are outside the pod. A peer that is
// neither loopback nor one of the pod's own addresses came in over the network,
// and in the intended deployment the only network path to the remote port is
// an authenticated ingress (a NetworkPolicy admitting only its proxy, an ACL
// admitting only the owner's devices to it). The browser inside the pod can
// only originate from loopback or the pod's own address. No header takes part.
type networkPeers struct {
	ownAddrs func() ([]net.Addr, error)
}

// own reads the pod's addresses. It runs per decision, not once at start, so an
// address added later (a second network attached to a container) is never
// mistaken for an outside peer; the read is one netlink dump.
func (p *networkPeers) own() ([]netip.Addr, error) {
	addrs, err := p.ownAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if addr, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, addr.Unmap())
		}
	}
	return out, nil
}

func (p *networkPeers) outside(remoteAddr string) bool {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return false
	}
	peer := ap.Addr().Unmap().WithZone("")
	if !peer.IsValid() || peer.IsLoopback() || peer.IsUnspecified() {
		return false
	}
	own, err := p.own()
	if err != nil {
		logWarn("remote: cannot read the pod's addresses, refusing peer %s: %v", remoteAddr, err)
		return false
	}
	return !slices.Contains(own, peer)
}

// trustedPeer reports whether req is from a trusted network peer. The Host,
// port included, must also be the public one: a page rebound to the ingress's
// address, or sent to the remote port directly over plain HTTP, would otherwise
// ride the owner's network position.
func (r *remote) trustedPeer(req *http.Request) bool {
	if r.peers == nil || r.publicHost == "" {
		return false
	}
	// Network position is ambient, so any page the owner visits could make the
	// owner's browser send blind requests here. From another site only a
	// top-level GET navigation crosses (a frame's is Sec-Fetch-Dest iframe), the
	// line the viewer cookie's SameSite=Lax draws. DevTools' own socket is
	// cross-site too, and its devtools:// Origin is one no page can send.
	site := req.Header.Get("Sec-Fetch-Site")
	if (site == "cross-site" || site == "same-site") && !isDevToolsSocket(req) {
		if req.Method != http.MethodGet || req.Header.Get("Sec-Fetch-Mode") != "navigate" || req.Header.Get("Sec-Fetch-Dest") != "document" {
			return false
		}
	}
	host, port, ok := hostPortFromNetloc(req.Host, 443)
	return ok && strings.TrimSuffix(host, ".") == r.publicHost && port == r.publicPort && r.peers.outside(req.RemoteAddr)
}

func isDevToolsSocket(req *http.Request) bool {
	_, devtools := trustedWSOrigins[req.Header.Get("Origin")]
	return devtools && headerHasToken(req.Header, "Connection", "upgrade")
}

// trustedPeerOnly gates raw CDP. It also tells the /json handlers to advertise
// the public origin, so a client given only the public URL connects back
// through it. A socket outlives the server's deadlines (see server).
func (r *remote) trustedPeerOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// A page's request has no business here, a cross-site navigation
		// included: discovery launches the browser on first use. A page's socket
		// on /devtools/ is the Origin rule's to judge (rejectUntrustedOrigin).
		site := req.Header.Get("Sec-Fetch-Site")
		socketPath := strings.HasPrefix(req.URL.Path, "/devtools/")
		if tail, seeded := strings.CutPrefix(req.URL.Path, "/fingerprint/"); seeded {
			_, rest, _ := strings.Cut(tail, "/")
			socketPath = strings.HasPrefix(rest, "devtools/")
		}
		socket := socketPath && headerHasToken(req.Header, "Connection", "upgrade")
		page := site != "" && site != "none" && !socket
		if page || !r.trustedPeer(req) {
			if r.peers != nil {
				logWarn("remote: refused raw CDP %s for peer %s, Host %q", req.URL.Path, req.RemoteAddr, req.Host)
			}
			http.Error(w, "Forbidden: raw CDP here answers only a trusted network peer (--trust-network-peers), never a page", http.StatusForbidden)
			return
		}
		ctx := context.WithValue(req.Context(), cdpPublicBaseKey{}, r.cdpWSBase)
		req = req.WithContext(ctx)
		// The query carries launch options (proxy, timezone, fingerprint flags),
		// which pick the session browser's identity for its lifetime: the pod's
		// own configuration decides those, never a caller on the network.
		req.URL.RawQuery = ""
		next.ServeHTTP(w, req)
	})
}

// refuseBrowserRequests closes the internal listener to the browser's own
// pages in remote mode. There an agent steers a tab, and a page on 127.0.0.1
// passes every loopback check (peer, Host, same-origin Origin), which would
// open raw CDP and the loopback API past masking and the lease.
// Chrome marks each request to a loopback address with Sec-Fetch-Site, which
// the CLI, the drivers and the aggregator never send; a page reached through a
// DNS name for 127.0.0.1 gets no such header but cannot send a loopback Host.
func refuseBrowserRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Fetch-Site") != "" || !hasLoopbackHost(r) {
			logWarn("remote: refused %s %s for Host %q on the internal listener", r.Method, r.URL.Path, r.Host)
			http.Error(w, "Forbidden: the internal listener serves only in-container clients while remote agents are on", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// server is the remote http.Server. A socket outlives the deadlines it sets:
// net/http clears them when the handler hijacks the connection.
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
	views := r.views
	r.views = map[[sha256.Size]byte]*viewSession{}
	r.mu.Unlock()
	for _, s := range views {
		s.timer.Stop()
		s.end()
	}
	r.mcp.close()
}

// Check current bindings because reload publishes them before this sweep runs;
// capabilities minted under the new binding must survive.
func (r *remote) revoke() {
	r.mu.Lock()
	for h, t := range r.tickets {
		if !r.principals.matches(t.principal, t.seed) {
			delete(r.tickets, h)
		}
	}
	for h, f := range r.files {
		if !r.principals.matches(f.principal, f.seed) {
			delete(r.files, h)
		}
	}
	var ended [][sha256.Size]byte
	for h, v := range r.views {
		if !r.principals.matches(v.principal, v.seed) {
			v.timer.Stop()
			ended = append(ended, h)
		}
	}
	r.mu.Unlock()
	for _, h := range ended {
		r.endView(h)
	}
	r.mcp.revoke()
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
	// Under r.mu, so a mint either precedes a revoke's sweep or sees the reload.
	if !r.principals.matches(t.principal, t.seed) {
		return "", errRevoked
	}
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
	if t == nil || !time.Now().Before(t.expires) || !r.principals.matches(t.principal, t.seed) {
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
	if t == nil || t.kind != kind || !time.Now().Before(t.expires) || !r.principals.matches(t.principal, t.seed) {
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
<p class="muted">The value goes to the browser session, not to the agent, and is typed only into the site above - check it is the one you expect. A page there can still read what is typed into it.</p>
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
	sessionHash := tokenHash(session)
	lifetime := min(time.Until(t.expires), viewSessionMax)
	if !r.startViewSeed(sessionHash, t.principal, t.seed, lifetime) {
		writeMessage(w, http.StatusGone, "Link expired", msgLinkGone)
		return
	}
	if _, err := r.m.pool.getOrLaunch(req.Context(), connectRequest{seed: t.seed}); err != nil {
		r.endView(sessionHash)
		writeMessage(w, http.StatusServiceUnavailable, "Viewer unavailable", "The browser could not start.")
		return
	}
	r.mu.Lock()
	view := r.views[sessionHash]
	valid := view != nil && time.Now().Before(view.expires) && r.principals.matches(t.principal, t.seed)
	if valid {
		lifetime = time.Until(view.expires)
	}
	r.mu.Unlock()
	if !valid {
		r.endView(sessionHash)
		writeMessage(w, http.StatusGone, "Link expired", msgLinkGone)
		return
	}
	if r.raiseSeed != nil {
		r.raiseSeed(req.Context(), boundSeedKey(t.seed))
	}
	http.SetCookie(w, &http.Cookie{
		Name: viewerCookie, Value: session, Path: "/", MaxAge: int(lifetime.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	logInfo("remote: viewer link from %s redeemed (session ends in %s)", t.principal, lifetime.Round(time.Second))
	http.Redirect(w, req, "/", http.StatusSeeOther)
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
	r.mu.Lock()
	if !r.principals.matches(t.principal, t.seed) {
		r.mu.Unlock()
		writeMessage(w, http.StatusGone, "Link expired", msgLinkGone)
		return
	}
	held := r.m.pool.secrets.putBound(boundSeedKey(t.seed), t.name, []byte(value), sourceHumanForm, ttl, origin)
	r.mu.Unlock()
	logInfo("secrets: %s set through a request form from %s (%d bytes, bound to %s, ttl %s)", t.name, t.principal, len(value), origin, held)
	writeMessage(w, http.StatusOK, "Saved", "Saved - you can close this page.")
}

// ---------------------------------------------------------------------------
// The viewer proxy (/ and its /v/ alias)
// ---------------------------------------------------------------------------

func (r *remote) startView(h [sha256.Size]byte, principal string, lifetime time.Duration) {
	r.startViewSeed(h, principal, "", lifetime)
}

func (r *remote) startViewSeed(h [sha256.Size]byte, principal, seed string, lifetime time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lifetime <= 0 || !r.principals.matches(principal, seed) {
		return false
	}
	ctx, end := context.WithCancel(context.Background())
	s := &viewSession{principal: principal, seed: seed, expires: time.Now().Add(lifetime), ctx: ctx, end: end}
	s.timer = time.AfterFunc(lifetime, func() { r.endView(h) })
	r.views[h] = s
	return true
}

func (r *remote) endView(h [sha256.Size]byte) {
	r.mu.Lock()
	s := r.views[h]
	delete(r.views, h)
	r.mu.Unlock()
	if s == nil {
		return
	}
	s.timer.Stop()
	s.end()
	logInfo("remote: viewer session from %s ended", s.principal)
}

func (r *remote) viewSessionOf(req *http.Request) *viewSession {
	c, err := req.Cookie(viewerCookie)
	if err != nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.views[tokenHash(c.Value)]; s != nil && time.Now().Before(s.expires) && r.principals.matches(s.principal, s.seed) {
		return s
	}
	return nil
}

// handleViewer proxies the viewer to a trusted peer, or to a redeemed viewer
// link's cookie, whose session bounds every request it carries.
func (r *remote) handleViewer(w http.ResponseWriter, req *http.Request) {
	var s *viewSession
	if !r.trustedPeer(req) {
		if s = r.viewSessionOf(req); s == nil {
			writeMessage(w, http.StatusUnauthorized, "Session ended", "This viewer session has ended - ask the agent for a new link.")
			return
		}
	}
	r.viewers.Add(1)
	defer r.viewers.Add(-1)
	if headerHasToken(req.Header, "Connection", "upgrade") {
		// CrossOriginProtection passes every GET, and a WebSocket handshake is
		// one: the cookie or the owner's network position alone would let any
		// page the owner visits open the viewer.
		if req.Header.Get("Origin") != r.publicURL {
			logWarn("remote: refused a viewer upgrade from Origin %q", req.Header.Get("Origin"))
			http.Error(w, "Forbidden: cross-origin viewer connection", http.StatusForbidden)
			return
		}
	}
	if s == nil {
		r.proxy.ServeHTTP(w, req)
		return
	}
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	r.proxy.ServeHTTP(w, req.WithContext(ctx))
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

// ---------------------------------------------------------------------------
// /files: download handles
// ---------------------------------------------------------------------------

// mintFile returns a download handle for dir/name, the live one this principal
// already holds for that file if any, so a re-fetching agent does not pile up
// handles for an hour.
func (r *remote) mintFile(principal, dir, name string) (string, error) {
	return r.mintFileSeed(principal, "", dir, name)
}

func (r *remote) mintFileSeed(principal, seed, dir, name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.principals.matches(principal, seed) {
		return "", errRevoked
	}
	now := time.Now()
	for h, f := range r.files {
		switch {
		case !now.Before(f.expires):
			delete(r.files, h)
		case f.principal == principal && boundSeedName(f.seed) == boundSeedName(seed) && f.dir == dir && f.name == name:
			f.expires = now.Add(fileHandleTTL)
			return h, nil
		}
	}
	h := newToken(16)
	r.files[h] = &fileHandle{principal: principal, seed: seed, dir: dir, name: name, expires: now.Add(fileHandleTTL)}
	return h, nil
}

func (r *remote) handleFile(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	var f *fileHandle
	if stored := r.files[req.PathValue("h")]; stored != nil {
		snapshot := *stored
		f = &snapshot
	}
	r.mu.Unlock()
	// A handle another principal minted is answered exactly like a missing one.
	if f == nil || f.principal != principalOf(req.Context()) || boundSeedName(f.seed) != principalSeedOf(req.Context()) || !r.principals.matches(f.principal, f.seed) || !time.Now().Before(f.expires) {
		writeJSON(w, http.StatusNotFound, map[string]any{keyError: "no such file handle"})
		return
	}
	serveDownload(w, f.dir, f.name)
}

func (r *remote) seedBusy(seedKey string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for _, t := range r.tickets {
		if boundSeedKey(t.seed) == seedKey && now.Before(t.expires) && r.principals.matches(t.principal, t.seed) {
			return true
		}
	}
	for _, v := range r.views {
		if boundSeedKey(v.seed) == seedKey && now.Before(v.expires) && r.principals.matches(v.principal, v.seed) {
			return true
		}
	}
	// A reap of a non-durable profile deletes its Downloads dir, which a live
	// file handle still points into.
	for _, f := range r.files {
		if boundSeedKey(f.seed) == seedKey && now.Before(f.expires) && r.principals.matches(f.principal, f.seed) {
			return true
		}
	}
	return false
}

// viewing reports whether any viewer request is in flight.
func (r *remote) viewing() bool { return r.viewers.Load() > 0 }
