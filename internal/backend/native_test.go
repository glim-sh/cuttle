package backend

import (
	"os"
	"strings"
	"testing"
)

func TestCmdlineTargetsDir(t *testing.T) {
	const base = "/data/cuttle/native/cuttle/profile"
	cases := []struct {
		name    string
		cmdline string
		dir     string
		want    bool
	}{
		{
			name:    "exact match, dir at end of argv",
			cmdline: "/opt/Chromium --headless=false --user-data-dir=" + base + "/a",
			dir:     base + "/a",
			want:    true,
		},
		{
			name:    "exact match, dir followed by another flag",
			cmdline: "--user-data-dir=" + base + "/a --remote-debugging-port=5100",
			dir:     base + "/a",
			want:    true,
		},
		{
			name:    "prefix seed must not cross-match (a vs ab)",
			cmdline: "--user-data-dir=" + base + "/ab --remote-debugging-port=5101",
			dir:     base + "/a",
			want:    false,
		},
		{
			name:    "different seed does not match",
			cmdline: "--user-data-dir=" + base + "/beta",
			dir:     base + "/a",
			want:    false,
		},
		{
			name:    "no user-data-dir flag",
			cmdline: "/opt/Chromium --headless=false",
			dir:     base + "/a",
			want:    false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cmdlineTargetsDir(c.cmdline, c.dir); got != c.want {
				t.Errorf("cmdlineTargetsDir(%q, %q) = %v, want %v", c.cmdline, c.dir, got, c.want)
			}
		})
	}
}

func TestWithinDir(t *testing.T) {
	base := "/cache/clark/tag"
	cases := []struct {
		path string
		want bool
	}{
		{"/cache/clark/tag/Chromium.app/Contents/MacOS/Chromium", true},
		{"/cache/clark/tag", true},
		{"/cache/clark/tag/../tag2/evil", false},
		{"/etc/passwd", false},
	}
	for _, c := range cases {
		if got := withinDir(base, c.path); got != c.want {
			t.Errorf("withinDir(%q, %q) = %v, want %v", base, c.path, got, c.want)
		}
	}
}

func TestLaunchFailureEADDRINUSE(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	n := &Native{name: "inst", cdpPort: 9270}
	if err := os.MkdirAll(n.stateDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	log := "INFO CDP multiplexer starting on 127.0.0.1:9270\n" +
		"cuttle: listen tcp 127.0.0.1:9270: bind: address already in use\n"
	if err := os.WriteFile(n.logPath(), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	err := n.launchFailure()
	if err == nil || !strings.Contains(err.Error(), "port 9270 is already in use") {
		t.Fatalf("want a port-in-use error, got %v", err)
	}
}

func TestListNative(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for _, name := range []string{"a", "b"} {
		if err := os.MkdirAll((&Native{name: name}).stateDir(), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// A stale pidfile (dead pid) must read as stopped, not running.
	if err := (&Native{name: "a"}).writeState(nativeState{PID: 2000000000}); err != nil {
		t.Fatal(err)
	}
	insts, err := ListNative()
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 2 {
		t.Fatalf("want 2 instances, got %d", len(insts))
	}
	for _, in := range insts {
		if in.Running {
			t.Errorf("%s should read as stopped", in.Name)
		}
	}
}
