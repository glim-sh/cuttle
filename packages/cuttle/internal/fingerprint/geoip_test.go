package fingerprint

import (
	"errors"
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
	// The region half must survive: it is what keeps number/date formatting
	// consistent with the exit IP, while the language half drives which language
	// servers negotiate.
	for _, tc := range []struct{ in, want string }{
		{"pt-PT", "en-PT"},
		{"de-DE", "en-DE"},
		{"ja-JP", "en-JP"},
		{"pt-BR", "en-BR"},
		{"en-GB", "en-GB"}, // already English: untouched, region preserved
		{"en-US", "en-US"},
		{"", ""},       // no geo resolved
		{"de", "de"},   // no region to keep - leave it alone
		{"-PT", "-PT"}, // malformed
	} {
		if got := EnglishContentLocale(tc.in); got != tc.want {
			t.Errorf("EnglishContentLocale(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Every locale the country map can produce must survive the English swap with a
// region intact - a bare "en" would drop the regional formatting that matches
// the exit IP.
func TestEnglishContentLocaleCoversCountryMap(t *testing.T) {
	t.Parallel()
	for country, locale := range CountryLocaleMap {
		got := EnglishContentLocale(locale)
		if !strings.HasPrefix(got, "en-") {
			t.Errorf("%s (%s): got %q, want an en-<region> tag", country, locale, got)
		}
		if _, region, _ := strings.Cut(got, "-"); region == "" {
			t.Errorf("%s (%s): region lost, got %q", country, locale, got)
		}
	}
}
