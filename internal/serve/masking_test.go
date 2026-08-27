package serve

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

func TestMaskingCatchesExpandedEncodings(t *testing.T) {
	const value = "fake&fake<fake"
	store := newSecretStore()
	store.put(testSeed, "GH_PASS", []byte(value), sourceStdin, secretTTLDefault)

	// Every form a value actually shows up in when something echoes it back: raw,
	// in a query string, in a JSON payload, in page text, in a Basic header.
	for name, line := range map[string]string{
		"raw":          "login failed for fake&fake<fake on retry",
		"url encoded":  "GET /login?pw=fake%26fake%3Cfake failed",
		"json escaped": `posting {"password":"fake\u0026fake\u003cfake"} failed`,
		"html escaped": "page text: fake&amp;fake&lt;fake",
		"base64":       "Authorization: Basic ZmFrZSZmYWtlPGZha2U=",
	} {
		t.Run(name, func(t *testing.T) {
			got := maskWith(store, line)
			for _, leak := range []string{value, "fake%26fake", "fake\\u0026fake", "fake&amp;fake", "ZmFrZSZmYWtlPGZha2U="} {
				if strings.Contains(got, leak) {
					t.Fatalf("masked line still carries the value (%s): %q", leak, got)
				}
			}
			if !strings.Contains(got, "<secret:GH_PASS>") {
				t.Fatalf("masked line does not name the secret: %q", got)
			}
		})
	}
}

// A short value would shred unrelated words, and a short numeric one every price
// and date in the daemon's own output. Both are deliberately not matched.
func TestMaskingHonoursTheLengthFloors(t *testing.T) {
	store := newSecretStore()
	store.put(testSeed, "SHORT", []byte("abc"), sourceStdin, secretTTLDefault)
	store.put(testSeed, "PIN", []byte("1234"), sourceStdin, secretTTLDefault)
	store.put(testSeed, "CODE", []byte("123456"), sourceStdin, secretTTLDefault)

	got := maskWith(store, "abc 1234 123456 abcdef")
	if !strings.Contains(got, "abc 1234 ") {
		t.Errorf("a 3-char value and a 4-digit value must not be redacted: %q", got)
	}
	if !strings.Contains(got, "<secret:CODE>") {
		t.Errorf("a 6-digit value must be redacted: %q", got)
	}
}

// The partial-leak bug: with a short secret matched first, the longer one comes
// out as "<secret:SHORT>word" and the long name never matches at all.
func TestMaskingPrefersTheLongestMatch(t *testing.T) {
	store := newSecretStore()
	store.put(testSeed, "SHORT", []byte("pass"), sourceStdin, secretTTLDefault)
	store.put(testSeed, "LONG", []byte("password"), sourceStdin, secretTTLDefault)

	got := maskWith(store, "typed password into the form")
	if got != "typed <secret:LONG> into the form" {
		t.Fatalf("masked = %q, want the longer secret matched whole", got)
	}
}

// The leak this half exists for is a value cuttle never held: a credential-shaped
// query parameter in a URL its own retry log was printing.
func TestMaskingRedactsCredentialShapedQueryParams(t *testing.T) {
	got := maskWith(nil, "retrying https://x.example/api?remix_userkey=25039df9abc123&page=2")
	if strings.Contains(got, "25039df9abc123") {
		t.Fatalf("the credential param survived: %q", got)
	}
	if !strings.Contains(got, "page=2") {
		t.Fatalf("an ordinary param must be left alone: %q", got)
	}
}

