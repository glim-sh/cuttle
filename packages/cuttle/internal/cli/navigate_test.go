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
//
// The context is one that cannot resolve, so resolveRunning would fail with its
// own error: getting errBadPredicate back is what proves the parse ran BEFORE
// the session was touched. Asserting only "some error mentions the predicate"
// would pass with the parse moved back down, on any machine with a live session.
func TestOpenValidatesThePredicateBeforeTouchingAnything(t *testing.T) {
	var out, errOut strings.Builder
	cmd := newOpenCmd()
	cmd.SetArgs([]string{"https://example.com", "--until", "bogus", "--context", "no-such-context-exists"})
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if !errors.Is(err, errBadPredicate) {
		t.Fatalf("error = %v, want errBadPredicate - the predicate must be parsed before the session is resolved", err)
	}
	if out.Len() != 0 {
		t.Fatalf("a refused predicate printed %q; nothing may happen before it is parsed", out.String())
	}
}
