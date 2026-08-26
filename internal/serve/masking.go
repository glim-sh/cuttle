package serve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/glim-sh/cuttle/internal/mask"
)

// Masking covers the text CUTTLE AUTHORS: its own log lines (which are teed to
// the profile volume on a durable session, so a leak there outlives the
// container) and the CDP errors it builds. It does NOT and cannot cover a
// driver's snapshot of a page - by the time a filled password is one string
// among thousands inside the driver's own Runtime payload there is no structured
// field to null out, and rewriting frame bytes on the wire corrupts every
// base64-carrying CDP result. SKILL.md says so plainly rather than implying
// coverage this does not have.
//
// Three details are load-bearing, each learned from an upstream bug:
//
//   - Expanded encodings. A secret in a log line is usually not raw bytes: it is
//     URL-encoded in a query string, JSON-escaped in a payload, HTML-escaped in
//     page text, or base64 in a header. Matching only the raw form is the gap in
//     both Playwright's and browser-use's redaction.
//   - Longest match first. With {a:"pass", b:"password"} and the short one first,
//     "password" comes out as "<secret:a>word" - a partial leak, not a tidy
//     replacement. strings.Replacer tries pairs in argument order at each
//     position, so the variants are sorted longest-first before it is built.
//   - Length floors. A 3-character value would shred every unrelated word in the
//     output, and a 4-digit one every price and date, so short values are not
//     matched at all. That is a deliberate hole: masking is a safety net for text
//     cuttle authors, not a control that makes short secrets safe.
const (
	minMaskLength        = 4
	minNumericMaskLength = 6
)

// logMaskStore is the store the log masker reads. The daemon's logger is a
// package-level global that exists before any pool does, so the store is
// published here when the pool is built rather than threaded through every
// logging call site.
var logMaskStore atomic.Pointer[secretStore]

// maskText redacts one line of cuttle-authored text against the store the pool
// published. Used by the log handler, which has no other way to reach it.
func maskText(text string) string { return maskWith(logMaskStore.Load(), text) }

// maskWith redacts every value the given store holds, plus every
// credential-shaped query parameter, whichever store the caller already has.
func maskWith(store *secretStore, text string) string {
	if store != nil {
		text = store.redact(text)
	}
	// The other half needs no store at all, and the CLI needs the same rule for
	// the URLs it prints, so it lives in internal/mask.
	return mask.Params(text)
}

// maskState is a replacer and the store version it was built from, published as
// ONE pointer. Publishing them as two atomics was wrong: with two rebuilds in
// flight, the older one could land its replacer after the newer one and then
// stamp the newer version over it, leaving a replacer that is missing a live
// secret while claiming to be current - and nothing would ever rebuild again.
// A nil replacer inside a non-nil state means "built, and there was nothing to
// mask", which is what keeps an empty store off the rebuild path entirely.
type maskState struct {
	version  uint64
	replacer *strings.Replacer
}

// redact replaces every live value - in any of its common encodings - with its
// own name.
//
// The steady state is two atomic loads and no lock, including on a daemon
// holding no secrets at all. That matters because it is the same mutex the fill
// path takes to copy a value out - but it does NOT make logging under that mutex
// safe: a store that has changed since the last publish rebuilds here, and the
// rebuild takes the lock. Never log while holding it.
func (s *secretStore) redact(text string) string {
	st := s.mask.Load()
	if st == nil || st.version != s.version.Load() {
		st = s.replacer()
	}
	if st.replacer == nil {
		return text
	}
	return st.replacer.Replace(text)
}

// replacer rebuilds the cached state and publishes it, returning what a caller
// should use now. The rebuild - encoding expansion, a sort, and a trie over
// every variant - happens with the mutex RELEASED: only the value snapshot needs
// it, and holding it through that work would put log formatting on the critical
// path of typing a credential.
func (s *secretStore) replacer() *maskState {
	s.mu.Lock()
	version := s.version.Load()
	type held struct{ name, value string }
	values := []held{}
	for _, bucket := range s.m {
		for name, e := range bucket {
			if e.live() {
				values = append(values, held{name, string(e.val)})
			}
		}
	}
	s.mu.Unlock()

	pairs := []string{}
	for _, v := range values {
		for _, variant := range maskVariants(v.value) {
			pairs = append(pairs, variant, "<secret:"+v.name+">")
		}
	}
	// Longest first: strings.Replacer takes the first pair that matches at a
	// position, so a short value would otherwise chew a hole through a longer one.
	idx := make([]int, len(pairs)/2)
	for i := range idx {
		idx[i] = i * 2
	}
	slices.SortFunc(idx, func(a, b int) int { return len(pairs[b]) - len(pairs[a]) })
	sorted := make([]string, 0, len(pairs))
	for _, i := range idx {
		sorted = append(sorted, pairs[i], pairs[i+1])
	}

	next := &maskState{version: version}
	if len(sorted) > 0 {
		next.replacer = strings.NewReplacer(sorted...)
	}
	for {
		cur := s.mask.Load()
		if cur != nil && cur.version >= version {
			// Someone published from a newer snapshot; theirs can only hold MORE
			// secrets than ours, so use it and leave it in place.
			return cur
		}
		if s.mask.CompareAndSwap(cur, next) {
			return next
		}
	}
}

