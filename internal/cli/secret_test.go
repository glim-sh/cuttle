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

// `set` with no source must not fall back to reading a value from argv - that is
// the whole point of the verb - and it must not silently pick one of two.
func TestSecretSetNeedsExactlyOneSource(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want error
	}{
		"no source": {[]string{"GH_PASS"}, errSecretNeedSource},
		"both":      {[]string{"GH_PASS", "--stdin", "--exec", "echo hi"}, errSecretBothInputs},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := newSecretSetCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(&strings.Builder{})
			cmd.SetErr(&strings.Builder{})
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			if err := cmd.Execute(); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// A resolver runs on the host and its stdout IS the value; its stderr never
// propagates, because vault error text quotes item names and partial values.
func TestResolveExec(t *testing.T) {
	got, err := resolveExec(t.Context(), "printf 'hunter2\n'")
	if err != nil {
		t.Fatalf("resolveExec: %v", err)
	}
	if string(got) != "hunter2" {
		t.Fatalf("value = %q, want hunter2 with the trailing newline stripped", got)
	}
	if _, emptyErr := resolveExec(t.Context(), "printf ''"); !errors.Is(emptyErr, errSecretExecEmpty) {
		t.Fatalf("empty resolver error = %v, want errSecretExecEmpty", emptyErr)
	}
	_, err = resolveExec(t.Context(), "echo 'ERROR: item \"prod db\" has secret abc' >&2; exit 1")
	if !errors.Is(err, errSecretExecFailed) {
		t.Fatalf("failing resolver error = %v, want errSecretExecFailed", err)
	}
	if strings.Contains(err.Error(), "prod db") || strings.Contains(err.Error(), "abc") {
		t.Fatalf("the resolver's stderr reached the error text: %v", err)
	}
}
