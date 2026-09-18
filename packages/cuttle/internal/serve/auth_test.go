package serve

import (
	"strings"
	"testing"
)

func TestFoldCookieDomains(t *testing.T) {
	got := foldCookieDomains([]rawCookie{
		{Domain: ".example.com", Expires: 2000},
		{Domain: ".example.com", Expires: 1000},
		{Domain: ".example.com", Expires: -1},
		{Domain: "other.test", Expires: -1},
	})
	if len(got) != 2 {
		t.Fatalf("folded to %d domains, want 2: %+v", len(got), got)
	}
	if got[0].Domain != ".example.com" || got[0].Cookies != 3 || got[0].Session != 1 {
		t.Fatalf("example.com folded to %+v", got[0])
	}
	if !strings.HasPrefix(got[0].SoonestExpiry, "1970-01-01T00:16:40") {
		t.Fatalf("soonest expiry = %q, want the EARLIEST non-session expiry", got[0].SoonestExpiry)
	}
	if got[1].SoonestExpiry != "" {
		t.Fatalf("a domain with only session cookies has no expiry: %+v", got[1])
	}
}

// The cookie that actually carries a session is usually on the parent domain, so
// asking about app.example.com must report .example.com too.
func TestFilterDomainsMatchesParents(t *testing.T) {
	domains := []originAuth{{Domain: ".example.com"}, {Domain: "app.example.com"}, {Domain: "other.test"}}
	got := filterDomains(domains, "https://app.example.com/login")
	if len(got) != 2 {
		t.Fatalf("filtered to %+v, want the exact domain and its parent", got)
	}
	if len(filterDomains(domains, "nothing.test")) != 0 {
		t.Fatal("an unrelated origin must match nothing")
	}
}

func TestHostOfOriginArg(t *testing.T) {
	for in, want := range map[string]string{
		"https://app.example.com/login": "app.example.com",
		"app.example.com":               "app.example.com",
		"http://x.test:8080":            "x.test",
		"x.test.":                       "x.test",
	} {
		if got := hostOfOriginArg(in); got != want {
			t.Errorf("hostOfOriginArg(%q) = %q, want %q", in, got, want)
		}
	}
}
