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
	if !errors.Is(err, errJevTextPair) {
		t.Fatalf("got %v, want errJevTextPair", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error echoed the value: %v", err)
	}
}

// The task is the one argument every run needs, so it is takeable the way a
// shell user reaches for it first - as the argument - with --task still canonical.
func TestJevBrowseTakesTheTaskAsAnArgument(t *testing.T) {
	got, err := jevTask("", []string{"find the support phone number"})
	if err != nil {
		t.Fatalf("jevTask: %v", err)
	}
	if got != "find the support phone number" {
		t.Errorf("task: got %q", got)
	}
	cmd := newJevBrowseCmd()
	if err := cmd.Args(cmd, []string{"one task", "another"}); err == nil {
		t.Error("a second positional argument was accepted")
	}
}

// Both spellings at once is a typo, and silently preferring one of them would
// run a task the caller did not mean to run.
func TestJevBrowseRefusesTheTaskTwice(t *testing.T) {
	if _, err := jevTask("sign in", []string{"find the support phone number"}); !errors.Is(err, errJevTaskTwice) {
		t.Errorf("got %v, want errJevTaskTwice", err)
	}
	if _, err := jevTask("", nil); !errors.Is(err, errJevTaskAbsent) {
		t.Errorf("got %v, want errJevTaskAbsent", err)
	}
}

// The help is where an agent learns the contract, so it has to carry the two
// things that are not guessable: where the key comes from, and what the exit
// codes mean.
func TestJevBrowseHelpNamesTheKeyEnvAndTheExitCodes(t *testing.T) {
	long := newJevBrowseCmd().Long
	for _, want := range []string{jev.APIKeyEnv, "Exit codes:", "cuttle pw", "EXPERIMENTAL", "--url is the page to start from"} {
		if !strings.Contains(long, want) {
			t.Errorf("the help does not mention %q", want)
		}
	}
	if !strings.Contains(newJevBrowseCmd().Short, "experimental") {
		t.Error("the short help does not mark the loop experimental")
	}
	if strings.Contains(long, "--endpoint") {
		t.Error("the help must not suggest pointing the driver at another browser")
	}
}

// jev re-gotos after every `go-back` to work around a ref-poisoning defect in
// the pinned driver (see remintAfterBack in internal/jev/loop.go). A pin bump
// must decide consciously whether that workaround still earns its place.
func TestJevBackWorkaroundTracksTheDriverPin(t *testing.T) {
	if BundledPlaywrightCLIVersion != "0.1.20" {
		t.Errorf("the bundled playwright-cli moved to %s: re-check the go-back ref defect that "+
			"jev's remintAfterBack works around, drop the workaround if it is fixed, then update this test",
			BundledPlaywrightCLIVersion)
	}
}
