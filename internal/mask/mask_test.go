package mask

import (
	"strings"
	"testing"
)

func TestParams(t *testing.T) {
	cases := map[string]struct{ leaks, keeps string }{
		"retry log":      {"25039df9abc123", "page=2"},
		"oauth callback": {"4/0AVGzR1B7xy", "state=xyz"},
	}
	lines := map[string]string{
		"retry log":      "retrying https://x.example/api?remix_userkey=25039df9abc123&page=2",
		"oauth callback": "signed in: https://app.example/cb?code=4/0AVGzR1B7xy&state=xyz",
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
