package cli

import (
	"errors"
	"strings"
	"testing"
)

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"https://x.example*", "https://x.example/login?next=/a", true},
		{"https://x.example*", "https://y.example/login", false},
		// path.Match would fail this one: it will not let a * cross a slash, which
		// is wrong for URLs.
		{"https://x.example/*/done", "https://x.example/a/b/done", true},
		{"*dashboard*", "https://x.example/app/dashboard/home", true},
		{"exact", "exact", true},
		{"exact", "exactly", false},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.s); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

func TestParsePredicate(t *testing.T) {
	// The default is "left the origin you opened", which is what finishing a
	// sign-in looks like from outside the page.
	p, err := parsePredicate("", "https://x.example/login?next=/a")
	if err != nil {
		t.Fatalf("default predicate: %v", err)
	}
	if p.kind != predGone || p.arg != "https://x.example*" || !p.implicit {
		t.Fatalf("default predicate = %+v, want gone: on the launch origin", p)
	}
	if p.holds("https://x.example/login", "", false) {
		t.Error("still on the sign-in origin must not satisfy gone:")
	}
	if !p.holds("https://app.example/home", "", false) {
		t.Error("leaving the origin must satisfy gone:")
	}

	for _, spec := range []string{"title:Dashboard", "url:https://x*", "js:1", "gone:https://x*"} {
		if _, err := parsePredicate(spec, ""); err != nil {
			t.Errorf("parsePredicate(%q): %v", spec, err)
		}
	}
	for _, spec := range []string{"nope:x", "title:", "no-colon"} {
		if _, err := parsePredicate(spec, ""); !errors.Is(err, errBadPredicate) {
			t.Errorf("parsePredicate(%q) error = %v, want errBadPredicate", spec, err)
		}
	}
	// With no URL to derive from there is no honest default to invent.
	if _, err := parsePredicate("", ""); !errors.Is(err, errBadPredicate) {
		t.Errorf("empty spec with no URL error = %v, want errBadPredicate", err)
	}
}

func TestPredicateHolds(t *testing.T) {
	if !(predicate{kind: predTitle, arg: "Dash"}).holds("", "My Dashboard", false) {
		t.Error("title: must match a substring of the title")
	}
	if !(predicate{kind: predJS, arg: "x"}).holds("", "", true) {
		t.Error("js: must follow the evaluated boolean")
	}
	if (predicate{kind: predURL, arg: "https://a*"}).holds("https://b/", "", true) {
		t.Error("url: must not be satisfied by an unrelated URL")
	}
}

// `open --until` navigates the session, raises a window and opens a viewer on
// someone's desktop. A typo in the predicate must not do all three first.
func TestOpenValidatesThePredicateBeforeTouchingAnything(t *testing.T) {
	cmd := newOpenCmd()
	cmd.SetArgs([]string{"https://example.com", "--until", "bogus"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); !errors.Is(err, errBadPredicate) {
		t.Fatalf("error = %v, want errBadPredicate before any side effect", err)
	}
}
