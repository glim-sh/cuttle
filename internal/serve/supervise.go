package serve

import (
	"context"
	"strconv"
	"sync"
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
//
// The reserved default seed is ALWAYS supervised. Its profile dir persists in the
// keep-profile named volume/PVC, which carries localStorage/IndexedDB/service
// workers - but Chrome never flushes its Cookies DB to disk on the SIGTERM
// teardown, and the reserved seed has no local-canonical mirror (the CLI can't
// address it via the state API). So its cookies would be lost across a recreate
// unless the daemon captures them over CDP into the durable snapshot store and
// re-injects them at the next launch. That capture+reinject is what makes the
// default profile's cookies survive `cuttle up --recreate` and image upgrades.
func (p *chromePool) supervised(seedKey string) bool {
	return seedKey == reservedSeed || !p.keepProfile || p.store.isSupervised(seedKey)
}

// captureMu returns the per-seed capture lock, creating it on first use. Held for
// the duration of one extract so a reap/shutdown can WAIT for an in-flight
// capture (mu.Lock) before tearing Chrome down, while a racing trigger collapses
// (mu.TryLock).
func (p *chromePool) captureMu(seedKey string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	mu := p.captureLocks[seedKey]
	if mu == nil {
		mu = &sync.Mutex{}
		p.captureLocks[seedKey] = mu
	}
	return mu
}

// captureSupervised is the non-blocking capture path (last-client-disconnect, the
// periodic ticker). Overlapping triggers collapse to one in-flight extract via
// TryLock. inst is passed directly (not re-looked-up) so the caller controls
// exactly which process is captured.
func (p *chromePool) captureSupervised(seedKey string, inst *chromeInstance) {
	if inst == nil || !inst.process.running() {
		return
	}
	mu := p.captureMu(seedKey)
	if !mu.TryLock() {
		return // a capture is already in flight; collapse to it
	}
	defer mu.Unlock()
	p.doCapture(seedKey, inst)
}

// captureAndTerminate captures a supervised seed's final state, then terminates
// it. It takes the capture lock with a BLOCKING Lock (not TryLock) so a
// concurrent in-flight capture completes before the browser dies - without this
// the disconnect capture goroutine would lose its target when a short
// --idle-timeout reap (or a clean shutdown) races it, stranding a never-yet
// snapshotted login. terminate runs after our capture releases the lock; a
// racing capture during teardown fails harmlessly (best-effort, never clobbers a
// good snapshot with a failed extract).
func (p *chromePool) captureAndTerminate(seedKey string, inst *chromeInstance, supervise bool) {
	if supervise {
		mu := p.captureMu(seedKey)
		mu.Lock()
		p.doCapture(seedKey, inst)
		mu.Unlock()
	}
	p.terminate(inst)
}

// doCapture extracts a running seed's storage state and records it in the daemon
// snapshot store. Best-effort: a failed extract logs and leaves the last snapshot
// in place. The caller owns the seed's capture lock.
func (p *chromePool) doCapture(seedKey string, inst *chromeInstance) {
	if inst == nil || !inst.process.running() {
		return
	}
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
// loopback CDP. The extract reads localStorage in place from every open tab, so a
// brand-new login is captured on its first checkpoint without any navigation - no
// second discovery pass is needed. It passes the origins already known from the
// prior snapshot so any of them whose tab is now closed is reported failed and
// keeps its prior localStorage (carry-forward), never cleared on a transient blip.
func (p *chromePool) extractSeedState(ctx context.Context, cdpBase string, prior *cdp.StorageState) (*cdp.StorageState, bool) {
	known := profile.CandidateOrigins(prior)
	st, failed, err := p.state.extract(ctx, cdpBase, known)
	if err != nil {
		logWarn("state capture: extract failed (%s): %v", cdpBase, err)
		return nil, false
	}
	if len(failed) > 0 {
		st = profile.CarryForward(prior, st, failed)
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
