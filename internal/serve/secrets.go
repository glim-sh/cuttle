package serve

import (
	"context"
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
	// unconsumed, and allowLiteralMax is the longest anyone may ask for. Both are
	// short and deliberately unrelated to a secret's TTL: an armed-and-forgotten
	// token must not silently disarm the credential-field refusal for the rest of
	// the session, which an hours-long one would.
	allowLiteralTTL = 60 * time.Second
	allowLiteralMax = 10 * time.Minute
)

// Sources a value can come from. The source decides what a stale-value error can
// suggest: only an "exec" entry has a recipe on the host to re-run.
const (
	sourceStdin   = "stdin"
	sourceExec    = "exec"
	sourceCapture = "capture"
	sourcePrompt  = "prompt"
)

// knownSources is what a PUT may claim. The source decides what a stale-value
// error tells the agent to run, so an unrecognized one would produce advice that
// cannot work.
var knownSources = []string{sourceStdin, sourceExec, sourceCapture, sourcePrompt}

// captureSelectorTimeout bounds one page read: attach, build a world, evaluate.
const captureSelectorTimeout = 10 * time.Second

// JSON field names shared by the secret routes and the CDP payloads they build.
const (
	keyName   = "name"
	keyValue  = "value"
	keyLength = "length"
	keyTTL    = "ttl_seconds"
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
	origin string // where the value was first typed successfully; "" until then
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
	// mu also guards the masker's cache below. NEVER log while holding it: the log
	// handler masks through this same store, so a line written under the lock
	// would deadlock on itself.
	mu      sync.Mutex
	m       map[string]map[string]*secretEntry
	literal map[string]*literalToken // seed -> armed single-use literal-fill exemption

	// version counts changes to the held values; the masker rebuilds its replacer
	// only when it has fallen behind, so an unchanged store costs one comparison
	// per log line rather than a rebuild.
	version     uint64
	maskVersion uint64
	mask        *strings.Replacer
}

// literalToken is one armed exemption. It carries its own deadline because the
// timer is not the gate: a firing timer and a consuming fill can both take the
// mutex, and presence in the map alone would let an expired token through.
type literalToken struct {
	expires time.Time
	timer   *time.Timer
}

func newSecretStore() *secretStore {
	return &secretStore{m: map[string]map[string]*secretEntry{}, literal: map[string]*literalToken{}}
}