// Both halves of the daemon's logging go through one handler wrap, so a value
// cannot reach stderr or the log file on the profile volume.
func TestLogHandlerMasks(t *testing.T) {
	store := newSecretStore()
	store.put(testSeed, "GH_PASS", []byte("hunter2000"), sourceStdin, secretTTLDefault)
	logMaskStore.Store(store)
	t.Cleanup(func() { logMaskStore.Store(nil) })

	var buf bytes.Buffer
	prev := logger
	logger = slog.New(newLogHandler(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { logger = prev })

	logWarn("a driver echoed hunter2000 back at us")
	logger.Info("attr form", slog.String("value", "hunter2000"))
	if strings.Contains(buf.String(), "hunter2000") {
		t.Fatalf("the log carries the value: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "<secret:GH_PASS>") {
		t.Fatalf("the log does not name the secret: %s", buf.String())
	}
}

// An expired value is not held any more, so it stops being matched - the masker
// must follow the store rather than keeping a stale copy alive.
func TestMaskingFollowsTheStore(t *testing.T) {
	store := newSecretStore()
	store.put(testSeed, "GH_PASS", []byte("hunter2000"), sourceStdin, secretTTLDefault)
	if got := maskWith(store, "hunter2000"); got == "hunter2000" {
		t.Fatal("a live value must be masked")
	}
	store.expireNow("GH_PASS")
	if got := maskWith(store, "hunter2000"); got != "hunter2000" {
		t.Fatalf("masked = %q, want the expired value left alone", got)
	}
}

// An empty store must not rebuild on every line: the rebuild takes the store
// mutex, which is the fill path's mutex, and it is what makes "never log under
// mu" a live deadlock rather than a rule about writers.
func TestMaskingDoesNotRebuildForAnEmptyStore(t *testing.T) {
	store := newSecretStore()
	if got := maskWith(store, "nothing to mask here"); got != "nothing to mask here" {
		t.Fatalf("masked = %q, want it untouched", got)
	}
	first := store.mask.Load()
	if first == nil {
		t.Fatal("an empty store must still publish a state, or every line rebuilds")
	}
	maskWith(store, "another line")
	if store.mask.Load() != first {
		t.Fatal("a second line rebuilt the replacer for an unchanged store")
	}
}

// Two rebuilds can be in flight at once. The older one must never end up
// published under the newer one's version: that pins a replacer missing a live
// secret while claiming to be current, and nothing rebuilds again.
func TestMaskingNeverPublishesAStaleStateOverANewerOne(t *testing.T) {
	store := newSecretStore()
	store.put(testSeed, "A", []byte("hunter2000"), sourceStdin, secretTTLDefault)
	maskWith(store, "warm the cache")

	// A rebuild that snapshotted the older version, finishing last.
	stale := &maskState{version: store.version.Load() - 1}
	store.put(testSeed, "B", []byte("s3cretvalue"), sourceStdin, secretTTLDefault)
	maskWith(store, "rebuild at the new version")
	if cur := store.mask.Load(); cur.version < store.version.Load() {
		t.Fatalf("published version %d is behind the store's %d", cur.version, store.version.Load())
	}
	store.mask.CompareAndSwap(store.mask.Load(), stale)

	// The next line must notice and rebuild rather than trust the stale state.
	got := maskWith(store, "typed s3cretvalue into the form")
	if strings.Contains(got, "s3cretvalue") {
		t.Fatalf("a stale published state kept a live secret unmasked: %q", got)
	}
}

// Concurrent readers and writers must not race, and no reader may see a state
// whose replacer and version disagree.
func TestMaskingUnderConcurrentChange(t *testing.T) {
	store := newSecretStore()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			store.put(testSeed, "A", []byte("hunter2000"), sourceStdin, secretTTLDefault)
			if i%2 == 0 {
				store.remove(testSeed, "A")
			}
		}
	}()
	for range 200 {
		maskWith(store, "a line mentioning hunter2000")
	}
	<-done
	store.put(testSeed, "A", []byte("hunter2000"), sourceStdin, secretTTLDefault)
	if got := maskWith(store, "hunter2000"); got == "hunter2000" {
		t.Fatal("after the churn a live value is no longer masked")
	}
}

// A value is not always text by the time it reaches a log line. These are the
// shapes a red-team pass found going through unmasked while the handler was
// paying for the coverage it did not have.
func TestMaskingCoversEveryRecordShape(t *testing.T) {
	const value = "hunter2000"
	store := newSecretStore()
	store.put(testSeed, "GH_PASS", []byte(value), sourceStdin, secretTTLDefault)
	logMaskStore.Store(store)
	t.Cleanup(func() { logMaskStore.Store(nil) })

	var buf bytes.Buffer
	lg := slog.New(newLogHandler(slog.NewTextHandler(&buf, nil)))
	// A byte slice renders as a quoted string, not as the numbers Value.String()
	// would produce; an attr KEY can be the secret; so can a group name; and a
	// bound attr is formatted into every record the logger writes.
	lg.Info("bytes", slog.Any("v", []byte(value)))
	lg.Info("named bytes", slog.Any("v", json.RawMessage(value)))
	lg.Info("keyed", slog.String(value, "x"))
	lg.WithGroup(value).Info("group name")
	lg.With(slog.String("bound", value)).Info("bound attr")
	lg.Info("grouped", slog.Group("g", slog.String("inner", value)))

	if strings.Contains(buf.String(), value) {
		t.Fatalf("a value reached the log unmasked:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "<secret:GH_PASS>") {
		t.Fatalf("nothing was masked at all:\n%s", buf.String())
	}
}

// The encodings a token actually travels in. base64 alone has four spellings,
// and a producer picks whichever it likes.
func TestMaskingCoversTheEncodingsAValueTravelsIn(t *testing.T) {
	const value = "p@ssw0rd!x"
	store := newSecretStore()
	store.put(testSeed, "GH_PASS", []byte(value), sourceStdin, secretTTLDefault)

	var lowerPct, uEsc, entities strings.Builder
	for _, b := range []byte(value) {
		fmt.Fprintf(&lowerPct, "%%%02x", b)
		fmt.Fprintf(&uEsc, `\u%04x`, b)
		fmt.Fprintf(&entities, "&#%d;", b)
	}
	for label, encoded := range map[string]string{
		"raw":              value,
		"base64 std":       base64.StdEncoding.EncodeToString([]byte(value)),
		"base64 rawstd":    base64.RawStdEncoding.EncodeToString([]byte(value)),
		"base64 url":       base64.URLEncoding.EncodeToString([]byte(value)),
		"base64 rawurl":    base64.RawURLEncoding.EncodeToString([]byte(value)),
		"percent upper":    url.QueryEscape(value),
		"percent lower":    lowerPct.String(),
		"unicode escaped":  uEsc.String(),
		"numeric entities": entities.String(),
	} {
		if got := maskWith(store, encoded); !strings.Contains(got, "<secret:GH_PASS>") {
			t.Errorf("%s went unmasked: %q", label, got)
		}
	}
}
