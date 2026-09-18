package serve

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// healthLockBudget is how long /healthz waits for the pool's lock before calling
// the daemon wedged. The lock guards maps and is never held across I/O, so a
// healthy daemon answers in microseconds; a second of waiting is a real hang.
const healthLockBudget = time.Second

// Metric label name and values. The values are consts because they are written
// in two places - the pre-create loop below and the call sites that increment -
// and a typo in either would silently start a second, empty series instead of
// failing.
const (
	labelResult = "result"
	labelCause  = "cause"

	keyReady = "ready"

	resultOK     = "ok"
	resultFailed = "failed"

	causeIdleReap   = "idle_reap"
	causeShutdown   = "shutdown"
	causeUnexpected = "unexpected"
)

// readyBudget bounds one readiness check, which touches the profile volume.
const readyBudget = 2 * time.Second

// readyProbeFile is the fixed name the readiness check writes to and removes.
const readyProbeFile = ".readyz-probe"

// readyFailThreshold is how many consecutive failed launches of the session
// browser make the daemon report not-ready: one failure is a blip, a run of them
// means every attach is being refused and the pod should leave the Service.
const readyFailThreshold = 3

// handleHealthz is the liveness probe: the process serves HTTP and the pool's
// lock can be taken. It never touches Chrome, so a restart loop can never be
// caused by a slow or absent browser - that is readiness's business.
func (m *multiplexer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthLockBudget)
	defer cancel()
	if !m.pool.lockResponsive(ctx) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{keyStatus: "wedged", keyError: "pool lock not acquired within " + healthLockBudget.String()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{keyStatus: "ok"})
}

// lockResponsive reports whether p.mu can be taken before ctx ends.
//
// The probe goroutine is shared: a wedged lock parks whichever goroutine is
// waiting on it FOREVER (there is no cancellable Lock), so spawning one per
// request would leak one per probe - and the endpoint is unauthenticated, so
// that is not a bounded number. One outstanding prober serves every caller, and
// it also answers the question they are all asking.
func (p *chromePool) lockResponsive(ctx context.Context) bool {
	p.probeMu.Lock()
	waiting := p.lockProbe
	if waiting == nil {
		waiting = make(chan struct{})
		p.lockProbe = waiting
		go func() {
			p.mu.Lock()
			p.mu.Unlock() //nolint:staticcheck // taken only to prove it can be
			p.probeMu.Lock()
			p.lockProbe = nil
			p.probeMu.Unlock()
			close(waiting)
		}()
	}
	p.probeMu.Unlock()

	select {
	case <-waiting:
		return true
	case <-ctx.Done():
		return false
	}
}

// handleReadyz is the readiness probe: may traffic be routed here right now. It
// fails while the daemon drains at shutdown (so the Service stops sending new
// attaches before the browsers die), when the display a headed browser needs is
// gone, when the data dir cannot be written, and when the session browser has
// failed to launch several times in a row (every attach is being refused).
func (m *multiplexer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// Bounded like the liveness probe: the checks below touch the profile volume,
	// and a hung mount would otherwise park a goroutine per probe forever while
	// the prober itself gives up client-side.
	ctx, cancel := context.WithTimeout(r.Context(), readyBudget)
	defer cancel()
	reason := make(chan string, 1)
	go func() { reason <- m.notReady() }()
	select {
	case why := <-reason:
		if why != "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{keyReady: false, "reason": why})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{keyReady: true})
	case <-ctx.Done():
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			keyReady: false, "reason": "the readiness checks did not finish within " + readyBudget.String(),
		})
	}
}

func (m *multiplexer) notReady() string {
	if m.draining.Load() {
		return "draining: the daemon is shutting down"
	}
	if !m.pool.headless {
		if display := os.Getenv("DISPLAY"); display != "" {
			if err := displayUp(display); err != nil {
				return "display " + display + " is not up: " + err.Error()
			}
		}
	}
	if err := os.MkdirAll(m.pool.dataDir, 0o700); err != nil {
		return "data dir: " + err.Error()
	}
	// One fixed name, not a fresh temp file per probe: this runs every few
	// seconds forever, and a process killed between create and remove would
	// otherwise strand a file in the profile volume that nothing ever collects.
	// A byte is written because a zero-length create still succeeds on a full
	// disk, and "writable" is meant to include "has room".
	probePath := filepath.Join(m.pool.dataDir, readyProbeFile)
	probe, err := os.OpenFile(probePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "data dir not writable: " + err.Error()
	}
	_, werr := probe.Write([]byte{'.'})
	_ = probe.Close()
	_ = os.Remove(probePath)
	if werr != nil {
		return "data dir not writable: " + werr.Error()
	}
	if m.pool.mode == modeSession {
		if _, fails := m.pool.launchCooldown(reservedSeed); fails >= readyFailThreshold {
			return "the session browser failed to launch " + strconv.Itoa(fails) + " times in a row"
		}
	}
	return ""
}

