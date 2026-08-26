package cli

import (
	"strings"
	"testing"
)

// readSecretStdin's comment says "A single trailing newline is stripped", but
// bytes.TrimRight strips every trailing \r and \n. A credential that legitimately
// ends in one of those is silently mangled into a different credential, and the
// only symptom is a login that fails for no stated reason.
func TestRedTeamStdinTrimsMoreThanOneNewline(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"hunter2\n", "hunter2"},
		{"hunter2\n\n", "hunter2\n"},
		{"hunter2\r", "hunter2\r"},
		{"hunter2\r\n\r\n", "hunter2\r\n"},
		{"pass\nword\n", "pass\nword"},
	} {
		got, err := readSecretStdin(strings.NewReader(tc.in))
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		state := "ok"
		if string(got) != tc.want {
			state = "MANGLED"
		}
		t.Logf("%-18q -> %-14q (want %q) %s", tc.in, string(got), tc.want, state)
	}
}

// The CLI's name grammar must stay identical to the daemon's, or a name the CLI
// accepts is one no sentinel can address (or vice versa).
func TestRedTeamCLINameGrammarParity(t *testing.T) {
	for _, name := range []string{
		"A", strings.Repeat("A", 64), strings.Repeat("A", 65),
		"has-dash", "has space", "1leading", "_ok", "A}}", "{{cuttle:X}}", "", "A\n",
		"Ａ", "A",
	} {
		cliOK := checkSecretName(name) == nil
		t.Logf("%-20q cli=%v", name, cliOK)
	}
}
