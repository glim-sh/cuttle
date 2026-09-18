package serve

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// Driver output is the half the log masker never covered: a snapshot of a page
// with a filled password in it. maskOutput is what `cuttle pw` streams through.
func TestMaskOutputReplacesHeldValues(t *testing.T) {
	t.Parallel()
	store := storeWith(t, "GH_PASS", "hunter2-not-a-real-password", sourceStdin)
	out := maskOutput(store, testSeed,
		`- textbox "Sign-in field" [ref=e8]: hunter2-not-a-real-password (also hunter2-not-a-real-password%21)`)
	if strings.Contains(out, "hunter2-not-a-real-password") {
		t.Fatalf("the held value survived: %q", out)
	}
	if !strings.Contains(out, "<secret:GH_PASS>") {
		t.Fatalf("nothing was named: %q", out)
	}
}

// A one-time token the page showed once must not be destroyed on its way out:
// masking keeps it, so it can still be filled elsewhere as {{cuttle:TOKEN_1}}.
func TestMaskOutputCapturesRatherThanDestroys(t *testing.T) {
	t.Parallel()
	store := newSecretStore()
	token := "ghp_" + strings.Repeat("A", 36)

	out := maskOutput(store, testSeed, "your new token is "+token)
	if !strings.Contains(out, "<secret:TOKEN_1>") || strings.Contains(out, token) {
		t.Fatalf("output = %q, want the token replaced by its auto name", out)
	}
	val, source, status := store.take(testSeed, "TOKEN_1")
	if status != secretLive || string(val) != token {
		t.Fatalf("the daemon did not keep the token: status=%v value=%d bytes", status, len(val))
	}
	if source != sourceAuto {
		t.Errorf("source = %q, want %q", source, sourceAuto)
	}

	// The same token in the next snapshot is the same name - an agent that saw
	// TOKEN_1 must be able to keep filling it.
	if again := maskOutput(store, testSeed, "still "+token); !strings.Contains(again, "<secret:TOKEN_1>") {
		t.Errorf("a second sighting was renamed: %q", again)
	}
	other := "glpat-" + strings.Repeat("h", 20)
	if next := maskOutput(store, testSeed, "and "+other); !strings.Contains(next, "<secret:TOKEN_2>") {
		t.Errorf("a different credential must take the next name: %q", next)
	}
}

// A value the daemon already holds is masked by its own name, never captured a
// second time under an auto one.
func TestMaskOutputPrefersAHeldName(t *testing.T) {
	t.Parallel()
	token := "ghp_" + strings.Repeat("B", 36)
	store := storeWith(t, "CI_TOKEN", token, sourceCapture)
	out := maskOutput(store, testSeed, "token: "+token)
	if !strings.Contains(out, "<secret:CI_TOKEN>") {
		t.Fatalf("output = %q, want the name it is held under", out)
	}
	if _, _, status := store.take(testSeed, "TOKEN_1"); status != secretUnknown {
		t.Error("a held value must not be captured again under an auto name")
	}
}

func TestMaskRouteMasksAndRefusesANonLoopbackHost(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5300}
	pool := newTestPool(t, serveConfig{}, fl.toLauncher())
	pool.secrets.put(reservedSeed, "GH_PASS", []byte("hunter2-not-a-real-password"), sourceStdin, 0)
	m := &multiplexer{pool: pool, port: 9222}

	req := httptest.NewRequest(http.MethodPost, "/mask", strings.NewReader("typed hunter2-not-a-real-password"))
	req.Host = "127.0.0.1:9222"
	rec := httptest.NewRecorder()
	m.handleMask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "typed <secret:GH_PASS>" {
		t.Fatalf("body = %q, want the masked text", got)
	}

	req = httptest.NewRequest(http.MethodPost, "/mask", strings.NewReader("typed hunter2-not-a-real-password"))
	req.Host = "cuttle.example:9222"
	rec = httptest.NewRecorder()
	m.handleMask(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 - the masker is loopback-only like every other secret route", rec.Code)
	}
}

// URL credentials in driver output: a magic link's query token is masked by the
// log masker's own rule, and an inline URL password is captured while the rest
// of the URL stays readable.
func TestMaskOutputCoversURLCredentials(t *testing.T) {
	t.Parallel()
	store := newSecretStore()
	out := maskOutput(store, testSeed,
		"- link: https://app.example/login?token=fAkE1234magic&next=home\n"+
			"- text: postgres://app:fakepass99@db.example:5432/app")
	if strings.Contains(out, "fAkE1234magic") || strings.Contains(out, "fakepass99") {
		t.Fatalf("a URL credential survived: %q", out)
	}
	if !strings.Contains(out, "postgres://app:<secret:TOKEN_1>@db.example:5432/app") {
		t.Errorf("the URL around the password must stay readable: %q", out)
	}
	if !strings.Contains(out, "next=home") {
		t.Errorf("an ordinary parameter was scrubbed: %q", out)
	}
}

// A held value's placeholder in a secret-labelled field looks like a long value
// to that rule; it must stay the held name, not become an auto capture of itself.
func TestMaskOutputDoesNotCaptureAPlaceholder(t *testing.T) {
	t.Parallel()
	value := "fakepass99" + "-not-real"
	store := storeWith(t, "PASSWORD", value, sourceStdin)
	out := maskOutput(store, testSeed, `- textbox "Password" [ref=e4]: `+value)
	if !strings.Contains(out, "<secret:PASSWORD>") {
		t.Fatalf("output = %q, want the held name", out)
	}
	if _, _, status := store.take(testSeed, "TOKEN_1"); status != secretUnknown {
		t.Error("the placeholder was captured as a credential")
	}
}

// With no seed to capture into, a recognized credential is still masked.
func TestMaskOutputMasksWithoutASeed(t *testing.T) {
	t.Parallel()
	token := "ghp_" + strings.Repeat("C", 36)
	out := maskOutput(newSecretStore(), "", "token: "+token)
	if strings.Contains(out, token) || !strings.Contains(out, "<redacted>") {
		t.Fatalf("output = %q, want the token redacted", out)
	}
}

// Page content drives auto-capture, so the store it grows is bounded: past the
// cap a credential is masked but not kept.
func TestMaskOutputCapsAutoCaptures(t *testing.T) {
	t.Parallel()
	store := newSecretStore()
	var page strings.Builder
	for i := range maxAutoCaptures + 5 {
		fmt.Fprintf(&page, "ghp_%036d\n", i)
	}
	out := maskOutput(store, testSeed, page.String())
	if strings.Contains(out, "ghp_") {
		t.Fatalf("a token past the cap was printed: %q", out)
	}
	if n := strings.Count(out, "<redacted>"); n != 5 {
		t.Errorf("%d tokens redacted without capture, want 5", n)
	}
	if _, _, status := store.take(testSeed, fmt.Sprintf("TOKEN_%d", maxAutoCaptures+1)); status != secretUnknown {
		t.Error("the store grew past the cap")
	}
}