// maskVariants expands one value into the forms it can appear in, dropping
// anything under the length floors and any duplicate a form collapses to.
func maskVariants(value string) []string {
	if !maskable(value) {
		return nil
	}
	encoded, err := json.Marshal(value)
	jsonEscaped := value
	if err == nil {
		jsonEscaped = strings.Trim(string(encoded), `"`)
	}
	// Every form a value has been seen in, and the ones a red-team pass found
	// missing: base64 comes in four spellings (padded or not, standard or
	// URL-safe) and a token in a header or a query string uses whichever the
	// producer picked; percent-encoding is case-insensitive in its hex digits and
	// Go emits upper; and a value can arrive fully \u-escaped or as HTML numeric
	// entities without any of the characters html.EscapeString cares about.
	out := []string{}
	for _, v := range []string{
		value,
		url.QueryEscape(value),
		strings.ToLower(url.QueryEscape(value)),
		url.PathEscape(value),
		percentAll(value),
		jsonEscaped,
		unicodeEscaped(value),
		html.EscapeString(value),
		numericEntities(value),
		base64.StdEncoding.EncodeToString([]byte(value)),
		base64.RawStdEncoding.EncodeToString([]byte(value)),
		base64.URLEncoding.EncodeToString([]byte(value)),
		base64.RawURLEncoding.EncodeToString([]byte(value)),
	} {
		if maskable(v) && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// percentAll percent-encodes every byte in lower hex, the form a hand-rolled
// encoder emits and url.QueryEscape never does (it leaves unreserved bytes
// alone and uses upper hex).
func percentAll(value string) string {
	var b strings.Builder
	for _, c := range []byte(value) {
		fmt.Fprintf(&b, "%%%02x", c)
	}
	return b.String()
}

// unicodeEscaped is the all-\u form: legal JSON, and it survives a round trip
// through anything that re-encodes conservatively.
func unicodeEscaped(value string) string {
	var b strings.Builder
	for _, r := range value {
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	return b.String()
}

// numericEntities is the &#NN; form, which page text and HTML-escaping libraries
// produce for characters html.EscapeString leaves alone.
func numericEntities(value string) string {
	var b strings.Builder
	for _, r := range value {
		fmt.Fprintf(&b, "&#%d;", r)
	}
	return b.String()
}

// maskable enforces the two floors: a short value is not matched, and an
// all-numeric one needs to be longer still - a 4-digit secret would redact every
// price, year and count in the daemon's own output.
func maskable(value string) bool {
	if len(value) < minMaskLength {
		return false
	}
	if len(value) < minNumericMaskLength && strings.IndexFunc(value, func(r rune) bool {
		return r < '0' || r > '9'
	}) < 0 {
		return false
	}
	return true
}

// maskingHandler redacts a record on its way to the real handler, so both halves
// of the daemon's logging - stderr and the log file on the profile volume - are
// covered by one wrap.
type maskingHandler struct{ inner slog.Handler }

func newLogHandler(inner slog.Handler) slog.Handler { return maskingHandler{inner: inner} }

func (h maskingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h maskingHandler) Handle(ctx context.Context, r slog.Record) error {
	masked := slog.NewRecord(r.Time, r.Level, maskText(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		masked.AddAttrs(maskAttr(a))
		return true
	})
	return h.inner.Handle(ctx, masked) //nolint:wrapcheck // pass-through
}

// WithAttrs masks the bound attrs too. They are formatted into every subsequent
// record, so an unmasked one would pay the whole cost of this handler and get
// none of its coverage.
func (h maskingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	masked := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		masked[i] = maskAttr(a)
	}
	return maskingHandler{inner: h.inner.WithAttrs(masked)}
}

// maskAttr masks an attribute's KEY as well as its value, and reaches into a
// group rather than passing it through. A value can be anywhere in a record:
// `slog.Any("v", []byte(secret))` renders the bytes, an attr key can BE the
// secret, and only masking string-kinded values let all three through while
// paying the whole cost of this handler.
func maskAttr(a slog.Attr) slog.Attr {
	a.Key = maskText(a.Key)
	// Only two kinds need their own handling; the rest are covered by the default,
	// which is why this is not written as an exhaustive switch.
	//nolint:exhaustive // default covers every remaining kind
	switch a.Value.Kind() {
	case slog.KindGroup:
		group := a.Value.Group()
		masked := make([]slog.Attr, len(group))
		for i, g := range group {
			masked[i] = maskAttr(g)
		}
		a.Value = slog.GroupValue(masked...)
	case slog.KindString:
		a.Value = slog.StringValue(maskText(a.Value.String()))
	default:
		// A byte slice needs its own case: slog's handlers render one as a quoted
		// STRING, while Value.String() renders it as a list of numbers - so masking
		// the latter inspects text the log will never contain and passes the
		// credential straight through.
		if bs, ok := byteSliceOf(a.Value.Any()); ok {
			if m := maskText(string(bs)); m != string(bs) {
				a.Value = slog.StringValue(m)
			}
			return a
		}
		// Rendered form for everything else - a fmt.Stringer, a struct, an error.
		// Replaced only when masking changed it, so an int stays an int.
		if rendered := a.Value.String(); rendered != "" {
			if m := maskText(rendered); m != rendered {
				a.Value = slog.StringValue(m)
			}
		}
	}
	return a
}

// byteSliceOf mirrors the check slog's own handlers make before rendering a
// value as a quoted string, named types (json.RawMessage and friends) included.
func byteSliceOf(v any) ([]byte, bool) {
	if b, ok := v.([]byte); ok {
		return b, true
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
		return rv.Bytes(), true
	}
	return nil, false
}

// WithGroup masks the group NAME: it is caller-supplied text that lands in
// every record the group produces.
func (h maskingHandler) WithGroup(name string) slog.Handler {
	return maskingHandler{inner: h.inner.WithGroup(maskText(name))}
}
