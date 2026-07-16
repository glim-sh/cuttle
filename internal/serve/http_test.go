package serve

import (
	"slices"
	"testing"
)

func TestParseConnectionParams(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		query string
		want  connectRequest
	}{
		{
			name:  "all recognized params",
			query: "fingerprint=seed1&timezone=America/New_York&locale=en-US&proxy=http://p:1&geoip=true",
			want: connectRequest{
				seed: "seed1", timezone: "America/New_York", locale: "en-US",
				proxy: "http://p:1", geoip: true,
			},
		},
		{
			name:  "generic param becomes fingerprint flag",
			query: "fingerprint=s&platform=windows&webrtc-ip=auto",
			want: connectRequest{
				seed:      "s",
				extraArgs: []string{"--fingerprint-platform=windows", "--fingerprint-webrtc-ip=auto"},
			},
		},
		{
			name:  "blank values dropped",
			query: "fingerprint=&timezone=&locale=en-GB",
			want:  connectRequest{locale: "en-GB"},
		},
		{
			name:  "geoip falsey",
			query: "fingerprint=s&geoip=no",
			want:  connectRequest{seed: "s", geoip: false},
		},
		{
			name:  "first value wins on duplicate key",
			query: "fingerprint=first&fingerprint=second",
			want:  connectRequest{seed: "first"},
		},
		{
			name:  "url-encoded value",
			query: "proxy=http%3A%2F%2Fu%3Ap%40h%3A8080",
			want:  connectRequest{proxy: "http://u:p@h:8080"},
		},
		{
			name:  "empty query",
			query: "",
			want:  connectRequest{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseConnectionParams(tc.query)
			if got.seed != tc.want.seed || got.timezone != tc.want.timezone ||
				got.locale != tc.want.locale || got.proxy != tc.want.proxy || got.geoip != tc.want.geoip {
				t.Errorf("scalar mismatch: got %+v want %+v", got, tc.want)
			}
			if !slices.Equal(got.extraArgs, tc.want.extraArgs) {
				t.Errorf("extraArgs got %v want %v", got.extraArgs, tc.want.extraArgs)
			}
		})
	}
}

func TestOriginIsAllowed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		origin  string
		present bool
		host    string
		scheme  string
		want    bool
	}{
		{name: "absent origin allowed (non-browser client)", present: false, host: "127.0.0.1:9222", scheme: "http", want: true},
		{name: "empty origin rejected", origin: "", present: true, host: "127.0.0.1:9222", scheme: "http", want: false},
		{name: "null origin rejected", origin: "null", present: true, host: "127.0.0.1:9222", scheme: "http", want: false},
		{name: "devtools trusted", origin: "devtools://devtools", present: true, host: "example:9222", scheme: "http", want: true},
		{name: "loopback same-origin allowed (port-forward)", origin: "http://127.0.0.1:9222", present: true, host: "127.0.0.1:9222", scheme: "http", want: true},
		{name: "localhost same-origin allowed", origin: "http://localhost:9222", present: true, host: "localhost:9222", scheme: "http", want: true},
		{name: "port mismatch rejected", origin: "http://127.0.0.1:9223", present: true, host: "127.0.0.1:9222", scheme: "http", want: false},
		{name: "non-loopback host rejected", origin: "http://evil.example", present: true, host: "evil.example", scheme: "http", want: false},
		{name: "cross-origin against loopback rejected", origin: "http://evil.example", present: true, host: "127.0.0.1:9222", scheme: "http", want: false},
		{name: "origin with path rejected", origin: "http://127.0.0.1:9222/x", present: true, host: "127.0.0.1:9222", scheme: "http", want: false},
		{name: "origin with userinfo rejected", origin: "http://u@127.0.0.1:9222", present: true, host: "127.0.0.1:9222", scheme: "http", want: false},
		{name: "bad scheme rejected", origin: "ftp://127.0.0.1", present: true, host: "127.0.0.1", scheme: "http", want: false},
		{name: "default ports match (http 80)", origin: "http://localhost", present: true, host: "localhost:80", scheme: "http", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := originIsAllowed(tc.origin, tc.present, tc.host, tc.scheme); got != tc.want {
				t.Errorf("originIsAllowed(%q,%v,%q,%q)=%v want %v", tc.origin, tc.present, tc.host, tc.scheme, got, tc.want)
			}
		})
	}
}
