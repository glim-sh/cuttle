package serve

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestMaskingCatchesExpandedEncodings(t *testing.T) {
	const value = "pa55&w<rd"
	store := newSecretStore()
	store.put(testSeed, "GH_PASS", []byte(value), sourceStdin, secretTTLDefault)

	// Every form a value actually shows up in when something echoes it back: raw,
	// in a query string, in a JSON payload, in page text, in a Basic header.
	for name, line := range map[string]string{
		"raw":          "login failed for pa55&w<rd on retry",
		"url encoded":  "GET /login?pw=pa55%26w%3Crd failed",
		"json escaped": `posting {"password":"pa55\u0026w\u003crd"} failed`,
		"html escaped": "page text: pa55&amp;w&lt;rd",
		"base64":       "Authorization: Basic cGE1NSZ3PHJk",
	} {
		t.Run(name, func(t *testing.T) {
			got := maskWith(store, line)
			for _, leak := range []string{value, "pa55%26w", "pa55\\u0026w", "pa55&amp;w", "cGE1NSZ3PHJk"} {
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
	store.expire(testSeed, "GH_PASS")
	if got := maskWith(store, "hunter2000"); got != "hunter2000" {
		t.Fatalf("masked = %q, want the expired value left alone", got)
	}
}
