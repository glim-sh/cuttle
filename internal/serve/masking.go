package serve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"html"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
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

// credentialParamRE matches a credential-shaped URL query parameter. This half
// needs no store: the leak it exists for was a routine retry log carrying
// `remix_userkey=25039df9...` in the URL it was retrying, a value cuttle never
// held and therefore could never have matched by content.
var credentialParamRE = regexp.MustCompile(
	`(?i)([A-Za-z0-9_.-]*(?:token|key|secret|password|passwd|pwd|auth|session|credential)[A-Za-z0-9_.-]*)=([A-Za-z0-9%._~+/-]{8,}=*)`,
)

// maskText redacts one line of cuttle-authored text against the store the pool
// published. Used by the log handler, which has no other way to reach it.
func maskText(text string) string { return maskWith(logMaskStore.Load(), text) }

// maskWith redacts every value the given store holds, plus every
// credential-shaped query parameter, whichever store the caller already has.
func maskWith(store *secretStore, text string) string {
	if store != nil {
		text = store.redact(text)
	}
	// The regex is the expensive half - it forces the NFA engine at every
	// position, measured ~150ns per character - and it cannot match without an
	// `=`. One IndexByte keeps the common log line off that path entirely.
	if strings.IndexByte(text, '=') < 0 {
		return text
	}
	return credentialParamRE.ReplaceAllString(text, "$1=<redacted>")
}

// redact replaces every live value - in any of its common encodings - with its
// own name. The replacer is cached and rebuilt only when the store changes, so
// the steady-state cost of a log line is one atomic load and one pass.
func (s *secretStore) redact(text string) string {
	r := s.replacer()
	if r == nil {
		return text
	}
	return r.Replace(text)
}

// replacer returns the cached replacer, rebuilding it when the store has moved
// on. The rebuild - encoding expansion, a sort, and a trie over every variant -
// happens with the mutex RELEASED: it is the same mutex the fill path takes to
// copy a value out, and holding it through that work would put log formatting on
// the critical path of typing a credential. A lost race just rebuilds twice.
func (s *secretStore) replacer() *strings.Replacer {
	s.mu.Lock()
	version := s.version
	if s.maskVersion == version {
		mask := s.mask
		s.mu.Unlock()
		return mask
	}
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

	var mask *strings.Replacer
	if len(sorted) > 0 {
		mask = strings.NewReplacer(sorted...)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Only publish if nothing changed while the lock was down; a newer version
	// gets its own rebuild on the next line logged.
	if s.version == version {
		s.mask, s.maskVersion = mask, version
	}
	return mask
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
	out := []string{}
	for _, v := range []string{
		value,
		url.QueryEscape(value),
		url.PathEscape(value),
		jsonEscaped,
		html.EscapeString(value),
		base64.StdEncoding.EncodeToString([]byte(value)),
	} {
		if maskable(v) && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
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

func maskAttr(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindString {
		a.Value = slog.StringValue(maskText(a.Value.String()))
	}
	return a
}

func (h maskingHandler) WithGroup(name string) slog.Handler {
	return maskingHandler{inner: h.inner.WithGroup(name)}
}
