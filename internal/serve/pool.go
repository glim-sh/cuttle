package serve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/glim-sh/cuttle/internal/fingerprint"
)

// launchError carries an HTTP status for a get-or-launch failure so handlers can
// translate it into a CDP-style JSON error before any WebSocket upgrade.
type launchError struct {
	status int
	msg    string
}

func (e *launchError) Error() string { return e.msg }

// processHandle abstracts a spawned Chrome so the pool can be tested without a
// real browser.
type processHandle interface {
	running() bool
	signalTerm() error
	kill() error
	wait(timeout time.Duration) bool
	pid() int
}

// launcher is the injectable seam that allocates a CDP port, starts the process,
// and waits for its CDP endpoint to answer. The default wires os/exec + a real
// port probe; tests substitute a fake CDP server.
type launcher struct {
	allocPort func() (int, error)
	start     func(binary string, args []string) (processHandle, error)
	waitReady func(ctx context.Context, port int) bool
}

type chromeInstance struct {
	seed        string
	process     processHandle
	cdpPort     int
	userDataDir string
	timezone    string
	locale      string
	proxy       string
}

type chromePool struct {
	binary          string
	globalArgs      []string
	headless        bool
	dataDir         string
	defaultSeed     string
	defaultLocale   string
	defaultTimezone string
	defaultProxy    string
	idleTimeout     time.Duration
	keepProfile     bool
	ephemeral       bool
	launch          launcher
	geo             fingerprint.GeoResolver

	mu         sync.Mutex
	processes  map[string]*chromeInstance
	seedLocks  map[string]*sync.Mutex
	conns      map[string]int
	idleTimers map[string]*time.Timer
}

func newChromePool(cfg serveConfig, binary string, globalArgs []string, l launcher, geo fingerprint.GeoResolver) *chromePool {
	return &chromePool{
		binary:          binary,
		globalArgs:      globalArgs,
		headless:        cfg.headless,
		dataDir:         cfg.dataDir,
		defaultSeed:     cfg.defaultSeed,
		defaultLocale:   cfg.defaultLocale,
		defaultTimezone: cfg.defaultTimezone,
		defaultProxy:    cfg.proxy,
		idleTimeout:     cfg.idleTimeout,
		keepProfile:     cfg.keepProfile,
		ephemeral:       cfg.ephemeral,
		launch:          l,
		geo:             geo,
		processes:       map[string]*chromeInstance{},
		seedLocks:       map[string]*sync.Mutex{},
		conns:           map[string]int{},
		idleTimers:      map[string]*time.Timer{},
	}
}

func (p *chromePool) seedLock(key string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	l := p.seedLocks[key]
	if l == nil {
		l = &sync.Mutex{}
		p.seedLocks[key] = l
	}
	return l
}

// connect increments a seed's connection refcount and cancels any pending idle
// reap.
func (p *chromePool) connect(seedKey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancelIdleLocked(seedKey)
	p.conns[seedKey]++
}

// disconnect decrements a seed's refcount and schedules an idle reap when it
// reaches zero.
func (p *chromePool) disconnect(seedKey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns[seedKey]--
	if p.conns[seedKey] <= 0 {
		delete(p.conns, seedKey)
		p.scheduleIdleLocked(seedKey)
	}
}

func (p *chromePool) cancelIdleLocked(seedKey string) {
	if t := p.idleTimers[seedKey]; t != nil {
		t.Stop()
		delete(p.idleTimers, seedKey)
	}
}

// scheduleIdleLocked arms an idle reap. Reaping is disabled when idleTimeout <= 0
// (remote browsers never reap); local runs pass a positive timeout.
func (p *chromePool) scheduleIdleLocked(seedKey string) {
	if p.idleTimeout <= 0 {
		return
	}
	if _, ok := p.processes[seedKey]; !ok {
		return
	}
	p.cancelIdleLocked(seedKey)
	p.idleTimers[seedKey] = time.AfterFunc(p.idleTimeout, func() { p.idleReap(seedKey) })
}

func (p *chromePool) idleReap(seedKey string) {
	p.mu.Lock()
	if p.conns[seedKey] > 0 {
		p.mu.Unlock()
		return
	}
	inst := p.processes[seedKey]
	if inst == nil {
		p.mu.Unlock()
		return
	}
	delete(p.processes, seedKey)
	delete(p.idleTimers, seedKey)
	delete(p.conns, seedKey)
	delete(p.seedLocks, seedKey)
	p.mu.Unlock()

	logInfo("cleaning up idle Chrome process (seed=%s)", seedKey)
	p.terminate(inst)
}

// connectRequest carries the per-connection parameters resolved from the query
// string (and server defaults).
type connectRequest struct {
	seed      string
	extraArgs []string
	timezone  string
	locale    string
	proxy     string
	geoip     bool
}

