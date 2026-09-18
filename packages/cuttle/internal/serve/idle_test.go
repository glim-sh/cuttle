package serve

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestEstablishedOnPort(t *testing.T) {
	t.Parallel()
	const header = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	// 0x17C0 = 6080.
	listen4 := "   0: 00000000:17C0 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1\n"
	viewer4 := "   1: 020011AC:17C0 010011AC:D2F4 01 00000000:00000000 00:00000000 00000000     0        0 2 1\n"
	closing4 := "   2: 020011AC:17C0 010011AC:D2F6 06 00000000:00000000 00:00000000 00000000     0        0 0 3\n"
	outbound4 := "   3: 020011AC:D2F8 010011AC:17C0 01 00000000:00000000 00:00000000 00000000     0        0 4 1\n"
	cdp4 := "   4: 0100007F:240E 0100007F:A1B2 01 00000000:00000000 00:00000000 00000000     0        0 5 1\n"
	viewer6 := "   0: 0000000000000000FFFF0000020011AC:17C0 0000000000000000FFFF0000010011AC:D2F4 01 00000000:00000000 00:00000000 00000000     0        0 6 1\n"

	for _, tc := range []struct {
		name  string
		table string
		want  bool
	}{
		{"an attached viewer", header + listen4 + viewer4, true},
		{"an attached viewer over IPv6", header + viewer6, true},
		{"only the listening socket", header + listen4, false},
		{"a viewer that has gone (TIME_WAIT)", header + listen4 + closing4, false},
		{"an outbound connection TO the port is not a viewer", header + outbound4, false},
		{"traffic on another port", header + cdp4, false},
		{"an empty table", "", false},
		{"a garbled line", header + "   0: nonsense 01\n", false},
	} {
		if got := establishedOnPort(tc.table, defaultVNCPort); got != tc.want {
			t.Errorf("%s: establishedOnPort = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// idleSessionPool launches session mode's one browser - with nothing attached,
// the way `cuttle up`'s readiness probe leaves it - behind a switchable fake
// viewer.
func idleSessionPool(t *testing.T, timeout time.Duration) (*chromePool, *fakeLauncher, *atomic.Bool) {
	t.Helper()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{mode: modeSession, idleTimeout: timeout}, fl.toLauncher())
	viewer := &atomic.Bool{}
	pool.viewerAttached = viewer.Load
	if _, err := pool.getOrLaunch(context.Background(), connectRequest{}); err != nil {
		t.Fatal(err)
	}
	return pool, fl, viewer
}

func browserUp(pool *chromePool) bool {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	_, ok := pool.processes[reservedSeed]
	return ok
}

// awaitReap waits for the browser to be reaped and returns when it was noticed,
// or fails the test when it outlives within.
func awaitReap(t *testing.T, pool *chromePool, within time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !browserUp(pool) {
			return time.Now()
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the idle session browser was not closed within %s", within)
	return time.Time{}
}

// Each of the three things that count as using the session browser must hold it
// up for as long as it lasts, and the browser must then get a whole quiet
// timeout from the moment it lets go - never less.
func TestSessionIdleReapWaitsForEveryUser(t *testing.T) {
	t.Parallel()
	const timeout = 40 * time.Millisecond
	for _, tc := range []struct {
		name string
		hold func(*testing.T, *chromePool, *atomic.Bool) (letGo func())
	}{
		{"a held lease", func(t *testing.T, p *chromePool, _ *atomic.Bool) func() {
			t.Helper()
			l, err := p.leases.acquire(reservedSeed, "agent", "")
			if err != nil {
				t.Fatal(err)
			}
			return func() { p.leases.release(reservedSeed, l.token) }
		}},
		{"an attached CDP client", func(_ *testing.T, p *chromePool, _ *atomic.Bool) func() {
			p.connect(reservedSeed)
			return func() { p.disconnect(reservedSeed) }
		}},
		{"an open viewer", func(_ *testing.T, _ *chromePool, v *atomic.Bool) func() {
			v.Store(true)
			return func() { v.Store(false) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool, fl, viewer := idleSessionPool(t, timeout)
			letGo := tc.hold(t, pool, viewer)

			time.Sleep(5 * timeout)
			if !browserUp(pool) {
				t.Fatalf("closed the browser while %s was on it", tc.name)
			}

			freed := time.Now()
			letGo()
			reapedAt := awaitReap(t, pool, 2*time.Second)
			if quiet := reapedAt.Sub(freed); quiet < timeout {
				t.Errorf("closed %s after it let go, want at least the %s timeout", quiet, timeout)
			}
			// The reap drops the browser from the pool before it captures state and
			// signals the process, so the terminate lags what awaitReap saw.
			fl.mu.Lock()
			first := fl.procs[0]
			fl.mu.Unlock()
			select {
			case <-first.waitExit():
			case <-time.After(2 * time.Second):
				t.Error("the reaped browser was not terminated")
			}
		})
	}
}

// A browser nothing ever attached to still idles out: the clock starts at launch,
// not only at a last disconnect that may never come. It stays closed - the
// default seed's self-heal must not undo a reap - until the next verb brings it
// back.
func TestSessionIdleReapThenRelaunchOnNextVerb(t *testing.T) {
	t.Parallel()
	pool, fl, _ := idleSessionPool(t, 30*time.Millisecond)
	awaitReap(t, pool, 2*time.Second)

	time.Sleep(60 * time.Millisecond)
	if n := fl.launchCount(); n != 1 {
		t.Fatalf("the reaped browser relaunched on its own (launchCount=%d)", n)
	}
	if _, err := pool.getOrLaunch(context.Background(), connectRequest{}); err != nil {
		t.Fatal(err)
	}
	if n := fl.launchCount(); n != 2 || !browserUp(pool) {
		t.Fatalf("the next verb did not relaunch the browser (launchCount=%d, up=%v)", n, browserUp(pool))
	}
}

// Any activity restarts the whole timeout, not what was left of it. The viewer
// visit outlasts one poll (the poll is the timeout itself at this scale), which
// is the shortest visit the daemon promises to see.
func TestSessionIdleActivityRestartsTheClock(t *testing.T) {
	t.Parallel()
	const timeout = 80 * time.Millisecond
	pool, _, viewer := idleSessionPool(t, timeout)

	for _, touch := range []func(){
		func() { pool.connect(reservedSeed); pool.disconnect(reservedSeed) },
		func() { viewer.Store(true); time.Sleep(2 * timeout); viewer.Store(false) },
	} {
		time.Sleep(timeout / 2)
		if !browserUp(pool) {
			t.Fatal("closed before the timeout")
		}
		touch()
	}
	last := time.Now()
	if quiet := awaitReap(t, pool, 2*time.Second).Sub(last); quiet < timeout {
		t.Errorf("closed %s after the last activity, want at least %s", quiet, timeout)
	}
}

// A timer that fired just before a verb re-armed the clock must not reap the
// browser that verb was handed.
func TestIdleReapYieldsToARearmedClock(t *testing.T) {
	t.Parallel()
	pool, _, _ := idleSessionPool(t, time.Hour)
	if _, err := pool.getOrLaunch(context.Background(), connectRequest{}); err != nil {
		t.Fatal(err)
	}
	pool.idleReap(reservedSeed) // the stale fire
	if !browserUp(pool) {
		t.Fatal("a stale idle fire reaped a browser a verb had just re-armed")
	}
}

func TestSessionIdleTimeoutZeroIsOff(t *testing.T) {
	t.Parallel()
	pool, _, _ := idleSessionPool(t, 0)
	pool.connect(reservedSeed)
	pool.disconnect(reservedSeed)

	time.Sleep(60 * time.Millisecond)
	if !browserUp(pool) {
		t.Fatal("idle-timeout 0 must never close the session browser")
	}
}

// Pool mode keeps its one rule - the last CDP client leaving starts the clock -
// and nothing session mode added reaches it: a lease or a viewer on a pool seed
// holds nothing up, and a seed nothing ever attached to is not reaped.
func TestPoolIdleReapUnchanged(t *testing.T) {
	t.Parallel()
	const timeout = 30 * time.Millisecond
	for _, tc := range []struct {
		name       string
		attach     bool
		lease      bool
		viewer     bool
		wantReaped bool
	}{
		{"last client left", true, false, false, true},
		{"last client left, lease held", true, true, false, true},
		{"last client left, viewer open", true, false, true, true},
		{"never attached", false, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fl := &fakeLauncher{port: 5100}
			pool := newTestPool(t, serveConfig{idleTimeout: timeout}, fl.toLauncher())
			pool.viewerAttached = func() bool { return tc.viewer }
			if _, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"}); err != nil {
				t.Fatal(err)
			}
			if tc.lease {
				if _, err := pool.leases.acquire("s1", "agent", ""); err != nil {
					t.Fatal(err)
				}
			}
			if tc.attach {
				pool.connect("s1")
				pool.disconnect("s1")
			}

			time.Sleep(10 * timeout)
			pool.mu.Lock()
			_, up := pool.processes["s1"]
			pool.mu.Unlock()
			if up == tc.wantReaped {
				t.Fatalf("reaped=%v, want %v", !up, tc.wantReaped)
			}
		})
	}
}
