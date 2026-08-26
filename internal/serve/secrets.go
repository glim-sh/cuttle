package serve

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Daemon-owned secrets. A driver types the sentinel {{cuttle:NAME}} and the
// substitution happens here, inside the CDP proxy, so the value never enters
// argv, a driver transcript, or an agent's context. The store is the memory half
// of that contract: values live only here, per seed, under a TTL, and are never
// mirrored to dataDir the way stateStore's snapshots are. Config (the --exec
// recipe that produced a value) is the host's half and lives in config.toml.
//
// The one shape worth reading twice: a TTL expiry CLEARS the value but KEEPS the
// entry. An entry with no value is a registration - the daemon knows the name
// exists without holding its secret - which is what lets the substitution path
// answer "run cuttle secret refresh NAME" instead of "unknown name". The daemon
// cannot read config.toml (it runs in a container, config.toml is a host file),
// so an explicit registration is its only way to tell those two apart.

const (
	// sentinelPrefix opens the substitution token. A text that IS exactly
	// sentinelPrefix + NAME + sentinelSuffix is a sentinel; the prefix appearing
	// anywhere else in a typed value is a hard error, never a literal to type.
	sentinelPrefix = "{{cuttle:"
	sentinelSuffix = "}}"

	// secretTTLDefault bounds how long a resolved value stays in daemon memory.
	// Short on purpose: a stale-value error naming `cuttle secret refresh` is the
	// expected path for anything time-bounded (a TOTP), not a failure.
	secretTTLDefault = 15 * time.Minute
	secretTTLMax     = 12 * time.Hour

	// allowLiteralTTL is how long an armed literal-fill exemption survives
	// unconsumed. An armed-and-forgotten token must not silently disarm the
	// credential-field refusal for the rest of the session.
	allowLiteralTTL = 60 * time.Second
)

// Sources a value can come from. The source decides what a stale-value error can
// suggest: only an "exec" entry has a recipe on the host to re-run.
const (
	sourceStdin = "stdin"
	sourceExec  = "exec"
)

// secretNamePattern is deliberately narrow: a sentinel is parsed out of typed
// text, so a name may never carry a brace, a colon, or whitespace. No dash
// either, which keeps the `allow-literal` route from ever colliding with a name.
var secretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func validSecretName(name string) bool { return secretNamePattern.MatchString(name) }

// secretEntry is one named secret for one seed. val is nil once the TTL has
// fired; the entry itself survives that (see the package comment).
type secretEntry struct {
	val    []byte
	timer  *time.Timer
	setAt  time.Time
	ttl    time.Duration
	origin string // where the value was first typed successfully (3.5); "" until then
	source string
}

func (e *secretEntry) live() bool { return e.val != nil }

// secretInfo is what the list API and the briefing may see: shape, never value.
type secretInfo struct {
	Name         string `json:"name"`
	Source       string `json:"source"`
	Live         bool   `json:"live"`
	Length       int    `json:"length"`
	AgeSeconds   int    `json:"age_seconds"`
	TTLRemaining int    `json:"ttl_remaining_seconds"`
	Origin       string `json:"origin,omitempty"`
}

// secretStatus is the three-way answer the substitution path needs, because
// "unknown name" and "expired value" call for completely different fixes.
type secretStatus int

const (
	secretUnknown secretStatus = iota // never registered on this daemon
	secretStale                       // registered, value gone (TTL fired, or never resolved)
	secretLive
)

// secretStore holds every seed's secrets. Mirrors stateStore's shape minus its
// persist half: nothing here ever reaches disk.
type secretStore struct {
	mu      sync.Mutex
	m       map[string]map[string]*secretEntry
	literal map[string]*time.Timer // seed -> armed single-use literal-fill exemption
}

func newSecretStore() *secretStore {
	return &secretStore{m: map[string]map[string]*secretEntry{}, literal: map[string]*time.Timer{}}
}

