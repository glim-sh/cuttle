package serve

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/glim-sh/cuttle/internal/cdp"
	"github.com/glim-sh/cuttle/internal/profile"
)

const (
	// captureTimeout bounds one extract/inject so a wedged browser can never stall
	// the serve path; it mirrors the CLI session's checkpoint timeout.
	captureTimeout = 30 * time.Second
	// supervisorInterval is the slow backstop that checkpoints long-held
	// connections which never hit the last-client-disconnect trigger.
	supervisorInterval = 5 * time.Minute
)

// stateOps is the injectable CDP seam for the daemon's own state capture: it runs
// cdp.Extract/Inject directly against a seed's loopback CDP port (seed="", since
// the port already belongs to that one browser - no ?fingerprint routing). Tests
// substitute fakes so supervision is exercised without a real Chrome.
type stateOps struct {
	extract func(ctx context.Context, cdpBase string, origins []string) (*cdp.StorageState, []string, error)
	inject  func(ctx context.Context, cdpBase string, st *cdp.StorageState) error
}

func defaultStateOps() stateOps {
	return stateOps{
		extract: func(ctx context.Context, cdpBase string, origins []string) (*cdp.StorageState, []string, error) {
			return cdp.Extract(ctx, cdpBase, "", origins)
		},
		inject: func(ctx context.Context, cdpBase string, st *cdp.StorageState) error {
			return cdp.Inject(ctx, cdpBase, "", st)
		},
	}
}

func loopbackBase(port int) string {
	return "http://127.0.0.1:" + strconv.Itoa(port)
}

// supervised reports whether a seed's auth state should be captured on lifecycle
// events. In the default disposable mode (profile dirs ephemeral, !keepProfile)
// every launched seed is supervised so a login survives Chrome teardown; when
// --keep-profile makes dirs durable, only seeds explicitly seeded via a PUT are.
func (p *chromePool) supervised(seedKey string) bool {
	return !p.keepProfile || p.store.isSupervised(seedKey)
}

// beginCapture claims the per-seed capture guard so overlapping triggers (a
// disconnect race with the periodic ticker) collapse to one in-flight extract.
func (p *chromePool) beginCapture(seedKey string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.capturing[seedKey] {
		return false
	}
	p.capturing[seedKey] = true
	return true
}

func (p *chromePool) endCapture(seedKey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.capturing, seedKey)
}

// captureSupervised extracts a running seed's storage state and records it in the
// daemon snapshot store. Best-effort: a failed extract logs and leaves the last
// snapshot in place. inst is passed directly (not re-looked-up) so a reap or
// shutdown can capture just before it deletes the process from the pool.
func (p *chromePool) captureSupervised(seedKey string, inst *chromeInstance) {
	if inst == nil || !inst.process.running() {
		return
	}
	if !p.beginCapture(seedKey) {
		return
	}
	defer p.endCapture(seedKey)

	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	var prior *cdp.StorageState
	if e, ok := p.store.get(seedKey); ok {
		prior = e.State
	}
	st, ok := p.extractSeedState(ctx, loopbackBase(inst.cdpPort), prior)
	if !ok {
		return
	}
	if _, _, err := p.store.put(seedKey, st, false, ""); err != nil {
		logWarn("state capture: persisting snapshot for seed=%s failed: %v", seedKey, err)
	}
}

// extractSeedState reads a seed's cookies and per-origin localStorage over its
// loopback CDP. It re-reads the origins already known from the prior snapshot,
// then does a second targeted pass for origins freshly discovered from this
// pass's cookie domains (so a brand-new login's localStorage is captured on the
// very first checkpoint, not only the next one). Origins that fail to load keep
// their prior localStorage (carry-forward) so a transient blip never clears it.
func (p *chromePool) extractSeedState(ctx context.Context, cdpBase string, prior *cdp.StorageState) (*cdp.StorageState, bool) {
	known := profile.CandidateOrigins(prior)
	st, failed, err := p.state.extract(ctx, cdpBase, known)
	if err != nil {
		logWarn("state capture: extract failed (%s): %v", cdpBase, err)
		return nil, false
	}
	if extra := originsNotIn(profile.CandidateOrigins(st), known); len(extra) > 0 {
		if st2, failed2, err2 := p.state.extract(ctx, cdpBase, extra); err2 == nil {
			st.Origins = append(st.Origins, st2.Origins...)
			failed = append(failed, failed2...)
		}
	}
	if len(failed) > 0 {
		st = carryForwardOrigins(prior, st, failed)
	}
	return st, true
}

// injectSeedState writes a storage state into a running seed's browser over its
// loopback CDP.
func (p *chromePool) injectSeedState(ctx context.Context, inst *chromeInstance, st *cdp.StorageState) error {
	return p.state.inject(ctx, loopbackBase(inst.cdpPort), st)
}

// startSupervisor runs the slow backstop checkpoint loop until ctx is cancelled,
// so a connection held open past a disconnect trigger is still snapshotted.
func (p *chromePool) startSupervisor(ctx context.Context) {
	go func() {
		t := time.NewTicker(supervisorInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				for seedKey, inst := range p.runningSupervised() {
					p.captureSupervised(seedKey, inst)
				}
			}
		}
	}()
}

// runningSupervised snapshots the running, supervised seeds and their instances.
func (p *chromePool) runningSupervised() map[string]*chromeInstance {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]*chromeInstance{}
	for seedKey, inst := range p.processes {
		if inst.process.running() && p.supervised(seedKey) {
			out[seedKey] = inst
		}
	}
	return out
}

// carryForwardOrigins re-attaches the prior localStorage for origins that failed
// to load this pass, so an unconditional overwrite never drops persisted state on
// a transient per-origin blip.
func carryForwardOrigins(prior, st *cdp.StorageState, failed []string) *cdp.StorageState {
	if prior == nil {
		return st
	}
	byOrigin := make(map[string]cdp.Origin, len(prior.Origins))
	for _, o := range prior.Origins {
		byOrigin[o.Origin] = o
	}
	for _, origin := range failed {
		if o, ok := byOrigin[origin]; ok {
			st.Origins = append(st.Origins, o)
		}
	}
	return st
}

func originsNotIn(candidates, known []string) []string {
	var out []string
	for _, o := range candidates {
		if !slices.Contains(known, o) {
			out = append(out, o)
		}
	}
	return out
}
