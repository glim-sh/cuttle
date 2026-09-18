package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The lease is the only thing standing between two drivers on one page, so its
// guarantees are asserted here under contention rather than in sequence: exactly
// one holder at a time, a force-released token that can never come back, and no
// seed that can be freed or blocked by another seed's traffic. Every test here
// is meant to be run under -race.

// stressClock is a hand-moved clock many goroutines can read at once. The
// sequential tests move a plain time.Time, which races the moment a second
// goroutine reads it.
type stressClock struct{ nanos atomic.Int64 }

func newStressClock() *stressClock {
	c := &stressClock{}
	c.nanos.Store(time.Unix(1_700_000_000, 0).UnixNano())
	return c
}

func (c *stressClock) now() time.Time          { return time.Unix(0, c.nanos.Load()) }
func (c *stressClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

func (c *stressClock) table() *leaseTable {
	t := newLeaseTable()
	t.now = c.now
	return t
}

// waitFor joins wg, failing instead of hanging if the table ever deadlocks.
func waitFor(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("lease operations deadlocked")
	}
}

func TestLeaseStressOneWinnerPerRound(t *testing.T) {
	t.Parallel()
	const (
		seed    = "s"
		workers = 200
		rounds  = 20
	)
	table := newLeaseTable()
	type result struct {
		owner string
		got   lease
		err   error
	}
	for round := range rounds {
		results := make([]result, workers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range workers {
			wg.Go(func() {
				owner := fmt.Sprintf("w%d", i)
				<-start
				got, err := table.acquire(seed, owner, "")
				results[i] = result{owner: owner, got: got, err: err}
			})
		}
		close(start)
		waitFor(t, &wg)

		var winner result
		wins := 0
		for _, r := range results {
			if r.err == nil {
				wins++
				winner = r
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: %d of %d acquires won, want exactly 1", round, wins, workers)
		}
		if winner.got.owner != winner.owner || len(winner.got.token) != 32 {
			t.Fatalf("round %d: grant %+v does not match its winner %q", round, winner.got, winner.owner)
		}
		for _, r := range results {
			if r.err == nil {
				continue
			}
			if !errors.Is(r.err, errLeaseHeld) {
				t.Fatalf("round %d: %s got %v, want errLeaseHeld", round, r.owner, r.err)
			}
			if r.got.owner != winner.owner {
				t.Fatalf("round %d: %s was refused naming %q, want the winner %q", round, r.owner, r.got.owner, winner.owner)
			}
		}
		cur, held := table.status(seed)
		if !held || cur.token != winner.got.token {
			t.Fatalf("round %d: table holds %+v, want the winner's lease", round, cur)
		}
		table.release(seed, winner.got.token)
		if _, held := table.status(seed); held {
			t.Fatalf("round %d: the winner's release left the slot held", round)
		}
	}
}

func TestLeaseStressForcedTokenNeverRenewsAgain(t *testing.T) {
	t.Parallel()
	const (
		seed   = "s"
		rounds = 200
	)
	table := newLeaseTable()
	for round := range rounds {
		held, err := table.acquire(seed, "a", "")
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		var (
			dead        atomic.Bool
			resurrected atomic.Bool
			firstErr    atomic.Pointer[error]
		)
		ready, stop := make(chan struct{}), make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() { // the holder, heartbeating as fast as it can
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				_, rerr := table.acquire(seed, "a", held.token)
				if n == 0 {
					close(ready)
				}
				if rerr == nil {
					if dead.Load() {
						resurrected.Store(true)
						return
					}
					continue
				}
				if !dead.Swap(true) {
					firstErr.Store(&rerr)
				}
			}
		})
		<-ready // the force lands while renews are in flight, not before the first
		table.forceRelease(seed, "pw")
		// Keep renewing a while after the force, so a resurrection has time to show.
		deadline := time.Now().Add(10 * time.Second)
		for !dead.Load() && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		noticed := dead.Load()
		for range 100 {
			runtime.Gosched()
		}
		close(stop)
		waitFor(t, &wg)

		if !noticed {
			t.Fatalf("round %d: the evicted holder kept renewing; the takeover never took", round)
		}
		if resurrected.Load() {
			t.Fatalf("round %d: a token renewed again after the daemon had refused it: a takeover was undone", round)
		}
		if e := firstErr.Load(); e == nil || !errors.Is(*e, errLeaseLost) {
			t.Fatalf("round %d: the renew over a forced slot failed with %v, want errLeaseLost", round, e)
		}
		got, err := table.acquire(seed, "a", held.token)
		if !errors.Is(err, errLeaseLost) || got.owner != "pw" {
			t.Fatalf("round %d: renew after the force: %+v err=%v, want errLeaseLost naming pw", round, got, err)
		}
	}
}

