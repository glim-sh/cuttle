package serve

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/glim-sh/cuttle/internal/cdp"
)

const (
	// captureTimeout bounds one extract/inject so a wedged browser can never stall
	// the serve path; it mirrors the CLI session's checkpoint timeout.
	captureTimeout = 30 * time.Second
	// supervisorInterval is the slow backstop that checkpoints long-held
	// connections which never hit the last-client-disconnect trigger.
	supervisorInterval = 5 * time.Minute
	// injectOriginBudget and injectTimeoutMax size the localStorage pass, which
	// navigates one tab per origin. A flat budget silently truncated the restore
	// at whatever origin the clock ran out on - a snapshot that grew past the
	// budget quietly stopped restoring its tail - so the budget grows with the
	// work, capped so a wedged browser still cannot stall a launch forever.
	injectOriginBudget = 10 * time.Second
	injectTimeoutMax   = 5 * time.Minute
	// injectLaunchMax is the tighter ceiling for a re-inject at launch, which runs
	// holding the seed lock - in session mode every attach queues behind it, so a
	// huge snapshot must not be able to stall the whole daemon for the full
	// injectTimeoutMax. An explicit PUT is a request that asked for the work and
	// holds no lock, so it keeps the larger budget.
	injectLaunchMax = 90 * time.Second
)

// stateOps is the injectable CDP seam for the daemon's own state capture: it runs
// cdp.Extract/Inject directly against a seed's loopback CDP port (seed="", since
// the port already belongs to that one browser - no ?fingerprint routing). Tests
// substitute fakes so supervision is exercised without a real Chrome.
type stateOps struct {
	extract func(ctx context.Context, cdpBase string, origins []string) (*cdp.StorageState, []string, error)
	inject  func(ctx context.Context, cdpBase string, st *cdp.StorageState, opt cdp.InjectOptions) error
}

