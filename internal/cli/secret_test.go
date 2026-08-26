package cli

import (
	"errors"
	"strings"
	"testing"
)

// A password with a stray newline fails a login in a way nothing explains, and
// every natural way to pipe one in appends exactly one.
func TestReadSecretStdinStripsOneTrailingNewline(t *testing.T) {
	for in, want := range map[string]string{
		"hunter2\n":      "hunter2",
		"hunter2\r\n":    "hunter2",
		"hunter2":        "hunter2",
		"two words\n":    "two words",
		"keeps\ninner\n": "keeps\ninner",
	} {
		got, err := readSecretStdin(strings.NewReader(in))
		if err != nil {
			t.Fatalf("readSecretStdin(%q): %v", in, err)
		}
		if string(got) != want {
			t.Errorf("readSecretStdin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadSecretStdinRefusesNothing(t *testing.T) {
	for _, in := range []string{"", "\n", "\r\n"} {
		if _, err := readSecretStdin(strings.NewReader(in)); !errors.Is(err, errSecretNoInput) {
			t.Errorf("readSecretStdin(%q) error = %v, want errSecretNoInput", in, err)
		}
	}
}

// `set` without --stdin must not fall back to reading argv - that is the whole
// point of the verb.
func TestSecretSetRequiresStdinFlag(t *testing.T) {
	cmd := newSecretSetCmd()
	cmd.SetArgs([]string{"GH_PASS"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); !errors.Is(err, errSecretNeedStdin) {
		t.Fatalf("error = %v, want errSecretNeedStdin", err)
	}
}
