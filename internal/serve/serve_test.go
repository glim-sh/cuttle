package serve

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

// parseServeArgs drives the real serve command's flag parsing + env fallback,
// then splits the Chrome passthrough exactly as RunE does.
func parseServeArgs(t *testing.T, args []string) (serveConfig, []string) {
	t.Helper()
	cmd := newServeCmd()
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	pass := []string{}
	if n := cmd.Flags().ArgsLenAtDash(); n >= 0 {
		pass = cmd.Flags().Args()[n:]
	}
	cfg, err := serveConfigFromFlags(cmd.Flags())
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg, pass
}

func TestHumanizeDefaultsOn(t *testing.T) {
	if cfg, _ := parseServeArgs(t, nil); !cfg.humanize {
		t.Fatal("humanize should default on")
	}
	if cfg, _ := parseServeArgs(t, []string{"--humanize=false"}); cfg.humanize {
		t.Fatal("--humanize=false should disable humanize")
	}
	t.Setenv("CUTTLE_HUMANIZE", "0")
	if cfg, _ := parseServeArgs(t, nil); cfg.humanize {
		t.Fatal("CUTTLE_HUMANIZE=0 should disable humanize")
	}
}

func TestParseIdleTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"zero", "0", 0, false},
		{"disabled-word", "disabled", 0, false},
		{"off", "off", 0, false},
		{"none", "none", 0, false},
		{"seconds", "30", 30 * time.Second, false},
		{"fractional", "1.5", 1500 * time.Millisecond, false},
		{"negative", "-1", 0, true},
		{"garbage", "soon", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseIdleTimeout(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestServeFlags(t *testing.T) {
	t.Setenv(proxyEnv, "")
	t.Setenv(ephemeralEnv, "")
	t.Setenv(idleTimeoutEnv, "")
	t.Setenv("HOME", "/home/tester")

	cfg, passthrough := parseServeArgs(t, []string{
		"--port=9333",
		"--data-dir=/data",
		"--idle-timeout=45",
		"--keep-profile",
		"--ephemeral",
		"--proxy=http://user:pass@proxy.example:8080",
		"--fingerprint=abc",
		"--fingerprint-locale=en-GB",
		"--fingerprint-timezone=Europe/London",
		"--headless=false",
		"--", // Chrome passthrough is strictly what follows the dash.
		"--some-chrome-flag",
	})
	if cfg.port != 9333 {
		t.Errorf("port=%d want 9333", cfg.port)
	}
	if cfg.dataDir != "/data" {
		t.Errorf("dataDir=%q", cfg.dataDir)
	}
	if cfg.idleTimeout != 45*time.Second {
		t.Errorf("idleTimeout=%v", cfg.idleTimeout)
	}
	if !cfg.keepProfile || !cfg.ephemeral {
		t.Errorf("keepProfile=%v ephemeral=%v", cfg.keepProfile, cfg.ephemeral)
	}
	if cfg.proxy != "http://user:pass@proxy.example:8080" {
		t.Errorf("proxy=%q", cfg.proxy)
	}
	if cfg.defaultSeed != "abc" || cfg.defaultLocale != "en-GB" || cfg.defaultTimezone != "Europe/London" {
		t.Errorf("fingerprint defaults: %q %q %q", cfg.defaultSeed, cfg.defaultLocale, cfg.defaultTimezone)
	}
	if cfg.headless {
		t.Errorf("headless should be false")
	}
	// Only what follows `--` is Chrome passthrough now; --headless is a real flag.
	if want := []string{"--some-chrome-flag"}; !slices.Equal(passthrough, want) {
		t.Fatalf("passthrough=%v want %v", passthrough, want)
	}
	// The headed daemon re-adds --headless=false to the Chrome argv (preserved).
	chrome := chromePassthrough(cfg, passthrough)
	if !slices.Contains(chrome, "--headless=false") || !slices.Contains(chrome, "--some-chrome-flag") {
		t.Fatalf("chrome passthrough missing expected flags: %v", chrome)
	}
}

func TestChromePassthroughHeadlessOmitsFlag(t *testing.T) {
	t.Parallel()
	got := chromePassthrough(serveConfig{headless: true}, []string{"--foo"})
	if slices.Contains(got, "--headless=false") {
		t.Fatalf("headless run should not inject --headless=false: %v", got)
	}
}

func TestServeEnvDefaults(t *testing.T) {
	t.Setenv(proxyEnv, "http://env-proxy:3128")
	t.Setenv(ephemeralEnv, "true")
	t.Setenv(idleTimeoutEnv, "60")
	t.Setenv("CUTTLE_KEEP_PROFILE", "yes") // lenient bool form preserved
	t.Setenv("HOME", "/home/tester")

	cfg, _ := parseServeArgs(t, nil)
	if cfg.proxy != "http://env-proxy:3128" {
		t.Errorf("proxy from env=%q", cfg.proxy)
	}
	if !cfg.ephemeral {
		t.Errorf("ephemeral from env not set")
	}
	if cfg.idleTimeout != 60*time.Second {
		t.Errorf("idleTimeout from env=%v", cfg.idleTimeout)
	}
	if !cfg.keepProfile {
		t.Errorf("keep-profile from CUTTLE_KEEP_PROFILE=yes not set")
	}
	// A CLI flag overrides the env fallback.
	cfg2, _ := parseServeArgs(t, []string{"--proxy=http://cli-proxy:8888"})
	if cfg2.proxy != "http://cli-proxy:8888" {
		t.Errorf("cli proxy override=%q", cfg2.proxy)
	}
}

func TestServeRejectsUnknownFlag(t *testing.T) {
	t.Parallel()
	cmd := newServeCmd()
	if err := cmd.Flags().Parse([]string{"--remote-debugging-port=1"}); err == nil {
		t.Fatal("expected an unknown-flag error under strict parsing")
	}
}

func TestValidSeed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		seed string
		want bool
	}{
		{"abc123", true},
		{"seed_with-dashes", true},
		{"__default__", false},
		{"", false},
		{"has space", false},
		{"has/slash", false},
		{"has.dot", false},
	}
	for _, tc := range tests {
		if got := validSeed(tc.seed); got != tc.want {
			t.Errorf("validSeed(%q)=%v want %v", tc.seed, got, tc.want)
		}
	}
}

func TestBindHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		probe envProbe
		want  string
	}{
		{
			name: "env override wins",
			probe: envProbe{
				getenv: func(k string) string {
					if k == hostEnv {
						return "10.0.0.5"
					}
					return ""
				},
				stat:     func(string) bool { return true },
				readFile: func(string) ([]byte, error) { return nil, nil },
			},
			want: "10.0.0.5",
		},
		{
			name: "dockerenv marker -> 0.0.0.0",
			probe: envProbe{
				getenv:   func(string) string { return "" },
				stat:     func(p string) bool { return p == "/.dockerenv" },
				readFile: func(string) ([]byte, error) { return nil, errFakeNoFile },
			},
			want: "0.0.0.0",
		},
		{
			name: "kubernetes env -> 0.0.0.0",
			probe: envProbe{
				getenv: func(k string) string {
					if k == "KUBERNETES_SERVICE_HOST" {
						return "10.96.0.1"
					}
					return ""
				},
				stat:     func(string) bool { return false },
				readFile: func(string) ([]byte, error) { return nil, errFakeNoFile },
			},
			want: "0.0.0.0",
		},
		{
			name: "containerd cgroup (no marker files) -> 0.0.0.0",
			probe: envProbe{
				getenv:   func(string) string { return "" },
				stat:     func(string) bool { return false },
				readFile: func(string) ([]byte, error) { return []byte("0::/kubepods/pod123/abc"), nil },
			},
			want: "0.0.0.0",
		},
		{
			name: "bare metal -> loopback",
			probe: envProbe{
				getenv:   func(string) string { return "" },
				stat:     func(string) bool { return false },
				readFile: func(string) ([]byte, error) { return []byte("0::/user.slice/session.scope"), nil },
			},
			want: "127.0.0.1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := bindHost(tc.probe); got != tc.want {
				t.Errorf("bindHost=%q want %q", got, tc.want)
			}
		})
	}
}

// The whole point of the warning is that "0.0.0.0:9222" does not, on its own,
// tell an operator the surface is unauthenticated. Assert it fires only for a
// wide bind and that it carries the remedy, not just the fact.
func TestWarnWideBind(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		wantWarn bool
	}{
		{name: "loopback v4 stays quiet", host: "127.0.0.1"},
		{name: "loopback v6 stays quiet", host: "::1"},
		{name: "localhost stays quiet", host: "localhost"},
		{name: "wildcard v4 warns", host: "0.0.0.0", wantWarn: true},
		{name: "wildcard v6 warns", host: "::", wantWarn: true},
		{name: "routable address warns", host: "10.0.0.5", wantWarn: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			orig := logger
			logger = slog.New(slog.NewTextHandler(&buf, nil))
			defer func() { logger = orig }()

			warnWideBind(tc.host, 9222)

			got := buf.String()
			if !tc.wantWarn {
				if got != "" {
					t.Errorf("expected no warning for %q, got %q", tc.host, got)
				}
				return
			}
			if got == "" {
				t.Fatalf("expected a warning for %q, got none", tc.host)
			}
			for _, want := range []string{"no authentication", "127.0.0.1:9222", "CUTTLE_HOST"} {
				if !strings.Contains(got, want) {
					t.Errorf("warning for %q missing %q; got %q", tc.host, want, got)
				}
			}
		})
	}
}

