package mask

import (
	"strings"
	"testing"
)

func TestParams(t *testing.T) {
	// The kept values are deliberately long enough to match the pattern's own
	// length floor, so surviving proves the KEY was judged, not that the value was
	// too short to be considered.
	cases := map[string]struct{ leaks, keeps string }{
		"retry log":      {"25039df9abc123", "page=1234567890"},
		"oauth callback": {"4/0AVGzR1B7xy", "returnTo=dashboard-overview"},
	}
	lines := map[string]string{
		"retry log":      "retrying https://x.example/api?remix_userkey=25039df9abc123&page=1234567890",
		"oauth callback": "signed in: https://app.example/cb?code=4/0AVGzR1B7xy&returnTo=dashboard-overview",
	}
	for name, tc := range cases {
		got := Params(lines[name])
		if strings.Contains(got, tc.leaks) {
			t.Errorf("%s: the credential survived: %q", name, got)
		}
		if !strings.Contains(got, tc.keeps) {
			t.Errorf("%s: an ordinary parameter was scrubbed: %q", name, got)
		}
	}
	// Nothing to match, nothing to change - including the no-`=` fast path.
	for _, plain := range []string{"a plain log line", "seed=__default__"} {
		if got := Params(plain); got != plain {
			t.Errorf("Params(%q) = %q, want it untouched", plain, got)
		}
	}
}

func TestParamsUserinfo(t *testing.T) {
	for line, want := range map[string]string{
		"proxy=http://user__cr.us;sessttl.10:s3cret@gw.example:823 ok": "proxy=http://<redacted>@gw.example:823 ok",
		"connect socks5://u:p@h:1080":                                  "connect socks5://<redacted>@h:1080",
		"clone https://ghp_TOKEN@github.com/o/r":                       "clone https://<redacted>@github.com/o/r",
		"proxy=u:s3cret@gw.example:823":                                "proxy=<redacted>@gw.example:823",
		"(u:s3cret@h:1)":                                               "(<redacted>@h:1)",
	} {
		if got := Params(line); got != want {
			t.Errorf("Params(%q) = %q, want %q", line, got, want)
		}
	}
	for _, plain := range []string{"mail ops@example.com", "reply to <ops@example.com>"} {
		if got := Params(plain); got != plain {
			t.Errorf("Params(%q) = %q, want it untouched", plain, got)
		}
	}
}