func TestLeaseStressCompetingTakeovers(t *testing.T) {
	t.Parallel()
	const (
		seed    = "s"
		parties = 8
		rounds  = 50
	)
	table := newLeaseTable()
	for round := range rounds {
		granted := make([]lease, parties)
		errs := make([]error, parties)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range parties {
			wg.Go(func() {
				me := fmt.Sprintf("p%d", i)
				<-start
				table.forceRelease(seed, me)
				granted[i], errs[i] = table.acquire(seed, me, "")
			})
		}
		close(start)
		waitFor(t, &wg)

		cur, held := table.status(seed)
		if !held {
			t.Fatalf("round %d: every takeover ended with nobody holding the lease", round)
		}
		holders := 0
		for i := range parties {
			me := fmt.Sprintf("p%d", i)
			if errs[i] != nil {
				if !errors.Is(errs[i], errLeaseHeld) {
					t.Fatalf("round %d: %s got %v, want errLeaseHeld", round, me, errs[i])
				}
				continue
			}
			if granted[i].token == cur.token {
				holders++
				if cur.owner != me {
					t.Fatalf("round %d: the live lease has token of %s but owner %q", round, me, cur.owner)
				}
				continue
			}
			// This party was granted the lease and then evicted by a later
			// takeover: its token must be dead, not merely shadowed.
			if _, err := table.acquire(seed, me, granted[i].token); err == nil {
				t.Fatalf("round %d: %s still renews after being taken over", round, me)
			}
		}
		if holders != 1 {
			t.Fatalf("round %d: %d parties hold the lease, want exactly 1", round, holders)
		}
		table.forceRelease(seed, "reset")
	}
}

func TestLeaseStressSeedsDoNotInterfere(t *testing.T) {
	t.Parallel()
	const (
		seeds          = 50
		workersPerSeed = 20
		iterations     = 20
	)
	table := newLeaseTable()
	bad := make(chan string, 64)
	var wg sync.WaitGroup
	for s := range seeds {
		seed := fmt.Sprintf("seed%02d", s)
		for w := range workersPerSeed {
			owner := fmt.Sprintf("%s/w%d", seed, w)
			wg.Go(func() {
				for range iterations {
					got, err := table.acquire(seed, owner, "")
					switch {
					case errors.Is(err, errLeaseHeld):
						if !strings.HasPrefix(got.owner, seed+"/") {
							report(bad, "%s was refused by %q, which drives another seed", owner, got.owner)
						}
					case err != nil:
						report(bad, "%s got %v", owner, err)
					default:
						if got.owner != owner {
							report(bad, "%s was granted a lease owned by %q", owner, got.owner)
						}
						// Nothing here forces, so a held lease must stay mine.
						if cur, held := table.status(seed); !held || cur.token != got.token {
							report(bad, "%s holds the lease but the table says %+v (held=%v)", owner, cur, held)
						}
						table.release(seed, got.token)
					}
				}
			})
		}
	}
	waitFor(t, &wg)
	close(bad)
	for msg := range bad {
		t.Error(msg)
	}
}

// report records a failure from a worker goroutine, where t.Fatalf is illegal,
// and drops it when the buffer is full - the first ones are the informative ones.
func report(bad chan<- string, format string, args ...any) {
	select {
	case bad <- fmt.Sprintf(format, args...):
	default:
	}
}

