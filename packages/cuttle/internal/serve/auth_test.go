package serve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFoldCookieDomains(t *testing.T) {
	got := foldCookieDomains([]rawCookie{
		{Domain: ".example.com", Expires: 2000},
		{Domain: ".example.com", Expires: 1000},
		{Domain: ".example.com", Expires: -1},
		{Domain: "other.test", Expires: -1},
	})
	if len(got) != 2 {
		t.Fatalf("folded to %d domains, want 2: %+v", len(got), got)
	}
	if got[0].Domain != ".example.com" || got[0].Cookies != 3 || got[0].Session != 1 {
		t.Fatalf("example.com folded to %+v", got[0])
	}
	if !strings.HasPrefix(got[0].SoonestExpiry, "1970-01-01T00:16:40") {
		t.Fatalf("soonest expiry = %q, want the EARLIEST non-session expiry", got[0].SoonestExpiry)
	}
	if got[1].SoonestExpiry != "" {
		t.Fatalf("a domain with only session cookies has no expiry: %+v", got[1])
	}
}

// The cookie that actually carries a session is usually on the parent domain, so
// asking about app.example.com must report .example.com too.
func TestFilterDomainsMatchesParents(t *testing.T) {
	domains := []originAuth{{Domain: ".example.com"}, {Domain: "app.example.com"}, {Domain: "other.test"}}
	got := filterDomains(domains, "https://app.example.com/login")
	if len(got) != 2 {
		t.Fatalf("filtered to %+v, want the exact domain and its parent", got)
	}
	if len(filterDomains(domains, "nothing.test")) != 0 {
		t.Fatal("an unrelated origin must match nothing")
	}
}

func TestHostOfOriginArg(t *testing.T) {
	for in, want := range map[string]string{
		"https://app.example.com/login": "app.example.com",
		"app.example.com":               "app.example.com",
		"http://x.test:8080":            "x.test",
		"x.test.":                       "x.test",
	} {
		if got := hostOfOriginArg(in); got != want {
			t.Errorf("hostOfOriginArg(%q) = %q, want %q", in, got, want)
		}
	}
}

type handoverProcess struct {
	*fakeProcess
	id int
}

func (p *handoverProcess) pid() int { return p.id }

func handoverPool() *chromePool {
	return &chromePool{processes: map[string]*chromeInstance{
		reservedSeed: {process: &handoverProcess{newFakeProcess(), 101}},
		"other":      {process: &handoverProcess{newFakeProcess(), 202}},
	}}
}

func fakeXdotool(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\ngetwindowfocus) cat \"$FOCUS_FILE\";;\nsearch) echo 7;;\nwindowactivate) echo \"$*\" >> \"$RAISE_FILE\";;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "xdotool"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	focus := filepath.Join(dir, "focus")
	t.Setenv("FOCUS_FILE", focus)
	t.Setenv("RAISE_FILE", filepath.Join(dir, "raises"))
	return focus
}

func TestFocusedInstanceFollowsXAndCaches(t *testing.T) {
	focus := fakeXdotool(t)
	setPID := func(pid string) {
		t.Helper()
		if err := os.WriteFile(focus, []byte(pid), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := handoverPool()
	setPID("202")
	if got := p.focusedInstance(t.Context()); got != p.processes["other"] {
		t.Fatal("did not select X-focused browser")
	}
	setPID("101")
	if got := p.focusedInstance(t.Context()); got != p.processes["other"] {
		t.Fatal("focus lookup was not cached")
	}
	p.handover.focusUntil = time.Now().Add(-time.Second)
	if got := p.focusedInstance(t.Context()); got != p.processes[reservedSeed] {
		t.Fatal("expired focus cache did not refresh")
	}
	for _, pid := range []string{"999", "bad", "0"} {
		setPID(pid)
		p.handover.focusUntil = time.Time{}
		if got := p.focusedInstance(t.Context()); got != p.processes[reservedSeed] {
			t.Fatalf("PID %s did not fall back to main", pid)
		}
	}
	setPID("202")
	p.handover.focusUntil = time.Time{}
	p.processes["other"].process.(*handoverProcess).crash()
	if got := p.focusedInstance(t.Context()); got != p.processes[reservedSeed] {
		t.Fatal("selected dead process")
	}
}

func TestRaiseSeedAndRestoreFront(t *testing.T) {
	fakeXdotool(t)
	p := handoverPool()
	if !p.raiseSeed(t.Context(), "other") || p.handover.front != "other" {
		t.Fatal("raise did not record front seed")
	}
	if !p.restoreFrontSeed(t.Context()) || p.handover.front != "other" {
		t.Fatal("restore lost front seed")
	}
	if p.raiseSeed(t.Context(), "missing") || p.handover.front != "other" {
		t.Fatal("failed raise changed front seed")
	}
	p.processes["other"].process.(*handoverProcess).crash()
	if !p.restoreFrontSeed(t.Context()) || p.handover.front != reservedSeed {
		t.Fatal("restore did not fall back to main")
	}
	if !p.raiseSeed(t.Context(), "main") {
		t.Fatal("public main name not normalized")
	}
	t.Setenv("PATH", t.TempDir())
	if p.raiseSeed(t.Context(), reservedSeed) {
		t.Fatal("raise succeeded without xdotool")
	}
}

func TestRaiseSeedBoundsWindowWait(t *testing.T) {
	focus := fakeXdotool(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(focus), "xdotool"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := handoverPool()
	started := time.Now()
	if p.raiseSeed(t.Context(), "other") {
		t.Fatal("raise unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > raiseTimeout+2*time.Second {
		t.Fatalf("raise exceeded its timeout: %v", elapsed)
	}
	if p.handover.front != "" {
		t.Fatal("failed raise changed front")
	}
}