// getOrLaunch returns the running Chrome for a seed, launching it on first use.
// A missing seed maps to the shared "__default__" process with a random
// fingerprint. First-launch wins: later params for a live seed are ignored.
func (p *chromePool) getOrLaunch(ctx context.Context, req connectRequest) (*chromeInstance, error) {
	seed := req.seed
	if seed == "" && p.defaultSeed != "" {
		seed = p.defaultSeed
	}
	locale := req.locale
	if locale == "" {
		locale = p.defaultLocale
	}
	timezone := req.timezone
	if timezone == "" {
		timezone = p.defaultTimezone
	}
	proxy := req.proxy
	if proxy == "" {
		proxy = p.defaultProxy
	}

	var seedKey, actualSeed string
	if seed == "" {
		seedKey = reservedSeed
		actualSeed = strconv.Itoa(randSeed())
	} else {
		if !validSeed(seed) {
			return nil, &launchError{status: http.StatusBadRequest, msg: "Invalid fingerprint seed"}
		}
		seedKey = seed
		actualSeed = seed
	}

	lock := p.seedLock(seedKey)
	lock.Lock()
	defer lock.Unlock()

	p.mu.Lock()
	existing := p.processes[seedKey]
	pending := p.idleTimers[seedKey] != nil
	p.mu.Unlock()

	if existing != nil && existing.process.running() {
		if pending {
			p.mu.Lock()
			p.scheduleIdleLocked(seedKey)
			p.mu.Unlock()
		}
		if len(req.extraArgs) > 0 || timezone != "" || locale != "" || proxy != "" || req.geoip {
			logWarn("seed %s already running (port %d, tz=%s, locale=%s, proxy=%s) - ignoring new params (first-launch wins)",
				seedKey, existing.cdpPort, existing.timezone, existing.locale, existing.proxy)
		}
		return existing, nil
	}
	if existing != nil {
		p.removeProcess(seedKey)
		p.terminate(existing)
	}

	var exitIP string
	if req.geoip && proxy != "" {
		timezone, locale, exitIP = p.resolveGeo(proxy, timezone, locale)
	}

	fpExtra := []string{"--fingerprint=" + actualSeed}
	fpExtra = append(fpExtra, req.extraArgs...)
	if proxy != "" {
		// Fork binaries reject inline creds on --proxy-server; strip them here
		// and answer the proxy 407 over CDP (see wsproxy). geoip above still uses
		// the credentialed proxy; the full proxy is stored on the instance so the
		// ws proxy can recover the credentials.
		stripped, _, _ := fingerprint.SplitProxyAuth(proxy)
		fpExtra = append(fpExtra, "--proxy-server="+fingerprint.NormalizeSocksStringURL(stripped))
	}

	// resolveGeo above already resolved the exit IP over the network; reuse it for
	// --fingerprint-webrtc-ip=auto instead of letting ResolveWebRTCArgs hit the
	// echo services a second time on the launch path.
	webrtcResolver := p.exitIPForWebRTC
	if exitIP != "" {
		webrtcResolver = func(string) string { return exitIP }
	}
	fpExtra = fingerprint.ResolveWebRTCArgs(fpExtra, proxy, webrtcResolver)
	if exitIP != "" && !slices.ContainsFunc(fpExtra, func(a string) bool {
		return strings.HasPrefix(a, "--fingerprint-webrtc-ip")
	}) {
		fpExtra = append(fpExtra, "--fingerprint-webrtc-ip="+exitIP)
	}

	fpExtra = append(fpExtra, fingerprint.ForkParityArgs(locale, proxy)...)

	chromeArgs := fingerprint.BuildArgs(fingerprint.BuildArgsInput{
		StealthArgs: true,
		ExtraArgs:   fpExtra,
		Timezone:    timezone,
		Locale:      locale,
		Headless:    p.headless,
	})

	inst, err := p.spawn(ctx, seedKey, actualSeed, chromeArgs, timezone, locale, proxy)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.processes[seedKey] = inst
	p.mu.Unlock()
	return inst, nil
}

