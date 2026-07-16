package fingerprint

import (
	"errors"
	"testing"
)

const testExitIP = "203.0.113.7"

var errNoRoute = errors.New("no route")

func TestResolveProxyGeoWithIPDegrades(t *testing.T) {
	tests := []struct {
		name       string
		exitIP     ExitIPFunc
		dbPath     func() string
		wantTZ     string
		wantLocale string
		wantIP     string
	}{
		{
			name:   "exit-ip failure yields nothing",
			exitIP: func(string) (string, error) { return "", errNoRoute },
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
			r := GeoResolver{ExitIP: tt.exitIP, DBPath: tt.dbPath}
			tz, locale, ip := r.ResolveProxyGeoWithIP("http://proxy.example:8080")
			if tz != tt.wantTZ || locale != tt.wantLocale || ip != tt.wantIP {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)", tz, locale, ip, tt.wantTZ, tt.wantLocale, tt.wantIP)
			}
		})
	}
}