func TestLeaseStressExpiryBoundary(t *testing.T) {
	t.Parallel()
	const (
		seed       = "s"
		iterations = 200
	)
	tests := []struct {
		name string
		skew time.Duration
		// wantRenew, when set, is the only legal outcome; at and past expiry
		// either side may win, but never both.
		wantRenew *bool
	}{
		{name: "a nanosecond before expiry the holder always wins", skew: -time.Nanosecond, wantRenew: new(true)},
		{name: "at expiry exactly one of the two wins", skew: 0},
		{name: "past expiry exactly one of the two wins", skew: time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for iter := range iterations {
				clock := newStressClock()
				table := clock.table()
				a, err := table.acquire(seed, "a", "")
				if err != nil {
					t.Fatal(err)
				}
				clock.advance(leaseTTL + tt.skew)

				var (
					renewed, taken lease
					renewErr       error
					takeErr        error
					wg             sync.WaitGroup
				)
				start := make(chan struct{})
				wg.Add(2)
				go func() { defer wg.Done(); <-start; renewed, renewErr = table.acquire(seed, "a", a.token) }()
				go func() { defer wg.Done(); <-start; taken, takeErr = table.acquire(seed, "b", "") }()
				close(start)
				waitFor(t, &wg)

				renewOK, takeOK := renewErr == nil, takeErr == nil
				if renewOK == takeOK {
					t.Fatalf("iter %d: renew(err=%v) and acquire(err=%v) must not both %s", iter, renewErr, takeErr,
						map[bool]string{true: "win", false: "lose"}[renewOK])
				}
				if tt.wantRenew != nil && renewOK != *tt.wantRenew {
					t.Fatalf("iter %d: renewed=%v, want %v (renewErr=%v takeErr=%v)", iter, renewOK, *tt.wantRenew, renewErr, takeErr)
				}
				cur, held := table.status(seed)
				if !held {
					t.Fatalf("iter %d: nobody holds the lease after the race", iter)
				}
				switch {
				case renewOK:
					if cur.token != renewed.token || cur.owner != "a" || renewed.token != a.token {
						t.Fatalf("iter %d: the renewal should have kept a's token: table=%+v renewed=%+v", iter, cur, renewed)
					}
					if !errors.Is(takeErr, errLeaseHeld) || taken.owner != "a" {
						t.Fatalf("iter %d: b should be refused naming a: %+v err=%v", iter, taken, takeErr)
					}
				default:
					if cur.token != taken.token || cur.owner != "b" {
						t.Fatalf("iter %d: b won but the table holds %+v", iter, cur)
					}
					if !errors.Is(renewErr, errLeaseHeld) || renewed.owner != "b" {
						t.Fatalf("iter %d: a's renew should be refused naming b: %+v err=%v", iter, renewed, renewErr)
					}
				}
			}
		})
	}
}

//go:fix inline
func TestLeaseStressStaleReleasesCannotFreeTheHolder(t *testing.T) {
	t.Parallel()
	const (
		seed       = "s"
		releasers  = 32
		iterations = 200
	)
	clock := newStressClock()
	table := clock.table()
	// A previous holder whose lease lapsed: the most plausible stale token there is.
	old, err := table.acquire(seed, "old", "")
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(leaseTTL)
	held, err := table.acquire(seed, "a", "")
	if err != nil {
		t.Fatal(err)
	}
	stale := []string{"", old.token, "deadbeef", strings.Repeat("0", 32), newLeaseToken()}

	bad := make(chan string, 64)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range releasers {
		wg.Go(func() {
			for range iterations {
				for _, tok := range stale {
					table.release(seed, tok)
				}
			}
		})
	}
	wg.Add(2)
	go func() { // the holder, heartbeating through the storm
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, rerr := table.acquire(seed, "a", held.token)
			if rerr != nil || got.token != held.token {
				report(bad, "the holder's renew failed under stale releases: %+v err=%v", got, rerr)
				return
			}
		}
	}()
	go func() { // and an onlooker, who must never see the browser go free
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if cur, ok := table.status(seed); !ok || cur.owner != "a" || cur.token != held.token {
				report(bad, "a stale release freed the live lease: %+v held=%v", cur, ok)
				return
			}
		}
	}()

	go func() {
		// The releasers finish on their own; the two watchers run until told.
		time.Sleep(50 * time.Millisecond)
		close(stop)
	}()
	waitFor(t, &wg)
	close(bad)
	for msg := range bad {
		t.Error(msg)
	}
	if cur, ok := table.status(seed); !ok || cur.token != held.token {
		t.Fatalf("after the storm the lease is %+v (held=%v), want a's", cur, ok)
	}
}

// ---------------------------------------------------------------------------
// The same guarantees over HTTP
// ---------------------------------------------------------------------------

// leaseStressMux is the lease routes on the real clock, so many goroutines can
// drive them at once; leaseMux's hand-moved clock cannot be read concurrently.
func leaseStressMux(t *testing.T, mode serveMode) http.Handler {
	t.Helper()
	pool := newTestPool(t, serveConfig{mode: mode}, (&fakeLauncher{port: 5100}).toLauncher())
	m := &multiplexer{pool: pool, port: 9222}
	return m.routes()
}

var errLeaseNotJSON = errors.New("the lease route answered with something other than JSON")

// leaseCall is leaseDo without the t.Fatalf, so a worker goroutine may call it.
func leaseCall(h http.Handler, method, target string) (int, map[string]any, error) {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "127.0.0.1:9222"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		return rec.Code, nil, fmt.Errorf("%w: %s %s: %q", errLeaseNotJSON, method, target, rec.Body.String())
	}
	return rec.Code, body, nil
}