func (p *chromePool) spawn(ctx context.Context, seedKey, actualSeed string, chromeArgs []string, timezone, locale, proxy string) (*chromeInstance, error) {
	userDataDir, err := p.profileDir(seedKey)
	if err != nil {
		logError("failed to create profile dir for seed=%s: %v", seedKey, err)
		return nil, &launchError{status: http.StatusBadGateway, msg: msgChromeFailed}
	}
	seedProfileDefaults(userDataDir)

	port, err := p.launch.allocPort()
	if err != nil {
		p.safeRemoveTree(userDataDir)
		return nil, &launchError{status: http.StatusBadGateway, msg: msgChromeFailed}
	}

	fullArgs := slices.Clone(baseChromeArgs)
	fullArgs = append(fullArgs, chromeArgs...)
	fullArgs = append(fullArgs, p.globalArgs...)
	fullArgs = append(
		fullArgs,
		"--remote-debugging-port="+strconv.Itoa(port),
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir="+userDataDir,
	)

	logInfo("launching Chrome (seed=%s, port=%d)", actualSeed, port)
	proc, err := p.launch.start(p.binary, fullArgs)
	if err != nil {
		p.safeRemoveTree(userDataDir)
		return nil, &launchError{status: http.StatusBadGateway, msg: msgChromeFailed}
	}

	if !p.launch.waitReady(ctx, port) {
		_ = proc.kill()
		proc.wait(terminateGrace)
		p.safeRemoveTree(userDataDir)
		return nil, &launchError{status: http.StatusBadGateway, msg: msgChromeFailed}
	}

	logInfo("Chrome ready (seed=%s, port=%d, pid=%d)", actualSeed, port, proc.pid())
	return &chromeInstance{
		seed:        actualSeed,
		process:     proc,
		cdpPort:     port,
		userDataDir: userDataDir,
		timezone:    timezone,
		locale:      locale,
		proxy:       proxy,
	}, nil
}

// profileDir returns the seed's user_data_dir. When ephemeral, it is a fresh
// scratch dir under dataDir (nothing persists across sessions); otherwise it is
// the stable per-seed path.
func (p *chromePool) profileDir(seedKey string) (string, error) {
	if err := os.MkdirAll(p.dataDir, 0o700); err != nil {
		return "", err //nolint:wrapcheck
	}
	if p.ephemeral {
		dir, err := os.MkdirTemp(p.dataDir, seedKey+"-")
		if err != nil {
			return "", err //nolint:wrapcheck
		}
		return dir, nil
	}
	dir := filepath.Join(p.dataDir, seedKey)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err //nolint:wrapcheck
	}
	return dir, nil
}

func (p *chromePool) removeProcess(seedKey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.processes, seedKey)
	p.cancelIdleLocked(seedKey)
	delete(p.conns, seedKey)
}

// terminate stops a Chrome (SIGTERM, then SIGKILL after the grace period) and
// removes its profile dir. It blocks and must be called without p.mu held.
func (p *chromePool) terminate(inst *chromeInstance) {
	if inst.process.running() {
		_ = inst.process.signalTerm()
		if !inst.process.wait(terminateGrace) {
			_ = inst.process.kill()
		}
	}
	p.safeRemoveTree(inst.userDataDir)
}

// safeRemoveTree deletes a profile dir, refusing any path outside dataDir. An
// ephemeral profile is always removed; otherwise --keep-profile preserves it.
func (p *chromePool) safeRemoveTree(path string) {
	if p.keepProfile && !p.ephemeral {
		logInfo("keep-profile: preserving %s", path)
		return
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return
	}
	dataResolved, err := filepath.Abs(p.dataDir)
	if err != nil {
		return
	}
	if resolved == dataResolved || !withinDir(dataResolved, resolved) {
		logError("refusing to delete path outside data_dir: %s", resolved)
		return
	}
	_ = os.RemoveAll(path)
}

func withinDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// shutdown terminates every Chrome process on daemon exit.
func (p *chromePool) shutdown() {
	p.mu.Lock()
	for key, t := range p.idleTimers {
		t.Stop()
		delete(p.idleTimers, key)
	}
	insts := make([]*chromeInstance, 0, len(p.processes))
	keys := make([]string, 0, len(p.processes))
	for key, inst := range p.processes {
		keys = append(keys, key)
		insts = append(insts, inst)
	}
	for _, key := range keys {
		delete(p.processes, key)
		delete(p.conns, key)
	}
	p.mu.Unlock()

	for _, inst := range insts {
		p.terminate(inst)
	}
	logInfo("all Chrome processes terminated")
}

// status reports the live process table for the health-check endpoint.
func (p *chromePool) status() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	procs := map[string]any{}
	for key, inst := range p.processes {
		if !inst.process.running() {
			continue
		}
		procs[key] = map[string]any{
			"pid":                  inst.process.pid(),
			"port":                 inst.cdpPort,
			"seed":                 inst.seed,
			"connections":          p.conns[key],
			"idle_cleanup_pending": p.idleTimers[key] != nil,
			"timezone":             inst.timezone,
			"locale":               inst.locale,
			"proxy":                inst.proxy,
		}
	}
	return map[string]any{
		"status":       "ok",
		"active":       len(procs),
		"idle_timeout": p.idleTimeout.Seconds(),
		"processes":    procs,
	}
}

