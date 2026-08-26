package serve

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://x.example/api/me", "https://x.example/app", true},
		{"https://x.example:8443/api", "https://x.example/app", false},
		{"https://api.x.example/me", "https://x.example/app", false},
		{"http://x.example/a", "https://x.example/a", false},
		{"/relative", "https://x.example/app", false},
		{"https://x.example/a", "about:blank", false},
	}
	for _, tc := range cases {
		if got := sameOrigin(tc.a, tc.b); got != tc.want {
			t.Errorf("sameOrigin(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// A navigation routinely rewrites the URL it was given by exactly one trailing
// slash, and the response event must still be recognized as ours.
func TestSameURLIgnoresATrailingSlash(t *testing.T) {
	if !sameURL("https://x.example/api/", "https://x.example/api") {
		t.Error("a trailing slash must not lose the response")
	}
	if sameURL("https://x.example/a", "https://x.example/b") {
		t.Error("different paths are different URLs")
	}
}

// A browser showing only chrome:// surfaces has no signed-in context, and
// guessing one produces a confusing empty result instead of a fixable error.
func TestActivePageRefusesToGuess(t *testing.T) {
	for name, tabs := range map[string]string{
		"only chrome surfaces": `[{"id":"1","type":"page","url":"chrome://newtab/"}]`,
		"only workers":         `[{"id":"1","type":"service_worker","url":"https://x.example/sw.js"}]`,
		"nothing at all":       `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			port := fakeTabsServer(t, tabs)
			if _, _, err := activePage(t.Context(), port); !errors.Is(err, errNoGrabTarget) {
				t.Fatalf("error = %v, want errNoGrabTarget", err)
			}
		})
	}

	port := fakeTabsServer(t, `[{"id":"1","type":"page","url":"chrome://newtab/"},
		{"id":"2","type":"page","url":"https://x.example/app"}]`)
	id, pageURL, err := activePage(t.Context(), port)
	if err != nil {
		t.Fatalf("activePage: %v", err)
	}
	if id != "2" || pageURL != "https://x.example/app" {
		t.Fatalf("picked %s (%s), want the http(s) page", id, pageURL)
	}
}

// fakeTabsServer serves one /json/list body on a loopback port, which is what
// fetchCDP addresses.
func fakeTabsServer(t *testing.T, body string) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	_, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing test server port: %v", err)
	}
	return port
}