// put stores (or replaces) a value under a fresh TTL and returns the entry's
// source. The expiry timer closure captures the seed and name ONLY - capturing
// the buffer would pin the secret in memory for as long as the timer lives.
func (s *secretStore) put(seed, name string, val []byte, source string, ttl time.Duration) {
	// The one place TTL policy lives. Over-max CLAMPS rather than resetting: a
	// `--ttl 24h` that silently became 15 minutes would look like the value had
	// expired for no reason.
	switch {
	case ttl <= 0:
		ttl = secretTTLDefault
	case ttl > secretTTLMax:
		ttl = secretTTLMax
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
	s.version++
}

// ttlOf reports the TTL an entry ended up with, so a caller can print what was
// actually stored rather than what it asked for.
func (s *secretStore) ttlOf(seed, name string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.m[seed][name]; e != nil {
		return e.ttl
	}
	return 0
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
	s.version++
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
	s.version++
	return true
}

// dropSeed forgets everything held for one seed. Called when its browser is
// reaped: the profile dir is gone, so a value that outlived it would sit in
// daemon memory for up to its TTL, belonging to a browser that no longer exists.
func (s *secretStore) dropSeed(seed string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.m[seed] {
		if e.timer != nil {
			e.timer.Stop()
		}
		clear(e.val)
	}
	delete(s.m, seed)
	if tok := s.literal[seed]; tok != nil {
		tok.timer.Stop()
		delete(s.literal, seed)
	}
	s.version++
}

// armLiteral stores a seed's single-use exemption from the credential-field
// refusal. Single-use IS the semantics - there is deliberately no persistent
// variant - and it expires on its own so a forgotten token cannot disarm the
// refusal for the rest of the session.
func (s *secretStore) armLiteral(seed string, ttl time.Duration) time.Duration {
	switch {
	case ttl <= 0:
		ttl = allowLiteralTTL
	case ttl > allowLiteralMax:
		ttl = allowLiteralMax
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev := s.literal[seed]; prev != nil {
		prev.timer.Stop()
	}
	tok := &literalToken{expires: time.Now().Add(ttl)}
	tok.timer = time.AfterFunc(ttl, func() { s.disarmLiteral(seed, tok) })
	s.literal[seed] = tok
	return ttl
}

// disarmLiteral drops a token only if it is still THE armed one. A timer that
// fires after its token was replaced would otherwise delete the replacement -
// re-arming would silently disarm.
func (s *secretStore) disarmLiteral(seed string, tok *literalToken) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.literal[seed] == tok {
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
	tok := s.literal[seed]
	if tok == nil {
		return false
	}
	tok.timer.Stop()
	delete(s.literal, seed)
	// The deadline, not the map entry, is the gate: the expiry timer may not have
	// won the mutex yet.
	return time.Now().Before(tok.expires)
}

func (s *secretStore) literalArmed(seed string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok := s.literal[seed]
	return tok != nil && time.Now().Before(tok.expires)
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
	if !slices.Contains(knownSources, source) {
		writeJSON(w, http.StatusBadRequest, map[string]any{keyError: "unknown secret source"})
		return
	}
	ttl := time.Duration(body.TTLSeconds) * time.Second
	m.pool.secrets.put(seed, name, []byte(body.Value), source, ttl)
	held := m.pool.secrets.ttlOf(seed, name)
	logInfo("secrets: %s registered for seed=%s (source=%s, %d bytes, ttl %s)", name, seed, source, len(body.Value), held)
	writeJSON(w, http.StatusOK, map[string]any{
		keyName: name, keyLength: len(body.Value), keyTTL: int(held.Seconds()),
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
	writeJSON(w, http.StatusOK, map[string]any{keyStatus: "ok", keyName: name})
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
	writeJSON(w, http.StatusOK, map[string]any{keyStatus: "armed", keyTTL: int(ttl.Seconds())})
}

// handleSecretCapture reads a value out of the page and either keeps it (the
// default: it lands in the store and the route answers with a length, so the
// value never leaves the daemon at all) or returns it for the CLI to pipe into a
// sink it owns.
//
// This is the one place cuttle resolves a selector and reads a DOM node - a
// driver's job everywhere else in this codebase. The boundary-clean alternative
// works (`playwright-cli eval 'el => el.value' e5 | cuttle secret set NAME
// --stdin`, and SKILL.md documents it), but its failure mode is a value that has
// already escaped into driver stdout, which the transport cannot take back. That
// reason is specific to a credential and licenses no second exception.
func (m *multiplexer) handleSecretCapture(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	seed, inst := m.runningSeedInstance(w, r)
	if inst == nil {
		return
	}
	name, ok := secretName(w, r)
	if !ok {
		return
	}
	var body struct {
		Selector  string `json:"selector"`
		Clipboard bool   `json:"clipboard"`
		Return    bool   `json:"return"`
		TTL       int    `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, secretBodyLimit)).Decode(&body); err != nil ||
		(body.Selector == "" && !body.Clipboard) {
		writeJSON(w, http.StatusBadRequest, map[string]any{keyError: "a selector or the clipboard source is required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), captureSelectorTimeout)
	defer cancel()
	source := body.Selector
	var value []byte
	var err error
	if body.Clipboard {
		source = "clipboard"
		value, err = captureClipboard(ctx, inst.cdpPort)
	} else {
		value, err = captureSelector(ctx, inst.cdpPort, body.Selector)
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{keyError: err.Error()})
		return
	}
	defer clear(value)

	if body.Return {
		// The only path that hands a captured value back, and only because the
		// sinks that need it - a file, a command's stdin - live on the host. It is
		// also the only capture the daemon does not keep, so it says so: a value
		// leaving here should never be the one event with no line about it.
		logInfo("secrets: %s captured from %s for seed=%s (%d bytes) and returned to the caller's sink - not stored",
			name, source, seed, len(value))
		writeJSON(w, http.StatusOK, map[string]any{keyName: name, keyLength: len(value), keyValue: string(value)})
		return
	}
	m.pool.secrets.put(seed, name, slices.Clone(value), sourceCapture, time.Duration(body.TTL)*time.Second)
	held := m.pool.secrets.ttlOf(seed, name)
	logInfo("secrets: %s captured from %s for seed=%s (%d bytes, ttl %s)", name, source, seed, len(value), held)
	writeJSON(w, http.StatusOK, map[string]any{keyName: name, keyLength: len(value), keyTTL: int(held.Seconds())})
}
