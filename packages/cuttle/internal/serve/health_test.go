package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func probe(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

// The probes are the k8s contract: liveness must never depend on a browser, and
// readiness must drop the pod out of the Service while it drains and while the
// session browser cannot be launched at all.
func TestHealthAndReadinessProbes(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{mode: modeSession}, fl.toLauncher())
	m := &multiplexer{pool: pool}
	h := m.routes()

	if code, _ := probe(t, h, "/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz = %d with no browser at all, want 200: liveness must not need Chrome", code)
	}
	if code, _ := probe(t, h, "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz = %d with no browser yet, want 200", code)
	}
	if fl.launchCount() != 0 {
		t.Fatal("a probe must never launch a browser")
	}
	// A run of failed launches means every attach is being refused.
	for range readyFailThreshold {
		pool.recordLaunchFailure(reservedSeed)
	}
	if code, body := probe(t, h, "/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "failed to launch") {
		t.Fatalf("/readyz after %d failed launches = %d %s, want 503 naming the failures", readyFailThreshold, code, body)
	}
	if code, _ := probe(t, h, "/healthz"); code != http.StatusOK {
		t.Fatal("launch failures are a readiness matter, not a liveness one")
	}

	m.draining.Store(true)
	if code, body := probe(t, h, "/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "draining") {
		t.Fatalf("/readyz while draining = %d %s, want 503 draining", code, body)
	}
}

// A daemon whose pool lock is stuck is alive to the TCP stack and dead to every
// client; /healthz is the probe that turns that into a restart.
func TestHealthzReportsAStuckPoolLock(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t, serveConfig{mode: modeSession}, (&fakeLauncher{port: 5100}).toLauncher())
	pool.mu.Lock()
	defer pool.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), healthLockBudget/10)
	defer cancel()
	if pool.lockResponsive(ctx) {
		t.Fatal("a held lock must read as unresponsive")
	}
}

// /metrics is the Prometheus contract; the series names are what dashboards key
// on, so they are pinned here.
func TestMetricsExposeThePoolSeries(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{mode: modeSession}, fl.toLauncher())
	if _, err := pool.getOrLaunch(context.Background(), connectRequest{}); err != nil {
		t.Fatal(err)
	}
	pool.connect(reservedSeed)
	code, body := probe(t, (&multiplexer{pool: pool}).routes(), "/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d", code)
	}
	for _, want := range []string{
		"cuttle_browsers_active 1",
		"cuttle_cdp_connections_active 1",
		"cuttle_cdp_attaches_total 1",
		`cuttle_browser_launches_total{result="ok"} 1`,
		`cuttle_browser_launches_total{result="failed"} 0`,
		"cuttle_browser_launch_seconds_count 1",
		`cuttle_browser_exits_total{cause="idle_reap"} 0`,
		`cuttle_state_captures_total{result="ok"}`,
		"cuttle_state_inject_seconds_count",
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}