func defaultStateOps() stateOps {
	return stateOps{
		extract: func(ctx context.Context, cdpBase string, origins []string) (*cdp.StorageState, []string, error) {
			return cdp.Extract(ctx, cdpBase, "", origins)
		},
		inject: func(ctx context.Context, cdpBase string, st *cdp.StorageState, opt cdp.InjectOptions) error {
			return cdp.Inject(ctx, cdpBase, "", st, opt)
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
// teardown, and the reserved seed has no mirror anywhere else (the state API
// cannot address it - its key is not a legal seed). So its cookies would be lost across a recreate
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
	// A shutdown ends every CDP connection, so the disconnect trigger fires for a
	// browser the shutdown path is already capturing (and then terminating). Let
	// that one win: this one would only race it and log a failed extract against
	// a browser on its way out.
	p.mu.Lock()
	closing := p.closing
	p.mu.Unlock()
	if closing {
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
		p.metrics.captures.WithLabelValues("failed").Inc()
		return
	}
	if _, _, err := p.store.put(seedKey, st, false, ""); err != nil {
		logWarn("state capture: persisting snapshot for seed=%s failed: %v", seedKey, err)
		p.metrics.captures.WithLabelValues("failed").Inc()
		return
	}
	p.metrics.captures.WithLabelValues("ok").Inc()
	// Log success too, not just failure: a login that never persists is otherwise
	// invisible - you cannot tell a captured-nothing checkpoint from a captured-
	// the-login one. Counts (not values) are enough to watch a login appear.
	logInfo("state captured (seed=%s): %s", seedKey, stateSummary(st))
}

// stateSummary renders a privacy-safe one-line count of a snapshot: how many
// cookies, across how many distinct domains, plus localStorage origins. Enough to
// watch an auth login land (counts jump) or fail to persist (counts stay flat)
// without logging any cookie value or domain name.
func stateSummary(st *cdp.StorageState) string {
	if st == nil {
		return "empty"
	}
	domains := make(map[string]struct{}, len(st.Cookies))
	for _, c := range st.Cookies {
		domains[c.Domain] = struct{}{}
	}
	return fmt.Sprintf("%d cookies / %d domains / %d origins", len(st.Cookies), len(domains), len(st.Origins))
}

// extractSeedState reads a seed's cookies and per-origin localStorage over its
// loopback CDP. The extract reads localStorage in place from every open tab, so a
// brand-new login is captured on its first checkpoint without any navigation - no
// second discovery pass is needed. It passes the origins already known from the
// prior snapshot so any of them whose tab is now closed is reported failed and
// keeps its prior localStorage (carry-forward), never cleared on a transient blip.
func (p *chromePool) extractSeedState(ctx context.Context, cdpBase string, prior *cdp.StorageState) (*cdp.StorageState, bool) {
	known := candidateOrigins(prior)
	st, failed, err := p.state.extract(ctx, cdpBase, known)
	if err != nil {
		logWarn("state capture: extract failed (%s): %v", cdpBase, err)
		return nil, false
	}
	if len(failed) > 0 {
		st = carryForward(prior, st, failed)
	}
	return st, true
}

// injectSeedState writes a storage state into a running seed's browser over its
// loopback CDP.
func (p *chromePool) injectSeedState(ctx context.Context, inst *chromeInstance, st *cdp.StorageState, opt cdp.InjectOptions) error {
	start := time.Now()
	err := p.state.inject(ctx, loopbackBase(inst.cdpPort), st, opt)
	p.metrics.injectSeconds.Observe(time.Since(start).Seconds())
	result := "ok"
	if err != nil {
		result = "failed"
	}
	p.metrics.injects.WithLabelValues(result).Inc()
	return err
}

// durableProfile reports whether a seed's user-data-dir outlives its Chrome. It
// is the same condition that makes the default seed's fingerprint persist: the
// dir is kept, and not a per-session scratch copy. A free function so the viewer
// geometry command answers it from a serveConfig without a pool.
func durableProfile(keepProfile, ephemeral bool) bool {
	return keepProfile && !ephemeral
}

func (p *chromePool) durableProfile() bool {
	return durableProfile(p.keepProfile, p.ephemeral)
}

// localStorageOrigins counts the origins an inject would have to navigate to.
func localStorageOrigins(st *cdp.StorageState) int {
	if st == nil {
		return 0
	}
	n := 0
	for _, o := range st.Origins {
		if len(o.LocalStorage) > 0 {
			n++
		}
	}
	return n
}

// injectTimeout budgets one inject. Cookies land in a single call; the
// localStorage pass costs a page load per origin, so it gets a per-origin budget,
// capped at ceiling (see injectLaunchMax vs injectTimeoutMax).
func injectTimeout(st *cdp.StorageState, cookiesOnly bool, ceiling time.Duration) time.Duration {
	if cookiesOnly {
		return captureTimeout
	}
	return min(captureTimeout+time.Duration(localStorageOrigins(st))*injectOriginBudget, ceiling)
}

// reinjectAtLaunch restores a seed's snapshot into its freshly launched Chrome,
// narrating what it does. On a durable profile it writes cookies ONLY: the
// profile dir already carries its own localStorage, while Chrome never flushes
// its cookie DB on the SIGTERM teardown - so cookies are the only half that
// needs restoring, and the origin walk it replaces was a visible minutes-long
// march of the browser through every site the profile had ever stored.
func (p *chromePool) reinjectAtLaunch(seedKey string, inst *chromeInstance, st *cdp.StorageState) {
	cookiesOnly := p.durableProfile()
	budget := injectTimeout(st, cookiesOnly, injectLaunchMax)
	origins := localStorageOrigins(st)

	if cookiesOnly {
		logInfo("state re-inject (seed=%s): %s - cookies only, the durable profile dir keeps its own localStorage", seedKey, stateSummary(st))
	} else {
		logInfo("state re-inject (seed=%s): %s - navigating %d origins for localStorage (budget %s)", seedKey, stateSummary(st), origins, budget)
	}

	done := 0
	opt := cdp.InjectOptions{
		CookiesOnly: cookiesOnly,
		OnOrigin: func(index, total int, origin string) {
			done = index - 1
			logInfo("state re-inject (seed=%s): navigating %d/%d to %s", seedKey, index, total, origin)
		},
	}

	ctx, cancel := context.WithTimeout(p.baseCtx, budget)
	defer cancel()
	if err := p.injectSeedState(ctx, inst, st, opt); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && origins > 0 {
			logWarn("state re-inject (seed=%s): out of budget after %d/%d origins - the remaining %d were NOT restored: %v",
				seedKey, done, origins, origins-done, err)
			return
		}
		logWarn("state re-inject failed (seed=%s): %v", seedKey, err)
		return
	}
	logInfo("state re-injected (seed=%s): %s", seedKey, stateSummary(st))
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