func TestDefaultDataDir(t *testing.T) {
	t.Parallel()
	container := envProbe{
		getenv:   func(string) string { return "" },
		stat:     func(p string) bool { return p == "/.dockerenv" },
		readFile: func(string) ([]byte, error) { return nil, errFakeNoFile },
	}
	if got := defaultDataDir(container); got != "/data" {
		t.Errorf("container dataDir=%q", got)
	}
	bare := envProbe{
		getenv:   func(string) string { return "" },
		stat:     func(string) bool { return false },
		readFile: func(string) ([]byte, error) { return nil, errFakeNoFile },
		homeDir:  func() (string, error) { return "/home/tester", nil },
	}
	if got := defaultDataDir(bare); got != "/home/tester/.local/share/cuttle/serve" {
		t.Errorf("bare dataDir=%q", got)
	}
	xdgSet := envProbe{
		getenv:   func(k string) string { return map[string]string{"XDG_DATA_HOME": "/xdg/data"}[k] },
		stat:     func(string) bool { return false },
		readFile: func(string) ([]byte, error) { return nil, errFakeNoFile },
		homeDir:  func() (string, error) { return "/home/tester", nil },
	}
	if got := defaultDataDir(xdgSet); got != "/xdg/data/cuttle/serve" {
		t.Errorf("xdg dataDir=%q", got)
	}
}

func TestShmSizeMB(t *testing.T) {
	t.Parallel()
	// Real /proc/mounts lines, copied from the runtime image at both sizes.
	const dflt = "shm /dev/shm tmpfs rw,nosuid,nodev,noexec,relatime,size=65536k,inode64 0 0\n"
	const big = "shm /dev/shm tmpfs rw,nosuid,nodev,noexec,relatime,size=2097152k,inode64 0 0\n"
	for _, tc := range []struct {
		name   string
		mounts string
		wantMB uint64
		wantOK bool
	}{
		{"docker default 64m", dflt, 64, true},
		{"shm-size=2g", big, 2048, true},
		{"no /dev/shm line", "proc /proc proc rw 0 0\n", 0, false},
		// No size= means the kernel default of half of RAM, which is fine and
		// must not warn.
		{"no size option", "shm /dev/shm tmpfs rw,nosuid 0 0\n", 0, false},
		// /dev/shm must not be matched by a substring of another mount point.
		{"lookalike mount point", "shm /dev/shmfoo tmpfs rw,size=65536k 0 0\n", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mb, ok := shmSizeMB(tc.mounts)
			if mb != tc.wantMB || ok != tc.wantOK {
				t.Errorf("shmSizeMB() = (%d, %v), want (%d, %v)", mb, ok, tc.wantMB, tc.wantOK)
			}
		})
	}
}

// shmSizeMB had unit tests; warnSmallShm itself had none, and the gap hid a
// startup panic - run() called it with a zero envProbe whose funcs are all nil,
// so inContainer() dereferenced nil and the daemon died before it listened.
func TestWarnSmallShm(t *testing.T) {
	const dflt = "shm /dev/shm tmpfs rw,size=65536k 0 0\n"
	for _, tc := range []struct {
		name  string
		probe envProbe
		want  bool
	}{
		{
			name: "container with 64m shm warns",
			probe: envProbe{
				stat:     func(p string) bool { return p == "/.dockerenv" },
				getenv:   func(string) string { return "" },
				readFile: func(string) ([]byte, error) { return []byte(dflt), nil },
			},
			want: true,
		},
		{
			name: "bare metal never warns",
			probe: envProbe{
				stat:     func(string) bool { return false },
				getenv:   func(string) string { return "" },
				readFile: func(string) ([]byte, error) { return nil, errFakeNoFile },
			},
			want: false,
		},
		{
			name: "container with 2g shm stays quiet",
			probe: envProbe{
				stat:     func(p string) bool { return p == "/.dockerenv" },
				getenv:   func(string) string { return "" },
				readFile: func(string) ([]byte, error) { return []byte("shm /dev/shm tmpfs rw,size=2097152k 0 0\n"), nil },
			},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := logger
			logger = slog.New(slog.NewTextHandler(&buf, nil))
			defer func() { logger = prev }()

			warnSmallShm(tc.probe)
			if got := strings.Contains(buf.String(), "/dev/shm is"); got != tc.want {
				t.Errorf("warned=%v want=%v log=%q", got, tc.want, buf.String())
			}
		})
	}
}

// The probe run() actually passes must be fully populated: every field is a
// func, and a nil one panics on the first call.
func TestDefaultEnvProbeIsComplete(t *testing.T) {
	e := defaultEnvProbe()
	if e.stat == nil || e.getenv == nil || e.readFile == nil || e.homeDir == nil {
		t.Fatal("defaultEnvProbe left a nil field")
	}
	for _, use := range []func(){
		func() { bindHost(e) },
		func() { warnSmallShm(e) },
		func() { defaultDataDir(e) },
	} {
		use()
	}
}
