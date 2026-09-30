package fingerprint

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

const testExitIP = "203.0.113.7"

var errNoRoute = errors.New("no route")

func TestResolveProxyGeoWithIPNilExitIP(t *testing.T) {
	// The default-seed direct-egress path and the test harness both use a zero
	// GeoResolver (nil ExitIP); it must degrade to empty, not panic.
	tz, locale, ip := GeoResolver{}.ResolveProxyGeoWithIP("")
	if tz != "" || locale != "" || ip != "" {
		t.Errorf("got (%q,%q,%q), want all empty", tz, locale, ip)
	}
}

func TestResolveProxyGeoWithIPDegrades(t *testing.T) {
	tests := []struct {
		name        string
		exitIP      ExitIPFunc
		dbPath      func() string
		resolveHost func(string) string
		wantTZ      string
		wantLocale  string
		wantIP      string
	}{
		{
			name:        "echo and host resolution both fail yields nothing",
			exitIP:      func(string) (string, error) { return "", errNoRoute },
			resolveHost: func(string) string { return "" },
		},
		{
			name:        "echo failure falls back to proxy host resolution",
			exitIP:      func(string) (string, error) { return "", errNoRoute },
			resolveHost: func(string) string { return testExitIP },
			dbPath:      func() string { return "" },
			wantIP:      testExitIP,
		},
		{
			name:   "no db degrades to exit-ip only",
			exitIP: func(string) (string, error) { return testExitIP, nil },
			dbPath: func() string { return "" },
			wantIP: testExitIP,
		},
		{
			name:   "missing db file degrades to exit-ip only",
			exitIP: func(string) (string, error) { return testExitIP, nil },
			dbPath: func() string { return "testdata/does-not-exist.mmdb" },
			wantIP: testExitIP,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := GeoResolver{ExitIP: tt.exitIP, DBPath: tt.dbPath, ResolveHost: tt.resolveHost}
			tz, locale, ip := r.ResolveProxyGeoWithIP("http://proxy.example:8080")
			if tz != tt.wantTZ || locale != tt.wantLocale || ip != tt.wantIP {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)", tz, locale, ip, tt.wantTZ, tt.wantLocale, tt.wantIP)
			}
		})
	}
}

func TestEnglishContentLocale(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"pt-PT", "en-GB"}, // Europe, no Chrome en-PT
		{"de-DE", "en-GB"},
		{"en-MT", "en-GB"},
		{"ja-JP", "en-US"},
		{"pt-BR", "en-US"},
		{"en-SG", "en-US"},
		{"hi-IN", "en-IN"}, // Chrome offers en-IN
		{"en-GB", "en-GB"},
		{"en-US", "en-US"},
		{"", ""},       // no geo resolved
		{"de", "de"},   // no region - leave it alone
		{"-PT", "-PT"}, // malformed
	} {
		if got := EnglishContentLocale(tc.in); got != tc.want {
			t.Errorf("EnglishContentLocale(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Every locale the country map can produce must come out as an English
// variant Chrome's own language list offers.
func TestEnglishContentLocaleCoversCountryMap(t *testing.T) {
	t.Parallel()
	for country, locale := range CountryLocaleMap {
		got := EnglishContentLocale(locale)
		region, ok := strings.CutPrefix(got, "en-")
		if !ok || !slices.Contains(chromeEnglishRegions, region) {
			t.Errorf("%s (%s): got %q, want an English variant Chrome offers", country, locale, got)
		}
	}
}