// put stores (or replaces) a value under a fresh TTL and returns the entry's
// source. The expiry timer closure captures the seed and name ONLY - capturing
// the buffer would pin the secret in memory for as long as the timer lives.
func (s *secretStore) put(seed, name string, val []byte, source string, ttl time.Duration) {
	if ttl <= 0 || ttl > secretTTLMax {
		ttl = secretTTLDefault
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket := s.m[seed]
	if bucket == nil {
		bucket = map[string]*secretEntry{}
		s.m[seed] = bucket
	}
	e := bucket[name]
	if e == nil {
		e = &secretEntry{}
		bucket[name] = e
	}
	if e.timer != nil {
		e.timer.Stop()
	}
	clear(e.val)
	e.val, e.source, e.setAt, e.ttl = val, source, time.Now(), ttl
	e.timer = time.AfterFunc(ttl, func() { s.expire(seed, name) })
}

// expire zeroes a value at its TTL and keeps the entry as a registration.
func (s *secretStore) expire(seed, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[seed][name]
	if e == nil {
		return
	}
	clear(e.val)
	e.val, e.timer = nil, nil
}

// take returns a COPY of a live value plus the entry's source and status. The
// copy is what makes the TTL safe against an in-flight type: the expiry timer
// zeroes the store's buffer, and a typing loop ranging over the store's own
// backing array would type the tail as NULs. Callers clear their copy.
func (s *secretStore) take(seed, name string) ([]byte, string, secretStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[seed][name]
	switch {
	case e == nil:
		return nil, "", secretUnknown
	case !e.live():
		return nil, e.source, secretStale
	default:
		return slices.Clone(e.val), e.source, secretLive
	}
}

// noteOrigin records where a secret was first typed successfully and reports the
// origin it was bound to before (empty on the first use). Derived, never
// configured: the check teaches only on the anomaly.
func (s *secretStore) noteOrigin(seed, name, origin string) string {
	if origin == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[seed][name]
	if e == nil {
		return ""
	}
	prev := e.origin
	if prev == "" {
		e.origin = origin
	}
	return prev
}

// names lists a seed's registered names, sorted. Used by the briefing: a
// substitution mechanism the model is never told about does not get used.
func (s *secretStore) names(seed string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.m[seed]))
	for name := range s.m[seed] {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// list reports every entry's shape for one seed. It returns no value under any
// flag - that invariant is the whole point of the verb.
func (s *secretStore) list(seed string) []secretInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := make([]secretInfo, 0, len(s.m[seed]))
	for name, e := range s.m[seed] {
		info := secretInfo{
			Name: name, Source: e.source, Live: e.live(),
			AgeSeconds: int(now.Sub(e.setAt).Seconds()), Origin: e.origin,
		}
		if e.live() {
			info.Length = len(e.val)
			if remaining := e.ttl - now.Sub(e.setAt); remaining > 0 {
				info.TTLRemaining = int(remaining.Seconds())
			}
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b secretInfo) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// remove deletes a name outright - the only thing that does. A TTL expiry keeps
// the entry so the name stays known; rm makes it unknown again.
func (s *secretStore) remove(seed, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[seed][name]
	if e == nil {
		return false
	}
	if e.timer != nil {
		e.timer.Stop()
	}
	clear(e.val)
	delete(s.m[seed], name)
	return true
}

// armLiteral stores a seed's single-use exemption from the credential-field
// refusal. Single-use IS the semantics - there is deliberately no persistent
// variant - and it expires on its own so a forgotten token cannot disarm the
// refusal for the rest of the session.
func (s *secretStore) armLiteral(seed string, ttl time.Duration) time.Duration {
	if ttl <= 0 || ttl > secretTTLMax {
		ttl = allowLiteralTTL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.literal[seed]; t != nil {
		t.Stop()
	}
	s.literal[seed] = time.AfterFunc(ttl, func() { s.disarmLiteral(seed) })
	return ttl
}

func (s *secretStore) disarmLiteral(seed string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.literal[seed]; t != nil {
		t.Stop()
		delete(s.literal, seed)
	}
}

// takeLiteral consumes an armed exemption, reporting whether it got one. Taken
// under the store mutex, so two refusable fills arriving together resolve by
// exactly one being allowed and the other refused - never by queueing a fill to
// wait for a token.
func (s *secretStore) takeLiteral(seed string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.literal[seed]
	if t == nil {
		return false
	}
	t.Stop()
	delete(s.literal, seed)
	return true
}

func (s *secretStore) literalArmed(seed string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.literal[seed] != nil
}

// ---------------------------------------------------------------------------
// HTTP surface
// ---------------------------------------------------------------------------

// secretBodyLimit caps a PUT body. A secret is a credential, not a payload.
const secretBodyLimit = 64 << 10

// secretSeed resolves which seed's bucket a request addresses. Per-seed does NOT
// mean a /profile/{seed}/... route: in session mode - the primary mode - a
// non-empty seed is a 400 and the browser lives under the reserved key, so a
// {seed} path segment would be unusable exactly where it matters most. Every
// secret route follows the downloads rule instead: an optional ?fingerprint=,
// resolved by seedKeyFor, behind rejectUntrustedLoopback.
func (m *multiplexer) secretSeed(w http.ResponseWriter, r *http.Request) (string, bool) {
	seedKey, lerr := m.pool.seedKeyFor(r.URL.Query().Get(keyFingerprint))
	if lerr != nil {
		writeLaunchError(w, lerr)
		return "", false
	}
	return seedKey, true
}

// secretName validates the {name} path segment against the sentinel grammar, so
// a name can never be something no sentinel could address.
func secretName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !validSecretName(name) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			keyError: "invalid secret name: letters, digits and underscore only, starting with a letter or underscore",
		})
		return "", false
	}
	return name, true
}

