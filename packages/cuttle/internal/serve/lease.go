package serve

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// leaseTTL is how long a driving lease lives without a renew. A holder renews
// every third of it, so two missed heartbeats still leave it held, and a holder
// that died frees the browser within two minutes without anyone stepping in.
const leaseTTL = 120 * time.Second

// leaseOwnerMax caps the holder label. It is a human-readable name, echoed back
// in every refusal, not a payload.
const leaseOwnerMax = 128

var (
	errLeaseHeld    = errors.New("the browser session is being driven by another client")
	errLeaseLost    = errors.New("the session lease was taken over")
	errLeaseNoOwner = errors.New("a lease needs an owner label")
)

// lease is one driver's exclusive claim on a seed's browser. An empty token is a
// free slot: what a forced release leaves behind, remembering in owner who took
// the browser over so the evicted holder's next renew can say who.
type lease struct {
	owner    string
	token    string
	acquired time.Time
	expires  time.Time
}

func (l lease) live(now time.Time) bool { return l.token != "" && now.Before(l.expires) }

// leaseTable guards exclusive driving of each seed's browser. Two drivers on one
// page interleave clicks and navigations into nonsense, and a browser cannot run
// one profile twice, so serializing them here is the only place it can happen.
// Expiry is lazy - checked on access - so there is no goroutine to stop.
type leaseTable struct {
	mu   sync.Mutex
	held map[string]lease
	now  func() time.Time
}

func newLeaseTable() *leaseTable {
	return &leaseTable{held: map[string]lease{}, now: time.Now}
}

// acquire grants the seed's lease to owner, or renews it when token is the
// holder's. A free or expired slot is granted; a live foreign one is refused
// with errLeaseHeld and the holder. A renew (non-empty token) that finds its
// lease gone - force-released, not merely expired - fails with errLeaseLost,
// carrying who took it, instead of quietly re-granting over the takeover.
func (t *leaseTable) acquire(seed, owner, token string) (lease, error) {
	if owner == "" || len(owner) > leaseOwnerMax {
		return lease{}, errLeaseNoOwner
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	cur, ok := t.held[seed]
	if cur.live(now) && cur.token != token {
		return cur, errLeaseHeld
	}
	if token != "" && (!ok || cur.token != token) {
		return cur, errLeaseLost
	}
	if token == "" {
		token = newLeaseToken()
		cur.acquired = now
	}
	granted := lease{owner: owner, token: token, acquired: cur.acquired, expires: now.Add(leaseTTL)}
	t.held[seed] = granted
	return granted, nil
}

// release frees the seed's lease when token is the holder's. Anything else is a
// no-op, so a release is safe to repeat and can never free someone else's lease.
// The freed slot keeps the owner label, so a holder evicted by a takeover still
// learns who took over after the taker has come and gone.
func (t *leaseTable) release(seed, token string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur, ok := t.held[seed]; ok && token != "" && cur.token == token {
		t.held[seed] = lease{owner: cur.owner}
	}
}

// forceRelease is the takeover: it frees the seed's lease whoever holds it and
// leaves by in the slot, so the evicted holder learns who took over.
func (t *leaseTable) forceRelease(seed, by string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.held[seed] = lease{owner: by, acquired: t.now()}
}

// status returns the seed's live lease, if any.
func (t *leaseTable) status(seed string) (lease, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cur := t.held[seed]
	return cur, cur.live(t.now())
}

func newLeaseToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}

// leaseView is the wire shape of a lease. The token is only ever returned to
// the client that was granted it, never in a status or a refusal.
func (t *leaseTable) leaseView(l lease, withToken bool) map[string]any {
	v := map[string]any{"owner": l.owner, "age_seconds": 0, "ttl_seconds": int(leaseTTL.Seconds())}
	if !l.acquired.IsZero() {
		v["age_seconds"] = int(t.now().Sub(l.acquired).Seconds())
	}
	if withToken {
		v["token"] = l.token
	}
	return v
}

// handleLeaseAcquire grants or renews the driving lease: ?owner= names the
// holder, ?token= renews. 409 carries the holder and its age.
func (m *multiplexer) handleLeaseAcquire(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	seed, ok := m.requestSeed(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	leases := m.pool.leases
	got, err := leases.acquire(seed, q.Get("owner"), q.Get("token"))
	switch {
	case errors.Is(err, errLeaseNoOwner):
		writeJSON(w, http.StatusBadRequest, map[string]any{keyError: err.Error()})
	case err != nil:
		v := leases.leaseView(got, false)
		v[keyError] = err.Error()
		writeJSON(w, http.StatusConflict, v)
	default:
		writeJSON(w, http.StatusOK, leases.leaseView(got, true))
	}
}

// handleLeaseRelease frees the lease: ?token= releases your own (idempotent),
// ?force=true takes it from whoever holds it, recording ?owner= as the taker.
func (m *multiplexer) handleLeaseRelease(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	seed, ok := m.requestSeed(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if force, _ := strconv.ParseBool(q.Get("force")); force {
		m.pool.leases.forceRelease(seed, q.Get("owner"))
	} else {
		m.pool.leases.release(seed, q.Get("token"))
	}
	writeJSON(w, http.StatusOK, map[string]any{keyStatus: "ok"})
}

// handleLeaseStatus reports whether the browser is being driven, and by whom.
func (m *multiplexer) handleLeaseStatus(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	seed, ok := m.requestSeed(w, r)
	if !ok {
		return
	}
	cur, held := m.pool.leases.status(seed)
	if !held {
		writeJSON(w, http.StatusOK, map[string]any{"held": false})
		return
	}
	v := m.pool.leases.leaseView(cur, false)
	v["held"] = true
	writeJSON(w, http.StatusOK, v)
}