// resolveGeo mirrors maybe_resolve_geoip: when both tz and locale are explicit,
// only the exit IP is resolved (for WebRTC); otherwise tz/locale are filled from
// the egress geo. The credentialed proxy is used so the egress IP is the proxy's
// exit IP.
func (p *chromePool) resolveGeo(proxy, timezone, locale string) (string, string, string) {
	proxyURL := fingerprint.EnsureProxyScheme(proxy)
	if timezone != "" && locale != "" {
		exitIP := ""
		if p.geo.ExitIP != nil {
			if ip, err := p.geo.ExitIP(proxyURL); err == nil {
				exitIP = ip
			}
		}
		return timezone, locale, exitIP
	}
	geoTZ, geoLocale, exitIP := p.geo.ResolveProxyGeoWithIP(proxyURL)
	if timezone == "" {
		timezone = geoTZ
	}
	if locale == "" {
		locale = geoLocale
	}
	return timezone, locale, exitIP
}

func (p *chromePool) exitIPForWebRTC(proxyURL string) string {
	if p.geo.ExitIP == nil {
		return ""
	}
	ip, err := p.geo.ExitIP(proxyURL)
	if err != nil {
		return ""
	}
	return ip
}

// seedProfileDefaults writes DuckDuckGo as the default search on a brand-new
// profile (matching the upstream seeding). Chrome owns the file afterward; tab
// restore is handled by clean shutdown, not by forging flags here.
func seedProfileDefaults(userDataDir string) {
	defaultDir := filepath.Join(userDataDir, "Default")
	prefsPath := filepath.Join(defaultDir, "Preferences")
	if _, err := os.Stat(prefsPath); err == nil {
		return
	}
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		return
	}
	prefs := map[string]any{
		"default_search_provider_data": map[string]any{
			"template_url_data": map[string]any{
				"keyword":         "duckduckgo.com",
				"short_name":      "DuckDuckGo",
				"url":             "https://duckduckgo.com/?q={searchTerms}",
				"suggestions_url": "https://duckduckgo.com/ac/?q={searchTerms}&type=list",
				"favicon_url":     "https://duckduckgo.com/favicon.ico",
			},
		},
		"default_search_provider": map[string]any{"enabled": true},
	}
	data, err := json.Marshal(prefs)
	if err != nil {
		return
	}
	_ = os.WriteFile(prefsPath, data, 0o600)
}

func randSeed() int {
	// A fingerprint seed, not a security token; math/rand mirrors the oracle.
	return rand.IntN(90000) + 10000 //nolint:gosec // non-cryptographic seed
}

// ---------------------------------------------------------------------------
// Default launcher (os/exec)
// ---------------------------------------------------------------------------

func defaultLauncher() launcher {
	return launcher{
		allocPort: newSequentialPortAllocator(),
		start:     startChrome,
		waitReady: waitForCDP,
	}
}

var errNoFreePorts = errors.New("no free ports available for Chrome CDP")

func newSequentialPortAllocator() func() (int, error) {
	var mu sync.Mutex
	next := basePort
	return func() (int, error) {
		mu.Lock()
		defer mu.Unlock()
		var lc net.ListenConfig
		for range 100 {
			port := next
			next++
			l, err := lc.Listen(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err != nil {
				continue
			}
			_ = l.Close()
			return port, nil
		}
		return 0, errNoFreePorts
	}
}

func startChrome(binary string, args []string) (processHandle, error) {
	// context.Background, not any request context: the pool owns Chrome's
	// lifecycle (signalTerm/kill), so the process must outlive the HTTP request
	// that launched it.
	cmd := exec.CommandContext(context.Background(), binary, args...)
	cmd.Stdout = nil
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err //nolint:wrapcheck
	}
	h := &osProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		h.mu.Lock()
		h.exited = true
		h.mu.Unlock()
		close(h.done)
	}()
	return h, nil
}

func waitForCDP(ctx context.Context, port int) bool {
	deadline := time.Now().Add(10 * time.Second)
	delay := 100 * time.Millisecond
	endpoint := "http://127.0.0.1:" + strconv.Itoa(port) + "/json/version"
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return true
				}
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		}
		delay = min(delay*2, time.Second)
	}
	return false
}

type osProcess struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	done   chan struct{}
	exited bool
}

func (o *osProcess) running() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return !o.exited
}

func (o *osProcess) signalTerm() error {
	return o.cmd.Process.Signal(syscall.SIGTERM) //nolint:wrapcheck
}

func (o *osProcess) kill() error {
	return o.cmd.Process.Kill() //nolint:wrapcheck
}

func (o *osProcess) wait(timeout time.Duration) bool {
	select {
	case <-o.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (o *osProcess) pid() int { return o.cmd.Process.Pid }