// handleSecretList reports what the daemon holds for a seed - names, sources,
// ages, lengths - and never a value under any flag.
func (m *multiplexer) handleSecretList(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	seed, ok := m.secretSeed(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"secrets":       m.pool.secrets.list(seed),
		"allow_literal": m.pool.secrets.literalArmed(seed),
	})
}

// handleSecretPut stores a value under a fresh TTL. The value rides the request
// body, never a query param or a path segment, so it cannot land in an access
// log or a process listing.
func (m *multiplexer) handleSecretPut(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	seed, ok := m.secretSeed(w, r)
	if !ok {
		return
	}
	name, ok := secretName(w, r)
	if !ok {
		return
	}
	var body struct {
		Value      string `json:"value"`
		Source     string `json:"source"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, secretBodyLimit)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{keyError: "invalid secret body"})
		return
	}
	if body.Value == "" {
		// An empty value would otherwise register a name whose substitution types
		// nothing - Playwright's falsy-secret bug, which types the NAME instead.
		writeJSON(w, http.StatusBadRequest, map[string]any{keyError: "empty secret value"})
		return
	}
	source := body.Source
	if source == "" {
		source = sourceStdin
	}
	ttl := time.Duration(body.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = secretTTLDefault
	}
	m.pool.secrets.put(seed, name, []byte(body.Value), source, ttl)
	logInfo("secrets: %s registered for seed=%s (source=%s, %d bytes, ttl %s)", name, seed, source, len(body.Value), ttl)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "length": len(body.Value), "ttl_seconds": int(ttl.Seconds()),
	})
}

// handleSecretDelete forgets a name entirely - the only thing that does. A TTL
// expiry keeps the registration so the sentinel error can say "refresh it".
func (m *multiplexer) handleSecretDelete(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	seed, ok := m.secretSeed(w, r)
	if !ok {
		return
	}
	name, ok := secretName(w, r)
	if !ok {
		return
	}
	if !m.pool.secrets.remove(seed, name) {
		writeJSON(w, http.StatusNotFound, map[string]any{keyError: "no such secret"})
		return
	}
	logInfo("secrets: %s removed for seed=%s", name, seed)
	writeJSON(w, http.StatusOK, map[string]any{keyStatus: "ok", "name": name})
}

// handleSecretAllowLiteral arms the single-use exemption from the
// credential-field refusal.
func (m *multiplexer) handleSecretAllowLiteral(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	seed, ok := m.secretSeed(w, r)
	if !ok {
		return
	}
	var body struct {
		TTLSeconds int `json:"ttl_seconds"`
	}
	// A body is optional here; a malformed one falls back to the default TTL.
	_ = json.NewDecoder(io.LimitReader(r.Body, secretBodyLimit)).Decode(&body)
	ttl := m.pool.secrets.armLiteral(seed, time.Duration(body.TTLSeconds)*time.Second)
	logWarn("secrets: literal fills into credential fields allowed once for seed=%s (expires in %s)", seed, ttl)
	writeJSON(w, http.StatusOK, map[string]any{keyStatus: "armed", "ttl_seconds": int(ttl.Seconds())})
}