// displayUp checks for the X server's socket. DISPLAY is ":N" (set by the image,
// not by a request); only a bare display number is accepted, so the path built
// from it can never leave the X socket directory.
func displayUp(display string) error {
	n, err := strconv.Atoi(strings.TrimPrefix(display, ":"))
	if err != nil {
		return fmt.Errorf("unsupported DISPLAY %q: %w", display, err)
	}
	_, err = os.Stat(filepath.Join("/tmp/.X11-unix", "X"+strconv.Itoa(n)))
	return err //nolint:wrapcheck // the caller prefixes the display
}

// ---------------------------------------------------------------------------
// metrics
// ---------------------------------------------------------------------------

// poolMetrics is what the daemon exports at /metrics. Counters follow the log
// lines one-for-one: anything the daemon narrates it also counts, so a dashboard
// can watch what an operator would otherwise grep for.
type poolMetrics struct {
	registry      *prometheus.Registry
	launches      *prometheus.CounterVec
	launchSeconds prometheus.Histogram
	exits         *prometheus.CounterVec
	captures      *prometheus.CounterVec
	injects       *prometheus.CounterVec
	injectSeconds prometheus.Histogram
	attaches      prometheus.Counter
}

func newPoolMetrics(p *chromePool) *poolMetrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	pm := &poolMetrics{
		registry: reg,
		launches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cuttle_browser_launches_total", Help: "Chrome launches by result.",
		}, []string{labelResult}),
		launchSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "cuttle_browser_launch_seconds", Help: "Time from launch request to a ready CDP endpoint.",
			Buckets: []float64{0.5, 1, 2, 3, 5, 8, 13, 21, 34},
		}),
		exits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cuttle_browser_exits_total", Help: "Chrome exits by cause: idle_reap, shutdown, unexpected.",
		}, []string{labelCause}),
		captures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cuttle_state_captures_total", Help: "Auth-state snapshots by result.",
		}, []string{labelResult}),
		injects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cuttle_state_injects_total", Help: "Auth-state restores into a browser by result.",
		}, []string{labelResult}),
		injectSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "cuttle_state_inject_seconds", Help: "Duration of one auth-state restore.",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		}),
		attaches: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cuttle_cdp_attaches_total", Help: "CDP client connections accepted.",
		}),
	}
	reg.MustRegister(
		pm.launches, pm.launchSeconds, pm.exits, pm.captures, pm.injects, pm.injectSeconds, pm.attaches,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cuttle_browsers_active", Help: "Chrome processes currently running.",
		}, func() float64 { return float64(p.activeCount()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cuttle_cdp_connections_active", Help: "CDP clients currently attached, across all browsers.",
		}, func() float64 { return float64(p.connectionCount()) }),
	)
	// Pre-create the label values so a dashboard sees zeros, not absence.
	for _, r := range []string{resultOK, resultFailed} {
		pm.launches.WithLabelValues(r)
		pm.captures.WithLabelValues(r)
		pm.injects.WithLabelValues(r)
	}
	for _, c := range []string{causeIdleReap, causeShutdown, causeUnexpected} {
		pm.exits.WithLabelValues(c)
	}
	return pm
}

// handler serves the registry. Scrapes are serialized and bounded: both gauges
// read pool state under p.mu, so an unbounded handler would pile scrapes up on a
// wedged lock exactly like the liveness probe used to.
func (pm *poolMetrics) handler() http.Handler {
	return promhttp.HandlerFor(pm.registry, promhttp.HandlerOpts{
		MaxRequestsInFlight: 1,
		Timeout:             metricsTimeout,
	})
}

const metricsTimeout = 5 * time.Second

func (p *chromePool) activeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, inst := range p.processes {
		if inst.process.running() {
			n++
		}
	}
	return n
}

func (p *chromePool) connectionCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.conns {
		n += c
	}
	return n
}
