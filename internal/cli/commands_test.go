package cli

import "testing"

func TestFlagSuffixAndResumeCmd(t *testing.T) {
	cases := []struct {
		name string
		cf   commonFlags
		want string
	}{
		{"defaults omit", commonFlags{name: defaultName, cdpPort: defaultCDPPort}, ""},
		{"custom name only", commonFlags{name: "ax", cdpPort: defaultCDPPort}, " --name ax"},
		{"custom port only", commonFlags{name: defaultName, cdpPort: 9313}, " --cdp-port 9313"},
		{"custom name and port", commonFlags{name: "ax", cdpPort: 9313}, " --name ax --cdp-port 9313"},
	}
	for _, c := range cases {
		if got := flagSuffix(c.cf); got != c.want {
			t.Errorf("%s: flagSuffix = %q, want %q", c.name, got, c.want)
		}
	}
	if got := resumeCmd(commonFlags{name: "ax", cdpPort: 9313}); got != "cuttle up --name ax --cdp-port 9313" {
		t.Errorf("resumeCmd = %q", got)
	}
}
