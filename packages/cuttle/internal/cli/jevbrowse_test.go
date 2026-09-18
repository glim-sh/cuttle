package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/glim-sh/cuttle/internal/jev"
)

func TestParseTextValues(t *testing.T) {
	values, err := parseTextValues([]string{"user=qa@example.com", "pass={{cuttle:QA_PASS}}", "blank="})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The sentinel has to survive byte for byte: cuttle substitutes it inside its
	// own CDP frame, on the fill path, so anything that rewrites it here types a
	// literal into the field instead.
	if got := values["pass"]; got != "{{cuttle:QA_PASS}}" {
		t.Errorf("sentinel: got %q", got)
	}
	// A value can legitimately contain '=' - only the FIRST one separates.
	more, err := parseTextValues([]string{"token=a=b=c"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := more["token"]; got != "a=b=c" {
		t.Errorf("value: got %q, want a=b=c", got)
	}
	if _, ok := values["blank"]; !ok {
		t.Error("an empty value is still a value")
	}
}

// The error for a malformed pair must not echo the half that may be a password.
func TestParseTextValuesRejectsAPairWithNoNameWithoutEchoingIt(t *testing.T) {
	_, err := parseTextValues([]string{"hunter2"})
	if !errors.Is(err, errBadValue) {
		t.Fatalf("got %v, want errBadValue", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error echoed the value: %v", err)
	}
}

// The help is where an agent learns the contract, so it has to carry the two
// things that are not guessable: where the key comes from, and what the exit
// codes mean.
func TestJevBrowseHelpNamesTheKeyEnvAndTheExitCodes(t *testing.T) {
	long := newJevBrowseCmd().Long
	for _, want := range []string{jev.APIKeyEnv, "Exit codes:", "cuttle pw"} {
		if !strings.Contains(long, want) {
			t.Errorf("the help does not mention %q", want)
		}
	}
	if strings.Contains(long, "--endpoint") {
		t.Error("the help must not suggest pointing the driver at another browser")
	}
}