func TestLeaseHTTPStress(t *testing.T) {
	t.Parallel()
	const (
		seeds      = 8
		workers    = 64
		iterations = 20
	)
	h := leaseStressMux(t, modePool)
	bad := make(chan string, 64)

	var mu sync.Mutex
	seen := map[string]string{} // token -> the owner it was granted to

	var wg sync.WaitGroup
	for i := range workers {
		seed := fmt.Sprintf("s%d", i%seeds)
		owner := fmt.Sprintf("%s-w%d", seed, i)
		q := "?fingerprint=" + seed
		wg.Go(func() {
			for range iterations {
				switch i % 4 {
				case 3: // a reader: status must never carry a token
					code, body, err := leaseCall(h, http.MethodGet, "/lease"+q)
					if err != nil || code != http.StatusOK {
						report(bad, "status: %d %v %v", code, body, err)
						continue
					}
					if _, leaked := body["token"]; leaked {
						report(bad, "status leaked a token: %v", body)
					}
				case 2: // a stale release, which must free nobody
					if code, body, err := leaseCall(h, http.MethodDelete, "/lease"+q+"&token="+strings.Repeat("f", 32)); err != nil || code != http.StatusOK {
						report(bad, "stale release: %d %v %v", code, body, err)
					}
				default: // a driver
					code, body, err := leaseCall(h, http.MethodPost, "/lease"+q+"&owner="+owner)
					if err != nil {
						report(bad, "acquire: %v", err)
						continue
					}
					switch code {
					case http.StatusConflict:
						holder, _ := body["owner"].(string)
						if !strings.HasPrefix(holder, seed+"-") {
							report(bad, "%s refused by %q, which drives another seed", owner, holder)
						}
						if _, leaked := body["token"]; leaked {
							report(bad, "a refusal leaked the holder's token: %v", body)
						}
					case http.StatusOK:
						token, _ := body["token"].(string)
						if len(token) != 32 {
							report(bad, "grant without a token: %v", body)
							continue
						}
						mu.Lock()
						prev, dup := seen[token]
						seen[token] = owner
						mu.Unlock()
						if dup {
							report(bad, "token %s was granted twice: to %s and %s", token, prev, owner)
						}
						// Nobody in this storm forces, so while I hold the
						// lease a renew must be granted and the status must
						// name me. Two live grants would break both.
						if c, b, err := leaseCall(h, http.MethodPost, "/lease"+q+"&owner="+owner+"&token="+token); err != nil || c != http.StatusOK {
							report(bad, "%s could not renew a lease it holds: %d %v %v", owner, c, b, err)
						}
						if c, b, err := leaseCall(h, http.MethodGet, "/lease"+q); err != nil || c != http.StatusOK || b["held"] != true || b["owner"] != owner {
							report(bad, "%s holds the lease but status says %d %v %v", owner, c, b, err)
						}
						if c, b, err := leaseCall(h, http.MethodDelete, "/lease"+q+"&token="+token); err != nil || c != http.StatusOK {
							report(bad, "release: %d %v %v", c, b, err)
						}
					default:
						report(bad, "acquire: unexpected %d %v", code, body)
					}
				}
			}
		})
	}
	waitFor(t, &wg)
	close(bad)
	for msg := range bad {
		t.Error(msg)
	}
}

func TestLeaseHTTPStressCompetingTakeovers(t *testing.T) {
	t.Parallel()
	const (
		takers = 8
		rounds = 20
	)
	h := leaseStressMux(t, modeSession)
	bad := make(chan string, 64)
	for round := range rounds {
		tokens := make([]string, takers)
		codes := make([]int, takers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range takers {
			wg.Go(func() {
				owner := fmt.Sprintf("pw%d", i)
				<-start
				if code, body, err := leaseCall(h, http.MethodDelete, "/lease?force=true&owner="+owner); err != nil || code != http.StatusOK {
					report(bad, "force: %d %v %v", code, body, err)
					return
				}
				code, body, err := leaseCall(h, http.MethodPost, "/lease?owner="+owner)
				if err != nil {
					report(bad, "acquire: %v", err)
					return
				}
				codes[i] = code
				tokens[i], _ = body["token"].(string)
			})
		}
		close(start)
		waitFor(t, &wg)

		// Exactly one taker ends up driving: every other token is refused.
		holders := 0
		for i, code := range codes {
			if code != http.StatusOK {
				if code != http.StatusConflict {
					t.Fatalf("round %d: taker %d got %d", round, i, code)
				}
				continue
			}
			c, body, err := leaseCall(h, http.MethodPost, fmt.Sprintf("/lease?owner=pw%d&token=%s", i, tokens[i]))
			if err != nil {
				t.Fatal(err)
			}
			if c == http.StatusOK {
				holders++
				continue
			}
			if c != http.StatusConflict {
				t.Fatalf("round %d: taker %d renew got %d %v", round, i, c, body)
			}
		}
		if holders != 1 {
			t.Fatalf("round %d: %d takers still hold a valid lease, want exactly 1", round, holders)
		}
	}
	close(bad)
	for msg := range bad {
		t.Error(msg)
	}
}
